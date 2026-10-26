package audit

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/db"
)

func TestRecordAndList(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	l, err := New(ctx, d, nil)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	i := 0
	l.now = func() time.Time { i++; return base.Add(time.Duration(i/2) * time.Second) } // pairs share a timestamp

	reqCtx := core.WithRequestMeta(ctx, core.RequestMeta{ID: "req1", IP: "192.0.2.1", UserAgent: "test-agent"})
	l.Record(reqCtx, "auth.login_failed", "user", "", map[string]any{"username": "bob", "password": "hunter2", "TOTP_code": "123456"})
	admin := core.WithPrincipal(reqCtx, &core.Principal{UserID: "a1", Username: "admin"})
	for n := 0; n < 6; n++ {
		l.Record(admin, "admin.user.update", "user", "u"+string(rune('0'+n)), map[string]any{"field": "role"})
	}
	l.Record(admin, "auth.logout", "session", "s1", nil)

	all, next, err := l.List(ctx, Query{})
	if err != nil || len(all) != 8 || next != nil {
		t.Fatalf("len=%d next=%v err=%v", len(all), next, err)
	}
	if all[0].Action != "auth.logout" || all[7].Action != "auth.login_failed" {
		t.Fatalf("order: first %s last %s", all[0].Action, all[7].Action)
	}
	failed := all[7]
	if failed.IP != "192.0.2.1" || failed.RequestID != "req1" || failed.ActorID != "" || failed.Meta["username"] != "bob" {
		t.Fatalf("%+v", failed)
	}
	if _, ok := failed.Meta["password"]; ok {
		t.Fatal("password stored in audit meta")
	}
	if _, ok := failed.Meta["TOTP_code"]; ok {
		t.Fatal("code stored in audit meta")
	}

	// Keyset pagination across identical timestamps returns every row exactly once.
	seen := map[string]bool{}
	var cur *Cursor
	pages := 0
	for {
		items, nc, err := l.List(ctx, Query{Limit: 3, After: cur})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range items {
			if seen[e.ID] {
				t.Fatalf("duplicate %s", e.ID)
			}
			seen[e.ID] = true
		}
		pages++
		if nc == nil {
			break
		}
		cur = nc
	}
	if len(seen) != 8 || pages != 3 {
		t.Fatalf("seen=%d pages=%d", len(seen), pages)
	}

	byActor, _, _ := l.List(ctx, Query{ActorID: "a1"})
	byAction, _, _ := l.List(ctx, Query{Action: "auth.*"})
	exact, _, _ := l.List(ctx, Query{Action: "auth.logout"})
	if len(byActor) != 7 || len(byAction) != 2 || len(exact) != 1 {
		t.Fatalf("actor=%d prefix=%d exact=%d", len(byActor), len(byAction), len(exact))
	}
	// LIKE metacharacters in the filter are literal.
	if none, _, _ := l.List(ctx, Query{Action: "%*"}); len(none) != 0 {
		t.Fatal("LIKE injection")
	}

	big := map[string]any{"blob": strings.Repeat("x", 20000)}
	l.Record(admin, "x.big", "", "", big)
	e, _, _ := l.List(ctx, Query{Action: "x.big"})
	if len(e) != 1 || e[0].Meta["_truncated"] != true {
		t.Fatalf("meta not capped: %v", e)
	}
	n, err := l.Purge(ctx, base.Add(2*time.Second))
	if err != nil || n == 0 {
		t.Fatalf("purge n=%d err=%v", n, err)
	}
}
