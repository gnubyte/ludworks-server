package media_test

import (
	"bytes"
	"net/http"
	"testing"

	"ludworks/internal/core"
	"ludworks/internal/media/mediatest"
)

// The cross-module API used by the cast module: playback metadata, progress recording and
// poster bytes, all with the given principal's permissions.
func TestCastAPI(t *testing.T) {
	v := newEnv(t)
	home := v.home(v.adm)
	id := v.upload(v.adm, home, "Show/Season 1", "Show.S01E02.Pilot.mp4", mediatest.MP4(100, 640, 360, 0))["id"].(string)
	v.waitProbed(v.adm, id)
	p := v.principalOf(v.adm)

	info, err := v.svc.PlaybackInfo(ctx(), p, id)
	if err != nil {
		t.Fatal(err)
	}
	if info.FileID != id || info.ShowTitle != "Show" || info.Season != 1 || info.Episode != 2 || info.DurationSeconds < 99 ||
		info.Container != "mp4" || info.VideoCodec != "h264" || info.Mime != "video/mp4" || info.Title == "" || info.PosterURL != "" {
		t.Fatalf("playback info: %+v", info)
	}

	// Progress follows the §6 rules: 95% marks the item watched.
	if err := v.svc.RecordProgress(ctx(), p, id, 30, nil); err != nil {
		t.Fatal(err)
	}
	if m := info2(v.media(v.adm, id)); m["position_seconds"].(float64) != 30 || m["watched"] != false {
		t.Fatalf("progress: %v", m)
	}
	if err := v.svc.RecordProgress(ctx(), p, id, 95, nil); err != nil {
		t.Fatal(err)
	}
	if m := info2(v.media(v.adm, id)); m["watched"] != true {
		t.Fatalf("not watched at 95%%: %v", m)
	}
	if info, _ := v.svc.PlaybackInfo(ctx(), p, id); !info.Watched || info.PositionSeconds != 95 {
		t.Fatalf("own progress in playback info: %+v", info)
	}

	// No poster (and no ffmpeg for a thumbnail): nil without error; a custom poster: JPEG bytes.
	if b, err := v.svc.PosterJPEG(ctx(), p, id, "sm"); err != nil || b != nil {
		t.Fatalf("poster without one: %d bytes, %v", len(b), err)
	}
	v.raw(v.adm, "PUT", "/api/v1/media/"+id+"/poster", map[string]string{"Content-Type": "image/png"}, pngImage(300, 450)).expect(t, 200)
	b, err := v.svc.PosterJPEG(ctx(), p, id, "md")
	if err != nil || !bytes.HasPrefix(b, []byte{0xFF, 0xD8}) {
		t.Fatalf("poster bytes: %d %v", len(b), err)
	}
	if info, _ := v.svc.PlaybackInfo(ctx(), p, id); info.PosterURL == "" {
		t.Fatal("poster URL missing")
	}
	if _, err := v.svc.PosterJPEG(ctx(), p, id, "huge"); err == nil {
		t.Fatal("bad size accepted")
	}

	// Another user: 404 everywhere (existence is not revealed).
	_, bob := v.user("bob")
	bp := v.principalOf(bob)
	for name, err := range map[string]error{
		"info":     second(v.svc.PlaybackInfo(ctx(), bp, id)),
		"progress": v.svc.RecordProgress(ctx(), bp, id, 1, nil),
		"poster":   second(v.svc.PosterJPEG(ctx(), bp, id, "md")),
	} {
		var ce *core.Error
		if !asCore(err, &ce) || ce.Status != http.StatusNotFound {
			t.Errorf("%s for another user: %v", name, err)
		}
	}
}

func info2(m map[string]any) map[string]any { return m["media"].(map[string]any) }

func second[T any](_ T, err error) error { return err }

func asCore(err error, target **core.Error) bool {
	ce, ok := err.(*core.Error)
	if ok {
		*target = ce
	}
	return ok
}
