package music

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/media/probe"
	"ludworks/internal/music/tags"
)

// Reading a file: tags and technical values from the pure-Go readers (internal/music/tags and
// the media probe), ffprobe for containers they do not know (WMA, APE, WavPack, …), the
// embedded picture stored as art and embedded lyrics.

// storedLyrics is music_tracks.lyrics (embedded lyrics).
type storedLyrics struct {
	Synced bool         `json:"synced"`
	Lines  []storedLine `json:"lines"`
}

type storedLine struct {
	T *float64 `json:"t,omitempty"` // seconds; absent for unsynced lyrics
	X string   `json:"x"`
}

const (
	maxStoredLyricLines = 5000
	maxStoredLyricBytes = 256 << 10
)

func toStoredLyrics(l *tags.Lyrics) *storedLyrics {
	if l == nil || len(l.Lines) == 0 {
		return nil
	}
	out := &storedLyrics{Synced: l.Synced}
	size := 0
	for _, ln := range l.Lines {
		if len(out.Lines) >= maxStoredLyricLines || size+len(ln.Text) > maxStoredLyricBytes {
			break
		}
		sl := storedLine{X: ln.Text}
		if l.Synced {
			t := math.Round(ln.Time*1000) / 1000
			sl.T = &t
		}
		out.Lines = append(out.Lines, sl)
		size += len(ln.Text) + 16
	}
	if len(out.Lines) == 0 {
		return nil
	}
	return out
}

// technical are a track's technical values.
type technical struct {
	Duration                                float64
	Codec, Container                        string
	Bitrate, SampleRate, BitDepth, Channels int
}

// mergeTechnical combines the tag reader's values with the media probe's.
func mergeTechnical(md *tags.Metadata, info *probe.Info) technical {
	var t technical
	if info != nil {
		t = technical{Duration: info.DurationSeconds, Codec: info.AudioCodec, Container: info.Container,
			Bitrate: info.BitrateKbps, SampleRate: info.SampleRate, Channels: info.AudioChannels}
	}
	if md == nil {
		return t
	}
	if t.Duration <= 0 {
		t.Duration = md.DurationSeconds
	}
	if t.Codec == "" {
		t.Codec = md.Codec
	}
	if t.Container == "" {
		t.Container = md.Container
	}
	if t.SampleRate <= 0 {
		t.SampleRate = md.SampleRate
	}
	if t.Channels <= 0 {
		t.Channels = md.Channels
	}
	t.BitDepth = md.BitDepth
	// The audio payload alone (embedded pictures excluded) gives the real average bitrate;
	// otherwise the bitrate the container declares, else the probe's estimate.
	est := 0
	if md.AudioBytes > 0 && t.Duration > 0.5 {
		est = int(math.Round(float64(md.AudioBytes) * 8 / t.Duration / 1000))
	}
	switch {
	case est > 0:
		t.Bitrate = est
	case md.BitrateKbps > 0:
		t.Bitrate = md.BitrateKbps
	}
	if t.Duration < 0 || t.Duration > 1e8 || math.IsNaN(t.Duration) {
		t.Duration = 0
	}
	return t
}

func tagsFrom(md *tags.Metadata) fileTags {
	if md == nil {
		return fileTags{}
	}
	return fileTags{
		Title: md.Title, Artist: md.Artist, Artists: md.Artists, Album: md.Album, AlbumArtist: md.AlbumArtist,
		TrackNumber: md.TrackNumber, TrackTotal: md.TrackTotal, DiscNumber: md.DiscNumber, DiscTotal: md.DiscTotal,
		Year: md.Year, Genres: md.Genres, Composer: md.Composer, Comment: md.Comment, Compilation: md.Compilation,
		SortTitle: md.SortTitle, SortArtist: md.SortArtist, SortAlbum: md.SortAlbum, SortAlbumArtist: md.SortAlbumArtist,
	}
}

func nullPositive(v float64) sql.NullFloat64 {
	if v > 0 && v < 1e8 {
		return sql.NullFloat64{Float64: v, Valid: true}
	}
	return sql.NullFloat64{}
}

func nullFloat(p *float64) sql.NullFloat64 {
	if p == nil {
		return sql.NullFloat64{}
	}
	return sql.NullFloat64{Float64: *p, Valid: true}
}

// hasTags reports whether the tag reader found anything beyond the container guess.
func hasTags(md *tags.Metadata) bool {
	return md != nil && (md.Title != "" || md.Artist != "" || md.Album != "" || md.AlbumArtist != "" || len(md.Genres) > 0 ||
		md.TrackNumber > 0 || md.Year > 0 || len(md.Pictures) > 0 || md.Lyrics != nil || md.DurationSeconds > 0 || md.SampleRate > 0)
}

// readFile reads tags and technical values of a file; ok is false when nothing could parse it.
func (s *Service) readFile(ctx context.Context, id string) (md *tags.Metadata, tech technical, ok bool, err error) {
	f, e, err := s.files.Open(ctx, s.sys, id)
	if err != nil {
		return nil, technical{}, false, err
	}
	defer f.Close()
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	md, terr := tags.Read(pctx, f, e.Size, e.Name)
	info, perr := probe.ProbeContext(pctx, f, e.Size, e.Name)
	if err := ctx.Err(); err != nil {
		return nil, technical{}, false, err
	}
	if info == nil && !hasTags(md) {
		// Unknown to the built-in readers (or a file that only claims to be MP3 by its name):
		// ffprobe, when available, reads the rest.
		if fp := s.ffprobePath(); fp != "" {
			if local, lerr := s.files.LocalPath(ctx, s.sys, id); lerr == nil {
				fctx, fcancel := context.WithTimeout(ctx, probeTimeout)
				fmd, finfo, ferr := s.ffprobeFile(fctx, fp, local, e.Name)
				fcancel()
				if ferr == nil {
					return fmd, mergeTechnical(fmd, finfo), true, nil
				}
				s.log.Debug("music: ffprobe failed", "id", id, "err", ferr)
			}
		}
		s.log.Debug("music: unreadable audio file", "id", id, "tags", terr, "probe", perr)
		return nil, technical{}, false, nil
	}
	return md, mergeTechnical(md, info), true, nil
}

// scanTrack reads a track's file and stores tags, technical values, art and lyrics; the row
// always leaves the pending state unless ctx is cancelled.
func (s *Service) scanTrack(ctx context.Context, id string) error {
	md, tech, ok, err := s.readFile(ctx, id)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	now := db.FormatTime(s.now())
	if err != nil || !ok {
		var ce *core.Error
		if err != nil && !(errors.As(err, &ce) && ce.Status == 404) {
			return err
		}
		_, uerr := s.d.DB.SQL.ExecContext(ctx, `UPDATE music_tracks SET probe_state = 2, probed_at = ?, updated_at = ?
			WHERE file_id = ?`, now, now, id)
		return errors.Join(err, uerr)
	}
	artID := ""
	if md != nil {
		if pic := md.BestPicture(); pic != nil {
			actx, cancel := context.WithTimeout(ctx, 90*time.Second)
			artID, err = s.storeArt(actx, pic.Data)
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				s.log.Debug("music: embedded picture", "id", id, "err", err)
				artID = ""
			}
		}
	}
	ft := tagsFrom(md)
	var (
		lyrics any
		rg     tags.ReplayGain
	)
	if md != nil {
		if l := toStoredLyrics(md.Lyrics); l != nil {
			b, _ := json.Marshal(l)
			lyrics = string(b)
		}
		rg = md.ReplayGain
	}
	if rg.TrackGain == nil {
		rg.TrackPeak = nil
	}
	if rg.AlbumGain == nil {
		rg.AlbumPeak = nil
	}
	return s.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE music_tracks SET file_tags = ?, duration_seconds = ?, codec = ?, container = ?,
			bitrate_kbps = ?, sample_rate = ?, bit_depth = ?, channels = ?, rg_track_gain = ?, rg_track_peak = ?,
			rg_album_gain = ?, rg_album_peak = ?, art_id = ?, lyrics = ?, probe_state = 1, probed_at = ?, updated_at = ?
			WHERE file_id = ?`,
			db.JSON[fileTags]{V: ft}, nullPositive(tech.Duration), cleanText(tech.Codec, 32), cleanText(tech.Container, 32),
			nullInt(tech.Bitrate), nullInt(tech.SampleRate), nullInt(tech.BitDepth), nullInt(tech.Channels),
			nullFloat(rg.TrackGain), nullFloat(rg.TrackPeak), nullFloat(rg.AlbumGain), nullFloat(rg.AlbumPeak),
			nullStr(artID), lyrics, now, now, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		return s.recompute(ctx, tx, id)
	})
}
