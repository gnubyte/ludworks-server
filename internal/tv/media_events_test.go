package tv

import (
	"testing"
	"time"

	"ludworks/internal/media"
)

// A probe that fills in a duration after the file event reaches the channel at once: the
// media module publishes media.TopicUpdated and tv drops its cached expansions. Before, the
// expansion built between upload and probe (duration unknown) stayed cached until its TTL.
func TestMediaUpdatedRefreshesExpansion(t *testing.T) {
	v := newEnv(t)
	if TopicMediaUpdated != media.TopicUpdated {
		t.Fatalf("topic %q, media publishes %q", TopicMediaUpdated, media.TopicUpdated)
	}
	id := v.addFile(v.adminID, "Shows/Z", "Z.S01E01.mp4", &mediaRow{Title: "One", ShowTitle: "Show Z", Season: ip(1), Episode: ip(1), Category: "tv"})
	var l lineupResp
	do(t, v.adm, "POST", "/api/v1/tv/lineups", map[string]any{"name": "Z", "items": []any{
		map[string]any{"kind": "show", "show_title": "Show Z"}}}, 201, &l)
	var x expandedResp
	do(t, v.adm, "GET", "/api/v1/tv/lineups/"+l.ID+"/expanded", nil, 200, &x)
	if len(x.Items) != 0 || len(x.Skipped) != 1 || x.Skipped[0].Reason != ReasonUnknownDuration {
		t.Fatalf("before probe: %+v", x)
	}
	// The probe result lands in the database (as media's probe job writes it)...
	if _, err := v.e.Deps.DB.SQL.Exec(`UPDATE media_items SET duration_seconds = 1500 WHERE file_id = ?`, id); err != nil {
		t.Fatal(err)
	}
	do(t, v.adm, "GET", "/api/v1/tv/lineups/"+l.ID+"/expanded", nil, 200, &x)
	if len(x.Items) != 0 {
		t.Fatalf("expansion is not cached, the test proves nothing: %+v", x)
	}
	// ...and the announcement makes it visible without waiting for the cache TTL.
	v.e.Deps.Events.Publish(media.TopicUpdated, media.UpdatedEvent{FileIDs: []string{id}})
	deadline := time.Now().Add(5 * time.Second)
	for {
		do(t, v.adm, "GET", "/api/v1/tv/lineups/"+l.ID+"/expanded", nil, 200, &x)
		if len(x.Items) == 1 && x.Items[0].FileID == id && x.TotalDurationSeconds == 1500 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("expansion not refreshed: %+v", x)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
