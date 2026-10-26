package admin

import (
	"encoding/json"
	"net/http"
	netmail "net/mail"
	"sort"
	"strings"

	"ludworks/internal/core"
	"ludworks/internal/httpx"
	"ludworks/internal/settings"
)

func (m *Module) getSettings(w http.ResponseWriter, r *http.Request) error {
	view, err := m.d.Settings.PublicView(r.Context())
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, view)
}

// patchSettings applies a partial update to any namespaces atomically and returns the full
// redacted settings. Only field names are audited, never values (some are secrets).
func (m *Module) patchSettings(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	var patch map[string]json.RawMessage
	if err := httpx.DecodeJSON(r, &patch); err != nil {
		return err
	}
	if patch == nil {
		return core.BadRequest("body must be a JSON object of settings namespaces")
	}
	changes := map[string][]string{}
	for ns, raw := range patch {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err == nil {
			for k := range fields {
				changes[ns] = append(changes[ns], k)
			}
			sort.Strings(changes[ns])
		}
	}
	if len(patch) > 0 {
		if err := m.d.Settings.UpdateMany(ctx, patch); err != nil {
			return err
		}
		nss := make([]string, 0, len(changes))
		for ns := range changes {
			nss = append(nss, ns)
		}
		sort.Strings(nss)
		m.audit(ctx, "admin.settings.update", "settings", strings.Join(nss, ","), map[string]any{"changes": changes})
	}
	view, err := m.d.Settings.PublicView(ctx)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, view)
}

type smtpTestRequest struct {
	To string `json:"to"`
}

func (m *Module) smtpTest(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	var req smtpTestRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	to := strings.TrimSpace(req.To)
	if a, err := netmail.ParseAddress(to); err != nil || a.Address != to || len(to) > 254 || settings.HasControl(to) {
		return core.Validation(map[string]string{"to": "must be a plain email address"})
	}
	err := m.mail.SendTest(ctx, to)
	m.audit(ctx, "admin.settings.smtp_test", "settings", "smtp", map[string]any{"ok": err == nil})
	if err != nil {
		return err
	}
	return httpx.NoContent(w)
}

func (m *Module) imapTest(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	res, err := m.mail.TestIMAP(ctx)
	m.audit(ctx, "admin.settings.imap_test", "settings", "imap", map[string]any{"ok": err == nil})
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, res)
}
