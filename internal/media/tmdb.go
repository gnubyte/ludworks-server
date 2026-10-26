package media

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/media/naming"
)

// TMDB (The Movie Database) v3 integration. The API key is the secret media.tmdb_api_key
// (a v3 key, sent as api_key, or a v4 read access token, sent as a Bearer token). Every
// request has a 10 s timeout and capped response sizes; images are re-encoded locally.
const (
	tmdbTimeout     = 10 * time.Second
	maxTMDBJSON     = 2 << 20
	maxTMDBImage    = 10 << 20
	maxTMDBResults  = 20
	tmdbPosterSize  = "w780"
	tmdbBackdropSz  = "w1280"
	tmdbPreviewSize = "w185"
)

type tmdbConfig struct {
	apiBase   string // https://api.themoviedb.org/3
	imageBase string // https://image.tmdb.org/t/p
	client    *http.Client
}

func defaultTMDB() tmdbConfig {
	return tmdbConfig{apiBase: "https://api.themoviedb.org/3", imageBase: "https://image.tmdb.org/t/p", client: newTMDBClient()}
}

// newTMDBClient follows at most 3 redirects and only to the original host.
func newTMDBClient() *http.Client {
	return &http.Client{
		Timeout: tmdbTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 || req.URL.Host != via[0].URL.Host || req.URL.Scheme != via[0].URL.Scheme {
				return errors.New("tmdb: refusing redirect")
			}
			return nil
		},
	}
}

var (
	tmdbKeyRe       = regexp.MustCompile(`^[A-Za-z0-9._-]{16,1000}$`)
	tmdbImagePathRe = regexp.MustCompile(`^/[A-Za-z0-9_-]{1,128}\.(?:jpg|jpeg|png)$`)
)

func errTMDB(msg string) *core.Error { return core.NewError(http.StatusBadGateway, "tmdb_failed", msg) }

var errTMDBNoKey = core.NotImplemented("TMDB is not configured: set the TMDB API key in Admin → Settings → Media")

func (s *Service) tmdbKey(ctx context.Context) (string, error) {
	key := strings.TrimSpace(s.settings(ctx).TMDBAPIKey)
	if key == "" {
		return "", errTMDBNoKey
	}
	if !tmdbKeyRe.MatchString(key) {
		return "", errTMDB("The configured TMDB API key is malformed")
	}
	return key, nil
}

// tmdbGet fetches an API path into dst.
func (s *Service) tmdbGet(ctx context.Context, key, path string, q url.Values, dst any) error {
	if q == nil {
		q = url.Values{}
	}
	bearer := strings.HasPrefix(key, "eyJ") // v4 read access token (JWT)
	if !bearer {
		q.Set("api_key", key)
	}
	ctx, cancel := context.WithTimeout(ctx, tmdbTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.tmdb.apiBase+path+"?"+q.Encode(), nil)
	if err != nil {
		return errTMDB("tmdb: bad request")
	}
	req.Header.Set("Accept", "application/json")
	if bearer {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := s.tmdb.client.Do(req)
	if err != nil {
		if ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return errTMDB("TMDB did not answer in time")
		}
		return errTMDB("TMDB could not be reached")
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return core.NotFound("TMDB title not found")
	case resp.StatusCode == http.StatusUnauthorized:
		return errTMDB("TMDB rejected the API key")
	case resp.StatusCode == http.StatusTooManyRequests:
		return errTMDB("TMDB rate limit reached; try again shortly")
	case resp.StatusCode != http.StatusOK:
		return errTMDB(fmt.Sprintf("TMDB answered HTTP %d", resp.StatusCode))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTMDBJSON+1))
	if err != nil {
		return errTMDB("TMDB response could not be read")
	}
	if len(body) > maxTMDBJSON {
		return errTMDB("TMDB response too large")
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return errTMDB("TMDB response could not be parsed")
	}
	return nil
}

// tmdbImage downloads an image path (validated) at a TMDB size and normalizes it.
func (s *Service) tmdbImage(ctx context.Context, imgPath, size string, maxEdge int) ([]byte, error) {
	if !tmdbImagePathRe.MatchString(imgPath) {
		return nil, errTMDB("TMDB returned an invalid image path")
	}
	ctx, cancel := context.WithTimeout(ctx, tmdbTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.tmdb.imageBase+"/"+size+imgPath, nil)
	if err != nil {
		return nil, errTMDB("tmdb: bad image request")
	}
	resp, err := s.tmdb.client.Do(req)
	if err != nil {
		return nil, errTMDB("TMDB image could not be downloaded")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errTMDB(fmt.Sprintf("TMDB image download failed (HTTP %d)", resp.StatusCode))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxTMDBImage+1))
	if err != nil {
		return nil, errTMDB("TMDB image could not be downloaded")
	}
	if len(data) > maxTMDBImage {
		return nil, errTMDB("TMDB image too large")
	}
	select {
	case s.imgSem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-s.imgSem }()
	jpeg, err := normalizeImage(data, maxEdge)
	if err != nil {
		return nil, errTMDB("TMDB image could not be decoded")
	}
	return jpeg, nil
}

func yearOf(date string) *int64 {
	if len(date) >= 4 {
		if y, err := strconv.Atoi(date[:4]); err == nil && y >= 1800 && y <= 3000 {
			v := int64(y)
			return &v
		}
	}
	return nil
}

/* ------------------------------------------------------------------ search */

// TMDBResult is one normalized search hit.
type TMDBResult struct {
	TMDBID        int64   `json:"tmdb_id"`
	Type          string  `json:"type"`
	Title         string  `json:"title"`
	OriginalTitle string  `json:"original_title"`
	Year          *int64  `json:"year"`
	Overview      string  `json:"overview"`
	PosterURL     *string `json:"poster_url"`
}

type tmdbSearchResp struct {
	Results []struct {
		ID            int64  `json:"id"`
		Title         string `json:"title"`
		Name          string `json:"name"`
		OriginalTitle string `json:"original_title"`
		OriginalName  string `json:"original_name"`
		ReleaseDate   string `json:"release_date"`
		FirstAirDate  string `json:"first_air_date"`
		Overview      string `json:"overview"`
		PosterPath    string `json:"poster_path"`
	} `json:"results"`
}

func (s *Service) tmdbSearch(ctx context.Context, query, typ string, year int) ([]TMDBResult, error) {
	key, err := s.tmdbKey(ctx)
	if err != nil {
		return nil, err
	}
	q := url.Values{"query": {query}, "include_adult": {"false"}}
	if year > 0 {
		if typ == "tv" {
			q.Set("first_air_date_year", strconv.Itoa(year))
		} else {
			q.Set("year", strconv.Itoa(year))
		}
	}
	var resp tmdbSearchResp
	if err := s.tmdbGet(ctx, key, "/search/"+typ, q, &resp); err != nil {
		return nil, err
	}
	out := []TMDBResult{}
	for _, r := range resp.Results {
		if len(out) == maxTMDBResults || r.ID <= 0 {
			continue
		}
		res := TMDBResult{TMDBID: r.ID, Type: typ, Overview: cleanMultiline(r.Overview, maxDescription)}
		if typ == "tv" {
			res.Title, res.OriginalTitle, res.Year = cleanText(r.Name, maxTitle), cleanText(r.OriginalName, maxTitle), yearOf(r.FirstAirDate)
		} else {
			res.Title, res.OriginalTitle, res.Year = cleanText(r.Title, maxTitle), cleanText(r.OriginalTitle, maxTitle), yearOf(r.ReleaseDate)
		}
		if tmdbImagePathRe.MatchString(r.PosterPath) {
			res.PosterURL = strPtr("https://image.tmdb.org/t/p/" + tmdbPreviewSize + r.PosterPath)
		}
		out = append(out, res)
	}
	return out, nil
}

/* ---------------------------------------------------------------- identify */

type tmdbGenre struct {
	Name string `json:"name"`
}

type tmdbMovie struct {
	Title        string      `json:"title"`
	ReleaseDate  string      `json:"release_date"`
	Overview     string      `json:"overview"`
	Genres       []tmdbGenre `json:"genres"`
	PosterPath   string      `json:"poster_path"`
	BackdropPath string      `json:"backdrop_path"`
	IMDBID       string      `json:"imdb_id"`
	ReleaseDates struct {
		Results []struct {
			Country string `json:"iso_3166_1"`
			Dates   []struct {
				Certification string `json:"certification"`
			} `json:"release_dates"`
		} `json:"results"`
	} `json:"release_dates"`
}

type tmdbShow struct {
	Name           string      `json:"name"`
	FirstAirDate   string      `json:"first_air_date"`
	Overview       string      `json:"overview"`
	Genres         []tmdbGenre `json:"genres"`
	PosterPath     string      `json:"poster_path"`
	BackdropPath   string      `json:"backdrop_path"`
	ContentRatings struct {
		Results []struct {
			Country string `json:"iso_3166_1"`
			Rating  string `json:"rating"`
		} `json:"results"`
	} `json:"content_ratings"`
	ExternalIDs struct {
		IMDBID string `json:"imdb_id"`
	} `json:"external_ids"`
}

type tmdbEpisode struct {
	Name     string `json:"name"`
	Overview string `json:"overview"`
	AirDate  string `json:"air_date"`
}

// IdentifyRequest is POST /media/{id}/identify.
type IdentifyRequest struct {
	TMDBID      int64  `json:"tmdb_id"`
	Type        string `json:"type"`
	ApplyToShow bool   `json:"apply_to_show"`
}

func genreNames(gs []tmdbGenre) []string {
	names := make([]string, 0, len(gs))
	for _, g := range gs {
		names = append(names, g.Name)
	}
	out, _ := normalizeGenres(names)
	if out == nil {
		out = []string{}
	}
	return out
}

func nullYear(y *int64) sql.NullInt64 {
	if y == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *y, Valid: true}
}

// identify fills an item's metadata (and poster/backdrop) from a TMDB title.
func (s *Service) identify(ctx context.Context, p *core.Principal, fileID string, req IdentifyRequest) (Media, error) {
	fe := map[string]string{}
	if req.TMDBID < 1 || req.TMDBID > 1<<40 {
		fe["tmdb_id"] = "must be a positive integer"
	}
	if req.Type != "movie" && req.Type != "tv" {
		fe["type"] = "must be movie or tv"
	}
	if len(fe) > 0 {
		return Media{}, core.Validation(fe)
	}
	it, err := s.itemFor(ctx, p, fileID, needWrite)
	if err != nil {
		return Media{}, err
	}
	key, err := s.tmdbKey(ctx)
	if err != nil {
		return Media{}, err
	}
	id := strconv.FormatInt(req.TMDBID, 10)
	var posterPath, backdropPath string
	oldShowKey := it.ShowKey
	if req.Type == "movie" {
		var m tmdbMovie
		if err := s.tmdbGet(ctx, key, "/movie/"+id, url.Values{"append_to_response": {"release_dates"}}, &m); err != nil {
			return Media{}, err
		}
		it.Title = cleanText(m.Title, maxTitle)
		if it.Title == "" {
			return Media{}, errTMDB("TMDB returned a title without a name")
		}
		it.SortTitle = sortTitleFor(it.Title)
		it.Year = nullYear(yearOf(m.ReleaseDate))
		it.Description = cleanMultiline(m.Overview, maxDescription)
		it.Genres = genreNames(m.Genres)
		it.ContentRating = ""
		for _, c := range m.ReleaseDates.Results {
			if c.Country == "US" {
				for _, d := range c.Dates {
					if r := cleanText(d.Certification, maxRating); r != "" {
						it.ContentRating = r
						break
					}
				}
			}
		}
		it.ShowTitle, it.ShowKey, it.EpisodeTitle, it.ShowDescription = "", "", "", ""
		it.Season, it.Episode = sql.NullInt64{}, sql.NullInt64{}
		it.IMDBID = ""
		if imdbRe.MatchString(m.IMDBID) {
			it.IMDBID = m.IMDBID
		}
		posterPath, backdropPath = m.PosterPath, m.BackdropPath
	} else {
		var sh tmdbShow
		if err := s.tmdbGet(ctx, key, "/tv/"+id, url.Values{"append_to_response": {"content_ratings,external_ids"}}, &sh); err != nil {
			return Media{}, err
		}
		show := cleanText(sh.Name, maxTitle)
		if show == "" {
			return Media{}, errTMDB("TMDB returned a show without a name")
		}
		it.ShowTitle, it.ShowKey = show, naming.ShowKey(show)
		it.ShowDescription = cleanMultiline(sh.Overview, maxDescription)
		it.Genres = genreNames(sh.Genres)
		it.ContentRating = ""
		for _, c := range sh.ContentRatings.Results {
			if c.Country == "US" {
				it.ContentRating = cleanText(c.Rating, maxRating)
			}
		}
		it.IMDBID = ""
		if imdbRe.MatchString(sh.ExternalIDs.IMDBID) {
			it.IMDBID = sh.ExternalIDs.IMDBID
		}
		it.Year = nullYear(yearOf(sh.FirstAirDate))
		it.Description = it.ShowDescription
		if it.Season.Valid && it.Episode.Valid {
			var ep tmdbEpisode
			err := s.tmdbGet(ctx, key, "/tv/"+id+"/season/"+strconv.FormatInt(it.Season.Int64, 10)+"/episode/"+strconv.FormatInt(it.Episode.Int64, 10), nil, &ep)
			var ce *core.Error
			switch {
			case err == nil:
				it.EpisodeTitle = cleanText(ep.Name, maxTitle)
				if d := cleanMultiline(ep.Overview, maxDescription); d != "" {
					it.Description = d
				}
				if y := yearOf(ep.AirDate); y != nil {
					it.Year = nullYear(y)
				}
			case errors.As(err, &ce) && ce.Status == http.StatusNotFound:
			default:
				return Media{}, err
			}
		}
		switch {
		case it.EpisodeTitle != "":
			it.Title = it.EpisodeTitle
		case it.Season.Valid && it.Episode.Valid:
			it.Title = show + " S" + pad2(it.Season.Int64) + "E" + pad2(it.Episode.Int64)
		default:
			it.Title = show
		}
		it.SortTitle = sortTitleFor(it.Title)
		posterPath, backdropPath = sh.PosterPath, sh.BackdropPath
	}
	it.TMDBID = sql.NullInt64{Int64: req.TMDBID, Valid: true}

	var poster, backdrop []byte
	if posterPath != "" {
		if poster, err = s.tmdbImage(ctx, posterPath, tmdbPosterSize, maxPosterEdge); err != nil {
			return Media{}, err
		}
	}
	if backdropPath != "" {
		if backdrop, err = s.tmdbImage(ctx, backdropPath, tmdbBackdropSz, maxBackdropEdge); err != nil {
			s.log.Debug("media: tmdb backdrop", "err", err) // optional
			backdrop = nil
		}
	}
	if err := s.saveDescriptive(ctx, s.d.DB.SQL, it, srcTMDB); err != nil {
		return Media{}, err
	}
	if err := s.storeImages(ctx, it.FileID, poster, backdrop); err != nil {
		return Media{}, err
	}
	cat, sub := "movie", "feature"
	if req.Type == "tv" {
		cat, sub = "tv", "episode"
	}
	if it.File.Category != cat {
		if _, err := s.files.SetMeta(ctx, p, fileID, &cat, &sub, nil); err != nil {
			return Media{}, err
		}
	}
	if req.Type == "tv" && req.ApplyToShow && oldShowKey != "" {
		if err := s.applyShowIdentity(ctx, p, it, oldShowKey, poster, backdrop); err != nil {
			return Media{}, err
		}
	}
	return s.mediaJSON(ctx, p, fileID)
}

func (s *Service) storeImages(ctx context.Context, fileID string, poster, backdrop []byte) error {
	if poster != nil {
		if err := s.storeImage(ctx, fileID, kindPoster, poster); err != nil {
			return err
		}
	}
	if backdrop != nil {
		if err := s.storeImage(ctx, fileID, kindBackdrop, backdrop); err != nil {
			return err
		}
	}
	return nil
}

// applyShowIdentity copies the series-level identity of src to the other episodes of the
// same (previous) show in the same space.
func (s *Service) applyShowIdentity(ctx context.Context, p *core.Principal, src *item, oldShowKey string, poster, backdrop []byte) error {
	rs, err := s.d.DB.SQL.QueryContext(ctx, `SELECT m.file_id FROM media_items m JOIN files f ON f.id = m.file_id
		WHERE m.show_key = ? AND f.space_id = ? AND f.trashed_at IS NULL AND m.file_id <> ? LIMIT ?`,
		oldShowKey, src.File.SpaceID, src.FileID, maxShowEpisodes)
	if err != nil {
		return err
	}
	var ids []string
	for rs.Next() {
		var id string
		if err := rs.Scan(&id); err != nil {
			rs.Close()
			return err
		}
		ids = append(ids, id)
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		it, err := loadItem(ctx, s.d.DB.SQL, p.UserID, id)
		if err != nil {
			return err
		}
		if it == nil {
			continue
		}
		it.ShowTitle, it.ShowKey, it.ShowDescription = src.ShowTitle, src.ShowKey, src.ShowDescription
		it.Genres, it.ContentRating, it.TMDBID = src.Genres, src.ContentRating, src.TMDBID
		if it.EpisodeTitle == "" && it.Season.Valid && it.Episode.Valid {
			it.Title = src.ShowTitle + " S" + pad2(it.Season.Int64) + "E" + pad2(it.Episode.Int64)
			it.SortTitle = sortTitleFor(it.Title)
		}
		if err := s.saveDescriptive(ctx, s.d.DB.SQL, it, srcTMDB); err != nil {
			return err
		}
		if err := s.storeImages(ctx, id, poster, backdrop); err != nil {
			return err
		}
	}
	return nil
}
