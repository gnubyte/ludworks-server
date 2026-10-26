package admin

import (
	"net/http"

	"ludworks/internal/audit"
	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/httpx"
	"ludworks/internal/settings"
)

// auditEvent is the API shape: actor_id is null for system/anonymous events.
type auditEvent struct {
	ID            string         `json:"id"`
	Time          string         `json:"time"`
	ActorID       *string        `json:"actor_id"`
	ActorUsername string         `json:"actor_username"`
	Action        string         `json:"action"`
	TargetType    string         `json:"target_type"`
	TargetID      string         `json:"target_id"`
	IP            string         `json:"ip"`
	UserAgent     string         `json:"user_agent"`
	RequestID     string         `json:"request_id"`
	Meta          map[string]any `json:"meta"`
}

func (m *Module) listAudit(w http.ResponseWriter, r *http.Request) error {
	al, ok := m.d.Audit.(*audit.Log)
	if !ok {
		return core.NotImplemented("The audit log cannot be queried")
	}
	qv := r.URL.Query()
	q := audit.Query{ActorID: qv.Get("actor_id"), Action: qv.Get("action"), Limit: httpx.ParseLimit(r, 50, 200)}
	fields := map[string]string{}
	if q.ActorID != "" && !httpx.ValidID(q.ActorID) {
		fields["actor_id"] = "invalid id"
	}
	if len(q.Action) > 100 || settings.HasControl(q.Action) {
		fields["action"] = "invalid action"
	}
	if len(fields) > 0 {
		return core.Validation(fields)
	}
	if c := qv.Get("cursor"); c != "" {
		var cur audit.Cursor
		if err := httpx.DecodeCursor(c, &cur); err != nil {
			return err
		}
		q.After = &cur
	}
	entries, next, err := al.List(r.Context(), q)
	if err != nil {
		return err
	}
	items := make([]auditEvent, len(entries))
	for i, e := range entries {
		ev := auditEvent{ID: e.ID, Time: db.FormatTime(e.Time), ActorUsername: e.ActorUsername, Action: e.Action,
			TargetType: e.TargetType, TargetID: e.TargetID, IP: e.IP, UserAgent: e.UserAgent, RequestID: e.RequestID, Meta: e.Meta}
		if e.ActorID != "" {
			id := e.ActorID
			ev.ActorID = &id
		}
		if ev.Meta == nil {
			ev.Meta = map[string]any{}
		}
		items[i] = ev
	}
	nextCursor := ""
	if next != nil {
		nextCursor = httpx.EncodeCursor(*next)
	}
	return httpx.WriteJSON(w, http.StatusOK, httpx.NewPage(items, nextCursor))
}
