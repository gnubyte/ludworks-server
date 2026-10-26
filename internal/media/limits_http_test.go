package media_test

import (
	"testing"

	"ludworks/internal/media/mediatest"
)

// TestManualProbeLimitedPerUser: POST /media/{id}/probe runs at most one probe per user at
// a time (429 beyond), without blocking other users.
func TestManualProbeLimitedPerUser(t *testing.T) {
	v := newEnv(t)
	id := v.upload(v.adm, v.home(v.adm), "", "Movie (2010).mp4", mediatest.MP4(42, 64, 64, 0))["id"].(string)
	v.waitProbed(v.adm, id)
	bobUID, bob := v.user("bob")
	bobFile := v.upload(bob, v.home(bob), "", "Bob (2011).mp4", mediatest.MP4(10, 64, 64, 0))["id"].(string)
	v.waitProbed(bob, bobFile)

	adminID := v.principalOf(v.adm).UserID
	release, ok := v.svc.HoldProbe(adminID) // a probe of the admin's is still running
	if !ok {
		t.Fatal("could not take the admin's probe slot")
	}
	r := v.raw(v.adm, "POST", "/api/v1/media/"+id+"/probe", nil, nil)
	if r.Status != 429 || r.code() != "rate_limited" || r.Header.Get("Retry-After") == "" {
		t.Fatalf("concurrent probe of the same user: %d %v %s", r.Status, r.Header, r.Body)
	}
	if m := bob.Do("POST", "/api/v1/media/"+bobFile+"/probe", nil).Expect(200).Map(); info(m)["probe_status"] != "ok" {
		t.Fatalf("other user's probe: %v", info(m))
	}
	release()
	v.adm.Do("POST", "/api/v1/media/"+id+"/probe", nil).Expect(200)
	// The slot is released after each request.
	if rel, ok := v.svc.HoldProbe(adminID); !ok {
		t.Fatal("probe slot leaked")
	} else {
		rel()
	}
	if rel, ok := v.svc.HoldProbe(bobUID); !ok {
		t.Fatal("bob's probe slot leaked")
	} else {
		rel()
	}
}
