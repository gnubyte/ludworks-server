// Package events is a small in-process publish/subscribe bus.
//
// Delivery is asynchronous: every subscriber has its own FIFO queue drained by one goroutine,
// so a subscriber sees events in publish order, a slow subscriber never blocks publishers or
// other subscribers, and panics in a handler are recovered and logged.
package events

import (
	"context"
	"log/slog"
	"runtime/debug"
	"sync"
)

// Topics used across modules.
const (
	FileCreated = "file.created" // payload files.Event
	FileUpdated = "file.updated" // payload files.Event
	FileDeleted = "file.deleted" // payload files.Event
	FileMoved   = "file.moved"   // payload files.Event
	UserDeleted = "user.deleted" // payload user id (string)
)

// DefaultQueueLimit bounds each subscriber's backlog; beyond it events are dropped (and
// logged) rather than letting memory grow without bound.
const DefaultQueueLimit = 100_000

// Bus is safe for concurrent use.
type Bus struct {
	log   *slog.Logger
	limit int

	mu     sync.RWMutex
	subs   map[string][]*subscriber
	closed bool
	wg     sync.WaitGroup
}

type subscriber struct {
	topic string
	fn    func(payload any)

	mu      sync.Mutex
	cond    *sync.Cond
	queue   []any
	closing bool
	dropped int
}

// New creates a bus. log may be nil.
func New(log *slog.Logger) *Bus {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Bus{log: log, limit: DefaultQueueLimit, subs: map[string][]*subscriber{}}
}

// Subscribe registers fn for topic. fn runs on a dedicated goroutine for this subscription.
func (b *Bus) Subscribe(topic string, fn func(payload any)) {
	s := &subscriber{topic: topic, fn: fn}
	s.cond = sync.NewCond(&s.mu)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.subs[topic] = append(b.subs[topic], s)
	b.wg.Add(1)
	go b.run(s)
}

// Publish delivers payload to every subscriber of topic without blocking.
func (b *Bus) Publish(topic string, payload any) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return
	}
	for _, s := range b.subs[topic] {
		s.mu.Lock()
		if len(s.queue) >= b.limit {
			s.dropped++
			if s.dropped == 1 || s.dropped%1000 == 0 {
				b.log.Warn("event queue full, dropping events", "topic", topic, "dropped", s.dropped)
			}
		} else {
			s.queue = append(s.queue, payload)
			s.cond.Signal()
		}
		s.mu.Unlock()
	}
}

func (b *Bus) run(s *subscriber) {
	defer b.wg.Done()
	for {
		s.mu.Lock()
		for len(s.queue) == 0 && !s.closing {
			s.cond.Wait()
		}
		if len(s.queue) == 0 && s.closing {
			s.mu.Unlock()
			return
		}
		p := s.queue[0]
		s.queue[0] = nil
		s.queue = s.queue[1:]
		if len(s.queue) == 0 {
			s.queue = nil // release the backing array after bursts
		}
		s.mu.Unlock()
		b.deliver(s, p)
	}
}

func (b *Bus) deliver(s *subscriber, p any) {
	defer func() {
		if r := recover(); r != nil {
			b.log.Error("event handler panic", "topic", s.topic, "panic", r, "stack", string(debug.Stack()))
		}
	}()
	s.fn(p)
}

// Close stops accepting events and waits (until ctx is done) for queued events to drain.
func (b *Bus) Close(ctx context.Context) error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	for _, list := range b.subs {
		for _, s := range list {
			s.mu.Lock()
			s.closing = true
			s.cond.Broadcast()
			s.mu.Unlock()
		}
	}
	b.mu.Unlock()
	done := make(chan struct{})
	go func() { b.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
