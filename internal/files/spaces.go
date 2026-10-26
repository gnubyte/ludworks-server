package files

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/httpx"
	"ludworks/internal/settings"
)

// APISpace is the Space JSON of API.md §5.
type APISpace struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Access     string `json:"access"`
	UsageBytes int64  `json:"usage_bytes"`
}

// Member is a library membership.
type Member struct {
	UserID   string `json:"user_id"`
	Access   string `json:"access"`
	Username string `json:"username,omitempty"`
}

type apiLibrary struct {
	APISpace
	Members   []Member `json:"members"`
	CreatedAt string   `json:"created_at"`
	FileCount int64    `json:"file_count"`
}

func (s *Service) spaceUsage(ctx context.Context, spaceID string) (bytes, count int64, err error) {
	err = s.d.DB.SQL.QueryRowContext(ctx, `SELECT COALESCE(SUM(size), 0), count(*) FROM files
		WHERE space_id = ? AND is_dir = 0 AND trashed_at IS NULL`, spaceID).Scan(&bytes, &count)
	return
}

// Spaces lists the spaces shown to p: their home first (created on demand), then the
// libraries they can access (admins: every library), by name.
func (s *Service) Spaces(ctx context.Context, p *core.Principal) ([]APISpace, error) {
	if _, err := s.HomeSpaceID(ctx, p.UserID); err != nil {
		return nil, err
	}
	ids, err := s.listedSpaceIDs(ctx, p)
	if err != nil {
		return nil, err
	}
	out := make([]APISpace, 0, len(ids))
	for _, id := range ids {
		sp, err := loadSpace(ctx, s.d.DB.SQL, id)
		if err != nil {
			return nil, err
		}
		if sp == nil {
			continue
		}
		lvl, err := s.accessLevel(ctx, s.d.DB.SQL, p, sp)
		if err != nil {
			return nil, err
		}
		if lvl == accNone {
			continue
		}
		usage, _, err := s.spaceUsage(ctx, sp.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, APISpace{ID: sp.ID, Kind: sp.Kind, Name: sp.Name, Access: accessNames[lvl], UsageBytes: usage})
	}
	sortSpaces(out)
	return out, nil
}

func sortSpaces(out []APISpace) {
	less := func(a, b APISpace) bool {
		if (a.Kind == "home") != (b.Kind == "home") {
			return a.Kind == "home"
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && less(out[j], out[j-1]); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
}

func (m *Module) listSpaces(w http.ResponseWriter, r *http.Request) error {
	out, err := m.svc.Spaces(r.Context(), core.MustPrincipal(r.Context()))
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

/* ------------------------------------------------------------ admin spaces */

func (s *Service) members(ctx context.Context, spaceID string) ([]Member, error) {
	rs, err := s.d.DB.SQL.QueryContext(ctx, `SELECT m.user_id, m.access, COALESCE(u.username, '') FROM space_members m
		LEFT JOIN users u ON u.id = m.user_id WHERE m.space_id = ? ORDER BY m.user_id = '*' DESC, u.username COLLATE NOCASE`, spaceID)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := []Member{}
	for rs.Next() {
		var m Member
		if err := rs.Scan(&m.UserID, &m.Access, &m.Username); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rs.Err()
}

func (s *Service) libraryJSON(ctx context.Context, sp *space) (apiLibrary, error) {
	usage, count, err := s.spaceUsage(ctx, sp.ID)
	if err != nil {
		return apiLibrary{}, err
	}
	mem, err := s.members(ctx, sp.ID)
	if err != nil {
		return apiLibrary{}, err
	}
	return apiLibrary{
		APISpace: APISpace{ID: sp.ID, Kind: sp.Kind, Name: sp.Name, Access: "write", UsageBytes: usage},
		Members:  mem, CreatedAt: db.FormatTime(sp.CreatedAt), FileCount: count,
	}, nil
}

func (m *Module) adminListSpaces(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	rs, err := m.d.DB.SQL.QueryContext(ctx, `SELECT `+spaceCols+` FROM spaces WHERE kind = 'library' ORDER BY name COLLATE NOCASE, id`)
	if err != nil {
		return err
	}
	var sps []*space
	for rs.Next() {
		sp, err := scanSpace(rs)
		if err != nil {
			rs.Close()
			return err
		}
		sps = append(sps, sp)
	}
	rs.Close()
	out := make([]apiLibrary, 0, len(sps))
	for _, sp := range sps {
		l, err := m.svc.libraryJSON(ctx, sp)
		if err != nil {
			return err
		}
		out = append(out, l)
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

type spaceInput struct {
	Name    *string   `json:"name"`
	Members *[]Member `json:"members"`
}

func (m *Module) validateSpaceInput(ctx context.Context, in *spaceInput, create bool) error {
	fe := map[string]string{}
	if in.Name != nil {
		n := strings.TrimSpace(*in.Name)
		*in.Name = n
		if n == "" || utf8.RuneCountInString(n) > 100 || settings.HasControl(n) {
			fe["name"] = "name must be 1-100 characters"
		}
	} else if create {
		fe["name"] = "required"
	}
	if in.Members != nil {
		ms := *in.Members
		if len(ms) > 10000 {
			fe["members"] = "at most 10000 members"
		}
		seen := map[string]bool{}
		for i, mem := range ms {
			if mem.UserID != "*" && !httpx.ValidID(mem.UserID) {
				fe["members"] = "invalid user id " + mem.UserID
				break
			}
			if mem.Access != "read" && mem.Access != "write" {
				fe["members"] = "access must be read or write"
				break
			}
			if seen[mem.UserID] {
				fe["members"] = "duplicate member " + mem.UserID
				break
			}
			seen[mem.UserID] = true
			if mem.UserID != "*" {
				var n int
				if err := m.d.DB.SQL.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE id = ?`, mem.UserID).Scan(&n); err != nil {
					return err
				}
				if n == 0 {
					fe["members"] = "unknown user " + mem.UserID
					break
				}
			}
			ms[i].Username = ""
		}
	} else if create {
		empty := []Member{}
		in.Members = &empty
	}
	if len(fe) > 0 {
		return core.Validation(fe)
	}
	if in.Name != nil {
		var n int
		if err := m.d.DB.SQL.QueryRowContext(ctx, `SELECT count(*) FROM spaces WHERE kind = 'library' AND name = ? COLLATE NOCASE`, *in.Name).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return core.Conflict("A library with this name already exists")
		}
	}
	return nil
}

func replaceMembers(ctx context.Context, tx *sql.Tx, spaceID string, ms []Member) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM space_members WHERE space_id = ?`, spaceID); err != nil {
		return err
	}
	for _, mem := range ms {
		if _, err := tx.ExecContext(ctx, `INSERT INTO space_members (space_id, user_id, access) VALUES (?, ?, ?)`, spaceID, mem.UserID, mem.Access); err != nil {
			return err
		}
	}
	return nil
}

func (m *Module) adminCreateSpace(w http.ResponseWriter, r *http.Request) error {
	var in spaceInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	ctx := r.Context()
	if err := m.validateSpaceInput(ctx, &in, true); err != nil {
		return err
	}
	root, err := m.svc.dataRoot()
	if err != nil {
		return err
	}
	id := core.NewID()
	rootDir := "libraries/" + id
	if err := root.MkdirAll(filepath.Join("storage", "libraries", id), 0o700); err != nil {
		return err
	}
	now := m.svc.now()
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO spaces (id, kind, name, owner_id, root_dir, created_at, updated_at)
			VALUES (?, 'library', ?, NULL, ?, ?, ?)`, id, *in.Name, rootDir, db.FormatTime(now), db.FormatTime(now)); err != nil {
			return err
		}
		if err := insertRootRow(ctx, tx, id, "", now); err != nil {
			return err
		}
		return replaceMembers(ctx, tx, id, *in.Members)
	})
	if err != nil {
		_ = root.Remove(filepath.Join("storage", "libraries", id))
		return err
	}
	sp, err := loadSpace(ctx, m.d.DB.SQL, id)
	if err != nil || sp == nil {
		return errors.Join(err, errors.New("files: space vanished after create"))
	}
	out, err := m.svc.libraryJSON(ctx, sp)
	if err != nil {
		return err
	}
	m.d.Audit.Record(ctx, "admin.space.create", "space", id, map[string]any{"name": sp.Name, "members": len(*in.Members)})
	return httpx.WriteJSON(w, http.StatusCreated, out)
}

func (m *Module) adminLibrary(ctx context.Context, r *http.Request) (*space, error) {
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return nil, err
	}
	sp, err := loadSpace(ctx, m.d.DB.SQL, id)
	if err != nil {
		return nil, err
	}
	if sp == nil || sp.Kind != "library" {
		return nil, core.NotFound("Library not found")
	}
	return sp, nil
}

func (m *Module) adminUpdateSpace(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	sp, err := m.adminLibrary(ctx, r)
	if err != nil {
		return err
	}
	var in spaceInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	if in.Name != nil && strings.TrimSpace(*in.Name) == sp.Name {
		in.Name = nil
	}
	if err := m.validateSpaceInput(ctx, &in, false); err != nil {
		return err
	}
	now := m.svc.now()
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		if in.Name != nil {
			if _, err := tx.ExecContext(ctx, `UPDATE spaces SET name = ?, updated_at = ? WHERE id = ?`, *in.Name, db.FormatTime(now), sp.ID); err != nil {
				return err
			}
		}
		if in.Members != nil {
			return replaceMembers(ctx, tx, sp.ID, *in.Members)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if sp, err = loadSpace(ctx, m.d.DB.SQL, sp.ID); err != nil || sp == nil {
		return errors.Join(err, core.NotFound("Library not found"))
	}
	out, err := m.svc.libraryJSON(ctx, sp)
	if err != nil {
		return err
	}
	meta := map[string]any{}
	if in.Name != nil {
		meta["name"] = *in.Name
	}
	if in.Members != nil {
		meta["members"] = len(*in.Members)
	}
	m.d.Audit.Record(ctx, "admin.space.update", "space", sp.ID, meta)
	return httpx.WriteJSON(w, http.StatusOK, out)
}

func (m *Module) adminDeleteSpace(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	sp, err := m.adminLibrary(ctx, r)
	if err != nil {
		return err
	}
	force := httpx.QueryBool(r, "force")
	var pd pending
	m.svc.treeMu.Lock()
	defer func() { m.svc.treeMu.Unlock(); pd.flush() }()
	var n int
	var bytes int64
	if err := m.d.DB.SQL.QueryRowContext(ctx, `SELECT count(*), COALESCE(SUM(size), 0) FROM files WHERE space_id = ? AND path <> ''`, sp.ID).Scan(&n, &bytes); err != nil {
		return err
	}
	if n > 0 && !force {
		return core.Conflict("The library is not empty; delete it with ?force=true to remove its files").
			WithDetails(map[string]any{"file_count": n, "usage_bytes": bytes})
	}
	if err := m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM spaces WHERE id = ?`, sp.ID)
		return err
	}); err != nil {
		return err
	}
	m.svc.removeLater(diskRel(sp, ""))
	m.svc.removeLater(filepath.Join("storage", ".trash", sp.ID))
	m.d.Audit.Record(ctx, "admin.space.delete", "space", sp.ID, map[string]any{"name": sp.Name, "force": force, "file_count": n})
	return httpx.NoContent(w)
}
