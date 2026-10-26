package tv

import (
	"time"

	"ludworks/internal/db"
)

// Item kinds and orders (API.md §7 LineupItem).
const (
	KindFile   = "file"
	KindShow   = "show"
	KindSeason = "season"
	KindFolder = "folder"
	KindQuery  = "query"

	OrderDefault = "default"
	OrderRandom  = "random"

	PlaybackLive     = "live"
	PlaybackOnDemand = "on_demand"

	VisibilityPrivate = "private"
	VisibilityShared  = "shared"
)

// Limits (validation and resource bounds).
const (
	MaxLineupItems      = 5000   // items stored per lineup
	MaxLineupsPerUser   = 2000   // lineups per owner
	MaxChannelsPerUser  = 2000   // channels per owner
	MaxExpandedFiles    = 50000  // files one lineup expands to (the rest is dropped)
	MaxExpandWork       = 100000 // candidate files one expansion may examine (playable or not, repeats included)
	MaxSkippedReported  = 1000   // skipped entries kept per expansion (skipped_count has the total)
	MaxProgramSubtitles = 32     // subtitle tracks listed per program
	MaxItemLimit        = 100000 // LineupItem.limit upper bound
	MaxGuideHours       = 72
	MaxGuideChannels    = 200   // channels per guide request
	MaxGuidePrograms    = 20000 // programs per guide response (all channels)
	MaxChannelNumber    = 9999
	MaxAutoGenerate     = 200   // channels one auto-generate call may create
	MaxXMLTVPerChannel  = 10000 // programmes per channel in xmltv.xml
	MaxLogoBytes        = 20 << 20
	logoEdge            = 512
	nameMax             = 200
	descriptionMax      = 2000
	groupMax            = 100
	labelMax            = 200
	showTitleMax        = 500
	pathMax             = 4096
	queryFieldMax       = 200
	maxSeason           = 10000
	maxPositionSeq      = int64(1) << 50
	maxCategoryListSize = 16
)

// LineupQuery is the kind=query filter (same semantics as GET /media).
type LineupQuery struct {
	Category    string `json:"category,omitempty"`
	Subcategory string `json:"subcategory,omitempty"`
	Genre       string `json:"genre,omitempty"`
	Tag         string `json:"tag,omitempty"`
	Year        *int   `json:"year,omitempty"`
}

// LineupItem is one program source of a lineup (API.md §7 LineupItem). Only the fields of
// its kind are set.
type LineupItem struct {
	ID        string       `json:"id"`
	Kind      string       `json:"kind"`
	FileID    string       `json:"file_id,omitempty"`
	ShowTitle string       `json:"show_title,omitempty"`
	Season    *int         `json:"season,omitempty"`
	SpaceID   string       `json:"space_id,omitempty"`
	Path      *string      `json:"path,omitempty"`
	Recursive *bool        `json:"recursive,omitempty"`
	Query     *LineupQuery `json:"query,omitempty"`
	Order     string       `json:"order"`
	Limit     *int         `json:"limit,omitempty"`
	Label     string       `json:"label,omitempty"`
}

// itemParams is the JSON stored in tv_lineup_items.params.
type itemParams struct {
	FileID    string       `json:"file_id,omitempty"`
	ShowTitle string       `json:"show_title,omitempty"`
	Season    *int         `json:"season,omitempty"`
	SpaceID   string       `json:"space_id,omitempty"`
	Path      *string      `json:"path,omitempty"`
	Recursive *bool        `json:"recursive,omitempty"`
	Query     *LineupQuery `json:"query,omitempty"`
}

// Lineup is the API view of a lineup.
type Lineup struct {
	ID                   string       `json:"id"`
	Name                 string       `json:"name"`
	Description          string       `json:"description"`
	OwnerID              string       `json:"owner_id"`
	Items                []LineupItem `json:"items"`
	ItemCount            int          `json:"item_count"`
	TotalDurationSeconds float64      `json:"total_duration_seconds"`
	CreatedAt            string       `json:"created_at"`
	UpdatedAt            string       `json:"updated_at"`
}

// lineupRow is a stored lineup (without items).
type lineupRow struct {
	ID, OwnerID, Name, Description string
	CreatedAt, UpdatedAt           time.Time
}

// Channel is the API view of a channel.
type Channel struct {
	ID           string  `json:"id"`
	Number       int     `json:"number"`
	Name         string  `json:"name"`
	Description  string  `json:"description"`
	Group        string  `json:"group"`
	LogoURL      *string `json:"logo_url"`
	LineupID     string  `json:"lineup_id"`
	LineupName   string  `json:"lineup_name"`
	Shuffle      bool    `json:"shuffle"`
	ShuffleMode  string  `json:"shuffle_mode"`
	PlaybackMode string  `json:"playback_mode"`
	AnchorAt     string  `json:"anchor_at"`
	Visibility   string  `json:"visibility"`
	OwnerID      string  `json:"owner_id"`
	CreatedAt    string  `json:"created_at"`
	UpdatedAt    string  `json:"updated_at"`
}

// channelRow is a stored channel.
type channelRow struct {
	ID, OwnerID                     string
	Number                          int
	Name, Description, Group        string
	LineupID, LineupName            string
	Shuffle                         bool
	ShuffleMode, Seed, PlaybackMode string
	Visibility                      string
	AnchorAt                        time.Time
	LogoVersion                     string
	CreatedAt, UpdatedAt            time.Time
}

func (c *channelRow) api() Channel {
	out := Channel{
		ID: c.ID, Number: c.Number, Name: c.Name, Description: c.Description, Group: c.Group,
		LineupID: c.LineupID, LineupName: c.LineupName, Shuffle: c.Shuffle, ShuffleMode: c.ShuffleMode,
		PlaybackMode: c.PlaybackMode, AnchorAt: db.FormatTime(c.AnchorAt), Visibility: c.Visibility,
		OwnerID: c.OwnerID, CreatedAt: db.FormatTime(c.CreatedAt), UpdatedAt: db.FormatTime(c.UpdatedAt),
	}
	if c.LogoVersion != "" {
		u := "/api/v1/tv/channels/" + c.ID + "/logo?v=" + c.LogoVersion
		out.LogoURL = &u
	}
	return out
}

// Program is one airing (API.md §7 Program). URLs are channel-scoped.
type Program struct {
	FileID          string  `json:"file_id"`
	Title           string  `json:"title"`
	ShowTitle       string  `json:"show_title"`
	Season          *int    `json:"season"`
	Episode         *int    `json:"episode"`
	EpisodeTitle    string  `json:"episode_title"`
	StartsAt        string  `json:"starts_at"`
	EndsAt          string  `json:"ends_at"`
	DurationSeconds float64 `json:"duration_seconds"`
	PosterURL       *string `json:"poster_url"`
	StreamURL       string  `json:"stream_url"`
	TranscodeURL    *string `json:"transcode_url"`
	BrowserPlayable bool    `json:"browser_playable"`
	// Subtitles are the program's subtitle tracks with channel-scoped WebVTT URLs (never nil).
	Subtitles []ProgramSubtitle `json:"subtitles"`
}

// ProgramSubtitle is one subtitle track of a program (API.md §7 Program.subtitles). Index is
// the media item's subtitle index; URL is relative and channel-scoped.
type ProgramSubtitle struct {
	Index    int    `json:"index"`
	Language string `json:"language"`
	Label    string `json:"label"`
	URL      string `json:"url"`
}

// NowPlaying is the GET /tv/channels/{id}/now response.
type NowPlaying struct {
	Channel       Channel   `json:"channel"`
	Program       *Program  `json:"program"`
	OffsetSeconds float64   `json:"offset_seconds"`
	ServerTime    string    `json:"server_time"`
	Next          []Program `json:"next"`
	Index         *int64    `json:"index"`
}

// ExpandedItem / SkippedItem / Expanded are the GET /tv/lineups/{id}/expanded shapes.
type ExpandedItem struct {
	FileID          string  `json:"file_id"`
	Title           string  `json:"title"`
	DurationSeconds float64 `json:"duration_seconds"`
	SourceItemID    string  `json:"source_item_id"`
}

type SkippedItem struct {
	FileID       string `json:"file_id"`
	Reason       string `json:"reason"`
	SourceItemID string `json:"source_item_id,omitempty"`
	Title        string `json:"title,omitempty"`
}

type Expanded struct {
	Items                []ExpandedItem `json:"items"`
	TotalDurationSeconds float64        `json:"total_duration_seconds"`
	Skipped              []SkippedItem  `json:"skipped"`
	// SkippedCount is the number of skipped files; Skipped lists at most MaxSkippedReported.
	SkippedCount int `json:"skipped_count"`
	// Truncated is set when a resource limit (MaxExpandedFiles playable files or
	// MaxExpandWork examined files) stopped the expansion before the end of the lineup.
	Truncated bool `json:"truncated"`
}
