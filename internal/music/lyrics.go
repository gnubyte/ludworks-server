package music

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"

	"ludworks/internal/core"
	"ludworks/internal/music/tags"
)

// Lyrics (API.md §12): embedded (ID3 USLT/SYLT, Vorbis LYRICS/UNSYNCEDLYRICS, MP4 ©lyr) or an
// .lrc sidecar with the track's base name. Synced lyrics beat unsynced ones; between two of the
// same kind the .lrc wins.

// LyricLine is one line of the lyrics response.
type LyricLine struct {
	TimeSeconds *float64 `json:"time_seconds"`
	Text        string   `json:"text"`
}

// Lyrics is GET /music/tracks/{id}/lyrics.
type Lyrics struct {
	Synced bool        `json:"synced"`
	Lines  []LyricLine `json:"lines"`
	Text   string      `json:"text"`
	Source string      `json:"source"` // embedded | lrc
}

var errNoLyrics = core.NotFound("No lyrics available")

func lyricsFromStored(l *storedLyrics) *Lyrics {
	out := &Lyrics{Synced: l.Synced, Source: "embedded", Lines: make([]LyricLine, 0, len(l.Lines))}
	for _, ln := range l.Lines {
		out.Lines = append(out.Lines, LyricLine{TimeSeconds: ln.T, Text: ln.X})
	}
	return out
}

func lyricsFromLRC(l *tags.Lyrics) *Lyrics {
	if sl := toStoredLyrics(l); sl != nil {
		out := lyricsFromStored(sl)
		out.Source = "lrc"
		return out
	}
	return nil
}

func (l *Lyrics) finish() *Lyrics {
	text := ""
	for i, ln := range l.Lines {
		if i > 0 {
			text += "\n"
		}
		text += ln.Text
	}
	l.Text = text
	return l
}

// lyrics resolves the lyrics of a track for p (permission already checked).
func (s *Service) lyrics(ctx context.Context, p *core.Principal, tr *track) (*Lyrics, error) {
	var (
		stored sql.NullString
		lrcID  sql.NullString
	)
	if err := s.d.DB.SQL.QueryRowContext(ctx, `SELECT lyrics, lrc_file_id FROM music_tracks WHERE file_id = ?`, tr.FileID).
		Scan(&stored, &lrcID); err != nil {
		return nil, err
	}
	var embedded *Lyrics
	if stored.Valid {
		var sl storedLyrics
		if err := json.Unmarshal([]byte(stored.String), &sl); err == nil && len(sl.Lines) > 0 {
			embedded = lyricsFromStored(&sl)
		}
	}
	var lrc *Lyrics
	if lrcID.Valid && lrcID.String != "" {
		// The sidecar is read with the caller's permissions (it sits next to the track).
		if f, e, err := s.files.Open(ctx, p, lrcID.String); err == nil {
			b, rerr := io.ReadAll(io.LimitReader(f, tags.MaxLRCBytes))
			f.Close()
			if rerr == nil && !e.IsDir {
				lrc = lyricsFromLRC(tags.ParseLRC(decodeText(b)))
			}
		}
	}
	switch {
	case lrc != nil && (embedded == nil || lrc.Synced || !embedded.Synced):
		return lrc.finish(), nil
	case embedded != nil:
		return embedded.finish(), nil
	}
	return nil, errNoLyrics
}
