package files_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"ludworks/internal/auth/authtest"
	"ludworks/internal/files"
)

const headroom = int64(1) << 30

// ownedBytes is what the quota charges userID for (files only, trash included).
func (v *env) ownedBytes(userID string) int64 {
	v.t.Helper()
	var n int64
	if err := v.e.Deps.DB.SQL.QueryRow(`SELECT COALESCE(SUM(size), 0) FROM files WHERE owner_id = ? AND is_dir = 0`, userID).Scan(&n); err != nil {
		v.t.Fatal(err)
	}
	return n
}

// library creates a library with the given write members.
func (v *env) library(name string, writers ...string) string {
	v.t.Helper()
	var members []map[string]any
	for _, id := range writers {
		members = append(members, map[string]any{"user_id": id, "access": "write"})
	}
	return v.adm.Do("POST", "/api/v1/admin/spaces", map[string]any{"name": name, "members": members}).Expect(201).Map()["id"].(string)
}

func (v *env) uploadRaw(c *authtest.Client, spaceID, dir, name string, content []byte, ctype string, extra ...string) *rawResp {
	v.t.Helper()
	q := url.Values{"space_id": {spaceID}, "path": {dir}, "name": {name}}
	for i := 0; i+1 < len(extra); i += 2 {
		q.Set(extra[i], extra[i+1])
	}
	return v.raw(c, "POST", "/api/v1/files/upload?"+q.Encode(), map[string]string{"Content-Type": ctype}, bytes.NewReader(content))
}

// tusUpload creates a tus upload and sends body in one PATCH; it returns the PATCH response.
func (v *env) tusUpload(c *authtest.Client, body []byte, kv ...string) *rawResp {
	v.t.Helper()
	r := v.raw(c, "POST", "/api/v1/uploads/", map[string]string{"Tus-Resumable": "1.0.0", "Upload-Length": strconv.Itoa(len(body)),
		"Upload-Metadata": tusMeta(kv...)}, nil)
	if r.Status != 201 {
		return r
	}
	return v.raw(c, "PATCH", r.Header.Get("Location"), map[string]string{"Tus-Resumable": "1.0.0",
		"Content-Type": "application/offset+octet-stream", "Upload-Offset": "0"}, bytes.NewReader(body))
}

func trashIDs(c *authtest.Client) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, it := range c.Do("GET", "/api/v1/trash", nil).Expect(200).Map()["items"].([]any) {
		m := it.(map[string]any)
		out[m["id"].(string)] = m
	}
	return out
}

// Finding: overwriting a library file kept the previous owner, so a library writer skipped
// their own quota and charged another member (twice: live + trashed version she could not see).
func TestLibraryOverwriteChargesTheUploader(t *testing.T) {
	v := newEnv(t)
	aliceID, alice := v.user("alice", 20000)
	bobID, bob := v.user("bob", 10000)
	lib := v.library("Shared", aliceID, bobID)

	a := v.upload(alice, lib, "", "a.txt", []byte("0123456789"))
	if a["owner_id"] != aliceID {
		t.Fatalf("owner: %v", a)
	}
	// Simple-upload overwrite by bob: same entry, now bob's.
	e := v.uploadRaw(bob, lib, "", "a.txt", bytes.Repeat([]byte("b"), 8000), "application/octet-stream", "overwrite", "1").expect(t, 201).json(t)
	if e["id"] != a["id"] || e["owner_id"] != bobID {
		t.Fatalf("overwrite must keep the id and belong to the uploader: %v", e)
	}
	if got, _ := v.svc.Get(ctx(), principal(t, v, bob), a["id"].(string)); got == nil || got.OwnerID != bobID {
		t.Fatalf("stored owner: %+v", got)
	}
	if b, al := v.ownedBytes(bobID), v.ownedBytes(aliceID); b != 8000 || al != 10 {
		t.Fatalf("charged bob %d (want 8000), alice %d (want 10: her trashed original)", b, al)
	}
	// Bob's quota now applies to further overwrites: 8000 live + 8000 new > 10000.
	r := v.uploadRaw(bob, lib, "", "a.txt", bytes.Repeat([]byte("c"), 8000), "application/octet-stream", "overwrite", "1").expect(t, 507)
	if errCode(r) != "quota_exceeded" {
		t.Fatalf("want quota_exceeded: %s", r.Body)
	}
	// tus overwrite also transfers ownership (bob replaces his own version: stays bob's).
	v.tusUpload(bob, []byte("tus"), "filename", "a.txt", "space_id", lib, "overwrite", "1").expect(t, 204)
	if b, al := v.ownedBytes(bobID), v.ownedBytes(aliceID); b != 8003 || al != 10 {
		t.Fatalf("after tus: bob %d alice %d", b, al)
	}
	// Alice sees (and can purge) the version she is charged for although bob trashed it.
	at := trashIDs(alice)
	var hers string
	for id, it := range at {
		if it["owner_id"] == aliceID && it["name"] == "a.txt" {
			hers = id
		}
	}
	if hers == "" || len(at) != 1 {
		t.Fatalf("alice's trash should list her replaced version only: %v", at)
	}
	if len(trashIDs(bob)) != 2 {
		t.Fatalf("bob's trash: %v", trashIDs(bob))
	}
	alice.Do("DELETE", "/api/v1/trash/"+hers, nil).Expect(204)
	if al := v.ownedBytes(aliceID); al != 0 {
		t.Fatalf("alice still charged %d", al)
	}
	// A file bob trashes is visible to its owner, who can restore it.
	b := v.upload(alice, lib, "", "b.txt", []byte("bb"))
	bob.Do("DELETE", "/api/v1/files/"+b["id"].(string), nil).Expect(204)
	if _, ok := trashIDs(alice)[b["id"].(string)]; !ok {
		t.Fatal("alice cannot see her trashed file")
	}
	alice.Do("POST", "/api/v1/trash/"+b["id"].(string)+"/restore", nil).Expect(200)
	// Alice's own uploads still fit her quota.
	v.upload(alice, v.home(alice), "", "mine.bin", bytes.Repeat([]byte("m"), 19000))

	// In a home space the content stays the home owner's whoever writes it.
	bobHome := v.home(bob)
	v.upload(v.adm, bobHome, "", "h.txt", []byte("1"))
	h := v.uploadRaw(v.adm, bobHome, "", "h.txt", []byte("22"), "application/octet-stream", "overwrite", "1").expect(t, 201).json(t)
	if h["owner_id"] != bobID {
		t.Fatalf("home overwrite owner: %v", h)
	}
}

// parallel runs fn(i) for i < n, all released at once.
func parallel(n int, fn func(i int)) {
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			fn(i)
		}()
	}
	close(start)
	wg.Wait()
}

// Finding: the quota check was not atomic with the insert; parallel copies passed together.
func TestParallelCopiesAndUploadsRespectQuota(t *testing.T) {
	v := newEnv(t)
	const quota, size, n = 10000, 6000, 8

	// Copies: the source sits in a library the user can only read, so they start at 0 bytes.
	cid, cu := v.user("copier", quota)
	lib := v.adm.Do("POST", "/api/v1/admin/spaces", map[string]any{"name": "Src", "members": []map[string]any{{"user_id": cid, "access": "read"}}}).Expect(201).Map()["id"].(string)
	src := v.upload(v.adm, lib, "", "src.bin", bytes.Repeat([]byte("s"), size))
	pc, home := principal(t, v, cu), v.home(cu)
	var ok, refused atomic.Int32
	parallel(n, func(int) {
		copied, errs, err := v.svc.Copy(ctx(), pc, []string{src["id"].(string)}, home, "", files.ConflictRename)
		switch {
		case err != nil:
			t.Error(err)
		case len(copied) == 1:
			ok.Add(1)
		case len(errs) == 1 && errs[0].Code == "quota_exceeded":
			refused.Add(1)
		default:
			t.Errorf("copy: %v %v", copied, errs)
		}
	})
	if ok.Load() != 1 || refused.Load() != n-1 || v.ownedBytes(cid) > quota {
		t.Fatalf("parallel copies: %d ok, %d refused, usage %d > quota %d?", ok.Load(), refused.Load(), v.ownedBytes(cid), quota)
	}
	if ents, _ := os.ReadDir(v.dataPath("tmp")); len(ents) > 0 {
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), "copy-") {
				t.Fatalf("staging left behind: %s", e.Name())
			}
		}
	}

	// Simple uploads streaming at the same time.
	sid, su := v.user("uploader", quota)
	shome := v.home(su)
	ok.Store(0)
	refused.Store(0)
	parallel(n, func(i int) {
		r := v.uploadRaw(su, shome, "", "f"+strconv.Itoa(i)+".bin", bytes.Repeat([]byte("u"), size), "application/octet-stream")
		switch {
		case r.Status == 201:
			ok.Add(1)
		case r.Status == 507 && errCode(r) == "quota_exceeded":
			refused.Add(1)
		default:
			t.Errorf("upload: %d %s", r.Status, r.Body)
		}
	})
	if ok.Load() != 1 || refused.Load() != n-1 || v.ownedBytes(sid) > quota {
		t.Fatalf("parallel uploads: %d ok, %d refused, usage %d", ok.Load(), refused.Load(), v.ownedBytes(sid))
	}

	// tus creations: check and reservation are atomic too.
	_, tu := v.user("tusser", quota)
	thome := v.home(tu)
	ok.Store(0)
	parallel(n, func(i int) {
		r := v.raw(tu, "POST", "/api/v1/uploads/", map[string]string{"Tus-Resumable": "1.0.0", "Upload-Length": strconv.Itoa(size),
			"Upload-Metadata": tusMeta("filename", "t"+strconv.Itoa(i)+".bin", "space_id", thome)}, nil)
		if r.Status == 201 {
			ok.Add(1)
		} else if r.Status != 507 {
			t.Errorf("tus create: %d %s", r.Status, r.Body)
		}
	})
	if ok.Load() != 1 {
		t.Fatalf("parallel tus creations: %d admitted", ok.Load())
	}
}

// Finding: the free-space headroom ignored other in-flight uploads and was never re-checked
// while writing, so uploads could fill the volume that also holds the database.
func TestFreeSpaceHeadroomIsKeptWhileWriting(t *testing.T) {
	v := newEnv(t)
	_, u := v.user("filler", 0) // unlimited quota: only the headroom protects the volume
	home := v.home(u)
	const gib = int64(1) << 30

	// (a) A user's own incomplete uploads count against the free space at creation; other
	// users' idle reservations do not block them.
	v.svc.SetFreeSpace(func(string) (int64, error) { return 10 * gib, nil })
	create := func(c *authtest.Client, name string, n int64) *rawResp {
		return v.raw(c, "POST", "/api/v1/uploads/", map[string]string{"Tus-Resumable": "1.0.0", "Upload-Length": strconv.FormatInt(n, 10),
			"Upload-Metadata": tusMeta("filename", name, "space_id", v.home(c))}, nil)
	}
	create(u, "big1.bin", 6*gib).expect(t, 201)
	if r := create(u, "big2.bin", 6*gib).expect(t, 507); errCode(r) != "insufficient_storage" {
		t.Fatalf("second promise: %s", r.Body)
	}
	_, other := v.user("other", 0)
	create(other, "big.bin", 6*gib).expect(t, 201)

	// (b) PATCH stops before writing into the headroom. Free space shrinks as the part grows.
	var part atomic.Value
	part.Store("")
	var tight atomic.Bool
	minFree := atomic.Int64{}
	minFree.Store(1 << 62)
	v.svc.SetFreeSpace(func(string) (int64, error) {
		if !tight.Load() {
			return 1 << 50, nil
		}
		var sz int64
		if p := part.Load().(string); p != "" {
			if st, err := os.Stat(p); err == nil {
				sz = st.Size()
			}
		}
		free := headroom + 12<<20 - sz
		if free < minFree.Load() {
			minFree.Store(free)
		}
		return free, nil
	})
	body := bytes.Repeat([]byte("x"), 32<<20)
	for _, checksum := range []bool{false, true} {
		tight.Store(false)
		r := v.raw(u, "POST", "/api/v1/uploads/", map[string]string{"Tus-Resumable": "1.0.0", "Upload-Length": strconv.Itoa(len(body)),
			"Upload-Metadata": tusMeta("filename", "chunk"+strconv.FormatBool(checksum)+".bin", "space_id", home)}, nil).expect(t, 201)
		loc := r.Header.Get("Location")
		partPath := filepath.Join(v.dataPath("tmp", "uploads"), filepath.Base(loc)+".part")
		part.Store(partPath)
		tight.Store(true)
		hdr := map[string]string{"Tus-Resumable": "1.0.0", "Content-Type": "application/offset+octet-stream", "Upload-Offset": "0"}
		if checksum {
			sum := sha256.Sum256(body)
			hdr["Upload-Checksum"] = "sha256 " + base64.StdEncoding.EncodeToString(sum[:])
		}
		if r := v.raw(u, "PATCH", loc, hdr, bytes.NewReader(body)); r.Status != 507 || errCode(r) != "insufficient_storage" {
			t.Fatalf("checksum=%v: PATCH into the headroom: %d %s", checksum, r.Status, r.Body)
		}
		st, err := os.Stat(partPath)
		if err != nil {
			t.Fatal(err)
		}
		off, _ := strconv.ParseInt(v.raw(u, "HEAD", loc, map[string]string{"Tus-Resumable": "1.0.0"}, nil).expect(t, 200).Header.Get("Upload-Offset"), 10, 64)
		switch {
		case checksum && (off != 0 || st.Size() != 0):
			t.Fatalf("checksummed chunk must be discarded: offset %d, part %d", off, st.Size())
		case !checksum && (off == 0 || off != st.Size() || off > 8<<20):
			t.Fatalf("kept bytes: offset %d, part %d", off, st.Size())
		}
		if minFree.Load() < headroom {
			t.Fatalf("free space went below the headroom: %d", minFree.Load())
		}
		v.raw(u, "DELETE", loc, map[string]string{"Tus-Resumable": "1.0.0"}, nil).expect(t, 204)
	}

	// (c) Simple uploads and copies are guarded while they write, and leave nothing behind.
	tight.Store(false)
	src := v.upload(u, home, "", "ten.bin", bytes.Repeat([]byte("t"), 10<<20))
	var calls atomic.Int32
	v.svc.SetFreeSpace(func(string) (int64, error) {
		if calls.Add(1) == 1 {
			return 1 << 50, nil // admission sees plenty; the disk then fills up behind its back
		}
		return headroom + 1<<20, nil
	})
	if r := v.uploadRaw(u, home, "", "four.bin", bytes.Repeat([]byte("f"), 4<<20), "application/octet-stream"); r.Status != 507 || errCode(r) != "insufficient_storage" {
		t.Fatalf("simple upload into the headroom: %d %s", r.Status, r.Body)
	}
	calls.Store(0)
	cp := u.Do("POST", "/api/v1/files/copy", map[string]any{"ids": []string{src["id"].(string)}, "dest_space_id": home, "dest_path": "", "on_conflict": "rename"}).Expect(200).Map()
	if e := cp["errors"].([]any); len(e) != 1 || e[0].(map[string]any)["code"] != "insufficient_storage" {
		t.Fatalf("copy into the headroom: %v", cp)
	}
	if n := names(v.list(u, home, "")["items"]); strings.Join(n, ",") != "ten.bin" {
		t.Fatalf("partial results indexed: %v", n)
	}
	ents, _ := os.ReadDir(v.dataPath("tmp"))
	for _, e := range ents {
		if !e.IsDir() || strings.HasPrefix(e.Name(), "copy-") {
			t.Fatalf("leftover in tmp: %s", e.Name())
		}
	}
}

// Finding: writes, moves, trash and restore resolved entry paths through the data root, so a
// folder swapped for a symlink on disk redirected them into other spaces or server files.
func TestSymlinkedFolderCannotRedirectWrites(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks needs privileges on Windows")
	}
	v := newEnv(t)
	victimID, victim := v.user("victim", 0)
	mallID, mall := v.user("mallory", 0)
	vhome, mhome := v.home(victim), v.home(mall)
	v.upload(victim, vhome, "", "x.txt", []byte("VICTIM DATA"))
	if err := os.WriteFile(v.dataPath("sentinel.txt"), []byte("SERVER FILE"), 0o600); err != nil {
		t.Fatal(err)
	}
	v.mkdir(mall, mhome, "", "d")
	x := v.upload(mall, mhome, "d", "x.txt", []byte("mine"))
	sentinel := v.upload(mall, mhome, "d", "sentinel.txt", []byte("mine"))
	v.mkdir(mall, mhome, "", "r")
	planted := v.upload(mall, mhome, "r", "planted.txt", []byte("PLANTED"))
	mall.Do("DELETE", "/api/v1/files/"+planted["id"].(string), nil).Expect(204)

	// Behind the server's back (an SMB/NFS share of the home, a restored backup...):
	// d -> the victim's home, r -> the victim's home, e -> the data directory.
	mdir := v.dataPath("storage", "home", mallID)
	swap := func(name, target string) {
		if err := os.RemoveAll(filepath.Join(mdir, name)); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(mdir, name)); err != nil {
			t.Fatal(err)
		}
	}
	swap("d", "../"+victimID)
	swap("r", "../"+victimID)

	victimFile := v.dataPath("storage", "home", victimID, "x.txt")
	intact := func(what string) {
		t.Helper()
		if b, err := os.ReadFile(victimFile); err != nil || string(b) != "VICTIM DATA" {
			t.Fatalf("%s reached the victim's file: %q %v", what, b, err)
		}
		if b, err := os.ReadFile(v.dataPath("sentinel.txt")); err != nil || string(b) != "SERVER FILE" {
			t.Fatalf("%s reached the server's file: %q %v", what, b, err)
		}
		if exists(v.dataPath("storage", "home", victimID, "planted.txt")) || exists(v.dataPath("storage", "home", victimID, "y.txt")) {
			t.Fatalf("%s planted a file in the victim's home", what)
		}
	}
	notOK := func(what string, status int) {
		t.Helper()
		if status < 400 {
			t.Fatalf("%s through a symlinked folder succeeded (%d)", what, status)
		}
		intact(what)
	}
	xid := x["id"].(string)
	notOK("trash", mall.Do("DELETE", "/api/v1/files/"+xid, nil).Status)
	notOK("rename", mall.Do("PATCH", "/api/v1/files/"+xid, map[string]any{"name": "y.txt"}).Status)
	mv := mall.Do("POST", "/api/v1/files/move", map[string]any{"ids": []string{xid}, "dest_space_id": mhome, "dest_path": ""}).Expect(200).Map()
	if len(mv["moved"].([]any)) != 0 {
		t.Fatalf("move out of a symlinked folder: %v", mv)
	}
	intact("move")
	notOK("overwrite upload", v.uploadRaw(mall, mhome, "d", "x.txt", []byte("evil"), "text/plain", "overwrite", "1").Status)
	notOK("upload into the folder", v.uploadRaw(mall, mhome, "d", "y.txt", []byte("evil"), "text/plain").Status)
	notOK("restore", mall.Do("POST", "/api/v1/trash/"+planted["id"].(string)+"/restore", nil).Status)
	cp := mall.Do("POST", "/api/v1/files/copy", map[string]any{"ids": []string{sentinel["id"].(string)}, "dest_space_id": mhome, "dest_path": "", "on_conflict": "rename"}).Expect(200).Map()
	_ = cp // copying out reads through the space root, which refuses the escape
	intact("copy")

	// A folder pointing at the data directory cannot touch the server's own files.
	swap("d", "../../..")
	notOK("trash of a server file", mall.Do("DELETE", "/api/v1/files/"+sentinel["id"].(string), nil).Status)
	notOK("overwrite of a server file", v.uploadRaw(mall, mhome, "d", "sentinel.txt", []byte("evil"), "text/plain", "overwrite", "1").Status)
	if st, err := os.Lstat(filepath.Join(mdir, "d")); err != nil || st.Mode().Type() != fs.ModeSymlink {
		t.Fatalf("test setup: %v", err)
	}
}

// Finding: client MIME hints were filtered by a denylist, so JavaScript types such as
// application/ecmascript were stored and served as such (same-origin script for CSP 'self').
func TestScriptMimeHintsAreNeverServed(t *testing.T) {
	v := newEnv(t)
	_, u := v.user("mimer", 0)
	home := v.home(u)
	js := []byte("/*\x01\x02 binary marker */ document.title='PWNED';")
	jsTypes := []string{"application/ecmascript", "application/x-ecmascript", "text/ecmascript", "text/x-ecmascript",
		"text/x-javascript", "text/jscript", "text/livescript", "text/javascript1.5", "application/javascript", "text/javascript"}
	check := func(id, hint string) {
		t.Helper()
		e := u.Do("GET", "/api/v1/files/"+id, nil).Expect(200).Map()
		if e["mime"] != "application/octet-stream" {
			t.Fatalf("hint %s stored as %v", hint, e["mime"])
		}
		for _, q := range []string{"", "?download=1"} {
			r := v.raw(u, "GET", "/api/v1/files/"+id+"/content"+q, nil, nil).expect(t, 200)
			if ct := r.Header.Get("Content-Type"); ct != "application/octet-stream" {
				t.Fatalf("hint %s served as %q", hint, ct)
			}
		}
	}
	for i, m := range jsTypes {
		e := v.uploadRaw(u, home, "", "s"+strconv.Itoa(i)+".bin", js, m).expect(t, 201).json(t)
		check(e["id"].(string), m)
		r := v.tusUpload(u, js, "filename", "t"+strconv.Itoa(i)+".bin", "space_id", home, "mime", m).expect(t, 204)
		check(r.Header.Get("Ludworks-File-Id"), "tus "+m)
	}
	// Allowlisted hints still work; attachments never carry the stored type.
	pdf := v.uploadRaw(u, home, "", "doc.bin", []byte("\x01\x02 not detected"), "application/pdf").expect(t, 201).json(t)
	if pdf["mime"] != "application/pdf" {
		t.Fatalf("allowlisted hint dropped: %v", pdf["mime"])
	}
	img := v.uploadRaw(u, home, "", "p.png", []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), "image/png").expect(t, 201).json(t)
	if ct := v.raw(u, "GET", "/api/v1/files/"+img["id"].(string)+"/content", nil, nil).expect(t, 200).Header.Get("Content-Type"); ct != "image/png" {
		t.Fatalf("inline image: %q", ct)
	}
	if ct := v.raw(u, "GET", "/api/v1/files/"+img["id"].(string)+"/content?download=1", nil, nil).expect(t, 200).Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("download: %q", ct)
	}
}
