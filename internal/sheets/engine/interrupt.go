package engine

import (
	"context"
	"errors"
	"sync/atomic"
)

// ErrInterrupted is returned by Run when its context ends (deadline or
// cancellation) before the operation completes. The engine is then left in
// an undefined, partially updated state and reports Broken() == true:
// callers must discard it and reload the workbook from storage.
var ErrInterrupted = errors.New("calculation interrupted")

// abortSignal is the panic value used to unwind an interrupted calculation.
type abortSignal struct{}

// pollMask controls how often poll consults the abort flag (every 1024
// calls): cheap enough for the evaluator's hottest loops.
const pollMask = 1023

// poll aborts the current operation when Run's context has ended. It is
// called from evaluation and iteration hot paths.
func (e *Engine) poll() {
	e.ticks++
	if e.ticks&pollMask == 0 {
		if f := e.abort; f != nil && f.Load() {
			panic(abortSignal{})
		}
	}
}

// Broken reports whether an operation was interrupted midway; the engine's
// contents must not be used or persisted afterwards.
func (e *Engine) Broken() bool { return e.broken }

// Run executes fn (which calls Engine methods) and aborts long-running
// recalculation, sorting or structural edits once ctx is done, returning
// ErrInterrupted. Run itself is not safe for concurrent use, like the rest
// of the Engine. Panics other than the internal abort signal propagate.
func (e *Engine) Run(ctx context.Context, fn func() error) (err error) {
	if e.broken {
		return ErrInterrupted
	}
	if ctx.Err() != nil {
		return ErrInterrupted
	}
	// A fresh flag per run: a late AfterFunc from a previous run can never
	// abort this one.
	flag := new(atomic.Bool)
	e.abort = flag
	stop := context.AfterFunc(ctx, func() { flag.Store(true) })
	defer func() {
		stop()
		e.abort = nil
		if r := recover(); r != nil {
			if _, ok := r.(abortSignal); ok {
				e.broken = true
				err = ErrInterrupted
				return
			}
			e.broken = true
			panic(r)
		}
	}()
	return fn()
}
