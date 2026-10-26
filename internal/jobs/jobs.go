// Package jobs runs background work on a bounded worker pool and schedules periodic tasks.
//
// Jobs receive a context that is cancelled when the runner stops, so long jobs (rescans,
// transcodes, purges) must check ctx and return promptly on shutdown.
package jobs

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"runtime/debug"
	"sync"
	"time"
)

// Func is a unit of background work.
type Func func(ctx context.Context) error

type job struct {
	name string
	fn   Func
}

// Runner is a bounded pool. Queued jobs wait in an unbounded FIFO so Go never blocks the
// caller (e.g. an HTTP handler answering 202).
type Runner struct {
	log     *slog.Logger
	workers int

	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	cond    *sync.Cond
	queue   []job
	active  map[string]int // name -> queued+running count (for GoOnce)
	started bool
	stopped bool

	wg sync.WaitGroup
}

// New creates a runner with the given number of workers (<=0 → 4). Call Start to run jobs;
// jobs queued before Start wait.
func New(log *slog.Logger, workers int) *Runner {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if workers <= 0 {
		workers = 4
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &Runner{log: log, workers: workers, ctx: ctx, cancel: cancel, active: map[string]int{}}
	r.cond = sync.NewCond(&r.mu)
	return r
}

// Start launches the workers. Calling it more than once is a no-op.
func (r *Runner) Start() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started || r.stopped {
		return
	}
	r.started = true
	for i := 0; i < r.workers; i++ {
		r.wg.Add(1)
		go r.worker()
	}
}

// Go queues fn. It is ignored after Stop.
func (r *Runner) Go(name string, fn func(ctx context.Context) error) {
	r.enqueue(name, fn, false)
}

// GoOnce queues fn unless a job with the same name is already queued or running; it reports
// whether the job was queued. Useful for idempotent work such as "rescan space X".
func (r *Runner) GoOnce(name string, fn func(ctx context.Context) error) bool {
	return r.enqueue(name, fn, true)
}

func (r *Runner) enqueue(name string, fn Func, once bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return false
	}
	if once && r.active[name] > 0 {
		return false
	}
	r.active[name]++
	r.queue = append(r.queue, job{name: name, fn: fn})
	r.cond.Signal()
	return true
}

func (r *Runner) worker() {
	defer r.wg.Done()
	for {
		r.mu.Lock()
		for len(r.queue) == 0 && !r.stopped {
			r.cond.Wait()
		}
		if r.stopped {
			r.mu.Unlock()
			return
		}
		j := r.queue[0]
		r.queue[0] = job{}
		r.queue = r.queue[1:]
		r.mu.Unlock()

		r.run(j)

		r.mu.Lock()
		if r.active[j.name]--; r.active[j.name] <= 0 {
			delete(r.active, j.name)
		}
		r.mu.Unlock()
	}
}

func (r *Runner) run(j job) {
	start := time.Now()
	defer func() {
		if p := recover(); p != nil {
			r.log.Error("job panic", "job", j.name, "panic", p, "stack", string(debug.Stack()))
		}
	}()
	if err := j.fn(r.ctx); err != nil && r.ctx.Err() == nil {
		r.log.Warn("job failed", "job", j.name, "err", err, "duration", time.Since(start).Round(time.Millisecond))
		return
	}
	r.log.Debug("job done", "job", j.name, "duration", time.Since(start).Round(time.Millisecond))
}

// Every runs fn periodically on the pool. The first run happens after a random delay in
// [0, min(d, 1m)) so tasks registered together do not fire in lockstep; later runs are spaced
// d ± 10% (jitter). A tick is skipped while the previous run of the same task is still
// queued or running, so slow tasks never pile up.
func (r *Runner) Every(name string, d time.Duration, fn func(ctx context.Context) error) {
	if d <= 0 {
		panic("jobs: Every needs a positive interval")
	}
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return
	}
	r.wg.Add(1)
	r.mu.Unlock()
	go func() {
		defer r.wg.Done()
		first := min(d, time.Minute)
		wait := time.Duration(rand.Int64N(int64(first)))
		for {
			t := time.NewTimer(wait)
			select {
			case <-r.ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
			r.GoOnce(name, fn)
			wait = jitter(d)
		}
	}()
}

func jitter(d time.Duration) time.Duration {
	spread := int64(d) / 5 // ±10%
	if spread <= 0 {
		return d
	}
	return d - time.Duration(spread/2) + time.Duration(rand.Int64N(spread))
}

// Context returns the runner's lifetime context (cancelled by Stop).
func (r *Runner) Context() context.Context { return r.ctx }

// Stop cancels running jobs' contexts, drops queued jobs and waits until ctx is done for
// workers and schedulers to exit.
func (r *Runner) Stop(ctx context.Context) error {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return nil
	}
	r.stopped = true
	if n := len(r.queue); n > 0 {
		r.log.Info("dropping queued jobs on shutdown", "count", n)
	}
	r.queue = nil
	r.cond.Broadcast()
	r.mu.Unlock()
	r.cancel()
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
