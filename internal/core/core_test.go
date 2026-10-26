package core

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"testing"
	"time"
)

func TestNewID(t *testing.T) {
	re := regexp.MustCompile(`^[a-z2-7]{26}$`)
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := NewID()
		if !re.MatchString(id) || seen[id] {
			t.Fatalf("bad or duplicate id %q", id)
		}
		seen[id] = true
	}
}

func TestNowAndFormat(t *testing.T) {
	n := Now()
	if n.Location() != time.UTC || n.Nanosecond()%int(time.Millisecond) != 0 {
		t.Fatalf("Now() = %v", n)
	}
	if s := FormatTime(time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)); s != "2026-10-26T12:00:00.000Z" {
		t.Fatal(s)
	}
}

func TestPrincipalContext(t *testing.T) {
	ctx := context.Background()
	if _, ok := PrincipalFrom(ctx); ok {
		t.Fatal("principal in empty ctx")
	}
	p := &Principal{UserID: "u", Role: RoleUser, Scopes: []string{"notes:read"}}
	ctx = WithPrincipal(ctx, p)
	if got := MustPrincipal(ctx); got != p {
		t.Fatal("round trip")
	}
	if !p.Has("notes:read") || p.Has("notes:write") || p.Has("admin") || p.IsAdmin() {
		t.Fatal("scopes")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("MustPrincipal did not panic")
		}
	}()
	MustPrincipal(context.Background())
}

func TestRequestMeta(t *testing.T) {
	ctx := WithRequestMeta(context.Background(), RequestMeta{ID: "r", IP: "1.2.3.4", UserAgent: "ua", Secure: true})
	if ClientIP(ctx) != "1.2.3.4" || UserAgent(ctx) != "ua" || RequestID(ctx) != "r" {
		t.Fatal("meta")
	}
	if ClientIP(context.Background()) != "" {
		t.Fatal("empty ctx")
	}
}

func TestErrors(t *testing.T) {
	err := fmt.Errorf("wrap: %w", Conflict("taken"))
	var ce *Error
	if !errors.As(err, &ce) || ce.Status != 409 || ce.Code != "conflict" {
		t.Fatal(err)
	}
	v := Validation(map[string]string{"name": "required"})
	if v.Status != 422 || v.Code != "validation_failed" {
		t.Fatal(v)
	}
	d := ErrNotFound.WithDetails(map[string]any{"id": "x"})
	if ErrNotFound.Details != nil || d.Details == nil {
		t.Fatal("WithDetails mutated the shared error")
	}
	if NotImplemented("x").Status != 501 || Unauthorized("x").Status != 401 || Forbidden("x").Status != 403 {
		t.Fatal("constructors")
	}
}
