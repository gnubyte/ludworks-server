package files

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/httpx"
	"ludworks/internal/settings"
)

// Backup devices (API.md §5 /backup/devices): desktop/mobile backup clients register here
// and report their runs. The files themselves arrive through the normal upload APIs.

const maxDevicesPerUser = 100

// BackupFolder maps a device folder to a destination in a space.
type BackupFolder struct {
	LocalPath     string `json:"local_path"`
	RemoteSpaceID string `json:"remote_space_id"`
	RemotePath    string `json:"remote_path"`
}

type apiDevice struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	Platform      string         `json:"platform"`
	Folders       []BackupFolder `json:"folders"`
	LastSyncAt    *string        `json:"last_sync_at"`
	LastStatus    *string        `json:"last_status"`
	LastError     *string        `json:"last_error"`
	BytesUploaded int64          `json:"bytes_uploaded"`
	FilesUploaded int64          `json:"files_uploaded"`
}

const deviceCols = `id, name, platform, folders, last_sync_at, last_status, last_error, bytes_uploaded, files_uploaded`

func scanDevice(sc scanner) (*apiDevice, error) {
	var (
		d          apiDevice
		folders    db.JSON[[]BackupFolder]
		last       db.NullTime
		status, er sql.NullString
	)
	if err := sc.Scan(&d.ID, &d.Name, &d.Platform, &folders, &last, &status, &er, &d.BytesUploaded, &d.FilesUploaded); err != nil {
		return nil, err
	}
	d.Folders = folders.V
	if d.Folders == nil {
		d.Folders = []BackupFolder{}
	}
	if last.Valid {
		t := db.FormatTime(last.Time)
		d.LastSyncAt = &t
	}
	if status.Valid {
		d.LastStatus = &status.String
	}
	if er.Valid {
		d.LastError = &er.String
	}
	return &d, nil
}

func (m *Module) device(ctx context.Context, r *http.Request) (*apiDevice, error) {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return nil, err
	}
	d, err := scanDevice(m.d.DB.SQL.QueryRowContext(ctx, `SELECT `+deviceCols+` FROM backup_devices WHERE id = ? AND user_id = ?`,
		id, core.MustPrincipal(ctx).UserID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, core.NotFound("Device not found")
	}
	return d, err
}

func checkText(fe map[string]string, field, v string, minLen, maxLen int) string {
	v = strings.TrimSpace(v)
	if n := utf8.RuneCountInString(v); n < minLen || n > maxLen || settings.HasControl(v) || !utf8.ValidString(v) {
		fe[field] = fmt.Sprintf("must be %d-%d characters without control characters", minLen, maxLen)
	}
	return v
}

func (m *Module) listDevices(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	rs, err := m.d.DB.SQL.QueryContext(ctx, `SELECT `+deviceCols+` FROM backup_devices WHERE user_id = ? ORDER BY created_at, id`,
		core.MustPrincipal(ctx).UserID)
	if err != nil {
		return err
	}
	defer rs.Close()
	out := []*apiDevice{}
	for rs.Next() {
		d, err := scanDevice(rs)
		if err != nil {
			return err
		}
		out = append(out, d)
	}
	if err := rs.Err(); err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

func (m *Module) createDevice(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Name     string `json:"name"`
		Platform string `json:"platform"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	fe := map[string]string{}
	in.Name = checkText(fe, "name", in.Name, 1, 100)
	in.Platform = strings.ToLower(checkText(fe, "platform", in.Platform, 1, 32))
	if len(fe) > 0 {
		return core.Validation(fe)
	}
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	var n int
	if err := m.d.DB.SQL.QueryRowContext(ctx, `SELECT count(*) FROM backup_devices WHERE user_id = ?`, p.UserID).Scan(&n); err != nil {
		return err
	}
	if n >= maxDevicesPerUser {
		return core.Conflict("Too many backup devices; remove an old one first")
	}
	id := core.NewID()
	now := db.FormatTime(m.svc.now())
	if err := m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO backup_devices (id, user_id, name, platform, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
			id, p.UserID, in.Name, in.Platform, now, now)
		return err
	}); err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusCreated, &apiDevice{ID: id, Name: in.Name, Platform: in.Platform, Folders: []BackupFolder{}})
}

func (m *Module) updateDevice(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	d, err := m.device(ctx, r)
	if err != nil {
		return err
	}
	var in struct {
		Name    *string         `json:"name"`
		Folders *[]BackupFolder `json:"folders"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	p := core.MustPrincipal(ctx)
	fe := map[string]string{}
	if in.Name != nil {
		d.Name = checkText(fe, "name", *in.Name, 1, 100)
	}
	if in.Folders != nil {
		fs := *in.Folders
		if len(fs) > 100 {
			fe["folders"] = "at most 100 folders"
		}
		for i := range fs {
			f := &fs[i]
			f.LocalPath = checkText(fe, "folders", f.LocalPath, 1, 4096)
			rp, err := cleanPath("folders", f.RemotePath)
			if err != nil {
				fe["folders"] = "invalid remote_path"
				continue
			}
			f.RemotePath = rp
			if !httpx.ValidID(f.RemoteSpaceID) || !m.svc.CanWrite(ctx, p, f.RemoteSpaceID) {
				fe["folders"] = "remote_space_id must be a space you can write"
			}
		}
		d.Folders = fs
	}
	if len(fe) > 0 {
		return core.Validation(fe)
	}
	if err := m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE backup_devices SET name = ?, folders = ?, updated_at = ? WHERE id = ? AND user_id = ?`,
			d.Name, db.JSON[[]BackupFolder]{V: d.Folders}, db.FormatTime(m.svc.now()), d.ID, p.UserID)
		return err
	}); err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, d)
}

func (m *Module) reportDevice(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	d, err := m.device(ctx, r)
	if err != nil {
		return err
	}
	var in struct {
		Status        string `json:"status"`
		Error         string `json:"error"`
		BytesUploaded *int64 `json:"bytes_uploaded"`
		FilesUploaded *int64 `json:"files_uploaded"`
		StartedAt     string `json:"started_at"`
		FinishedAt    string `json:"finished_at"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	fe := map[string]string{}
	switch in.Status {
	case "ok", "error", "running":
	default:
		fe["status"] = "must be ok, error or running"
	}
	if utf8.RuneCountInString(in.Error) > 4000 {
		fe["error"] = "at most 4000 characters"
	}
	if (in.BytesUploaded != nil && *in.BytesUploaded < 0) || (in.FilesUploaded != nil && *in.FilesUploaded < 0) {
		fe["bytes_uploaded"] = "must not be negative"
	}
	started, err := timeParam("started_at", in.StartedAt)
	if err != nil || started.IsZero() {
		fe["started_at"] = "required RFC 3339 timestamp"
	}
	finished, err := timeParam("finished_at", in.FinishedAt)
	if err != nil {
		fe["finished_at"] = "must be an RFC 3339 timestamp"
	}
	if len(fe) > 0 {
		return core.Validation(fe)
	}
	syncAt := started
	if !finished.IsZero() {
		syncAt = finished
	}
	if syncAt.After(m.svc.now().Add(24 * time.Hour)) {
		syncAt = m.svc.now()
	}
	var errText any
	if in.Status == "error" {
		errText = strings.ToValidUTF8(strings.TrimSpace(in.Error), "")
	}
	bytes, files := d.BytesUploaded, d.FilesUploaded
	if in.BytesUploaded != nil {
		bytes = *in.BytesUploaded
	}
	if in.FilesUploaded != nil {
		files = *in.FilesUploaded
	}
	if err := m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE backup_devices SET last_status = ?, last_error = ?, last_sync_at = ?, bytes_uploaded = ?,
			files_uploaded = ?, updated_at = ? WHERE id = ? AND user_id = ?`, in.Status, errText, db.FormatTime(syncAt), bytes, files,
			db.FormatTime(m.svc.now()), d.ID, core.MustPrincipal(ctx).UserID)
		return err
	}); err != nil {
		return err
	}
	return httpx.NoContent(w)
}

func (m *Module) deleteDevice(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	d, err := m.device(ctx, r)
	if err != nil {
		return err
	}
	if err := m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM backup_devices WHERE id = ? AND user_id = ?`, d.ID, core.MustPrincipal(ctx).UserID)
		return err
	}); err != nil {
		return err
	}
	return httpx.NoContent(w)
}
