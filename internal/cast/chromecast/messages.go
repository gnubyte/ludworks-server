package chromecast

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"ludworks/internal/cast/renderer"
)

// JSON payloads of the receiver and media namespaces. Requests are built from typed structs;
// device messages are decoded into structs holding only the fields the sender uses, and every
// value is checked (receivers are untrusted LAN devices).

// Limits applied to values reported by the device.
const (
	maxSeconds      = 1e7 // positions/durations beyond ~115 days are treated as unknown
	maxPlaybackRate = 4
	maxStatusItems  = 64 // MEDIA_STATUS entries / applications examined per message
	maxIDLen        = 256
)

// envelope is the part of every payload used for dispatch.
type envelope struct {
	Type      string `json:"type"`
	RequestID int64  `json:"requestId"`
}

type typeOnly struct {
	Type string `json:"type"`
}

type connectMsg struct {
	Type   string   `json:"type"`
	Origin struct{} `json:"origin"`
}

type requestMsg struct {
	Type      string `json:"type"`
	RequestID int64  `json:"requestId"`
}

type launchMsg struct {
	Type      string `json:"type"`
	RequestID int64  `json:"requestId"`
	AppID     string `json:"appId"`
}

type stopAppMsg struct {
	Type      string `json:"type"`
	RequestID int64  `json:"requestId"`
	SessionID string `json:"sessionId"`
}

type volumeLevel struct {
	Level *float64 `json:"level,omitempty"`
	Muted *bool    `json:"muted,omitempty"`
}

type setVolumeMsg struct {
	Type      string      `json:"type"`
	RequestID int64       `json:"requestId"`
	Volume    volumeLevel `json:"volume"`
}

type mediaCmdMsg struct {
	Type           string   `json:"type"`
	RequestID      int64    `json:"requestId"`
	MediaSessionID int64    `json:"mediaSessionId"`
	CurrentTime    *float64 `json:"currentTime,omitempty"` // SEEK
}

type loadMsg struct {
	Type           string    `json:"type"`
	RequestID      int64     `json:"requestId"`
	SessionID      string    `json:"sessionId,omitempty"`
	Media          mediaInfo `json:"media"`
	CurrentTime    float64   `json:"currentTime"`
	Autoplay       bool      `json:"autoplay"`
	ActiveTrackIDs []int     `json:"activeTrackIds"`
}

type mediaInfo struct {
	ContentID      string          `json:"contentId"`
	ContentType    string          `json:"contentType"`
	StreamType     string          `json:"streamType"`
	Duration       float64         `json:"duration,omitempty"`
	Metadata       mediaMetadata   `json:"metadata"`
	Tracks         []mediaTrack    `json:"tracks,omitempty"`
	TextTrackStyle *textTrackStyle `json:"textTrackStyle,omitempty"`
}

// Metadata types of the Cast media namespace.
const (
	metadataGeneric    = 0
	metadataMovie      = 1
	metadataTvShow     = 2
	metadataMusicTrack = 3
)

type mediaMetadata struct {
	MetadataType int     `json:"metadataType"`
	Title        string  `json:"title,omitempty"`
	Subtitle     string  `json:"subtitle,omitempty"`
	SeriesTitle  string  `json:"seriesTitle,omitempty"`
	Season       int     `json:"season,omitempty"`
	Episode      int     `json:"episode,omitempty"`
	Artist       string  `json:"artist,omitempty"`
	AlbumName    string  `json:"albumName,omitempty"`
	ReleaseDate  string  `json:"releaseDate,omitempty"`
	Images       []image `json:"images,omitempty"`
}

type image struct {
	URL string `json:"url"`
}

type mediaTrack struct {
	TrackID          int    `json:"trackId"`
	Type             string `json:"type"`
	Subtype          string `json:"subtype"`
	TrackContentID   string `json:"trackContentId"`
	TrackContentType string `json:"trackContentType"`
	Name             string `json:"name,omitempty"`
	Language         string `json:"language,omitempty"`
}

type textTrackStyle struct {
	BackgroundColor string  `json:"backgroundColor"`
	ForegroundColor string  `json:"foregroundColor"`
	EdgeType        string  `json:"edgeType"`
	EdgeColor       string  `json:"edgeColor"`
	FontScale       float64 `json:"fontScale"`
}

// defaultTextStyle: white text with a black outline on a transparent background.
var defaultTextStyle = textTrackStyle{
	BackgroundColor: "#00000000", ForegroundColor: "#FFFFFFFF", EdgeType: "OUTLINE", EdgeColor: "#000000FF", FontScale: 1,
}

// streamType returns the Cast stream type for req: LIVE only for live items without a known
// duration (a channel program is a file with a duration that starts at an offset, which LIVE
// would not honour).
func streamType(req *renderer.LoadRequest) string {
	if req.Live && !(req.DurationSeconds > 0) {
		return "LIVE"
	}
	return "BUFFERED"
}

// metadataFor maps a load request to the most specific Cast metadata type.
func metadataFor(req *renderer.LoadRequest) mediaMetadata {
	md := mediaMetadata{Title: req.Title}
	if req.ArtURL != "" {
		md.Images = []image{{URL: req.ArtURL}}
	}
	year := ""
	if req.Year > 0 && req.Year < 10000 {
		year = strconv.Itoa(req.Year)
	}
	switch {
	case req.Video && req.SeriesTitle != "":
		md.MetadataType = metadataTvShow
		md.SeriesTitle = req.SeriesTitle
		md.Season = max(req.Season, 0)
		md.Episode = max(req.Episode, 0)
	case req.Video:
		md.MetadataType = metadataMovie
		md.Subtitle = req.Subtitle
		md.ReleaseDate = year
	case req.Artist != "" || req.Album != "":
		md.MetadataType = metadataMusicTrack
		md.Artist = req.Artist
		md.AlbumName = req.Album
		md.ReleaseDate = year
	default:
		md.MetadataType = metadataGeneric
		md.Subtitle = req.Subtitle
	}
	return md
}

// buildLoad builds the LOAD request for req.
func buildLoad(id int64, appSession string, req *renderer.LoadRequest) loadMsg {
	mi := mediaInfo{
		ContentID: req.URL, ContentType: req.ContentType, StreamType: streamType(req),
		Metadata: metadataFor(req),
	}
	if d := req.DurationSeconds; d > 0 && d <= maxSeconds && !math.IsInf(d, 0) {
		mi.Duration = d
	}
	for i, s := range req.Subtitles {
		mi.Tracks = append(mi.Tracks, mediaTrack{
			TrackID: i + 1, Type: "TEXT", Subtype: "SUBTITLES", TrackContentID: s.URL, TrackContentType: "text/vtt",
			Name: s.Label, Language: s.Language,
		})
	}
	if len(mi.Tracks) > 0 {
		st := defaultTextStyle
		mi.TextTrackStyle = &st
	}
	start := req.StartSeconds
	if !(start > 0) || start > maxSeconds { // also catches NaN
		start = 0
	}
	return loadMsg{
		Type: "LOAD", RequestID: id, SessionID: appSession, Media: mi, CurrentTime: start, Autoplay: true,
		ActiveTrackIDs: []int{},
	}
}

/* ------------------------------------------------------------- device messages */

// receiverStatusMsg is RECEIVER_STATUS (and LAUNCH_ERROR / INVALID_REQUEST on the receiver
// namespace).
type receiverStatusMsg struct {
	Type      string `json:"type"`
	RequestID int64  `json:"requestId"`
	Reason    string `json:"reason"`
	Status    *struct {
		Applications []application `json:"applications"`
		Volume       *struct {
			Level *float64 `json:"level"`
			Muted *bool    `json:"muted"`
		} `json:"volume"`
	} `json:"status"`
}

type application struct {
	AppID       string `json:"appId"`
	SessionID   string `json:"sessionId"`
	TransportID string `json:"transportId"`
}

// mediaStatusMsg is MEDIA_STATUS and the media namespace's error replies (LOAD_FAILED,
// LOAD_CANCELLED, INVALID_REQUEST, INVALID_PLAYER_STATE).
type mediaStatusMsg struct {
	Type              string        `json:"type"`
	RequestID         int64         `json:"requestId"`
	Reason            string        `json:"reason"`
	DetailedErrorCode int           `json:"detailedErrorCode"`
	Status            []mediaStatus `json:"status"`
}

type mediaStatus struct {
	MediaSessionID int64    `json:"mediaSessionId"`
	PlaybackRate   *float64 `json:"playbackRate"`
	PlayerState    string   `json:"playerState"`
	IdleReason     string   `json:"idleReason"`
	CurrentTime    *float64 `json:"currentTime"`
	Media          *struct {
		StreamType string   `json:"streamType"`
		Duration   *float64 `json:"duration"`
	} `json:"media"`
}

// seconds validates a device-reported time: nil, negative or absurd values are not usable.
func seconds(v *float64) (float64, bool) {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) || *v < 0 || *v > maxSeconds {
		return 0, false
	}
	return *v, true
}

// rate validates a playbackRate (default 1).
func rate(v *float64) float64 {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) || *v < 0 {
		return 1
	}
	return math.Min(*v, maxPlaybackRate)
}

// level validates a volume level, clamping it to 0–1.
func level(v *float64) (float64, bool) {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) {
		return 0, false
	}
	return math.Max(0, math.Min(*v, 1)), true
}

// validID accepts session and transport ids: short, printable ASCII, no spaces.
func validID(s string) bool {
	if s == "" || len(s) > maxIDLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] <= ' ' || s[i] > '~' {
			return false
		}
	}
	return true
}

// decode unmarshals a payload into v, returning false for invalid JSON (ignored by callers).
func decode(payload string, v any) bool {
	return json.Unmarshal([]byte(payload), v) == nil
}

// redactURL hides the capability ticket of a cast media URL (".../cast/media/<ticket>/...")
// and drops the query string, for debug logs.
func redactURL(u string) string {
	if i := strings.IndexByte(u, '?'); i >= 0 {
		u = u[:i]
	}
	const marker = "/cast/media/"
	i := strings.Index(u, marker)
	if i < 0 {
		return u
	}
	rest := u[i+len(marker):]
	if j := strings.IndexByte(rest, '/'); j >= 0 {
		return u[:i+len(marker)] + "[redacted]" + rest[j:]
	}
	return u[:i+len(marker)] + "[redacted]"
}
