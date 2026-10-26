package engine

import "fmt"

// MaxRangeCells bounds one range read or write request.
const MaxRangeCells = 100_000

// RangeData is the body of GET /sheets/{id}/sheets/{sheet_id}/range:
// row-major 2-D arrays; formulas are "" and styles null where absent.
type RangeData struct {
	Ref      string     `json:"ref"`
	Values   [][]any    `json:"values"`
	Formulas [][]string `json:"formulas"`
	Styles   [][]*Style `json:"styles"`
}

// clipToGrid limits whole-row/column ranges to the sheet's grid size.
func clipToGrid(s *Sheet, rng Range) Range {
	rng.To.Row = min(rng.To.Row, max(s.Rows-1, rng.From.Row))
	rng.To.Col = min(rng.To.Col, max(s.Cols-1, rng.From.Col))
	return rng
}

// ReadRange extracts values, formulas and styles of rng. Ranges extending
// past the sheet's grid (e.g. "A:A") are clipped to it.
func ReadRange(s *Sheet, rng Range) (RangeData, error) {
	if !rng.Valid() {
		return RangeData{}, ErrInvalidRef
	}
	rng = clipToGrid(s, rng)
	if rng.Area() > MaxRangeCells {
		return RangeData{}, fmt.Errorf("%w: range larger than %d cells", ErrInvalidArgument, MaxRangeCells)
	}
	out := RangeData{Ref: rng.String()}
	for r := rng.From.Row; r <= rng.To.Row; r++ {
		vals := make([]any, rng.Cols())
		forms := make([]string, rng.Cols())
		styles := make([]*Style, rng.Cols())
		for c := rng.From.Col; c <= rng.To.Col; c++ {
			if cell := s.Cells[Ref{r, c}]; cell != nil {
				i := c - rng.From.Col
				vals[i], forms[i] = cell.Value.JSON(), cell.Formula
				if !cell.Style.IsZero() {
					styles[i] = cell.Style
				}
			}
		}
		out.Values = append(out.Values, vals)
		out.Formulas = append(out.Formulas, forms)
		out.Styles = append(out.Styles, styles)
	}
	return out, nil
}

// UpdatesFromValues converts PUT .../range {"ref","values"} into updates
// anchored at origin (see UpdateFromJSON for value semantics). Ragged rows
// are allowed; the block must stay on the grid and within MaxRangeCells.
func UpdatesFromValues(origin Ref, values [][]any) ([]CellUpdate, error) {
	if !origin.Valid() {
		return nil, ErrInvalidRef
	}
	n := 0
	for _, row := range values {
		n += len(row)
	}
	if n > MaxRangeCells {
		return nil, fmt.Errorf("%w: more than %d values", ErrInvalidArgument, MaxRangeCells)
	}
	ups := make([]CellUpdate, 0, n)
	for i, row := range values {
		for j, raw := range row {
			ref := Ref{origin.Row + i, origin.Col + j}
			if !ref.Valid() {
				return nil, fmt.Errorf("%w: values extend beyond the sheet", ErrInvalidRef)
			}
			u, err := UpdateFromJSON(ref, raw)
			if err != nil {
				return nil, fmt.Errorf("values[%d][%d]: %w", i, j, err)
			}
			ups = append(ups, u)
		}
	}
	return ups, nil
}
