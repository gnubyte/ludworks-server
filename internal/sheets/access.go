package sheets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/sheets/engine"
)

// withWorkbook locks the caller's workbook id (loading it into the cache when needed) and
// runs fn. Workbooks of other users, and missing ones, answer 404.
func (m *Module) withWorkbook(ctx context.Context, p *core.Principal, id string, fn func(e *entry) error) error {
	wctx, cancel := context.WithTimeout(ctx, lockWaitBudget)
	defer cancel()
	e, found, err := m.cache.acquire(wctx, id, p.UserID)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errBusy
	}
	if !found {
		return errWorkbookNotFound
	}
	done := false
	defer func() {
		if !done { // panic: never keep a possibly half-updated engine
			e.eng = nil
		}
		m.cache.release(e)
	}()
	if e.eng == nil {
		meta, wb, err := m.st.load(ctx, id, p.UserID)
		if err != nil {
			done = true
			return err
		}
		if meta == nil {
			done = true
			return errWorkbookNotFound
		}
		eng, err := engine.New(wb)
		if err != nil {
			done = true
			return fmt.Errorf("sheets: load workbook %s: %w", id, err)
		}
		e.eng, e.meta = eng, *meta
	} else if e.meta.OwnerID != p.UserID {
		done = true
		return errWorkbookNotFound
	}
	err = fn(e)
	done = true
	return err
}

// mutate runs op on the entry's engine within budget and commits what it changed in one
// transaction. op must leave the engine unchanged when it returns an error (validate before
// mutating). The request's cancellation does not abort an edit midway (only the budget
// does); on interruption or a failed commit the cached engine is discarded so the next
// request reloads the last committed state.
func (m *Module) mutate(ctx context.Context, e *entry, budget time.Duration, op func(eng *engine.Engine) (*change, error)) (*change, error) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), budget)
	defer cancel()
	var c *change
	err := e.eng.Run(rctx, func() error {
		var err error
		c, err = op(e.eng)
		return err
	})
	if err != nil {
		if errors.Is(err, engine.ErrInterrupted) || e.eng.Broken() {
			e.eng = nil
			return nil, errRecalcTimeout
		}
		return nil, err
	}
	if c == nil || c.empty() {
		return newChange(), nil
	}
	cctx, ccancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer ccancel()
	v, t, err := m.st.commit(cctx, &e.meta, e.eng.Workbook(), c)
	if err != nil {
		e.eng = nil
		if errors.Is(err, errWorkbookGone) {
			return nil, errConflict
		}
		return nil, err
	}
	e.meta.Version, e.meta.UpdatedAt = v, t
	return c, nil
}

// evaluateIn runs a read-only engine computation within budget.
func (m *Module) evaluateIn(ctx context.Context, eng *engine.Engine, budget time.Duration, fn func() error) error {
	rctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	err := eng.Run(rctx, fn)
	if errors.Is(err, engine.ErrInterrupted) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errRecalcTimeout
	}
	return err
}

// sheetOf returns the sheet with id in the entry's workbook or a 404 error.
func sheetOf(e *entry, id string) (*engine.Sheet, int, error) {
	wb := e.eng.Workbook()
	i := wb.Index(id)
	if i < 0 {
		return nil, -1, errSheetNotFound
	}
	return wb.Sheets[i], i, nil
}

// ---- JSON shapes -----------------------------------------------------------------------------

// workbookSummary is the API WorkbookSummary (plus version).
type workbookSummary struct {
	ID        string             `json:"id"`
	Name      string             `json:"name"`
	OwnerID   string             `json:"owner_id"`
	CreatedAt string             `json:"created_at"`
	UpdatedAt string             `json:"updated_at"`
	Version   int64              `json:"version"`
	Sheets    []engine.SheetJSON `json:"sheets"`
}

func summaryOf(meta wbMeta, wb *engine.Workbook) workbookSummary {
	s := workbookSummary{
		ID: meta.ID, Name: meta.Name, OwnerID: meta.OwnerID, Version: meta.Version,
		CreatedAt: db.FormatTime(meta.CreatedAt), UpdatedAt: db.FormatTime(meta.UpdatedAt),
		Sheets: make([]engine.SheetJSON, 0, len(wb.Sheets)),
	}
	for i, sh := range wb.Sheets {
		s.Sheets = append(s.Sheets, sh.ToJSON(i, false))
	}
	return s
}

func summaryFromRows(meta wbMeta, sheets []sheetMeta) workbookSummary {
	s := workbookSummary{
		ID: meta.ID, Name: meta.Name, OwnerID: meta.OwnerID, Version: meta.Version,
		CreatedAt: db.FormatTime(meta.CreatedAt), UpdatedAt: db.FormatTime(meta.UpdatedAt),
		Sheets: make([]engine.SheetJSON, 0, len(sheets)),
	}
	for i, sm := range sheets {
		cw, rh := sm.ColWidths, sm.RowHeights
		if cw == nil {
			cw = map[string]float64{}
		}
		if rh == nil {
			rh = map[string]float64{}
		}
		s.Sheets = append(s.Sheets, engine.SheetJSON{ID: sm.ID, Name: sm.Name, Index: i, Rows: sm.Rows, Cols: sm.Cols,
			ColWidths: cw, RowHeights: rh, FrozenRows: sm.FrozenRows, FrozenCols: sm.FrozenCols})
	}
	return s
}

// appendSheet renders a Sheet; with cells it splices the fast cell encoder into the
// metadata JSON.
func appendSheet(b []byte, enc *apiEncoder, sh *engine.Sheet, index int, withCells bool) []byte {
	meta, _ := json.Marshal(sh.ToJSON(index, false))
	if !withCells {
		return append(b, meta...)
	}
	b = append(b, meta[:len(meta)-1]...)
	b = append(b, `,"cells":`...)
	b = enc.cellMap(b, sh.Cells)
	return append(b, '}')
}

// appendWorkbook renders the full Workbook (all sheets with cells).
func appendWorkbook(b []byte, meta wbMeta, wb *engine.Workbook) []byte {
	head, _ := json.Marshal(struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		OwnerID   string `json:"owner_id"`
		CreatedAt string `json:"created_at"`
		UpdatedAt string `json:"updated_at"`
		Version   int64  `json:"version"`
	}{meta.ID, meta.Name, meta.OwnerID, db.FormatTime(meta.CreatedAt), db.FormatTime(meta.UpdatedAt), meta.Version})
	b = append(b, head[:len(head)-1]...)
	b = append(b, `,"sheets":[`...)
	enc := newAPIEncoder()
	for i, sh := range wb.Sheets {
		if i > 0 {
			b = append(b, ',')
		}
		b = appendSheet(b, enc, sh, i, true)
	}
	return append(b, ']', '}')
}

// changedBody renders {"changed":{...},"version":n}.
func changedBody(ch engine.Changes, version int64) []byte {
	b := make([]byte, 0, 64+48*ch.Count())
	b = append(b, `{"changed":`...)
	b = newAPIEncoder().changes(b, ch)
	b = append(b, `,"version":`...)
	b = fmt.Appendf(b, "%d", version)
	return append(b, '}')
}

// writeVersioned writes a JSON body with the workbook version as ETag.
func writeVersioned(w http.ResponseWriter, r *http.Request, status int, version int64, body []byte) error {
	w.Header().Set("ETag", formatVersion(version))
	return writeJSONBytes(w, r, status, body)
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
