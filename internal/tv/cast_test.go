package tv

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ludworks/internal/core"
)

// The cross-module API of the cast module: channel lookup, the program on air (live and
// on-demand), positions and channel-scoped program bytes for cast devices.
func TestCastAPI(t *testing.T) {
	v := newEnv(t)
	ctx := context.Background()
	lid, ids := v.seedShow(v.adminID, v.adm, "Toons", 3) // 61 + 62 + 63 s
	var live, od Channel
	do(t, v.adm, "POST", "/api/v1/tv/channels", map[string]any{"name": "Toons", "lineup_id": lid}, 201, &live)
	do(t, v.adm, "POST", "/api/v1/tv/channels", map[string]any{"name": "Toons OD", "lineup_id": lid, "playback_mode": "on_demand", "visibility": "shared"}, 201, &od)
	admin := v.principal(v.adminID)
	bobID, _ := v.user("bob")
	bob := v.principal(bobID)

	ch, err := v.tv.CastChannel(ctx, admin, live.ID)
	if err != nil || ch.Name != "Toons" || ch.Number != live.Number || ch.OnDemand || ch.LogoURL != "" {
		t.Fatalf("channel: %+v %v", ch, err)
	}
	if _, err := v.tv.CastChannel(ctx, bob, live.ID); !isNotFound(err) {
		t.Fatalf("private channel for bob: %v", err)
	}
	if c, err := v.tv.CastChannel(ctx, bob, od.ID); err != nil || !c.OnDemand {
		t.Fatalf("shared channel for bob: %+v %v", c, err)
	}

	// Live: the program on air with its offset, like /now.
	v.advance(70 * time.Second) // 70 s after the anchor: the second episode, 9 s in
	pr, err := v.tv.CastProgramAt(ctx, admin, live.ID, v.now())
	if err != nil || pr == nil || pr.FileID != ids[1] || pr.OffsetSeconds < 8.9 || pr.OffsetSeconds > 9.1 ||
		pr.ShowTitle != "Toons" || pr.Season != 1 || pr.Episode != 2 || pr.DurationSeconds != 62 || !strings.Contains(pr.Title, "S01E02") ||
		pr.Mime != "video/mp4" {
		t.Fatalf("live program: %+v %v", pr, err)
	}
	if _, err := v.tv.CastProgramByIndex(ctx, admin, live.ID, 0, 0); err == nil {
		t.Fatal("program index on a live channel")
	}
	if err := v.tv.CastSavePosition(ctx, admin, live.ID, 1, 0); err == nil {
		t.Fatal("position on a live channel")
	}

	// On demand: the viewer's own position.
	if err := v.tv.CastSavePosition(ctx, bob, od.ID, 2, 12.5); err != nil {
		t.Fatal(err)
	}
	pr, err = v.tv.CastProgramAt(ctx, bob, od.ID, v.now())
	if err != nil || pr.FileID != ids[2] || pr.Index != 2 || pr.OffsetSeconds != 12.5 {
		t.Fatalf("on-demand program: %+v %v", pr, err)
	}
	pr, err = v.tv.CastProgramByIndex(ctx, bob, od.ID, 3, 0) // wraps into the next cycle
	if err != nil || pr.FileID != ids[0] || pr.Index != 3 || pr.OffsetSeconds != 0 {
		t.Fatalf("next program: %+v %v", pr, err)
	}
	if pr, _ := v.tv.CastProgramAt(ctx, admin, od.ID, v.now()); pr.Index != 0 {
		t.Fatalf("positions are per viewer: %+v", pr)
	}
	if err := v.tv.CastSavePosition(ctx, bob, od.ID, -1, 0); err == nil {
		t.Fatal("negative index accepted")
	}

	// Program bytes: authorised by channel visibility + expansion, served as the channel owner.
	serve := func(p *core.Principal, channel, file string, a CastAsset) (int, error) {
		rec := httptest.NewRecorder()
		err := v.tv.ServeCastProgram(rec, httptest.NewRequest("GET", "/x", nil), p, channel, file, a)
		return rec.Code, err
	}
	if _, err := serve(bob, od.ID, ids[0], CastAsset{Kind: "stream"}); err != nil || v.media.last() != "stream "+v.adminID+" "+ids[0] {
		t.Fatalf("stream: %v %q", err, v.media.last())
	}
	if _, err := serve(bob, od.ID, ids[1], CastAsset{Kind: "transcode", Start: 7.5}); err != nil || v.media.last() != "transcode "+v.adminID+" "+ids[1]+" 7.5 720" {
		t.Fatalf("transcode: %v %q", err, v.media.last())
	}
	if _, err := serve(bob, od.ID, ids[1], CastAsset{Kind: "poster"}); err != nil || v.media.last() != "poster "+v.adminID+" "+ids[1]+" md" {
		t.Fatalf("poster: %v %q", err, v.media.last())
	}
	if _, err := serve(bob, od.ID, ids[1], CastAsset{Kind: "subtitle", Subtitle: 2}); err != nil || v.media.last() != "subtitle "+v.adminID+" "+ids[1]+" 2" {
		t.Fatalf("subtitle: %v %q", err, v.media.last())
	}
	other := v.addFile(v.adminID, "Else", "else.mp4", &mediaRow{Title: "Else", Duration: 30, Category: "movie"})
	for name, c := range map[string]struct {
		p       *core.Principal
		channel string
		file    string
		a       CastAsset
	}{
		"file outside the lineup": {bob, od.ID, other, CastAsset{Kind: "stream"}},
		"invisible channel":       {bob, live.ID, ids[0], CastAsset{Kind: "stream"}},
		"unknown asset":           {admin, live.ID, ids[0], CastAsset{Kind: "download"}},
		"bad file id":             {admin, live.ID, "x", CastAsset{Kind: "stream"}},
	} {
		if _, err := serve(c.p, c.channel, c.file, c.a); !isNotFound(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := serve(admin, live.ID, ids[0], CastAsset{Kind: "transcode", Height: 999}); err == nil {
		t.Error("bad height accepted")
	}
}

func isNotFound(err error) bool {
	ce, ok := err.(*core.Error)
	return ok && ce.Status == 404
}
