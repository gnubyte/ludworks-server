package cast

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"ludworks/internal/cast/renderer"
	"ludworks/internal/core"
	"ludworks/internal/httpx"
	"ludworks/internal/media"
	"ludworks/internal/music"
	"ludworks/internal/tv"
)

// Item kinds (CastItem.kind).
const (
	KindMedia   = "media"
	KindTrack   = "track"
	KindChannel = "channel"
)

// maxCastSubtitles bounds the subtitle tracks offered to a device per item.
const maxCastSubtitles = 32

// errItemGone answers device-side callbacks (artwork, PCM) once the owner lost access.
var errItemGone = errors.New("cast: the item is no longer available")

// itemRef is a requested queue item.
type itemRef struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// castItem is one queue entry with its display metadata (API.md §14 CastItem).
type castItem struct {
	Kind, ID        string
	Title, Subtitle string
	ArtURL          string  // browser URL (API path); "" when none
	Duration        float64 // seconds; 0 = unknown
	Video           bool
}

// itemJSON is the CastItem JSON.
type itemJSON struct {
	Kind            string   `json:"kind"`
	ID              string   `json:"id"`
	Title           string   `json:"title"`
	Subtitle        string   `json:"subtitle"`
	ArtURL          *string  `json:"art_url"`
	DurationSeconds *float64 `json:"duration_seconds"`
}

func (it *castItem) json() itemJSON {
	out := itemJSON{Kind: it.Kind, ID: it.ID, Title: it.Title, Subtitle: it.Subtitle}
	if it.ArtURL != "" {
		u := it.ArtURL
		out.ArtURL = &u
	}
	if it.Duration > 0 {
		d := it.Duration
		out.DurationSeconds = &d
	}
	return out
}

func (it *castItem) key() string { return it.Kind + "/" + it.ID }

// scopeFor is the read scope a kind of item needs.
func scopeFor(kind string) string {
	switch kind {
	case KindMedia:
		return "media:read"
	case KindTrack:
		return "music:read"
	case KindChannel:
		return "tv:read"
	}
	return ""
}

// checkRefs validates the shape of requested items (field is the JSON field name, e.g.
// "items"), returning a 422 for the first problems found.
func checkRefs(field string, refs []itemRef, min int) error {
	fe := map[string]string{}
	switch {
	case len(refs) < min:
		fe[field] = fmt.Sprintf("must list %d to %d items", min, MaxQueue)
	case len(refs) > MaxQueue:
		fe[field] = fmt.Sprintf("at most %d items", MaxQueue)
	}
	for i, r := range refs {
		if len(fe) >= 5 {
			break
		}
		switch {
		case scopeFor(r.Kind) == "":
			fe[fmt.Sprintf("%s.%d.kind", field, i)] = "must be media, track or channel"
		case !httpx.ValidID(r.ID):
			fe[fmt.Sprintf("%s.%d.id", field, i)] = "is not a valid id"
		}
	}
	if len(fe) > 0 {
		return core.Validation(fe)
	}
	return nil
}

// resolveItems checks p's scopes and access for every ref and returns the queue items. A
// missing scope is 403, an item p cannot read (or that is not of its kind) 404.
func (m *Module) resolveItems(ctx context.Context, p *core.Principal, refs []itemRef) ([]*castItem, error) {
	for _, r := range refs {
		if sc := scopeFor(r.Kind); !p.Has(sc) {
			return nil, core.Forbidden("Missing required scope: " + sc)
		}
	}
	out := make([]*castItem, 0, len(refs))
	for i, r := range refs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		it, err := m.resolveItem(ctx, p, r)
		if err != nil {
			var ce *core.Error
			if errors.As(err, &ce) && ce.Status == http.StatusNotFound {
				return nil, core.NotFound(fmt.Sprintf("Item %d (%s %s) was not found", i, r.Kind, r.ID)).WithDetails(map[string]any{"index": i})
			}
			return nil, err
		}
		out = append(out, it)
	}
	return out, nil
}

// resolveItem resolves one item's display metadata for p.
func (m *Module) resolveItem(ctx context.Context, p *core.Principal, r itemRef) (*castItem, error) {
	switch r.Kind {
	case KindMedia:
		if m.media == nil {
			return nil, core.NotFound("Media item not found")
		}
		info, err := m.media.PlaybackInfo(ctx, p, r.ID)
		if err != nil {
			return nil, err
		}
		it := &castItem{Kind: r.Kind, ID: r.ID, Video: true}
		applyMediaInfo(it, info)
		return it, nil
	case KindTrack:
		if m.music == nil {
			return nil, core.NotFound("Track not found")
		}
		info, err := m.music.TrackInfo(ctx, p, r.ID)
		if err != nil {
			return nil, err
		}
		it := &castItem{Kind: r.Kind, ID: r.ID}
		applyTrackInfo(it, info)
		return it, nil
	case KindChannel:
		if m.tv == nil {
			return nil, core.NotFound("Channel not found")
		}
		ch, err := m.tv.CastChannel(ctx, p, r.ID)
		if err != nil {
			return nil, err
		}
		it := &castItem{Kind: r.Kind, ID: r.ID, Video: true, Title: channelTitle(ch), ArtURL: ch.LogoURL}
		if pr, err := m.tv.CastProgramAt(ctx, p, r.ID, m.now()); err == nil && pr != nil {
			it.Subtitle = pr.Title
		}
		return it, nil
	}
	return nil, core.NotFound("Item not found")
}

func channelTitle(ch *tv.CastChannel) string { return strconv.Itoa(ch.Number) + " · " + ch.Name }

// episodeCode is "S01E02" (or "E02", or "") like the TV app shows it.
func episodeCode(season, episode int) string {
	switch {
	case season > 0 && episode > 0:
		return fmt.Sprintf("S%02dE%02d", season, episode)
	case episode > 0:
		return fmt.Sprintf("E%02d", episode)
	}
	return ""
}

func joinNonEmpty(sep string, parts ...string) string {
	out := parts[:0:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, sep)
}

func applyMediaInfo(it *castItem, info *media.PlaybackInfo) {
	if info.ShowTitle != "" {
		it.Title = info.ShowTitle
		ep := info.EpisodeTitle
		if ep == "" && info.Title != info.ShowTitle {
			ep = info.Title
		}
		it.Subtitle = joinNonEmpty(" · ", episodeCode(info.Season, info.Episode), ep)
	} else {
		it.Title = info.Title
		if info.Year > 0 {
			it.Subtitle = strconv.Itoa(info.Year)
		}
	}
	it.ArtURL = info.PosterURL
	it.Duration = info.DurationSeconds
}

func applyTrackInfo(it *castItem, info *music.TrackInfo) {
	it.Title = info.Title
	it.Subtitle = joinNonEmpty(" · ", info.Artist, info.Album)
	it.Duration = info.DurationSeconds
	it.ArtURL = ""
	if info.HasArt {
		it.ArtURL = "/api/v1/music/tracks/" + url.PathEscape(info.ID) + "/art"
	}
}

/* --------------------------------------------------------------- playability */

// chromecastVideoOK reports whether a Chromecast plays the file itself: MP4/WebM with
// H.264, VP8, VP9 or AV1 video and AAC, MP3, Opus or Vorbis audio (API.md §14). Without
// probed codecs the browser verdict (a close match) decides.
func chromecastVideoOK(container, vcodec, acodec string, browserPlayable bool) bool {
	if vcodec == "" {
		return browserPlayable
	}
	switch container {
	case "mp4", "m4v", "mov", "webm":
	default:
		return false
	}
	switch vcodec {
	case "h264", "vp8", "vp9", "av1":
	default:
		return false
	}
	switch acodec {
	case "", "aac", "mp3", "opus", "vorbis":
		return true
	}
	return false
}

// airplayVideoOK reports whether an Apple TV plays the file itself (MP4/MOV with H.264 or
// HEVC and AAC, AC-3, E-AC-3, MP3 or ALAC audio).
func airplayVideoOK(container, vcodec, acodec string) bool {
	switch container {
	case "mp4", "m4v", "mov":
	default:
		return false
	}
	switch vcodec {
	case "h264", "hevc":
	default:
		return false
	}
	switch acodec {
	case "", "aac", "ac3", "eac3", "mp3", "alac":
		return true
	}
	return false
}

// chromecastAudioOK reports whether a Chromecast plays an audio file itself (MP3, AAC, FLAC,
// Ogg Vorbis/Opus, WAV PCM); by MIME type when the codec is unknown.
func chromecastAudioOK(container, codec, mime string) bool {
	switch codec {
	case "mp3", "flac":
		return true
	case "aac":
		return container != "" && container != "mkv" && container != "avi"
	case "vorbis", "opus":
		return container == "ogg" || container == "webm" || container == "opus" || container == ""
	case "":
	default:
		return strings.HasPrefix(codec, "pcm_s16") || strings.HasPrefix(codec, "pcm_s24") || codec == "pcm_u8"
	}
	switch strings.ToLower(mime) {
	case "audio/mpeg", "audio/mp3", "audio/mp4", "audio/x-m4a", "audio/aac", "audio/flac", "audio/x-flac",
		"audio/ogg", "application/ogg", "audio/opus", "audio/wav", "audio/x-wav", "audio/wave", "audio/webm":
		return true
	}
	return false
}

/* ------------------------------------------------------------------ loading */

// loaded is what the device was last told to play (owned by the session loop).
type loaded struct {
	item     *castItem
	base     float64 // item position of the device's position 0 (transcodes start at ?start=)
	duration float64 // seconds; 0 = unknown
	live     bool    // a live channel program: no seeking
	direct   bool    // the file itself (seekable on the device) rather than a transcode
	program  *tv.CastProgram

	started   time.Time // when this playback began (track plays are dated by it)
	played    float64   // seconds actually played (track plays)
	playingAt time.Time // last time the played counter advanced
	recorded  bool      // the track play was recorded
	pausedAt  time.Time // live programs: when the device was paused
}

// mediaURL is the ticketed URL a device fetches an asset of an item with.
func (s *session) mediaURL(kind, id, asset string, q url.Values) string {
	u := s.base + mediaPathPrefix + s.ticket + "/" + kind + "/" + url.PathEscape(id) + "/" + asset
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

// secondsParam renders a start offset for ?start=.
func secondsParam(v float64) string { return strconv.FormatFloat(max(0, v), 'f', 1, 64) }

// buildLoad prepares the LoadRequest for item it from start seconds (for channel items pr
// is the program to play, start its offset) with the owner's current permissions.
func (s *session) buildLoad(ctx context.Context, owner *core.Principal, it *castItem, start float64, pr *tv.CastProgram) (*renderer.LoadRequest, *loaded, error) {
	m := s.m
	ff := m.media != nil && m.media.FFmpegPath(ctx) != ""
	airplay := s.entry.dev.Kind == renderer.KindAirPlay
	l := &loaded{item: it, started: m.now()}
	req := &renderer.LoadRequest{}
	switch it.Kind {
	case KindMedia:
		info, err := m.media.PlaybackInfo(ctx, owner, it.ID)
		if err != nil {
			return nil, nil, err
		}
		s.mu.Lock()
		applyMediaInfo(it, info)
		s.mu.Unlock()
		req.Video, req.DurationSeconds = true, info.DurationSeconds
		req.Title, req.Subtitle, req.Year = info.Title, it.Subtitle, info.Year
		if info.ShowTitle != "" {
			req.SeriesTitle, req.Season, req.Episode = info.ShowTitle, info.Season, info.Episode
			req.Title = info.EpisodeTitle
			if req.Title == "" {
				req.Title = info.Title
			}
		}
		ok := chromecastVideoOK(info.Container, info.VideoCodec, info.AudioCodec, info.BrowserPlayable)
		if airplay {
			ok = airplayVideoOK(info.Container, info.VideoCodec, info.AudioCodec)
		}
		l.duration = info.DurationSeconds
		if ok || !ff {
			// Without ffmpeg the file is the only option; many devices cope anyway.
			req.URL, req.ContentType, req.StartSeconds = s.mediaURL(KindMedia, it.ID, "stream", nil), info.Mime, start
			l.direct = true
		} else {
			req.URL, req.ContentType = s.mediaURL(KindMedia, it.ID, "transcode", url.Values{"start": {secondsParam(start)}}), "video/mp4"
			l.base = start
		}
		if info.PosterURL != "" {
			req.ArtURL = s.mediaURL(KindMedia, it.ID, "art", url.Values{"size": {"lg"}})
			id := it.ID
			req.Art = func(ctx context.Context) ([]byte, error) {
				p := s.ownerPrincipal(ctx)
				if p == nil {
					return nil, errItemGone
				}
				return m.media.PosterJPEG(ctx, p, id, "lg")
			}
		}
		// Cue times are relative to the file, so a transcode that starts later would show them
		// out of sync: subtitles go along with the file or a transcode from the start.
		if l.base == 0 {
			for _, sub := range info.Subtitles {
				if len(req.Subtitles) == maxCastSubtitles {
					break
				}
				if sub.Format == "embedded" && !ff {
					continue
				}
				req.Subtitles = append(req.Subtitles, renderer.SubtitleTrack{
					URL: s.mediaURL(KindMedia, it.ID, strconv.Itoa(sub.Index)+".vtt", nil), Language: sub.Language, Label: sub.Label})
			}
		}
	case KindTrack:
		info, err := m.music.TrackInfo(ctx, owner, it.ID)
		if err != nil {
			return nil, nil, err
		}
		s.mu.Lock()
		applyTrackInfo(it, info)
		s.mu.Unlock()
		req.Title, req.Artist, req.Album, req.Subtitle = info.Title, info.Artist, info.Album, info.Artist
		req.DurationSeconds, l.duration = info.DurationSeconds, info.DurationSeconds
		if airplay {
			// AirPlay audio is pushed by the server: the device fetches nothing.
			req.PCM = s.m.pcmSource(s, it.ID, info)
			req.StartSeconds = start
			l.direct = true
		} else if chromecastAudioOK(info.Container, info.Codec, info.Mime) || !ff {
			req.URL, req.ContentType, req.StartSeconds = s.mediaURL(KindTrack, it.ID, "stream", nil), info.Mime, start
			l.direct = true
		} else {
			req.URL, req.ContentType = s.mediaURL(KindTrack, it.ID, "transcode", url.Values{"start": {secondsParam(start)}}), "audio/aac"
			l.base = start
		}
		if info.HasArt {
			req.ArtURL = s.mediaURL(KindTrack, it.ID, "art", url.Values{"size": {"lg"}})
			id := it.ID
			req.Art = func(ctx context.Context) ([]byte, error) {
				p := s.ownerPrincipal(ctx)
				if p == nil {
					return nil, errItemGone
				}
				return m.music.ArtJPEG(ctx, p, id, "lg")
			}
		}
	case KindChannel:
		if pr == nil {
			return nil, nil, core.NotFound("Nothing is on air on this channel")
		}
		ch, err := m.tv.CastChannel(ctx, owner, it.ID)
		if err != nil {
			return nil, nil, err
		}
		s.mu.Lock()
		it.Title, it.ArtURL, it.Subtitle = channelTitle(ch), ch.LogoURL, pr.Title
		s.mu.Unlock()
		l.program, l.live, l.duration = pr, !ch.OnDemand, pr.DurationSeconds
		req.Video, req.Live, req.DurationSeconds = true, !ch.OnDemand, pr.DurationSeconds
		req.Title, req.Subtitle = pr.Title, it.Title
		if pr.ShowTitle != "" {
			req.SeriesTitle, req.Season, req.Episode = pr.ShowTitle, pr.Season, pr.Episode
			if pr.EpisodeTitle != "" {
				req.Title = pr.EpisodeTitle
			} else if pr.ItemTitle != "" {
				req.Title = pr.ItemTitle
			}
		}
		prog := url.Values{"program": {pr.FileID}}
		ok := chromecastVideoOK(pr.Container, pr.VideoCodec, pr.AudioCodec, pr.BrowserPlayable)
		if airplay {
			ok = airplayVideoOK(pr.Container, pr.VideoCodec, pr.AudioCodec)
		}
		if ok || !ff {
			req.URL, req.ContentType, req.StartSeconds = s.mediaURL(KindChannel, it.ID, "stream", prog), pr.Mime, start
			l.direct = true
		} else {
			q := url.Values{"program": {pr.FileID}, "start": {secondsParam(start)}}
			req.URL, req.ContentType = s.mediaURL(KindChannel, it.ID, "transcode", q), "video/mp4"
			l.base = start
		}
		if pr.HasPoster {
			req.ArtURL = s.mediaURL(KindChannel, it.ID, "art", url.Values{"program": {pr.FileID}, "size": {"lg"}})
		}
		if l.base == 0 {
			for _, sub := range pr.Subtitles {
				if len(req.Subtitles) == maxCastSubtitles {
					break
				}
				req.Subtitles = append(req.Subtitles, renderer.SubtitleTrack{
					URL: s.mediaURL(KindChannel, it.ID, strconv.Itoa(sub.Index)+".vtt", prog), Language: sub.Language, Label: sub.Label})
			}
		}
	default:
		return nil, nil, core.NotFound("Item not found")
	}
	if req.ContentType == "" && req.URL != "" {
		req.ContentType = "application/octet-stream"
	}
	return req, l, nil
}
