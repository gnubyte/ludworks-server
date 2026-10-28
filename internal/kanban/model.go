package kanban

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/httpx"
)

// Limits (API.md §13).
const (
	MaxBoardsPerUser      = 500
	MaxMembers            = 100
	MaxColumns            = 100
	MaxCards              = 10_000 // per board, archived included
	MaxLabels             = 200
	MaxSubtasks           = 200 // per card
	MaxComments           = 5000
	MaxAttachments        = 100
	MaxAttachmentBytes    = 50 << 20
	MaxBoardAttachBytes   = 2 << 30
	maxNameLen            = 200
	maxTitleLen           = 500
	maxDescriptionLen     = 100_000
	maxCommentLen         = 20_000
	maxWIPLimit           = 10_000
	listDescriptionLen    = 200 // descriptions in lists are cut to this many characters
	detailComments        = 100
	detailActivity        = 50
	maxActivityPerBoard   = 10_000
	maxUserResults        = 20
	maxCardRequestBody    = 1 << 20 // JSON: a 100 000-character description, escaped
	maxCommentRequestBody = 256 << 10
)

// Roles, from least to most privileged.
type role int

const (
	roleNone role = iota
	roleViewer
	roleEditor
	roleAdmin
	roleOwner
)

func parseRole(s string) role {
	switch s {
	case "viewer":
		return roleViewer
	case "editor":
		return roleEditor
	case "admin":
		return roleAdmin
	case "owner":
		return roleOwner
	}
	return roleNone
}

func (r role) String() string {
	switch r {
	case roleViewer:
		return "viewer"
	case roleEditor:
		return "editor"
	case roleAdmin:
		return "admin"
	case roleOwner:
		return "owner"
	}
	return ""
}

// colors are the label/board/column colors.
var colors = map[string]bool{
	"gray": true, "red": true, "orange": true, "amber": true, "yellow": true, "green": true,
	"teal": true, "blue": true, "indigo": true, "purple": true, "pink": true,
}

const colorList = "gray, red, orange, amber, yellow, green, teal, blue, indigo, purple or pink"

var priorities = map[string]bool{"none": true, "low": true, "medium": true, "high": true, "urgent": true}

// Member is a board member.
type Member struct {
	UserID      string `json:"user_id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
}

// Label is a board label.
type Label struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
}

// Column is a board column (a list of cards).
type Column struct {
	ID        string  `json:"id"`
	BoardID   string  `json:"board_id"`
	Name      string  `json:"name"`
	Position  int     `json:"position"`
	Color     *string `json:"color"`
	WIPLimit  *int    `json:"wip_limit"`
	Collapsed bool    `json:"collapsed"`
	CardCount int     `json:"card_count"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
}

// Card is a card. Description is cut to listDescriptionLen characters in lists.
type Card struct {
	ID                string   `json:"id"`
	BoardID           string   `json:"board_id"`
	ColumnID          string   `json:"column_id"`
	Number            int      `json:"number"`
	Title             string   `json:"title"`
	Description       string   `json:"description"`
	Position          int      `json:"position"`
	Labels            []string `json:"labels"`
	Assignees         []string `json:"assignees"`
	Priority          string   `json:"priority"`
	StartAt           *string  `json:"start_at"`
	DueAt             *string  `json:"due_at"`
	AllDay            bool     `json:"all_day"`
	Completed         bool     `json:"completed"`
	CompletedAt       *string  `json:"completed_at"`
	CoverAttachmentID *string  `json:"cover_attachment_id"`
	SubtasksTotal     int      `json:"subtasks_total"`
	SubtasksDone      int      `json:"subtasks_done"`
	CommentCount      int      `json:"comment_count"`
	AttachmentCount   int      `json:"attachment_count"`
	Archived          bool     `json:"archived"`
	ArchivedAt        *string  `json:"archived_at"`
	CreatedBy         string   `json:"created_by"`
	CreatedAt         string   `json:"created_at"`
	UpdatedAt         string   `json:"updated_at"`
	Version           int64    `json:"version"`
}

// CardListItem is an entry of GET /kanban/cards.
type CardListItem struct {
	Card
	BoardName  string `json:"board_name"`
	ColumnName string `json:"column_name"`
}

// CardDetail is GET /kanban/cards/{id}.
type CardDetail struct {
	Card
	Subtasks    []Subtask    `json:"subtasks"`
	Attachments []Attachment `json:"attachments"`
	Comments    []Comment    `json:"comments"`
	Activity    []Activity   `json:"activity"`
}

// Subtask is a checklist item of a card.
type Subtask struct {
	ID          string  `json:"id"`
	CardID      string  `json:"card_id"`
	Title       string  `json:"title"`
	Done        bool    `json:"done"`
	Position    int     `json:"position"`
	DueAt       *string `json:"due_at"`
	AssigneeID  *string `json:"assignee_id"`
	CompletedAt *string `json:"completed_at"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
}

// Comment is a card comment (Markdown).
type Comment struct {
	ID         string `json:"id"`
	CardID     string `json:"card_id"`
	AuthorID   string `json:"author_id"`
	AuthorName string `json:"author_name"`
	Body       string `json:"body"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
	Edited     bool   `json:"edited"`

	n int64
}

// Attachment is a file attached to a card.
type Attachment struct {
	ID           string  `json:"id"`
	CardID       string  `json:"card_id"`
	Filename     string  `json:"filename"`
	Mime         string  `json:"mime"`
	Size         int64   `json:"size"`
	IsImage      bool    `json:"is_image"`
	URL          string  `json:"url"`
	ThumbnailURL *string `json:"thumbnail_url"`
	UploadedBy   string  `json:"uploaded_by"`
	CreatedAt    string  `json:"created_at"`

	boardID    string
	thumbState int
}

// Activity is an entry of a board's activity log.
type Activity struct {
	ID        string         `json:"id"`
	BoardID   string         `json:"board_id"`
	CardID    *string        `json:"card_id"`
	ActorID   string         `json:"actor_id"`
	ActorName string         `json:"actor_name"`
	Action    string         `json:"action"`
	Meta      map[string]any `json:"meta"`
	CreatedAt string         `json:"created_at"`

	n int64
}

// Board is GET /kanban/boards/{id}.
type Board struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Color       string   `json:"color"`
	OwnerID     string   `json:"owner_id"`
	Role        string   `json:"role"`
	Archived    bool     `json:"archived"`
	Members     []Member `json:"members"`
	Labels      []Label  `json:"labels"`
	Columns     []Column `json:"columns"`
	Cards       []Card   `json:"cards"`
	CardCount   int      `json:"card_count"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
	Version     int64    `json:"version"`
}

// BoardSummary is an entry of GET /kanban/boards.
type BoardSummary struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Description   string   `json:"description"`
	Color         string   `json:"color"`
	OwnerID       string   `json:"owner_id"`
	Role          string   `json:"role"`
	Archived      bool     `json:"archived"`
	Members       []Member `json:"members"`
	CardCount     int      `json:"card_count"`
	MemberCount   int      `json:"member_count"`
	OpenCardCount int      `json:"open_card_count"`
	OverdueCount  int      `json:"overdue_count"`
	CreatedAt     string   `json:"created_at"`
	UpdatedAt     string   `json:"updated_at"`
	Version       int64    `json:"version"`
}

// UserResult is an entry of GET /kanban/users.
type UserResult struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
}

/* ------------------------------------------------------------- JSON input */

// optional captures presence and null-ness of a JSON field (PATCH semantics).
type optional[T any] struct {
	Set  bool
	Null bool
	V    T
}

func (o *optional[T]) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.Null = true
		return nil
	}
	return json.Unmarshal(b, &o.V)
}

// fieldErrors collects 422 messages.
type fieldErrors map[string]string

func (f fieldErrors) add(field, msg string) {
	if _, ok := f[field]; !ok {
		f[field] = msg
	}
}

func (f fieldErrors) err() error {
	if len(f) == 0 {
		return nil
	}
	return core.Validation(f)
}

func validation(field, msg string) error { return core.Validation(map[string]string{field: msg}) }

/* --------------------------------------------------------- text validation */

// cleanLine normalises a single-line name or title: tabs and line breaks become spaces, the
// result is trimmed, and other control characters are refused. max counts characters.
func cleanLine(s string, max int, required bool) (string, string) {
	s = strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' || r == ' ' || r == ' ' {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	switch {
	case !utf8.ValidString(s):
		return "", "must be valid UTF-8"
	case required && s == "":
		return "", "is required"
	case strings.ContainsFunc(s, unicode.IsControl):
		return "", "must not contain control characters"
	case utf8.RuneCountInString(s) > max:
		return "", "must be at most " + itoa(max) + " characters"
	}
	return s, ""
}

// cleanText checks a multi-line Markdown text (descriptions, comments): line breaks and tabs
// are kept, NUL and other C0 controls are refused.
func cleanText(s string, max int) (string, string) {
	switch {
	case !utf8.ValidString(s):
		return "", "must be valid UTF-8"
	case strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 && r != '\n' && r != '\r' && r != '\t' }):
		return "", "must not contain control characters"
	case utf8.RuneCountInString(s) > max:
		return "", "must be at most " + itoa(max) + " characters"
	}
	return s, ""
}

// excerpt cuts s to n characters (lists carry short descriptions).
func excerpt(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := 0
	for j := range s {
		if i == n {
			return s[:j]
		}
		i++
	}
	return s
}

func itoa(n int) string { return strconv.Itoa(n) }

// parseTimeField parses an RFC 3339 time into the stored fixed-width UTC form.
func parseTimeField(v string) (string, string) {
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(v))
	if err != nil {
		return "", "must be an RFC 3339 time"
	}
	if y := t.UTC().Year(); y < 1900 || y > 9999 {
		return "", "is out of range"
	}
	return db.FormatTime(t), ""
}

// optTime resolves an optional time field: (value, set, problem).
func optTime(o optional[string]) (*string, string) {
	if !o.Set || o.Null || strings.TrimSpace(o.V) == "" {
		return nil, ""
	}
	v, problem := parseTimeField(o.V)
	if problem != "" {
		return nil, problem
	}
	return &v, ""
}

// validIDs checks a list of ids (format, duplicates removed, order kept).
func validIDs(ids []string, max int) ([]string, string) {
	if len(ids) > max {
		return nil, "too many entries"
	}
	out := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		if !httpx.ValidID(id) {
			return nil, "contains an invalid id"
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, ""
}

// queryBoolPtr parses an optional boolean query parameter (nil when absent or empty).
func queryBoolPtr(r *http.Request, key string) (*bool, error) {
	v := strings.ToLower(strings.TrimSpace(r.URL.Query().Get(key)))
	switch v {
	case "":
		return nil, nil
	case "1", "true", "yes", "on":
		b := true
		return &b, nil
	case "0", "false", "no", "off":
		b := false
		return &b, nil
	}
	return nil, validation(key, "must be true or false")
}

func now() string { return db.FormatTime(core.Now()) }

func displayName(username, display string) string {
	if strings.TrimSpace(display) != "" {
		return display
	}
	return username
}

// deletedUser is shown for authors and actors whose account no longer exists.
const deletedUser = "Deleted user"

// errBoardNotFound is returned for missing boards and boards the caller is not a member of.
var (
	errBoardNotFound      = core.NotFound("Board not found")
	errColumnNotFound     = core.NotFound("Column not found")
	errCardNotFound       = core.NotFound("Card not found")
	errLabelNotFound      = core.NotFound("Label not found")
	errSubtaskNotFound    = core.NotFound("Subtask not found")
	errCommentNotFound    = core.NotFound("Comment not found")
	errAttachmentNotFound = core.NotFound("Attachment not found")
)

func needRole(have, want role) error {
	if have >= want {
		return nil
	}
	switch want {
	case roleOwner:
		return core.Forbidden("Only the board owner can do this")
	case roleAdmin:
		return core.Forbidden("Only board admins can do this")
	default:
		return core.Forbidden("Viewers cannot change this board")
	}
}

func tooMany(what string, max int) error {
	return core.Conflict("Too many " + what + " (maximum " + itoa(max) + ")")
}
