package tv

import (
	"context"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/httpx"
	"ludworks/internal/tv/schedule"
)

var errProgramNotFound = core.NotFound("Program not found")

// scheduleChannel converts a channel row to the schedule engine's settings.
func scheduleChannel(c *channelRow) schedule.Channel {
	return schedule.Channel{Anchor: c.AnchorAt, Shuffle: c.Shuffle, ShuffleMode: c.ShuffleMode, Seed: c.Seed}
}

// programBase is the channel-scoped URL prefix of a program.
func programBase(channelID, fileID string) string {
	return "/api/v1/tv/channels/" + channelID + "/programs/" + fileID
}

// program renders one slot of channel c.
func (m *Module) program(c *channelRow, exp *expansion, s schedule.Slot, ffmpeg bool) Program {
	f := exp.byID[s.FileID]
	if f == nil { // cannot happen: slots come from the same expansion
		f = &expFile{FileID: s.FileID, Title: s.FileID, Duration: s.Duration}
	}
	base := programBase(c.ID, f.FileID)
	p := Program{
		FileID: f.FileID, Title: f.Title, ShowTitle: f.ShowTitle, Season: f.Season, Episode: f.Episode,
		EpisodeTitle: f.EpisodeTitle, StartsAt: db.FormatTime(s.Start), EndsAt: db.FormatTime(s.End),
		DurationSeconds: float64(s.End.Sub(s.Start).Milliseconds()) / 1000, StreamURL: base + "/stream",
		BrowserPlayable: f.BrowserPlayable,
	}
	if f.HasPoster || f.ThumbState == 1 || (f.ThumbState == 0 && ffmpeg && strings.HasPrefix(f.Mime, "video/")) {
		u := base + "/poster?size=md"
		p.PosterURL = &u
	}
	if ffmpeg {
		u := base + "/transcode"
		p.TranscodeURL = &u
	}
	p.Subtitles = programSubtitles(base, f.Subs, ffmpeg)
	return p
}

// programSubtitles lists a program's subtitle tracks with channel-scoped WebVTT URLs.
// Embedded tracks need ffmpeg to be extracted, so they are only listed when it is available.
func programSubtitles(base string, subs []fileSub, ffmpeg bool) []ProgramSubtitle {
	out := make([]ProgramSubtitle, 0, len(subs))
	for _, s := range subs {
		if s.Embedded && !ffmpeg {
			continue
		}
		out = append(out, ProgramSubtitle{Index: s.Index, Language: s.Language, Label: s.Label,
			URL: base + "/subtitles/" + strconv.Itoa(s.Index) + ".vtt"})
	}
	return out
}

// current resolves what channel c shows to userID right now: the live schedule, or the
// user's saved position for on-demand channels.
func (m *Module) current(ctx context.Context, c *channelRow, exp *expansion, userID string, now time.Time) (*schedule.Slot, []schedule.Slot, error) {
	ch := scheduleChannel(c)
	if c.PlaybackMode == PlaybackOnDemand {
		seq, off, err := loadPosition(ctx, m.d.DB.SQL, userID, c.ID)
		if err != nil {
			return nil, nil, err
		}
		cur, next := schedule.Position(exp.items, ch, now, seq, off)
		return cur, next, nil
	}
	cur, next := schedule.Now(exp.items, ch, now)
	return cur, next, nil
}

func (m *Module) nowPlaying(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	c, err := m.channelFromPath(r, false)
	if err != nil {
		return err
	}
	exp, err := m.expansionFor(ctx, c.LineupID, c.OwnerID)
	if err != nil {
		return err
	}
	now := m.now()
	cur, next, err := m.current(ctx, c, exp, p.UserID, now)
	if err != nil {
		return err
	}
	ff := m.ffmpeg(ctx) != ""
	out := NowPlaying{Channel: c.api(), ServerTime: db.FormatTime(now), Next: []Program{}}
	if cur != nil {
		pr := m.program(c, exp, *cur, ff)
		out.Program = &pr
		out.OffsetSeconds = cur.Offset
		seq := cur.Seq
		out.Index = &seq
		for _, s := range next {
			out.Next = append(out.Next, m.program(c, exp, s, ff))
		}
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

type positionInput struct {
	Index         *int64   `json:"index"`
	OffsetSeconds *float64 `json:"offset_seconds"`
}

func (m *Module) setPosition(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	c, err := m.channelFromPath(r, false)
	if err != nil {
		return err
	}
	var in positionInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	if c.PlaybackMode != PlaybackOnDemand {
		return core.Conflict("Only on-demand channels keep a playback position")
	}
	errs := fieldErrs{}
	if in.Index == nil || *in.Index < 0 || *in.Index > maxPositionSeq {
		errs.set("index", "must be a program index of 0 or more")
	}
	if in.OffsetSeconds == nil || !validSeconds(*in.OffsetSeconds) {
		errs.set("offset_seconds", "must be a number of seconds of 0 or more")
	}
	if err := errs.err(); err != nil {
		return err
	}
	if err := savePosition(ctx, m.d.DB.SQL, p.UserID, c.ID, *in.Index, *in.OffsetSeconds, m.now()); err != nil {
		return err
	}
	return httpx.NoContent(w)
}

/* -------------------------------------------------------------------- guide */

// GuideChannel / Guide are the GET /tv/guide shapes.
type GuideChannel struct {
	Channel  Channel   `json:"channel"`
	Programs []Program `json:"programs"`
}

type Guide struct {
	Start    string         `json:"start"`
	End      string         `json:"end"`
	Channels []GuideChannel `json:"channels"`
}

// slotsBetween returns the programs of channel c overlapping [from, to) for userID (the
// user's own continuation for on-demand channels), at most max.
func (m *Module) slotsBetween(ctx context.Context, c *channelRow, exp *expansion, userID string, now, from, to time.Time, max int) ([]schedule.Slot, error) {
	ch := scheduleChannel(c)
	if c.PlaybackMode != PlaybackOnDemand {
		slots := schedule.Guide(exp.items, ch, from, to)
		if len(slots) > max {
			slots = slots[:max]
		}
		return slots, nil
	}
	// On-demand: what the user would watch from their saved position onward, starting now
	// (one schedule engine for the whole window, like the live guide).
	seq, off, err := loadPosition(ctx, m.d.DB.SQL, userID, c.ID)
	if err != nil {
		return nil, err
	}
	return schedule.PositionGuide(exp.items, ch, now, seq, off, from, to, max), nil
}

func (m *Module) guide(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	q := r.URL.Query()
	now := m.now()
	start := now.Truncate(30 * time.Minute)
	if v := q.Get("start"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return core.BadRequest("start must be an RFC 3339 timestamp")
		}
		if t.Before(now.AddDate(-1, 0, 0)) || t.After(now.AddDate(1, 0, 0)) {
			return core.BadRequest("start must be within a year of now")
		}
		start = t.UTC().Truncate(time.Millisecond)
	}
	hours := 6.0
	if v := q.Get("hours"); v != "" {
		h, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(h) || h < 1 || h > MaxGuideHours {
			return core.BadRequest("hours must be between 1 and 72")
		}
		hours = h
	}
	end := start.Add(time.Duration(hours * float64(time.Hour))).Truncate(time.Millisecond)

	rows, err := visibleChannels(ctx, m.d.DB.SQL, p.UserID)
	if err != nil {
		return err
	}
	if v := strings.TrimSpace(q.Get("channel_ids")); v != "" {
		want := map[string]int{}
		for i, id := range strings.Split(v, ",") {
			id = strings.TrimSpace(id)
			if !httpx.ValidID(id) {
				return core.BadRequest("channel_ids must be a comma-separated list of channel ids")
			}
			if _, dup := want[id]; !dup {
				want[id] = i
			}
			if len(want) > MaxGuideChannels {
				return core.BadRequest("too many channel_ids (maximum 200)")
			}
		}
		var sel []*channelRow
		for _, c := range rows {
			if _, ok := want[c.ID]; ok {
				sel = append(sel, c)
			}
		}
		rows = sel
	}
	if len(rows) > MaxGuideChannels {
		rows = rows[:MaxGuideChannels]
	}
	perChannel := MaxGuidePrograms
	if len(rows) > 0 {
		perChannel = max(50, MaxGuidePrograms/len(rows))
	}
	ff := m.ffmpeg(ctx) != ""
	out := Guide{Start: db.FormatTime(start), End: db.FormatTime(end), Channels: make([]GuideChannel, 0, len(rows))}
	for _, c := range rows {
		if err := ctx.Err(); err != nil {
			return err // the client went away: stop building programs nobody reads
		}
		exp, err := m.expansionFor(ctx, c.LineupID, c.OwnerID)
		if err != nil {
			return err
		}
		slots, err := m.slotsBetween(ctx, c, exp, p.UserID, now, start, end, perChannel)
		if err != nil {
			return err
		}
		gc := GuideChannel{Channel: c.api(), Programs: make([]Program, 0, len(slots))}
		for _, s := range slots {
			gc.Programs = append(gc.Programs, m.program(c, exp, s, ff))
		}
		out.Channels = append(out.Channels, gc)
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

/* ---------------------------------------------------- channel-scoped programs */

// programAccess authorises a channel-scoped program request: the viewer must see the
// channel and the file must be part of the channel's expansion. It returns the channel
// owner's principal, whose file permissions the media service applies.
func (m *Module) programAccess(r *http.Request) (*channelRow, string, *core.Principal, error) {
	ctx := r.Context()
	c, err := m.channelFromPath(r, false)
	if err != nil {
		return nil, "", nil, err
	}
	fileID, err := httpx.PathID(r, "file_id")
	if err != nil {
		return nil, "", nil, errProgramNotFound
	}
	exp, err := m.expansionFor(ctx, c.LineupID, c.OwnerID)
	if err != nil {
		return nil, "", nil, err
	}
	if _, ok := exp.byID[fileID]; !ok {
		return nil, "", nil, errProgramNotFound
	}
	owner, err := m.ownerPrincipal(ctx, c.OwnerID)
	if err != nil {
		return nil, "", nil, err
	}
	if owner == nil || m.media == nil {
		return nil, "", nil, errProgramNotFound
	}
	return c, fileID, owner, nil
}

func (m *Module) programStream(w http.ResponseWriter, r *http.Request) error {
	_, fileID, owner, err := m.programAccess(r)
	if err != nil {
		return err
	}
	return m.media.ServeStream(w, r, owner, fileID)
}

// transcodeHeights are the allowed ?height= values.
var transcodeHeights = map[int]bool{360: true, 480: true, 720: true, 1080: true}

func (m *Module) programTranscode(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	start := 0.0
	if v := q.Get("start"); v != "" {
		s, err := strconv.ParseFloat(v, 64)
		if err != nil || !validSeconds(s) {
			return core.BadRequest("start must be a number of seconds of 0 or more")
		}
		start = s
	}
	height := 720
	if v := q.Get("height"); v != "" {
		h, err := strconv.Atoi(v)
		if err != nil || !transcodeHeights[h] {
			return core.BadRequest("height must be 360, 480, 720 or 1080")
		}
		height = h
	}
	_, fileID, owner, err := m.programAccess(r)
	if err != nil {
		return err
	}
	return m.media.ServeTranscode(w, r, owner, fileID, start, height)
}

func (m *Module) programPoster(w http.ResponseWriter, r *http.Request) error {
	size := r.URL.Query().Get("size")
	switch size {
	case "":
		size = "md"
	case "sm", "md", "lg":
	default:
		return core.Validation(map[string]string{"size": "must be sm, md or lg"})
	}
	_, fileID, owner, err := m.programAccess(r)
	if err != nil {
		return err
	}
	return m.media.ServePoster(w, r, owner, fileID, size)
}

func (m *Module) programSubtitle(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("name")
	num, ok := strings.CutSuffix(name, ".vtt")
	idx, err := strconv.Atoi(num)
	if !ok || err != nil || idx < 0 || idx > 1000 || num != strconv.Itoa(idx) {
		return core.NotFound("Subtitle not found")
	}
	_, fileID, owner, err := m.programAccess(r)
	if err != nil {
		return err
	}
	return m.media.ServeSubtitle(w, r, owner, fileID, idx)
}

// play redirects to the current program's stream URL with a #t=<offset> fragment. When the
// request authenticated with ?access_token=, the token is carried over so external players
// can follow the redirect.
func (m *Module) play(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	c, err := m.channelFromPath(r, false)
	if err != nil {
		return err
	}
	exp, err := m.expansionFor(ctx, c.LineupID, c.OwnerID)
	if err != nil {
		return err
	}
	cur, _, err := m.current(ctx, c, exp, p.UserID, m.now())
	if err != nil {
		return err
	}
	if cur == nil {
		return core.NotFound("Nothing is on air on this channel")
	}
	loc := programBase(c.ID, cur.FileID) + "/stream"
	if tok := r.URL.Query().Get("access_token"); tok != "" && r.Header.Get("Authorization") == "" {
		loc += "?access_token=" + url.QueryEscape(tok)
	}
	loc += "#t=" + strconv.FormatFloat(math.Floor(cur.Offset*1000)/1000, 'f', -1, 64)
	w.Header().Set("Location", loc)
	w.WriteHeader(http.StatusFound)
	return nil
}
