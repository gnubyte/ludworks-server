package media_test

import (
	"slices"
	"sync"
	"testing"
	"time"

	"ludworks/internal/media"
	"ludworks/internal/media/mediatest"
)

// TestUpdatedEvents: consumers that cache media metadata (the tv module's lineup expansions)
// learn about new rows, probe results, edits and posters through media.TopicUpdated.
// Regression: nothing was published, so a freshly uploaded episode stayed out of every
// channel until the tv cache expired, because its duration arrived after the file event.
func TestUpdatedEvents(t *testing.T) {
	v := newEnv(t)
	var mu sync.Mutex
	got := map[string]int{}
	v.e.Deps.Events.Subscribe(media.TopicUpdated, func(p any) {
		ev, ok := p.(media.UpdatedEvent)
		if !ok {
			t.Errorf("payload %T", p)
			return
		}
		mu.Lock()
		for _, id := range ev.FileIDs {
			got[id]++
		}
		mu.Unlock()
	})
	count := func(id string) int {
		mu.Lock()
		defer mu.Unlock()
		return got[id]
	}
	waitMore := func(what, id string, than int) int {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if n := count(id); n > than {
				return n
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("no %s event for %s (count %d)", what, id, count(id))
		return 0
	}

	home := v.home(v.adm)
	ep := v.upload(v.adm, home, "Shows/Garden Hour/Season 1", "Garden.Hour.S01E01.mp4", mediatest.MP4(1440, 1280, 720, 0))
	id := ep["id"].(string)
	m := v.waitProbed(v.adm, id)
	if info(m)["duration_seconds"] != float64(1440) {
		t.Fatalf("probe: %v", info(m))
	}
	// Row creation and the probe result each publish; the probe one may trail the API by a hair.
	n := waitMore("ingest+probe", id, 1)

	v.adm.Do("PATCH", "/api/v1/media/"+id, map[string]any{"title": "Roses", "genres": []string{"Garden"}}).Expect(200)
	n = waitMore("edit", id, n)

	v.raw(v.adm, "PUT", "/api/v1/media/"+id+"/poster", map[string]string{"Content-Type": "image/png"}, pngImage(40, 60)).expect(t, 200)
	n = waitMore("poster", id, n)
	v.raw(v.adm, "DELETE", "/api/v1/media/"+id+"/poster", nil, nil).expect(t, 204)
	waitMore("poster removal", id, n)

	mu.Lock()
	ids := make([]string, 0, len(got))
	for k := range got {
		ids = append(ids, k)
	}
	mu.Unlock()
	if !slices.Contains(ids, id) || len(ids) != 1 {
		t.Fatalf("events for unexpected files: %v", ids)
	}
}
