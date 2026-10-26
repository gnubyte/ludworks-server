package jobs

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestBoundedPool(t *testing.T) {
	r := New(nil, 2)
	r.Start()
	var running, peak, done atomic.Int32
	for i := 0; i < 10; i++ {
		r.Go("work", func(ctx context.Context) error {
			n := running.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
			running.Add(-1)
			done.Add(1)
			if done.Load() == 3 {
				panic("recovered")
			}
			return errors.New("logged")
		})
	}
	deadline := time.Now().Add(5 * time.Second)
	for done.Load() < 10 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if done.Load() != 10 || peak.Load() > 2 {
		t.Fatalf("done=%d peak=%d", done.Load(), peak.Load())
	}
	if err := r.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestGoOnceAndStopCancels(t *testing.T) {
	r := New(nil, 1)
	r.Start()
	started := make(chan struct{})
	cancelled := make(chan struct{})
	if !r.GoOnce("scan", func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return ctx.Err()
	}) {
		t.Fatal("first GoOnce rejected")
	}
	<-started
	if r.GoOnce("scan", func(context.Context) error { return nil }) {
		t.Fatal("duplicate GoOnce accepted while running")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("job context not cancelled")
	}
	r.Go("late", func(context.Context) error { t.Error("ran after stop"); return nil })
}

func TestEvery(t *testing.T) {
	r := New(nil, 2)
	r.Start()
	var n atomic.Int32
	r.Every("tick", 20*time.Millisecond, func(ctx context.Context) error { n.Add(1); return nil })
	time.Sleep(200 * time.Millisecond)
	r.Stop(context.Background())
	if c := n.Load(); c < 3 || c > 15 {
		t.Fatalf("ticks = %d", c)
	}
	for i := 0; i < 1000; i++ {
		if j := jitter(time.Second); j < 900*time.Millisecond || j > 1100*time.Millisecond {
			t.Fatalf("jitter %v out of range", j)
		}
	}
}
