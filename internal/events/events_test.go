package events

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestOrderedAsyncDeliveryAndPanicRecovery(t *testing.T) {
	b := New(nil)
	var mu sync.Mutex
	var got []int
	done := make(chan struct{})
	b.Subscribe("t", func(p any) {
		mu.Lock()
		got = append(got, p.(int))
		n := len(got)
		mu.Unlock()
		if n == 100 {
			close(done)
		}
	})
	b.Subscribe("t", func(p any) { panic("boom") }) // must not affect the other subscriber
	b.Subscribe("other", func(p any) { t.Error("wrong topic delivered") })
	for i := 0; i < 100; i++ {
		b.Publish("t", i)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}
	mu.Lock()
	for i, v := range got {
		if v != i {
			t.Fatalf("out of order at %d: %v", i, v)
		}
	}
	mu.Unlock()
	if err := b.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	b.Publish("t", 1) // after close: ignored, no panic
}

func TestPublishDoesNotBlockOnSlowSubscriber(t *testing.T) {
	b := New(nil)
	b.limit = 10
	release := make(chan struct{})
	b.Subscribe("slow", func(any) { <-release })
	start := time.Now()
	for i := 0; i < 1000; i++ {
		b.Publish("slow", i)
	}
	if time.Since(start) > time.Second {
		t.Fatal("publish blocked")
	}
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.Close(ctx); err != nil {
		t.Fatal(err)
	}
}
