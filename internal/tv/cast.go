package tv

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/httpx"
	"ludworks/internal/tv/schedule"
)

// Cross-module API for the cast module (API.md §14 "channel" items). A cast session plays a
// channel the way the TV player does: the program on air from its live offset (on-demand
// channels from the viewer's saved position), then the following programs. The device fetches
// program bytes through the cast module's media tickets; ServeCastProgram authorises them
// exactly like the channel-scoped program routes — the viewer must see the channel and the
// file must be part of its expansion — and serves them with the channel owner's permissions.

// CastChannel describes a channel for a cast session.
type CastChannel struct {
	ID, Name string
	Number   int
	OnDemand bool
	LogoURL  string // API path for browsers; "" when the channel has no logo
}

// CastProgram is one program as a cast session plays it.
type CastProgram struct {
	FileID string
	// Title is the one-line display title ("Show · S01E02 · Episode" for episodes);
	// ItemTitle is the media item's own title.
	Title, ItemTitle        string
	ShowTitle, EpisodeTitle string
	Season, Episode         int // 0 = unknown
	Start, End              time.Time
	DurationSeconds         float64
	OffsetSeconds           float64 // how far into the program playback is
	// Index is the absolute program counter (on-demand channels store it as the position).
	Index           int64
	Mime            string
	VideoCodec      string
	AudioCodec      string
	Container       string
	BrowserPlayable bool
	HasPoster       bool
	// Subtitles are the program's subtitle tracks (embedded ones only when ffmpeg is available);
	// Index is the media item's subtitle index. URL is the channel-scoped browser URL.
	Subtitles []ProgramSubtitle
}

// CastAsset selects what ServeCastProgram serves.
type CastAsset struct {
	Kind     string  // "stream" | "transcode" | "poster" | "subtitle"
	Start    float64 // transcode: seconds into the program
	Height   int     // transcode: 0 = default
	Size     string  // poster: sm | md | lg ("" = md)
	Subtitle int     // subtitle: track index
}

var errCastNotOnDemand = core.Conflict("Only on-demand channels have a program index")

// castChannelRow loads channel id as viewer sees it (404 otherwise).
func (m *Module) castChannelRow(ctx context.Context, viewer *core.Principal, id string) (*channelRow, error) {
	if viewer == nil || !httpx.ValidID(id) {
		return nil, errChannelNotFound
	}
	return channelFor(ctx, m.d.DB.SQL, id, viewer.UserID, false)
}

// CastChannel returns channel id as viewer sees it; 404 when it does not exist or is not
// visible to viewer.
func (m *Module) CastChannel(ctx context.Context, viewer *core.Principal, id string) (*CastChannel, error) {
	c, err := m.castChannelRow(ctx, viewer, id)
	if err != nil {
		return nil, err
	}
	out := &CastChannel{ID: c.ID, Name: c.Name, Number: c.Number, OnDemand: c.PlaybackMode == PlaybackOnDemand}
	if api := c.api(); api.LogoURL != nil {
		out.LogoURL = *api.LogoURL
	}
	return out, nil
}

// CastProgramAt returns what channel id shows viewer: for live channels the program on air at
// at with its live offset, for on-demand channels the program at viewer's saved position (at
// is then the time the program is taken to resume). It returns nil (and no error) when
// nothing is on air.
func (m *Module) CastProgramAt(ctx context.Context, viewer *core.Principal, id string, at time.Time) (*CastProgram, error) {
	c, err := m.castChannelRow(ctx, viewer, id)
	if err != nil {
		return nil, err
	}
	exp, err := m.expansionFor(ctx, c.LineupID, c.OwnerID)
	if err != nil {
		return nil, err
	}
	cur, _, err := m.current(ctx, c, exp, viewer.UserID, at)
	if err != nil || cur == nil {
		return nil, err
	}
	return m.castProgram(ctx, c, exp, *cur), nil
}

// CastProgramByIndex returns program index of on-demand channel id for viewer, offset seconds
// into it (an offset at or past its end moves to the next program, like PUT /position). Live
// channels are 409. It returns nil when nothing is playable.
func (m *Module) CastProgramByIndex(ctx context.Context, viewer *core.Principal, id string, index int64, offset float64) (*CastProgram, error) {
	c, err := m.castChannelRow(ctx, viewer, id)
	if err != nil {
		return nil, err
	}
	if c.PlaybackMode != PlaybackOnDemand {
		return nil, errCastNotOnDemand
	}
	if index < 0 || index > maxPositionSeq || !validSeconds(offset) {
		return nil, core.BadRequest("invalid program position")
	}
	exp, err := m.expansionFor(ctx, c.LineupID, c.OwnerID)
	if err != nil {
		return nil, err
	}
	cur, _ := schedule.Position(exp.items, scheduleChannel(c), m.now(), index, offset)
	if cur == nil {
		return nil, nil
	}
	return m.castProgram(ctx, c, exp, *cur), nil
}

// CastSavePosition stores viewer's position on on-demand channel id (as PUT /position does);
// live channels are 409.
func (m *Module) CastSavePosition(ctx context.Context, viewer *core.Principal, id string, index int64, offset float64) error {
	c, err := m.castChannelRow(ctx, viewer, id)
	if err != nil {
		return err
	}
	if c.PlaybackMode != PlaybackOnDemand {
		return errCastNotOnDemand
	}
	if index < 0 || index > maxPositionSeq || !validSeconds(offset) {
		return core.BadRequest("invalid program position")
	}
	return savePosition(ctx, m.d.DB.SQL, viewer.UserID, c.ID, index, offset, m.now())
}

// castProgram renders slot s of channel c for casting.
func (m *Module) castProgram(ctx context.Context, c *channelRow, exp *expansion, s schedule.Slot) *CastProgram {
	ff := m.ffmpeg(ctx) != ""
	pr := m.program(c, exp, s, ff)
	f := exp.byID[s.FileID]
	out := &CastProgram{
		FileID: s.FileID, Title: pr.Title, ShowTitle: pr.ShowTitle, EpisodeTitle: pr.EpisodeTitle,
		Start: s.Start, End: s.End, DurationSeconds: pr.DurationSeconds, OffsetSeconds: s.Offset, Index: s.Seq,
		BrowserPlayable: pr.BrowserPlayable, HasPoster: pr.PosterURL != nil, Subtitles: pr.Subtitles,
	}
	if f != nil {
		out.ItemTitle, out.Mime = f.Title, f.Mime
		out.Title = displayTitle(f)
	}
	if pr.Season != nil {
		out.Season = *pr.Season
	}
	if pr.Episode != nil {
		out.Episode = *pr.Episode
	}
	// Codecs decide whether a device can play the file itself; unknown values (no media row
	// yet, or an older schema) simply mean "transcode when possible".
	var vc, ac, ct sql.NullString
	err := m.d.DB.SQL.QueryRowContext(ctx, `SELECT video_codec, audio_codec, container FROM media_items WHERE file_id = ?`,
		s.FileID).Scan(&vc, &ac, &ct)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		m.log.Debug("tv: cast program codecs unavailable", "file", s.FileID, "err", err)
	}
	out.VideoCodec, out.AudioCodec, out.Container = vc.String, ac.String, ct.String
	return out
}

// ServeCastProgram serves asset a of program fileID of channel id to a cast device on behalf
// of viewer (the session owner): viewer must see the channel and the file must be part of
// its expansion; the bytes are served with the channel owner's permissions. Transcodes are
// charged to the principal in r's context when there is one (the cast module puts viewer
// there), like the channel-scoped routes charge the caller.
func (m *Module) ServeCastProgram(w http.ResponseWriter, r *http.Request, viewer *core.Principal, id, fileID string, a CastAsset) error {
	ctx := r.Context()
	c, err := m.castChannelRow(ctx, viewer, id)
	if err != nil {
		return err
	}
	if !httpx.ValidID(fileID) {
		return errProgramNotFound
	}
	exp, err := m.expansionFor(ctx, c.LineupID, c.OwnerID)
	if err != nil {
		return err
	}
	if _, ok := exp.byID[fileID]; !ok {
		return errProgramNotFound
	}
	owner, err := m.ownerPrincipal(ctx, c.OwnerID)
	if err != nil {
		return err
	}
	if owner == nil || m.media == nil {
		return errProgramNotFound
	}
	switch a.Kind {
	case "stream":
		return m.media.ServeStream(w, r, owner, fileID)
	case "transcode":
		if !validSeconds(a.Start) {
			return core.BadRequest("start must be a number of seconds of 0 or more")
		}
		height := a.Height
		if height == 0 {
			height = 720
		}
		if !transcodeHeights[height] {
			return core.BadRequest("height must be 360, 480, 720 or 1080")
		}
		return m.media.ServeTranscode(w, r, owner, fileID, a.Start, height)
	case "poster":
		size := a.Size
		if size == "" {
			size = "md"
		}
		return m.media.ServePoster(w, r, owner, fileID, size)
	case "subtitle":
		if a.Subtitle < 0 || a.Subtitle > 1000 {
			return core.NotFound("Subtitle not found")
		}
		return m.media.ServeSubtitle(w, r, owner, fileID, a.Subtitle)
	}
	return core.ErrNotFound
}
