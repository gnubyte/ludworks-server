package media

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"ludworks/internal/core"
)

func TestSlotsPerUserAndGlobal(t *testing.T) {
	s := newSlots(2, 1)
	relA, ok := s.tryAcquire("alice")
	if !ok {
		t.Fatal("alice: first slot refused")
	}
	if _, ok := s.tryAcquire("alice"); ok {
		t.Fatal("alice got a second slot (per-user limit)")
	}
	relB, ok := s.tryAcquire("bob")
	if !ok {
		t.Fatal("bob refused while a global slot is free")
	}
	if _, ok := s.tryAcquire("carol"); ok {
		t.Fatal("carol got a slot beyond the global limit")
	}
	relA()
	relA() // releasing twice is harmless
	if total, a := s.inUse("alice"); total != 1 || a != 0 {
		t.Fatalf("in use after release: %d %d", total, a)
	}
	relC, ok := s.tryAcquire("carol")
	if !ok {
		t.Fatal("carol refused after a release")
	}
	relB()
	relC()

	// acquire waits for a release, gives up after the wait and honours ctx.
	rel, _ := s.tryAcquire("alice")
	go func() { time.Sleep(50 * time.Millisecond); rel() }()
	r2, ok, err := s.acquire(context.Background(), "alice", 2*time.Second)
	if err != nil || !ok {
		t.Fatalf("acquire after release: %v %v", ok, err)
	}
	start := time.Now()
	if _, ok, err := s.acquire(context.Background(), "alice", 50*time.Millisecond); ok || err != nil || time.Since(start) > time.Second {
		t.Fatalf("acquire should time out: %v %v", ok, err)
	}
	cx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := s.acquire(cx, "alice", time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled acquire: %v", err)
	}
	r2()
}

// TestRequesterIsTheCaller: tv programs transcode as the channel owner, but the slot is
// charged to the viewer who asked.
func TestRequesterIsTheCaller(t *testing.T) {
	owner := &core.Principal{UserID: "owner"}
	r := httptest.NewRequest("GET", "/", nil)
	if got := requester(r, owner); got != "owner" {
		t.Fatalf("no caller: %q", got)
	}
	viewer := &core.Principal{UserID: "viewer"}
	r = r.WithContext(core.WithPrincipal(r.Context(), viewer))
	if got := requester(r, owner); got != "viewer" {
		t.Fatalf("requester = %q, want the viewer", got)
	}
}
