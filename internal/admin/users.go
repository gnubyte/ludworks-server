package admin

import (
	"context"
	"net/http"

	"ludworks/internal/auth"
	"ludworks/internal/core"
	"ludworks/internal/events"
	"ludworks/internal/httpx"
)

func (m *Module) listUsers(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query().Get("q")
	if len(q) > 200 {
		return core.Validation(map[string]string{"q": "must be at most 200 characters"})
	}
	users, next, err := m.auth.ListUsers(r.Context(), q, r.URL.Query().Get("cursor"), httpx.ParseLimit(r, 50, 200))
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, httpx.NewPage(users, next))
}

func (m *Module) createUser(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	var in auth.CreateUserInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	u, err := m.auth.CreateUser(ctx, in)
	if err != nil {
		return err
	}
	m.audit(ctx, "admin.user.create", "user", u.ID, map[string]any{"username": u.Username, "role": u.Role})
	return httpx.WriteJSON(w, http.StatusCreated, u)
}

type userDetail struct {
	auth.User
	UsageBytes int64 `json:"usage_bytes"`
}

func (m *Module) getUser(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	u, err := m.auth.GetUserByID(ctx, id)
	if err != nil {
		return err
	}
	usage, err := m.usageBytes(ctx, id)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, userDetail{User: u, UsageBytes: usage})
}

// usageBytes sums the user's non-trashed files (home space and files they own in
// libraries). It is 0 until the files module has created its table.
func (m *Module) usageBytes(ctx context.Context, userID string) (int64, error) {
	var n int
	if err := m.d.DB.SQL.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'files'`).Scan(&n); err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, nil
	}
	var total int64
	err := m.d.DB.SQL.QueryRowContext(ctx, `SELECT COALESCE(SUM(size), 0) FROM files
		WHERE owner_id = ? AND is_dir = 0 AND trashed_at IS NULL`, userID).Scan(&total)
	return total, err
}

func (m *Module) patchUser(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	var p auth.UserPatch
	if err := httpx.DecodeJSON(r, &p); err != nil {
		return err
	}
	actor := core.MustPrincipal(ctx)
	u, changed, err := m.auth.UpdateUser(ctx, actor.UserID, id, p)
	if err != nil {
		return err
	}
	if len(changed) > 0 {
		meta := map[string]any{"username": u.Username, "fields": changed}
		if p.Role != nil {
			meta["role"] = u.Role
		}
		if p.Disabled != nil {
			meta["disabled"] = u.Disabled
		}
		m.audit(ctx, "admin.user.update", "user", id, meta)
	}
	return httpx.WriteJSON(w, http.StatusOK, u)
}

func (m *Module) deleteUser(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	deleteFiles := httpx.QueryBool(r, "delete_files")
	u, err := m.auth.DeleteUser(ctx, core.MustPrincipal(ctx).UserID, id)
	if err != nil {
		return err
	}
	// Other modules drop the user's notes, vault, workbooks, lineups... on this event.
	m.d.Events.Publish(events.UserDeleted, id)
	if deleteFiles {
		m.d.Events.Publish(TopicUserDeleteFiles, id)
	}
	m.audit(ctx, "admin.user.delete", "user", id, map[string]any{"username": u.Username, "delete_files": deleteFiles})
	return httpx.NoContent(w)
}

type passwordSet struct {
	NewPassword string `json:"new_password"`
}

func (m *Module) setPassword(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	var req passwordSet
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	u, err := m.auth.SetUserPassword(ctx, id, req.NewPassword)
	if err != nil {
		return err
	}
	m.audit(ctx, "admin.user.password", "user", id, map[string]any{"username": u.Username})
	return httpx.NoContent(w)
}

func (m *Module) revokeSessions(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	u, err := m.auth.RevokeUserSessions(ctx, id, true)
	if err != nil {
		return err
	}
	m.audit(ctx, "admin.user.revoke_sessions", "user", id, map[string]any{"username": u.Username})
	return httpx.NoContent(w)
}

func (m *Module) removeTOTP(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	u, err := m.auth.RemoveUserTOTP(ctx, id)
	if err != nil {
		return err
	}
	m.audit(ctx, "admin.user.totp_removed", "user", id, map[string]any{"username": u.Username})
	return httpx.NoContent(w)
}
