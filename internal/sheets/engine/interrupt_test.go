package engine

import (
	"context"
	"errors"
	"testing"
	"time"
)

// heavyWorkbook builds a sheet whose recalculation takes a noticeable time:
// many SUMPRODUCT formulas over a large data block.
func heavyWorkbook(t *testing.T, rows, formulas int) (*Engine, string) {
	t.Helper()
	s := NewSheet("s1", "Data", rows, 4)
	for r := 0; r < rows; r++ {
		s.SetCell(Ref{r, 0}, &Cell{Value: NumberValue(float64(r))})
		s.SetCell(Ref{r, 1}, &Cell{Value: NumberValue(float64(r % 7))})
	}
	for i := 0; i < formulas; i++ {
		s.SetCell(Ref{i, 3}, &Cell{Formula: "=SUMPRODUCT((A:A>B1)*B:B)+A1"})
	}
	e, err := New(&Workbook{Sheets: []*Sheet{s}})
	if err != nil {
		t.Fatal(err)
	}
	return e, s.ID
}

func TestRunInterruptsLongRecalc(t *testing.T) {
	e, sid := heavyWorkbook(t, 20000, 400)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := e.Run(ctx, func() error {
		// Changing A1 makes every formula recalculate.
		_, err := e.SetValue(sid, Ref{0, 0}, NumberValue(42))
		return err
	})
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("err = %v, want ErrInterrupted", err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("interruption took %v", el)
	}
	if !e.Broken() {
		t.Fatal("engine not marked broken")
	}
	if err := e.Run(context.Background(), func() error { return nil }); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("broken engine Run = %v", err)
	}
}

func TestRunCompletesWithinBudget(t *testing.T) {
	e, sid := heavyWorkbook(t, 200, 5)
	var ch Changes
	err := e.Run(context.Background(), func() error {
		var err error
		ch, err = e.SetValue(sid, Ref{0, 0}, NumberValue(1))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if e.Broken() || ch.Count() == 0 {
		t.Fatalf("broken=%v changes=%d", e.Broken(), ch.Count())
	}
	// Errors from fn pass through unchanged.
	sentinel := errors.New("boom")
	if err := e.Run(context.Background(), func() error { return sentinel }); err != sentinel {
		t.Fatalf("err = %v", err)
	}
	// An already-cancelled context never starts the work.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ran := false
	if err := e.Run(ctx, func() error { ran = true; return nil }); !errors.Is(err, ErrInterrupted) || ran {
		t.Fatalf("cancelled ctx: err=%v ran=%v", err, ran)
	}
}

func TestRunRepanicsForeignPanics(t *testing.T) {
	e, _ := New(NewWorkbook())
	defer func() {
		if r := recover(); r != "bug" {
			t.Fatalf("recover = %v", r)
		}
		if !e.Broken() {
			t.Fatal("engine not marked broken after panic")
		}
	}()
	_ = e.Run(context.Background(), func() error { panic("bug") })
}
