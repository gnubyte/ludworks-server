package cast

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"ludworks/internal/cast/chromecast"
	"ludworks/internal/cast/chromecast/chromecasttest"
)

// TestChromecastEndToEnd runs the real Chromecast driver against the in-process fake
// Chromecast, which fetches the media like a receiver: through a ticket URL on the cast
// listener, from its own (loopback) address.
func TestChromecastEndToEnd(t *testing.T) {
	v := newEnvWith(t, func(m *Module) {
		m.SetDrivers(chromecast.New(chromecast.Options{HeartbeatInterval: 100 * time.Millisecond,
			StatusInterval: 100 * time.Millisecond, RequestTimeout: 3 * time.Second, ReconnectDelay: 50 * time.Millisecond}))
		m.connectWait, m.loadTimeout = 5*time.Second, 8*time.Second
	})
	fake := chromecasttest.Start(t, chromecasttest.Options{FetchMedia: true, Name: "Fake TV"})
	inst := chromecastInstance(fake.ID(), "Fake TV", fake.Host())
	inst.Port = fake.Port()
	v.setInstances(inst)
	port := freePort(t)
	v.enable(map[string]any{"media_port": port})
	ids := v.movies(2)
	s := v.start(v.adm, "chromecast-"+fake.ID(), item("media", ids[0]), item("media", ids[1]))

	ld := fake.WaitLoads(t, 1)[0]
	prefix := "http://127.0.0.1:" + strconv.Itoa(port) + mediaPathPrefix
	if !strings.HasPrefix(ld.ContentID, prefix) || !strings.HasSuffix(ld.ContentID, "/media/"+ids[0]+"/stream") ||
		ld.MetadataType != 1 || ld.Metadata["title"] != "Movie 0" || !ld.Autoplay || ld.StreamType != "BUFFERED" {
		t.Fatalf("LOAD: %+v", ld)
	}
	if ld.FetchStatus != 206 && ld.FetchStatus != 200 {
		t.Fatalf("the device could not fetch its media: status %d", ld.FetchStatus)
	}
	// Controls reach the device.
	v.control(v.adm, s.ID, map[string]any{"action": "pause"})
	v.waitFor("device paused", func() bool { return fake.State() == "PAUSED" })
	v.control(v.adm, s.ID, map[string]any{"action": "volume", "volume": 0.3})
	v.waitFor("device volume", func() bool { lvl, _ := fake.Volume(); return lvl > 0.29 && lvl < 0.31 })
	v.control(v.adm, s.ID, map[string]any{"action": "play"})
	v.waitFor("device playing", func() bool { return fake.State() == "PLAYING" })
	// The item ends on the device: the queue advances.
	fake.Finish()
	ld2 := fake.WaitLoads(t, 2)[1]
	if !strings.HasSuffix(ld2.ContentID, "/media/"+ids[1]+"/stream") || (ld2.FetchStatus != 206 && ld2.FetchStatus != 200) {
		t.Fatalf("second LOAD: %+v", ld2)
	}
	v.waitSession(v.adm, s.ID, "second item", func(s sessionResp) bool { return s.Index == 1 && s.State == StatePlaying })
	// Stopping the session stops the receiver app on the TV.
	v.adm.Do("DELETE", "/api/v1/cast/sessions/"+s.ID, nil).Expect(204)
	v.waitFor("receiver stopped", func() bool { return fake.CountCommand("receiver:STOP") > 0 })
}
