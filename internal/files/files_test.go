package files_test

import (
	"archive/zip"
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"ludworks/internal/auth/authtest"
	"ludworks/internal/events"
	"ludworks/internal/files"
)

func TestSpacesListingAndPagination(t *testing.T) {
	v := newEnv(t)
	home := v.home(v.adm)
	v.mkdir(v.adm, home, "", "Zeta")
	v.mkdir(v.adm, home, "", "alpha")
	for i, n := range []string{"b.txt", "A.txt", "c.bin", "d.txt", "e.txt"} {
		v.upload(v.adm, home, "", n, bytes.Repeat([]byte("x"), (i+1)*10))
	}
	l := v.list(v.adm, home, "")
	if got := strings.Join(names(l["items"]), ","); got != "alpha,Zeta,A.txt,b.txt,c.bin,d.txt,e.txt" {
		t.Fatalf("default order: %s", got)
	}
	if l["dir"].(map[string]any)["parent_id"] != nil || l["dir"].(map[string]any)["path"] != "" {
		t.Fatalf("root dir: %v", l["dir"])
	}
	// Size desc, paginated by 3: dirs stay first.
	var all []string
	cursor := ""
	for page := 0; page < 5; page++ {
		extra := []string{"sort", "size", "order", "desc", "limit", "3"}
		if cursor != "" {
			extra = append(extra, "cursor", cursor)
		}
		p := v.list(v.adm, home, "", extra...)
		all = append(all, names(p["items"])...)
		nc, _ := p["next_cursor"].(string)
		if nc == "" {
			break
		}
		cursor = nc
	}
	if got := strings.Join(all, ","); got != "Zeta,alpha,e.txt,d.txt,c.bin,A.txt,b.txt" {
		t.Fatalf("size desc paged: %s", got)
	}
	// A cursor from another sort is rejected.
	p := v.list(v.adm, home, "", "limit", "2")
	v.adm.Do("GET", "/api/v1/files?sort=size&cursor="+url.QueryEscape(p["next_cursor"].(string)), nil).ExpectError(400, "bad_request")
	v.adm.Do("GET", "/api/v1/files?sort=evil", nil).ExpectError(422, "validation_failed")
	v.adm.Do("GET", "/api/v1/files?path=nope", nil).ExpectError(404, "not_found")
	v.adm.Do("GET", "/api/v1/files?path=..%2Fx", nil).ExpectError(422, "validation_failed")

	// Nested breadcrumbs.
	v.mkdir(v.adm, home, "alpha", "beta")
	l = v.list(v.adm, home, "alpha/beta")
	bc := l["breadcrumbs"].([]any)
	if len(bc) != 3 || bc[0].(map[string]any)["name"] != "Home" || bc[2].(map[string]any)["path"] != "alpha/beta" {
		t.Fatalf("breadcrumbs: %v", bc)
	}
	// Duplicate folder.
	v.adm.Do("POST", "/api/v1/files/folders", map[string]any{"space_id": home, "path": "alpha", "name": "beta"}).ExpectError(409, "conflict")
	v.adm.Do("POST", "/api/v1/files/folders", map[string]any{"space_id": home, "path": "missing", "name": "x"}).ExpectError(404, "not_found")
	v.adm.Do("POST", "/api/v1/files/folders", map[string]any{"space_id": home, "path": "", "name": "CON"}).ExpectError(422, "validation_failed")
	if !exists(v.dataPath("storage", "home")) {
		t.Fatal("home dir missing on disk")
	}
}

func TestContentServing(t *testing.T) {
	v := newEnv(t)
	home := v.home(v.adm)
	e := v.upload(v.adm, home, "docs", "hello.txt", []byte("hello world"), "mod_time", "2020-01-02T03:04:05Z")
	id := e["id"].(string)
	if e["mime"] != "text/plain" || e["mod_time"] != "2020-01-02T03:04:05.000Z" || e["path"] != "docs/hello.txt" {
		t.Fatalf("entry: %v", e)
	}
	r := v.raw(v.adm, "GET", "/api/v1/files/"+id+"/content", nil, nil).expect(t, 200)
	if string(r.Body) != "hello world" || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/plain") ||
		!strings.HasPrefix(r.Header.Get("Content-Disposition"), "inline") || !strings.Contains(r.Header.Get("Content-Security-Policy"), "sandbox") ||
		r.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("content headers: %v", r.Header)
	}
	etag := r.Header.Get("ETag")
	if !strings.HasPrefix(etag, `"`) || strings.HasPrefix(etag, "W/") {
		t.Fatalf("etag %q", etag)
	}
	v.raw(v.adm, "GET", "/api/v1/files/"+id+"/content", map[string]string{"If-None-Match": etag}, nil).expect(t, 304)
	rr := v.raw(v.adm, "GET", "/api/v1/files/"+id+"/content", map[string]string{"Range": "bytes=6-"}, nil).expect(t, 206)
	if string(rr.Body) != "world" || rr.Header.Get("Content-Range") != "bytes 6-10/11" {
		t.Fatalf("range: %q %v", rr.Body, rr.Header)
	}
	// If-Range with a stale validator returns the full body.
	v.raw(v.adm, "GET", "/api/v1/files/"+id+"/content", map[string]string{"Range": "bytes=6-", "If-Range": `"stale"`}, nil).expect(t, 200)
	d := v.raw(v.adm, "GET", "/api/v1/files/"+id+"/content?download=1", nil, nil).expect(t, 200)
	if !strings.HasPrefix(d.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("download disposition %q", d.Header.Get("Content-Disposition"))
	}
	// HTML and SVG are always attachments with a neutral type.
	h := v.upload(v.adm, home, "", "x.html", []byte("<script>alert(1)</script>"))
	hr := v.raw(v.adm, "GET", "/api/v1/files/"+h["id"].(string)+"/content", nil, nil).expect(t, 200)
	if hr.Header.Get("Content-Type") != "application/octet-stream" || !strings.HasPrefix(hr.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("html served unsafely: %v", hr.Header)
	}
	// Query-token access for media players.
	v.raw(nil, "GET", "/api/v1/files/"+id+"/content?access_token="+url.QueryEscape(v.adm.Bearer), nil, nil).expect(t, 200)
	v.raw(nil, "GET", "/api/v1/files/"+id+"/content", nil, nil).expect(t, 401)
	// Folder content is a 400, zip works.
	dir := v.list(v.adm, home, "docs")["dir"].(map[string]any)["id"].(string)
	v.raw(v.adm, "GET", "/api/v1/files/"+dir+"/content", nil, nil).expect(t, 400)
	// Service contract.
	p := principal(t, v, v.adm)
	f, ent, err := v.svc.Open(ctx(), p, id)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(f)
	f.Close()
	if string(b) != "hello world" || ent.Name != "hello.txt" {
		t.Fatalf("Open: %q %v", b, ent)
	}
	lp, err := v.svc.LocalPath(ctx(), p, id)
	if err != nil || !strings.HasSuffix(filepath.ToSlash(lp), "docs/hello.txt") {
		t.Fatalf("LocalPath %q %v", lp, err)
	}
}

func TestSimpleUploadValidation(t *testing.T) {
	v := newEnv(t)
	home := v.home(v.adm)
	v.upload(v.adm, home, "", "a.txt", []byte("1"))
	r := v.raw(v.adm, "POST", "/api/v1/files/upload?name=a.txt&space_id="+home, nil, bytes.NewReader([]byte("2")))
	if r.Status != 409 {
		t.Fatalf("conflict: %d %s", r.Status, r.Body)
	}
	e := v.upload(v.adm, home, "", "a.txt", []byte("22"), "overwrite", "1")
	if e["size"] != float64(2) {
		t.Fatalf("overwrite: %v", e)
	}
	// Chunked body without Content-Length is refused.
	pr, pw := io.Pipe()
	go func() { pw.Write([]byte("x")); pw.Close() }()
	r = v.raw(v.adm, "POST", "/api/v1/files/upload?name=b.txt", nil, pr)
	if r.Status != 400 {
		t.Fatalf("no content-length: %d", r.Status)
	}
	r = v.raw(v.adm, "POST", "/api/v1/files/upload?name="+url.QueryEscape("../x"), nil, bytes.NewReader([]byte("1")))
	if r.Status != 422 {
		t.Fatalf("bad name: %d", r.Status)
	}
	v.e.Patch("files", map[string]any{"max_upload_bytes": 3})
	r = v.raw(v.adm, "POST", "/api/v1/files/upload?name=big.txt", nil, bytes.NewReader([]byte("1234")))
	if r.Status != 413 {
		t.Fatalf("limit: %d %s", r.Status, r.Body)
	}
}

func TestTusUpload(t *testing.T) {
	v := newEnv(t)
	home := v.home(v.adm)
	o := v.raw(nil, "OPTIONS", "/api/v1/uploads/", nil, nil).expect(t, 204)
	if o.Header.Get("Tus-Version") != "1.0.0" || !strings.Contains(o.Header.Get("Tus-Extension"), "checksum") ||
		o.Header.Get("Tus-Checksum-Algorithm") != "sha1,sha256" {
		t.Fatalf("options: %v", o.Header)
	}
	data := bytes.Repeat([]byte("0123456789abcdef"), 4096) // 64 KiB
	create := func(cl *authtest.Client, name string, size int, extra ...string) *rawResp {
		if cl == nil {
			cl = v.adm
		}
		meta := tusMeta(append([]string{"filename", name, "space_id", home, "path", "up/loads"}, extra...)...)
		return v.raw(cl, "POST", "/api/v1/uploads/", map[string]string{"Tus-Resumable": "1.0.0", "Upload-Length": fmt.Sprint(size), "Upload-Metadata": meta}, nil)
	}
	v.raw(v.adm, "POST", "/api/v1/uploads/", map[string]string{"Upload-Length": "1", "Upload-Metadata": tusMeta("filename", "x", "space_id", home)}, nil).expect(t, 412)
	cr := create(nil, "big.bin", len(data), "tags", "a,b", "mod_time", "2021-05-06T07:08:09Z").expect(t, 201)
	loc := cr.Header.Get("Location")
	if !strings.HasPrefix(loc, "/api/v1/uploads/") || cr.Header.Get("Upload-Expires") == "" {
		t.Fatalf("create headers: %v", cr.Header)
	}
	th := func(extra map[string]string) map[string]string {
		h := map[string]string{"Tus-Resumable": "1.0.0", "Content-Type": "application/offset+octet-stream"}
		for k, x := range extra {
			h[k] = x
		}
		return h
	}
	sum := func(b []byte) string { s := sha256.Sum256(b); return "sha256 " + b64(string(s[:])) }
	chunk1 := data[:20000]
	v.raw(v.adm, "PATCH", loc, th(map[string]string{"Upload-Offset": "5"}), bytes.NewReader(chunk1)).expect(t, 409)
	v.raw(v.adm, "PATCH", loc, th(map[string]string{"Upload-Offset": "0", "Upload-Checksum": sum([]byte("nope"))}), bytes.NewReader(chunk1)).expect(t, 460)
	v.raw(v.adm, "PATCH", loc, map[string]string{"Tus-Resumable": "1.0.0", "Content-Type": "text/plain", "Upload-Offset": "0"}, bytes.NewReader(chunk1)).expect(t, 415)
	h := v.raw(v.adm, "HEAD", loc, map[string]string{"Tus-Resumable": "1.0.0"}, nil).expect(t, 200)
	if h.Header.Get("Upload-Offset") != "0" || h.Header.Get("Upload-Length") != fmt.Sprint(len(data)) || h.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("head after bad checksum: %v", h.Header)
	}
	p1 := v.raw(v.adm, "PATCH", loc, th(map[string]string{"Upload-Offset": "0", "Upload-Checksum": sum(chunk1)}), bytes.NewReader(chunk1)).expect(t, 204)
	if p1.Header.Get("Upload-Offset") != "20000" {
		t.Fatalf("offset after chunk 1: %v", p1.Header)
	}
	// Incomplete uploads are listed; other users cannot see or touch them.
	var ups []map[string]any
	v.adm.Do("GET", "/api/v1/uploads", nil).Expect(200).JSON(&ups)
	if len(ups) != 1 || ups[0]["offset"] != float64(20000) || ups[0]["filename"] != "big.bin" {
		t.Fatalf("uploads: %v", ups)
	}
	_, bob := v.user("bob", 0)
	v.raw(bob, "HEAD", loc, map[string]string{"Tus-Resumable": "1.0.0"}, nil).expect(t, 404)
	v.raw(bob, "PATCH", loc, th(map[string]string{"Upload-Offset": "20000"}), bytes.NewReader(data[20000:])).expect(t, 404)
	v.raw(bob, "DELETE", loc, map[string]string{"Tus-Resumable": "1.0.0"}, nil).expect(t, 404)

	// Concurrent PATCH on the same upload is refused while one is streaming.
	pr, pw := io.Pipe()
	done := make(chan *rawResp)
	go func() {
		done <- v.raw(v.adm, "PATCH", loc, th(map[string]string{"Upload-Offset": "20000"}), pr)
	}()
	pw.Write(data[20000:30000])
	// Wait (without touching the upload) until the streaming PATCH holds the lock: its bytes
	// only reach the .part file after the handler acquired it. Probing with PATCHes instead
	// raced the streaming request: a probe that arrived first was accepted (moving the offset)
	// or briefly took the lock itself, so under load the test saw 409s or a 423 on the stream.
	part := v.dataPath("tmp", "uploads", path.Base(loc)+".part")
	eventually(t, "streaming PATCH writing", func() bool {
		st, err := os.Stat(part)
		return err == nil && st.Size() >= 30000
	})
	v.raw(v.adm, "PATCH", loc, th(map[string]string{"Upload-Offset": "20000"}), bytes.NewReader([]byte("x"))).expect(t, 423)
	pw.Close()
	r := <-done
	r.expect(t, 204)
	if r.Header.Get("Upload-Offset") != "30000" {
		t.Fatalf("partial chunk offset %v", r.Header)
	}
	// Final chunk (sha1) completes the upload.
	rest := data[30000:]
	s1 := sha1Sum(rest)
	fin := v.raw(v.adm, "PATCH", loc, th(map[string]string{"Upload-Offset": "30000", "Upload-Checksum": "sha1 " + s1}), bytes.NewReader(rest)).expect(t, 204)
	fid := fin.Header.Get("Ludworks-File-Id")
	if fid == "" || fin.Header.Get("Ludworks-File-Path") != "up/loads/big.bin" {
		t.Fatalf("completion headers: %v", fin.Header)
	}
	// HEAD and a repeated final PATCH are idempotent.
	h = v.raw(v.adm, "HEAD", loc, map[string]string{"Tus-Resumable": "1.0.0"}, nil).expect(t, 200)
	if h.Header.Get("Ludworks-File-Id") != fid || h.Header.Get("Upload-Offset") != fmt.Sprint(len(data)) {
		t.Fatalf("head after completion: %v", h.Header)
	}
	v.raw(v.adm, "PATCH", loc, th(map[string]string{"Upload-Offset": fmt.Sprint(len(data))}), bytes.NewReader(nil)).expect(t, 204)
	v.adm.Do("GET", "/api/v1/uploads", nil).Expect(200).JSON(&ups)
	if len(ups) != 0 {
		t.Fatalf("completed upload still listed: %v", ups)
	}
	ent := v.adm.Do("GET", "/api/v1/files/"+fid, nil).Expect(200).Map()
	if ent["size"] != float64(len(data)) || ent["mod_time"] != "2021-05-06T07:08:09.000Z" || fmt.Sprint(ent["tags"]) != "[a b]" {
		t.Fatalf("entry: %v", ent)
	}
	got := v.raw(v.adm, "GET", "/api/v1/files/"+fid+"/content", nil, nil).expect(t, 200)
	if !bytes.Equal(got.Body, data) {
		t.Fatal("content mismatch")
	}
	if ents, _ := os.ReadDir(v.dataPath("tmp", "uploads")); len(ents) != 0 {
		t.Fatalf("part files left: %v", ents)
	}

	// Same name without overwrite → 409 at creation; with overwrite the entry keeps its id
	// and the old version goes to the trash.
	create(nil, "big.bin", 3).expect(t, 409)
	cr = create(nil, "big.bin", 3, "overwrite", "1").expect(t, 201)
	fin = v.raw(v.adm, "PATCH", cr.Header.Get("Location"), th(map[string]string{"Upload-Offset": "0"}), bytes.NewReader([]byte("new"))).expect(t, 204)
	if fin.Header.Get("Ludworks-File-Id") != fid {
		t.Fatalf("overwrite changed id: %v", fin.Header)
	}
	trash := v.adm.Do("GET", "/api/v1/trash", nil).Expect(200).Map()["items"].([]any)
	if len(trash) != 1 || trash[0].(map[string]any)["size"] != float64(len(data)) || trash[0].(map[string]any)["original_path"] != "up/loads/big.bin" {
		t.Fatalf("old version not in trash: %v", trash)
	}

	// Zero-length uploads complete at creation.
	z := create(nil, "empty.txt", 0).expect(t, 201)
	if z.Header.Get("Ludworks-File-Id") == "" {
		t.Fatalf("empty upload not committed: %v", z.Header)
	}
	// Oversized chunk and termination.
	cr = create(nil, "t.bin", 4).expect(t, 201)
	loc = cr.Header.Get("Location")
	v.raw(v.adm, "PATCH", loc, th(map[string]string{"Upload-Offset": "0"}), bytes.NewReader([]byte("12345"))).expect(t, 413)
	v.raw(v.adm, "DELETE", loc, map[string]string{"Tus-Resumable": "1.0.0"}, nil).expect(t, 204)
	v.raw(v.adm, "HEAD", loc, map[string]string{"Tus-Resumable": "1.0.0"}, nil).expect(t, 404)
	// Missing metadata and bad permissions.
	v.raw(v.adm, "POST", "/api/v1/uploads/", map[string]string{"Tus-Resumable": "1.0.0", "Upload-Length": "1", "Upload-Metadata": tusMeta("space_id", home)}, nil).expect(t, 422)
	v.raw(bob, "POST", "/api/v1/uploads/", map[string]string{"Tus-Resumable": "1.0.0", "Upload-Length": "1", "Upload-Metadata": tusMeta("filename", "x", "space_id", home)}, nil).expect(t, 404)
	v.raw(v.adm, "POST", "/api/v1/uploads/", map[string]string{"Tus-Resumable": "1.0.0", "Upload-Length": "-1", "Upload-Metadata": tusMeta("filename", "x", "space_id", home)}, nil).expect(t, 400)
	// Insufficient disk space.
	v.svc.SetFreeSpace(func(string) (int64, error) { return 2 << 30, nil })
	create(nil, "huge.bin", 3<<30).expect(t, 507)
}

func sha1Sum(b []byte) string { s := sha1.Sum(b); return base64.StdEncoding.EncodeToString(s[:]) }

func TestTusInterruptedChunk(t *testing.T) {
	v := newEnv(t)
	home := v.home(v.adm)
	r := v.raw(v.adm, "POST", "/api/v1/uploads/", map[string]string{"Tus-Resumable": "1.0.0", "Upload-Length": "100000",
		"Upload-Metadata": tusMeta("filename", "i.bin", "space_id", home)}, nil).expect(t, 201)
	loc := r.Header.Get("Location")
	send := func(extra string, body []byte, declared int) {
		u, _ := url.Parse(v.e.URL)
		conn, err := net.Dial("tcp", u.Host)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(conn, "PATCH %s HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nTus-Resumable: 1.0.0\r\n"+
			"Content-Type: application/offset+octet-stream\r\nUpload-Offset: 0\r\n%sContent-Length: %d\r\n\r\n", loc, u.Host, v.adm.Bearer, extra, declared)
		conn.Write(body)
		time.Sleep(200 * time.Millisecond)
		conn.Close()
	}
	offset := func() string {
		return v.raw(v.adm, "HEAD", loc, map[string]string{"Tus-Resumable": "1.0.0"}, nil).expect(t, 200).Header.Get("Upload-Offset")
	}
	// A checksummed chunk that is cut off is discarded completely.
	sum := sha256.Sum256(make([]byte, 50000))
	send("Upload-Checksum: sha256 "+base64.StdEncoding.EncodeToString(sum[:])+"\r\n", make([]byte, 20000), 50000)
	eventually(t, "lock released", func() bool {
		return offset() == "0" && v.raw(v.adm, "PATCH", loc, map[string]string{"Tus-Resumable": "1.0.0", "Content-Type": "application/offset+octet-stream", "Upload-Offset": "1"}, bytes.NewReader(nil)).Status == 409
	})
	// Without a checksum the bytes that arrived are kept, so the client resumes there.
	send("", make([]byte, 30000), 50000)
	eventually(t, "partial chunk kept", func() bool { return offset() == "30000" })
}

// caseInsensitiveDisk reports whether dir's file system folds letter case.
func caseInsensitiveDisk(t *testing.T, dir string) bool {
	t.Helper()
	p := filepath.Join(dir, "case-probe")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(p)
	_, err := os.Lstat(filepath.Join(dir, "CASE-PROBE"))
	return err == nil
}

// Regression (reported on the Windows build): names that differ only in letter case aliased
// one directory on a case-insensitive disk. POST /files/folders "CASETEST" next to "casetest"
// gave 201 with a second id, and uploads to "CASETEST/x.txt" landed in casetest/ where the
// listing of CASETEST never showed them. On such disks the second name is now 409 everywhere
// (folders, uploads, tus, overwrite, rename, move/copy); case-sensitive disks keep both.
// Runs for real on Windows/macOS; on Linux point TMPDIR at a case-insensitive mount.
func TestCaseVariantNamesNeverAliasOnDisk(t *testing.T) {
	v := newEnv(t)
	ci := caseInsensitiveDisk(t, v.e.Deps.Config.DataDir)
	t.Logf("case-insensitive disk: %v", ci)
	home := v.home(v.adm)
	lower := v.mkdir(v.adm, home, "", "casetest")
	orig := v.upload(v.adm, home, "", "Report.txt", []byte("original"))
	other := v.upload(v.adm, home, "", "b.txt", []byte("bee"))
	content := func(id string) string {
		return string(v.raw(v.adm, "GET", "/api/v1/files/"+id+"/content", nil, nil).expect(t, 200).Body)
	}
	upload := func(dir, name string, extra ...string) *rawResp {
		q := url.Values{"space_id": {home}, "path": {dir}, "name": {name}}
		for i := 0; i+1 < len(extra); i += 2 {
			q.Set(extra[i], extra[i+1])
		}
		return v.raw(v.adm, "POST", "/api/v1/files/upload?"+q.Encode(), map[string]string{"Content-Type": "application/octet-stream"}, bytes.NewReader([]byte("new")))
	}
	tusCreate := func(dir, name string) *rawResp {
		return v.raw(v.adm, "POST", "/api/v1/uploads/", map[string]string{"Tus-Resumable": "1.0.0", "Upload-Length": "3",
			"Upload-Metadata": tusMeta("filename", name, "space_id", home, "path", dir)}, nil)
	}

	if !ci {
		// Case-sensitive disk: case variants are distinct items, each with its own content.
		upper := v.mkdir(v.adm, home, "", "CASETEST")
		if upper["id"] == lower["id"] {
			t.Fatal("same id")
		}
		x := upload("CASETEST", "x.txt").expect(t, 201).json(t)
		if !exists(v.dataPath("storage", "home", principal(t, v, v.adm).UserID, "CASETEST", "x.txt")) {
			t.Fatalf("upload not stored under CASETEST: %v", x)
		}
		up := upload("", "REPORT.TXT").expect(t, 201).json(t)
		if content(up["id"].(string)) != "new" || content(orig["id"].(string)) != "original" {
			t.Fatal("variants share content")
		}
		return
	}

	conflict := func(what string, r *rawResp) {
		t.Helper()
		if r.Status != 409 || errCode(r) != "conflict" || !strings.Contains(string(r.Body), "not case-sensitive") {
			t.Fatalf("%s: %d %s", what, r.Status, r.Body)
		}
	}
	// Folders: the reported repro.
	r := v.adm.Do("POST", "/api/v1/files/folders", map[string]any{"space_id": home, "path": "", "name": "CASETEST"})
	if r.Status != 409 || !strings.Contains(string(r.Body), `"existing_name":"casetest"`) {
		t.Fatalf("CASETEST folder: %d %s", r.Status, r.Body)
	}
	conflict("upload into CASETEST/", upload("CASETEST", "x.txt"))
	conflict("upload into CASETEST/deeper/", upload("CASETEST/deeper", "x.txt"))
	conflict("tus into CASETEST/", tusCreate("CASETEST", "x.txt"))
	// Files: plain upload, overwrite (which must not trash or clobber "Report.txt"), tus.
	conflict("upload REPORT.TXT", upload("", "REPORT.TXT"))
	conflict("overwrite REPORT.TXT", upload("", "REPORT.TXT", "overwrite", "1"))
	conflict("tus REPORT.TXT", tusCreate("", "REPORT.TXT"))
	if content(orig["id"].(string)) != "original" {
		t.Fatal("Report.txt was clobbered")
	}
	if tr := v.adm.Do("GET", "/api/v1/trash", nil).Expect(200).Map()["items"].([]any); len(tr) != 0 {
		t.Fatalf("something went to the trash: %v", tr)
	}
	// Rename onto a case variant of a sibling is refused; a case-only rename of itself works.
	rr := v.adm.Do("PATCH", "/api/v1/files/"+other["id"].(string), map[string]any{"name": "REPORT.txt"})
	conflict("rename b.txt → REPORT.txt", &rawResp{Status: rr.Status, Body: rr.Body})
	rn := v.adm.Do("PATCH", "/api/v1/files/"+orig["id"].(string), map[string]any{"name": "report.TXT"}).Expect(200).Map()
	if rn["path"] != "report.TXT" || content(orig["id"].(string)) != "original" {
		t.Fatalf("case-only rename: %v", rn)
	}
	// Move/copy: fail and overwrite refuse, rename picks a free name.
	sub := v.mkdir(v.adm, home, "", "sub")
	v.upload(v.adm, home, "sub", "B.TXT", []byte("upper"))
	mv := v.adm.Do("POST", "/api/v1/files/move", map[string]any{"ids": []string{other["id"].(string)}, "dest_space_id": home, "dest_path": "sub", "on_conflict": "overwrite"}).Expect(200).Map()
	if errs := mv["errors"].([]any); len(errs) != 1 || !strings.Contains(errs[0].(map[string]any)["error"].(string), "not case-sensitive") {
		t.Fatalf("move overwrite onto case variant: %v", mv)
	}
	cp := v.adm.Do("POST", "/api/v1/files/copy", map[string]any{"ids": []string{other["id"].(string)}, "dest_space_id": home, "dest_path": "sub", "on_conflict": "rename"}).Expect(200).Map()
	if c := cp["copied"].([]any); len(c) != 1 || c[0].(map[string]any)["name"] != "b (1).txt" {
		t.Fatalf("copy rename next to a case variant: %v", cp)
	}
	l := names(v.list(v.adm, home, "sub")["items"])
	sort.Strings(l)
	if strings.Join(l, ",") != "B.TXT,b (1).txt" {
		t.Fatalf("sub: %v (%v)", l, sub)
	}
	// After all that, the index and the disk still agree.
	v.adm.Do("POST", "/api/v1/files/rescan", map[string]any{"space_id": home}).Expect(202)
	eventually(t, "rescan finished", func() bool {
		top := names(v.list(v.adm, home, "")["items"])
		sort.Strings(top)
		return strings.Join(top, ",") == "b.txt,casetest,report.TXT,sub"
	})
}

// lockedBuf is a goroutine-safe log sink.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// Regression: a client that hangs up mid-PATCH (or resets the connection) was logged as
// `level=ERROR msg="internal error" err="unexpected EOF"` and recorded as status=500. It is a
// client abort: 400 in the access log, nothing at ERROR, offset bookkeeping unchanged.
func TestTusClientAbortIsNotAnInternalError(t *testing.T) {
	logs := &lockedBuf{}
	v := newEnvLog(t, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	home := v.home(v.adm)
	u, _ := url.Parse(v.e.URL)
	for _, tc := range []struct {
		name  string
		extra string
		reset bool
		kept  string
	}{
		{"checksummed chunk, closed", "Upload-Checksum: sha256 " + base64.StdEncoding.EncodeToString(make([]byte, 32)) + "\r\n", false, "0"},
		{"plain chunk, closed", "", false, "30000"},
		{"plain chunk, reset", "", true, "30000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := v.raw(v.adm, "POST", "/api/v1/uploads/", map[string]string{"Tus-Resumable": "1.0.0", "Upload-Length": "100000",
				"Upload-Metadata": tusMeta("filename", "abort-"+strings.ReplaceAll(tc.name, " ", "_")+".bin", "space_id", home)}, nil).expect(t, 201)
			loc := r.Header.Get("Location")
			conn, err := net.Dial("tcp", u.Host)
			if err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(conn, "PATCH %s HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nTus-Resumable: 1.0.0\r\n"+
				"Content-Type: application/offset+octet-stream\r\nUpload-Offset: 0\r\n%sContent-Length: 50000\r\n\r\n", loc, u.Host, v.adm.Bearer, tc.extra)
			conn.Write(make([]byte, 30000))
			time.Sleep(200 * time.Millisecond)
			if tc.reset {
				conn.(*net.TCPConn).SetLinger(0) // RST instead of FIN
			}
			conn.Close()
			id := path.Base(loc)
			eventually(t, "access log line for the aborted PATCH", func() bool {
				return strings.Contains(logs.String(), "method=PATCH path=/api/v1/uploads/"+id+" status=")
			})
			out := logs.String()
			if !strings.Contains(out, "method=PATCH path=/api/v1/uploads/"+id+" status=400") {
				t.Fatalf("aborted PATCH not logged as 400:\n%s", out)
			}
			if strings.Contains(out, "level=ERROR") {
				t.Fatalf("client abort logged at ERROR:\n%s", out)
			}
			if !strings.Contains(out, `msg="request body incomplete"`) {
				t.Fatalf("missing client-abort log line:\n%s", out)
			}
			eventually(t, "offset after abort", func() bool {
				return v.raw(v.adm, "HEAD", loc, map[string]string{"Tus-Resumable": "1.0.0"}, nil).expect(t, 200).Header.Get("Upload-Offset") == tc.kept
			})
		})
	}
}

func TestTusCookieCSRFAndQuota(t *testing.T) {
	v := newEnv(t)
	_, bob := v.user("carol", 100)
	home := v.home(bob)
	meta := tusMeta("filename", "q.bin", "space_id", home)
	v.raw(bob, "POST", "/api/v1/uploads/", map[string]string{"Tus-Resumable": "1.0.0", "Upload-Length": "101", "Upload-Metadata": meta}, nil).expect(t, 507)
	r := v.raw(bob, "POST", "/api/v1/uploads/", map[string]string{"Tus-Resumable": "1.0.0", "Upload-Length": "60", "Upload-Metadata": meta}, nil).expect(t, 201)
	// The pending 60 bytes are reserved against the quota.
	meta2 := tusMeta("filename", "q2.bin", "space_id", home)
	v.raw(bob, "POST", "/api/v1/uploads/", map[string]string{"Tus-Resumable": "1.0.0", "Upload-Length": "50", "Upload-Metadata": meta2}, nil).expect(t, 507)
	v.raw(bob, "DELETE", r.Header.Get("Location"), map[string]string{"Tus-Resumable": "1.0.0"}, nil).expect(t, 204)
	v.raw(bob, "POST", "/api/v1/uploads/", map[string]string{"Tus-Resumable": "1.0.0", "Upload-Length": "50", "Upload-Metadata": meta2}, nil).expect(t, 201)

	// Cookie sessions need the CSRF header on tus writes.
	c := v.e.NewClient()
	c.Login("carol", userPW).Expect(200)
	csrf := c.CSRF
	c.CSRF = ""
	v.raw(c, "POST", "/api/v1/uploads/", map[string]string{"Tus-Resumable": "1.0.0", "Upload-Length": "1", "Upload-Metadata": tusMeta("filename", "c.bin", "space_id", home)}, nil).expect(t, 403)
	c.CSRF = csrf
	r = v.raw(c, "POST", "/api/v1/uploads/", map[string]string{"Tus-Resumable": "1.0.0", "Upload-Length": "1", "Upload-Metadata": tusMeta("filename", "c.bin", "space_id", home)}, nil).expect(t, 201)
	v.raw(c, "PATCH", r.Header.Get("Location"), map[string]string{"Tus-Resumable": "1.0.0", "Content-Type": "application/offset+octet-stream", "Upload-Offset": "0"},
		bytes.NewReader([]byte("z"))).expect(t, 204)
}

func TestUploadExpiry(t *testing.T) {
	v := newEnv(t)
	home := v.home(v.adm)
	r := v.raw(v.adm, "POST", "/api/v1/uploads/", map[string]string{"Tus-Resumable": "1.0.0", "Upload-Length": "10",
		"Upload-Metadata": tusMeta("filename", "x.bin", "space_id", home)}, nil).expect(t, 201)
	part := filepath.Join(v.dataPath("tmp", "uploads"), filepath.Base(r.Header.Get("Location"))+".part")
	if !exists(part) {
		t.Fatal("part file missing")
	}
	v.svc.SetClock(func() time.Time { return time.Now().Add(100 * time.Hour) })
	v.raw(v.adm, "HEAD", r.Header.Get("Location"), map[string]string{"Tus-Resumable": "1.0.0"}, nil).expect(t, 410)
	r = v.raw(v.adm, "POST", "/api/v1/uploads/", map[string]string{"Tus-Resumable": "1.0.0", "Upload-Length": "10",
		"Upload-Metadata": tusMeta("filename", "y.bin", "space_id", home)}, nil).expect(t, 201)
	v.svc.SetClock(func() time.Time { return time.Now().Add(300 * time.Hour) })
	if err := v.svc.PurgeUploads(ctx()); err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir(v.dataPath("tmp", "uploads")); len(ents) != 0 {
		t.Fatalf("expired parts left: %d", len(ents))
	}
}

func TestMoveCopyRenameTrashRestore(t *testing.T) {
	v := newEnv(t)
	home := v.home(v.adm)
	var mu sync.Mutex
	var evs []files.Event
	for _, topic := range []string{events.FileCreated, events.FileUpdated, events.FileDeleted, events.FileMoved} {
		v.e.Deps.Events.Subscribe(topic, func(p any) { mu.Lock(); evs = append(evs, p.(files.Event)); mu.Unlock() })
	}
	a := v.mkdir(v.adm, home, "", "A")
	b := v.mkdir(v.adm, home, "", "B")
	f1 := v.upload(v.adm, home, "A", "one.txt", []byte("1"))
	v.mkdir(v.adm, home, "A", "sub")
	v.upload(v.adm, home, "A/sub", "deep.txt", []byte("deep"))
	v.upload(v.adm, home, "B", "one.txt", []byte("other"))

	// Rename.
	v.adm.Do("PATCH", "/api/v1/files/"+f1["id"].(string), map[string]any{"name": "sub"}).ExpectError(409, "conflict")
	rn := v.adm.Do("PATCH", "/api/v1/files/"+f1["id"].(string), map[string]any{"name": "uno.md"}).Expect(200).Map()
	if rn["path"] != "A/uno.md" || rn["mime"] != "text/markdown" {
		t.Fatalf("rename: %v", rn)
	}
	rn = v.adm.Do("PATCH", "/api/v1/files/"+f1["id"].(string), map[string]any{"name": "one.txt"}).Expect(200).Map()
	// Folder rename updates descendants.
	v.adm.Do("PATCH", "/api/v1/files/"+a["id"].(string), map[string]any{"name": "Alpha"}).Expect(200)
	if l := v.list(v.adm, home, "Alpha/sub"); names(l["items"])[0] != "deep.txt" {
		t.Fatalf("descendant paths not updated: %v", l)
	}
	v.adm.Do("GET", "/api/v1/files?path=A", nil).ExpectError(404, "not_found")

	// Move into itself is refused per item.
	mv := v.adm.Do("POST", "/api/v1/files/move", map[string]any{"ids": []string{a["id"].(string)}, "dest_space_id": home, "dest_path": "Alpha/sub"}).Expect(200).Map()
	if len(mv["errors"].([]any)) != 1 || len(mv["moved"].([]any)) != 0 {
		t.Fatalf("move into self: %v", mv)
	}
	// Conflict policies.
	mv = v.adm.Do("POST", "/api/v1/files/move", map[string]any{"ids": []string{f1["id"].(string)}, "dest_space_id": home, "dest_path": "B"}).Expect(200).Map()
	if e := mv["errors"].([]any); len(e) != 1 || e[0].(map[string]any)["code"] != "conflict" {
		t.Fatalf("fail policy: %v", mv)
	}
	mv = v.adm.Do("POST", "/api/v1/files/move", map[string]any{"ids": []string{f1["id"].(string)}, "dest_space_id": home, "dest_path": "B", "on_conflict": "rename"}).Expect(200).Map()
	if m := mv["moved"].([]any); len(m) != 1 || m[0].(map[string]any)["name"] != "one (1).txt" || m[0].(map[string]any)["id"] != f1["id"] {
		t.Fatalf("rename policy: %v", mv)
	}
	cp := v.adm.Do("POST", "/api/v1/files/copy", map[string]any{"ids": []string{f1["id"].(string)}, "dest_space_id": home, "dest_path": "B", "on_conflict": "overwrite"}).Expect(200).Map()
	if c := cp["copied"].([]any); len(c) != 1 || c[0].(map[string]any)["name"] != "one (1).txt" || c[0].(map[string]any)["id"] == f1["id"] {
		t.Fatalf("overwrite copy: %v", cp)
	}
	// The overwritten original went to the trash.
	if items := v.adm.Do("GET", "/api/v1/trash", nil).Expect(200).Map()["items"].([]any); len(items) != 1 {
		t.Fatalf("overwrite should trash the target: %v", items)
	}

	// Cross-space recursive copy and move into a library.
	lib := v.adm.Do("POST", "/api/v1/admin/spaces", map[string]any{"name": "Media", "members": []any{}}).Expect(201).Map()["id"].(string)
	cp = v.adm.Do("POST", "/api/v1/files/copy", map[string]any{"ids": []string{a["id"].(string)}, "dest_space_id": lib, "dest_path": ""}).Expect(200).Map()
	if len(cp["copied"].([]any)) != 1 {
		t.Fatalf("copy to lib: %v", cp)
	}
	if l := v.list(v.adm, lib, "Alpha/sub"); names(l["items"])[0] != "deep.txt" {
		t.Fatalf("copied tree: %v", l)
	}
	mv = v.adm.Do("POST", "/api/v1/files/move", map[string]any{"ids": []string{b["id"].(string)}, "dest_space_id": lib, "dest_path": "Alpha"}).Expect(200).Map()
	if m := mv["moved"].([]any); len(m) != 1 || m[0].(map[string]any)["space_id"] != lib || m[0].(map[string]any)["path"] != "Alpha/B" {
		t.Fatalf("cross-space move: %v", mv)
	}
	if !exists(v.dataPath("storage", "libraries", lib, "Alpha", "B", "one (1).txt")) || exists(v.dataPath("storage", "home", principal(t, v, v.adm).UserID, "B")) {
		t.Fatal("disk not moved")
	}

	// Trash a folder, restore, conflicts on restore.
	v.adm.Do("DELETE", "/api/v1/files/"+a["id"].(string), nil).Expect(204)
	v.adm.Do("GET", "/api/v1/files/"+a["id"].(string), nil).ExpectError(404, "not_found")
	if n := v.list(v.adm, home, "")["items"].([]any); len(n) != 0 {
		t.Fatalf("home not empty after trash: %v", names(n))
	}
	v.mkdir(v.adm, home, "", "Alpha")
	v.adm.Do("POST", "/api/v1/trash/"+a["id"].(string)+"/restore", nil).ExpectError(409, "conflict")
	rs := v.adm.Do("POST", "/api/v1/trash/"+a["id"].(string)+"/restore?rename=1", nil).Expect(200).Map()
	if rs["name"] != "Alpha (1)" {
		t.Fatalf("restore rename: %v", rs)
	}
	if l := v.list(v.adm, home, "Alpha (1)/sub"); names(l["items"])[0] != "deep.txt" {
		t.Fatalf("restored subtree: %v", l)
	}
	// Trash then delete forever, then empty.
	v.adm.Do("POST", "/api/v1/files/delete", map[string]any{"ids": []string{rs["id"].(string), "zzzzzzzzzz"}}).Expect(200)
	items := v.adm.Do("GET", "/api/v1/trash", nil).Expect(200).Map()["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("trash: %v", items)
	}
	v.adm.Do("DELETE", "/api/v1/trash/"+rs["id"].(string), nil).Expect(204)
	res := v.adm.Do("DELETE", "/api/v1/trash", nil).Expect(200).Map()
	if res["deleted"] != float64(1) || res["freed_bytes"] != float64(1) {
		t.Fatalf("empty trash: %v", res)
	}
	eventually(t, "trash dir emptied", func() bool {
		ents, _ := os.ReadDir(v.dataPath("storage", ".trash", home))
		return len(ents) == 0
	})
	// FTS rows follow the index.
	var nFiles, nFTS int
	v.e.Deps.DB.SQL.QueryRow(`SELECT count(*) FROM files`).Scan(&nFiles)
	v.e.Deps.DB.SQL.QueryRow(`SELECT count(*) FROM files_fts`).Scan(&nFTS)
	if nFiles != nFTS {
		t.Fatalf("files %d vs fts %d", nFiles, nFTS)
	}
	eventually(t, "events", func() bool {
		mu.Lock()
		defer mu.Unlock()
		kinds := map[string]bool{}
		for _, e := range evs {
			kinds[e.Kind] = true
		}
		return kinds[files.KindCreated] && kinds[files.KindRenamed] && kinds[files.KindMoved] && kinds[files.KindTrashed] &&
			kinds[files.KindRestored] && kinds[files.KindDeleted]
	})
}

func TestPermissionsAcrossUsers(t *testing.T) {
	v := newEnv(t)
	bobID, bob := v.user("bob", 0)
	_, carol := v.user("carol", 0)
	bobHome := v.home(bob)
	secret := v.upload(bob, bobHome, "", "secret.txt", []byte("s"))
	// Another user: 404 for space, entry, content, trash ops.
	carol.Do("GET", "/api/v1/files?space_id="+bobHome, nil).ExpectError(404, "not_found")
	carol.Do("GET", "/api/v1/files/"+secret["id"].(string), nil).ExpectError(404, "not_found")
	v.raw(carol, "GET", "/api/v1/files/"+secret["id"].(string)+"/content", nil, nil).expect(t, 404)
	carol.Do("DELETE", "/api/v1/files/"+secret["id"].(string), nil).ExpectError(404, "not_found")
	carol.Do("PATCH", "/api/v1/files/"+secret["id"].(string), map[string]any{"name": "x"}).ExpectError(404, "not_found")
	carol.Do("POST", "/api/v1/files/copy", map[string]any{"ids": []string{secret["id"].(string)}, "dest_space_id": v.home(carol), "dest_path": ""}).Expect(200)
	carol.Do("GET", "/api/v1/files/search?q=secret", nil).Expect(200)
	if items := carol.Do("GET", "/api/v1/files/search?q=secret", nil).Expect(200).Map()["items"].([]any); len(items) != 0 {
		t.Fatalf("search leaked: %v", items)
	}

	// Library with bob read-only and everyone read.
	lib := v.adm.Do("POST", "/api/v1/admin/spaces", map[string]any{"name": "Family", "members": []map[string]any{{"user_id": bobID, "access": "read"}}}).Expect(201).Map()
	libID := lib["id"].(string)
	if m := lib["members"].([]any); len(m) != 1 || m[0].(map[string]any)["username"] != "bob" {
		t.Fatalf("members: %v", lib)
	}
	v.mkdir(v.adm, libID, "", "Movies")
	movie := v.upload(v.adm, libID, "Movies", "film.mkv", []byte("m"))
	var spaces []map[string]any
	bob.Do("GET", "/api/v1/spaces", nil).Expect(200).JSON(&spaces)
	if len(spaces) != 2 || spaces[1]["access"] != "read" || spaces[1]["name"] != "Family" {
		t.Fatalf("bob spaces: %v", spaces)
	}
	bob.Do("GET", "/api/v1/files/"+movie["id"].(string), nil).Expect(200)
	bob.Do("POST", "/api/v1/files/folders", map[string]any{"space_id": libID, "path": "", "name": "x"}).ExpectError(403, "forbidden")
	bob.Do("DELETE", "/api/v1/files/"+movie["id"].(string), nil).ExpectError(403, "forbidden")
	v.raw(bob, "POST", "/api/v1/uploads/", map[string]string{"Tus-Resumable": "1.0.0", "Upload-Length": "1", "Upload-Metadata": tusMeta("filename", "x", "space_id", libID)}, nil).expect(t, 403)
	carol.Do("GET", "/api/v1/files/"+movie["id"].(string), nil).ExpectError(404, "not_found")
	// Bob may copy out of a readable library into his home.
	bob.Do("POST", "/api/v1/files/copy", map[string]any{"ids": []string{movie["id"].(string)}, "dest_space_id": bobHome, "dest_path": ""}).Expect(200)
	if n := names(v.list(bob, bobHome, "")["items"]); strings.Join(n, ",") != "film.mkv,secret.txt" {
		t.Fatalf("bob home: %v", n)
	}
	// Grant write to everyone.
	v.adm.Do("PATCH", "/api/v1/admin/spaces/"+libID, map[string]any{"members": []map[string]any{{"user_id": "*", "access": "write"}}}).Expect(200)
	carol.Do("POST", "/api/v1/files/folders", map[string]any{"space_id": libID, "path": "", "name": "Carol"}).Expect(201)
	// Carol's trash shows only what she trashed in the library.
	f := v.upload(carol, libID, "Carol", "c.txt", []byte("c"))
	carol.Do("DELETE", "/api/v1/files/"+f["id"].(string), nil).Expect(204)
	v.adm.Do("DELETE", "/api/v1/files/"+movie["id"].(string), nil).Expect(204)
	if items := carol.Do("GET", "/api/v1/trash", nil).Expect(200).Map()["items"].([]any); len(items) != 1 {
		t.Fatalf("carol trash: %v", items)
	}
	carol.Do("POST", "/api/v1/trash/"+movie["id"].(string)+"/restore", nil).ExpectError(404, "not_found")
	// Read-only token cannot write.
	tok := v.adm.Do("POST", "/api/v1/me/tokens", map[string]any{"name": "ro", "scopes": []string{"files:read"}}).Expect(201).Map()["token"].(string)
	ro := v.e.WithBearer(tok)
	ro.Do("GET", "/api/v1/spaces", nil).Expect(200)
	ro.Do("POST", "/api/v1/files/folders", map[string]any{"space_id": libID, "path": "", "name": "y"}).ExpectError(403, "forbidden")
	// Admin endpoints.
	bob.Do("GET", "/api/v1/admin/spaces", nil).ExpectError(403, "forbidden")
	v.adm.Do("DELETE", "/api/v1/admin/spaces/"+libID, nil).ExpectError(409, "conflict")
	v.adm.Do("POST", "/api/v1/admin/spaces", map[string]any{"name": "family", "members": []any{}}).ExpectError(409, "conflict")
	v.adm.Do("POST", "/api/v1/admin/spaces", map[string]any{"name": "X", "members": []map[string]any{{"user_id": "nosuchuser", "access": "read"}}}).ExpectError(422, "validation_failed")
	v.adm.Do("DELETE", "/api/v1/admin/spaces/"+libID+"?force=true", nil).Expect(204)
	bob.Do("GET", "/api/v1/files/"+movie["id"].(string), nil).ExpectError(404, "not_found")
	eventually(t, "library dir removed", func() bool { return !exists(v.dataPath("storage", "libraries", libID)) })
	// Service helpers.
	pb := principal(t, v, bob)
	if v.svc.CanRead(ctx(), pb, libID) || !v.svc.CanWrite(ctx(), pb, bobHome) {
		t.Fatal("CanRead/CanWrite")
	}
	ids, _ := v.svc.ReadableSpaceIDs(ctx(), pb)
	if len(ids) != 1 || ids[0] != bobHome {
		t.Fatalf("readable: %v", ids)
	}
}

func TestSearchFiltersAndMeta(t *testing.T) {
	v := newEnv(t)
	home := v.home(v.adm)
	v.mkdir(v.adm, home, "", "Holiday 2024")
	beach := v.upload(v.adm, home, "Holiday 2024", "beach.jpg", []byte("\xff\xd8\xff\xe0 not really"))
	v.upload(v.adm, home, "", "notes.txt", []byte("n"))
	v.adm.Do("PATCH", "/api/v1/files/"+beach["id"].(string), map[string]any{"category": "image", "subcategory": "photo", "tags": []string{"Sea", "summer"}}).Expect(200)
	v.adm.Do("PATCH", "/api/v1/files/"+beach["id"].(string), map[string]any{"category": "nope"}).ExpectError(422, "validation_failed")
	v.adm.Do("PATCH", "/api/v1/files/"+beach["id"].(string), map[string]any{"subcategory": "episode"}).ExpectError(422, "validation_failed")
	search := func(q string) []string {
		return names(v.adm.Do("GET", "/api/v1/files/search?"+q, nil).Expect(200).Map()["items"])
	}
	if got := search("q=beach"); len(got) != 1 || got[0] != "beach.jpg" {
		t.Fatalf("q=beach: %v", got)
	}
	if got := search("q=holiday"); len(got) != 2 || got[0] != "Holiday 2024" {
		t.Fatalf("q=holiday (name match first): %v", got)
	}
	if got := search("q=hol&type=file"); len(got) != 1 {
		t.Fatalf("prefix + type: %v", got)
	}
	if got := search("tag=sea"); len(got) != 1 {
		t.Fatalf("tag: %v", got)
	}
	if got := search("category=image&subcategory=photo&mime_prefix=image%2F"); len(got) != 1 {
		t.Fatalf("category: %v", got)
	}
	if got := search("q=%22%29%28*+OR"); len(got) != 0 {
		t.Fatalf("hostile query: %v", got)
	}
	if got := search("type=dir"); len(got) != 1 {
		t.Fatalf("dirs: %v", got)
	}
	// Bulk meta.
	n := v.list(v.adm, home, "")["items"].([]any)
	var ids []string
	for _, it := range n {
		ids = append(ids, it.(map[string]any)["id"].(string))
	}
	r := v.adm.Do("POST", "/api/v1/files/bulk-meta", map[string]any{"ids": ids, "add_tags": []string{"x"}, "category": "document"}).Expect(200).Map()
	if r["updated"] != float64(2) {
		t.Fatalf("bulk: %v", r)
	}
	r = v.adm.Do("POST", "/api/v1/files/bulk-meta", map[string]any{"ids": []string{beach["id"].(string)}, "remove_tags": []string{"SEA"}}).Expect(200).Map()
	if e := v.adm.Do("GET", "/api/v1/files/"+beach["id"].(string), nil).Expect(200).Map(); fmt.Sprint(e["tags"]) != "[summer]" {
		t.Fatalf("remove tags: %v", e["tags"])
	}
	// Paging search.
	for i := 0; i < 5; i++ {
		v.upload(v.adm, home, "", fmt.Sprintf("page%d.txt", i), []byte("p"))
	}
	p1 := v.adm.Do("GET", "/api/v1/files/search?q=page&limit=3", nil).Expect(200).Map()
	cur := p1["next_cursor"].(string)
	p2 := v.adm.Do("GET", "/api/v1/files/search?q=page&limit=3&cursor="+url.QueryEscape(cur), nil).Expect(200).Map()
	if len(p2["items"].([]any)) != 2 || p2["next_cursor"] != nil {
		t.Fatalf("page 2: %v", p2)
	}
	v.adm.Do("GET", "/api/v1/files/search?q=other&cursor="+url.QueryEscape(cur), nil).ExpectError(400, "bad_request")
}

func TestTreeZipAndThumbnails(t *testing.T) {
	v := newEnv(t)
	home := v.home(v.adm)
	dir := v.mkdir(v.adm, home, "", "Pics")
	img := image.NewRGBA(image.Rect(0, 0, 800, 600))
	for y := 0; y < 600; y++ {
		for x := 0; x < 800; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	var pngBuf, jpgBuf bytes.Buffer
	png.Encode(&pngBuf, img)
	jpeg.Encode(&jpgBuf, img, &jpeg.Options{Quality: 80})
	p := v.upload(v.adm, home, "Pics", "a.png", pngBuf.Bytes())
	j := v.upload(v.adm, home, "Pics/raw", "b.jpg", jpgBuf.Bytes())
	txt := v.upload(v.adm, home, "Pics", "readme.txt", []byte("hi"))
	if p["has_thumbnail"] != true || j["has_thumbnail"] != true || txt["has_thumbnail"] != false {
		t.Fatalf("has_thumbnail: %v %v %v", p["has_thumbnail"], j["has_thumbnail"], txt["has_thumbnail"])
	}
	for _, sz := range []string{"sm", "md", "lg"} {
		r := v.raw(v.adm, "GET", "/api/v1/files/"+p["id"].(string)+"/thumbnail?size="+sz, nil, nil).expect(t, 200)
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(r.Body))
		want := map[string]int{"sm": 160, "md": 320, "lg": 640}[sz]
		if err != nil || cfg.Width != want {
			t.Fatalf("thumb %s: %v %d", sz, err, cfg.Width)
		}
		v.raw(v.adm, "GET", "/api/v1/files/"+p["id"].(string)+"/thumbnail?size="+sz, map[string]string{"If-None-Match": r.Header.Get("ETag")}, nil).expect(t, 304)
	}
	v.raw(v.adm, "GET", "/api/v1/files/"+j["id"].(string)+"/thumbnail", nil, nil).expect(t, 200)
	v.raw(v.adm, "GET", "/api/v1/files/"+txt["id"].(string)+"/thumbnail", nil, nil).expect(t, 404)
	v.raw(v.adm, "GET", "/api/v1/files/"+p["id"].(string)+"/thumbnail?size=xl", nil, nil).expect(t, 422)
	// A corrupt image is marked as having no thumbnail.
	bad := v.upload(v.adm, home, "Pics", "bad.png", []byte("\x89PNG\r\n\x1a\nbroken"))
	v.raw(v.adm, "GET", "/api/v1/files/"+bad["id"].(string)+"/thumbnail", nil, nil).expect(t, 404)
	if e := v.adm.Do("GET", "/api/v1/files/"+bad["id"].(string), nil).Expect(200).Map(); e["has_thumbnail"] != false {
		t.Fatalf("failed thumb still advertised: %v", e)
	}
	// Video without ffmpeg: no thumbnail.
	vid := v.upload(v.adm, home, "Pics", "clip.mp4", []byte("not a video"))
	if vid["has_thumbnail"] != false {
		t.Fatal("video thumb without ffmpeg")
	}

	// Tree.
	tr := v.adm.Do("GET", "/api/v1/files/tree?space_id="+home+"&path=Pics", nil).Expect(200).Map()
	var paths []string
	for _, it := range tr["items"].([]any) {
		paths = append(paths, it.(map[string]any)["path"].(string))
	}
	sort.Strings(paths)
	if strings.Join(paths, ",") != "Pics/a.png,Pics/bad.png,Pics/clip.mp4,Pics/raw,Pics/raw/b.jpg,Pics/readme.txt" {
		t.Fatalf("tree: %v", paths)
	}

	// ZIP round trip.
	z := v.raw(v.adm, "GET", "/api/v1/files/"+dir["id"].(string)+"/zip", nil, nil).expect(t, 200)
	if z.Header.Get("Content-Type") != "application/zip" || !strings.Contains(z.Header.Get("Content-Disposition"), "Pics.zip") {
		t.Fatalf("zip headers: %v", z.Header)
	}
	zr, err := zip.NewReader(bytes.NewReader(z.Body), int64(len(z.Body)))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		got[f.Name] = fmt.Sprintf("%d/%d", len(b), f.Method)
	}
	if got["Pics/readme.txt"] != "2/8" || got["Pics/raw/b.jpg"] != fmt.Sprintf("%d/0", jpgBuf.Len()) || got["Pics/raw/"] == "" {
		t.Fatalf("zip entries: %v", got)
	}
	v.raw(v.adm, "GET", "/api/v1/files/"+p["id"].(string)+"/zip", nil, nil).expect(t, 400)
}

func TestSHA256AndRescan(t *testing.T) {
	v := newEnv(t)
	home := v.home(v.adm)
	data := []byte("hash me please")
	e := v.upload(v.adm, home, "", "h.txt", data)
	want := sha256.Sum256(data)
	eventually(t, "sha256", func() bool {
		return v.adm.Do("GET", "/api/v1/files/"+e["id"].(string), nil).Expect(200).Map()["sha256"] == hex.EncodeToString(want[:])
	})
	// Files added and removed behind the server's back.
	uid := principal(t, v, v.adm).UserID
	root := v.dataPath("storage", "home", uid)
	os.MkdirAll(filepath.Join(root, "outside", "nested"), 0o700)
	os.WriteFile(filepath.Join(root, "outside", "nested", "new.txt"), []byte("new"), 0o600)
	os.WriteFile(filepath.Join(root, "bad\x01name"), []byte("x"), 0o600)
	os.Symlink("/etc/passwd", filepath.Join(root, "link"))
	os.Remove(filepath.Join(root, "h.txt"))
	v.adm.Do("POST", "/api/v1/files/rescan", map[string]any{"space_id": home}).Expect(202)
	eventually(t, "rescan", func() bool {
		l := v.list(v.adm, home, "")
		return strings.Join(names(l["items"]), ",") == "outside"
	})
	l := v.list(v.adm, home, "outside/nested")
	if n := names(l["items"]); len(n) != 1 || n[0] != "new.txt" {
		t.Fatalf("rescanned: %v", n)
	}
	v.adm.Do("POST", "/api/v1/files/rescan", map[string]any{"space_id": home, "path": "nope"}).ExpectError(404, "not_found")
	if err := v.svc.HashNow(ctx()); err != nil {
		t.Fatal(err)
	}
	if e := l["items"].([]any)[0].(map[string]any); v.adm.Do("GET", "/api/v1/files/"+e["id"].(string), nil).Expect(200).Map()["sha256"] == "" {
		t.Fatal("rescanned file not hashed")
	}
}

func TestTrashRetention(t *testing.T) {
	v := newEnv(t)
	home := v.home(v.adm)
	e := v.upload(v.adm, home, "", "old.txt", []byte("o"))
	v.adm.Do("DELETE", "/api/v1/files/"+e["id"].(string), nil).Expect(204)
	if err := v.svc.PurgeExpiredTrash(ctx()); err != nil {
		t.Fatal(err)
	}
	if len(v.adm.Do("GET", "/api/v1/trash", nil).Expect(200).Map()["items"].([]any)) != 1 {
		t.Fatal("fresh trash purged")
	}
	v.svc.SetClock(func() time.Time { return time.Now().Add(31 * 24 * time.Hour) })
	if err := v.svc.PurgeExpiredTrash(ctx()); err != nil {
		t.Fatal(err)
	}
	if len(v.adm.Do("GET", "/api/v1/trash", nil).Expect(200).Map()["items"].([]any)) != 0 {
		t.Fatal("expired trash kept")
	}
}

func TestUserDeletionKeepsOrRemovesFiles(t *testing.T) {
	v := newEnv(t)
	bobID, bob := v.user("bob", 0)
	home := v.home(bob)
	f := v.upload(bob, home, "", "keep.txt", []byte("k"))
	t2 := v.upload(bob, home, "", "trashed.txt", []byte("t"))
	bob.Do("DELETE", "/api/v1/files/"+t2["id"].(string), nil).Expect(204)
	v.admin.Do("DELETE", "/api/v1/admin/users/"+bobID+"?delete_files=false", nil).Expect(204)
	eventually(t, "orphaned move", func() bool { return exists(v.dataPath("storage", "orphaned", bobID, "keep.txt")) })
	if !exists(v.dataPath("storage", "orphaned", bobID+"-trash")) || exists(v.dataPath("storage", "home", bobID)) {
		t.Fatal("trash not kept / home not moved")
	}
	var n int
	v.e.Deps.DB.SQL.QueryRow(`SELECT count(*) FROM files WHERE id = ?`, f["id"]).Scan(&n)
	if n != 0 {
		t.Fatal("index rows of deleted user remain")
	}
	var fts, total int
	v.e.Deps.DB.SQL.QueryRow(`SELECT count(*) FROM files_fts`).Scan(&fts)
	v.e.Deps.DB.SQL.QueryRow(`SELECT count(*) FROM files`).Scan(&total)
	if fts != total {
		t.Fatalf("fts out of sync after cascade: %d vs %d", fts, total)
	}

	carolID, carol := v.user("carol", 0)
	ch := v.home(carol)
	v.upload(carol, ch, "", "gone.txt", []byte("g"))
	v.admin.Do("DELETE", "/api/v1/admin/users/"+carolID+"?delete_files=true", nil).Expect(204)
	eventually(t, "files removed", func() bool {
		return !exists(v.dataPath("storage", "home", carolID)) && !exists(v.dataPath("storage", "orphaned", carolID))
	})
}

func TestCategoriesAndBackupDevices(t *testing.T) {
	v := newEnv(t)
	var cats []map[string]any
	v.adm.Do("GET", "/api/v1/categories", nil).Expect(200).JSON(&cats)
	if len(cats) != 8 || cats[0]["slug"] != "movie" || cats[0]["icon"] != "film" {
		t.Fatalf("default taxonomy: %v", cats)
	}
	_, bob := v.user("bob", 0)
	bob.Do("PUT", "/api/v1/admin/categories", []any{}).ExpectError(403, "forbidden")
	v.adm.Do("PUT", "/api/v1/admin/categories", []map[string]any{{"slug": "BAD", "name": "x", "icon": "", "subcategories": []any{}}}).ExpectError(422, "validation_failed")
	v.adm.Do("PUT", "/api/v1/admin/categories", []map[string]any{{"slug": "comics", "name": "Comics", "icon": "book", "subcategories": []map[string]any{{"slug": "manga", "name": "Manga"}}}}).Expect(200)
	bob.Do("GET", "/api/v1/categories", nil).Expect(200).JSON(&cats)
	if len(cats) != 1 || cats[0]["slug"] != "comics" {
		t.Fatalf("replaced taxonomy: %v", cats)
	}

	home := v.home(bob)
	d := bob.Do("POST", "/api/v1/backup/devices", map[string]any{"name": "Laptop", "platform": "Windows"}).Expect(201).Map()
	id := d["id"].(string)
	if d["platform"] != "windows" || d["last_status"] != nil {
		t.Fatalf("device: %v", d)
	}
	bob.Do("PUT", "/api/v1/backup/devices/"+id, map[string]any{"folders": []map[string]any{{"local_path": `C:\Users\bob\Pictures`, "remote_space_id": home, "remote_path": "Backups/Pictures"}}}).Expect(200)
	v.adm.Do("PUT", "/api/v1/backup/devices/"+id, map[string]any{"name": "x"}).ExpectError(404, "not_found")
	bob.Do("PUT", "/api/v1/backup/devices/"+id, map[string]any{"folders": []map[string]any{{"local_path": "/x", "remote_space_id": v.home(v.adm), "remote_path": ""}}}).ExpectError(422, "validation_failed")
	bob.Do("POST", "/api/v1/backup/devices/"+id+"/report", map[string]any{"status": "ok", "bytes_uploaded": 10, "files_uploaded": 2,
		"started_at": "2026-01-01T00:00:00Z", "finished_at": "2026-01-01T00:05:00Z"}).Expect(204)
	bob.Do("POST", "/api/v1/backup/devices/"+id+"/report", map[string]any{"status": "bogus", "started_at": "2026-01-01T00:00:00Z"}).ExpectError(422, "validation_failed")
	var devs []map[string]any
	bob.Do("GET", "/api/v1/backup/devices", nil).Expect(200).JSON(&devs)
	if len(devs) != 1 || devs[0]["last_status"] != "ok" || devs[0]["last_sync_at"] != "2026-01-01T00:05:00.000Z" || devs[0]["files_uploaded"] != float64(2) ||
		len(devs[0]["folders"].([]any)) != 1 {
		t.Fatalf("devices: %v", devs)
	}
	bob.Do("DELETE", "/api/v1/backup/devices/"+id, nil).Expect(204)
	bob.Do("DELETE", "/api/v1/backup/devices/"+id, nil).ExpectError(404, "not_found")
}

func TestServiceContract(t *testing.T) {
	v := newEnv(t)
	_, bob := v.user("bob", 0)
	pb := principal(t, v, bob)
	home, err := v.svc.HomeSpaceID(ctx(), pb.UserID)
	if err != nil || home != v.home(bob) {
		t.Fatalf("HomeSpaceID %q %v", home, err)
	}
	if _, err := v.svc.HomeSpaceID(ctx(), "doesnotexist1"); err == nil {
		t.Fatal("home for unknown user")
	}
	d, err := v.svc.EnsureDir(ctx(), pb, home, "Mail Attachments/2026")
	if err != nil || d.Path != "Mail Attachments/2026" || !d.IsDir {
		t.Fatalf("EnsureDir %v %v", d, err)
	}
	if _, err := v.svc.EnsureDir(ctx(), pb, home, "Mail Attachments/2026"); err != nil {
		t.Fatal("EnsureDir not idempotent")
	}
	e, err := v.svc.CreateFromReader(ctx(), pb, home, "Mail Attachments/2026", "a.pdf", strings.NewReader("%PDF-1.4"), files.CreateOpts{Category: "document", Subcategory: "pdf", Tags: []string{"mail"}})
	if err != nil || e.Mime != "application/pdf" || e.Category != "document" || e.OwnerID != pb.UserID {
		t.Fatalf("CreateFromReader %v %v", e, err)
	}
	if _, err := v.svc.CreateFromReader(ctx(), pb, home, "Mail Attachments/2026", "a.pdf", strings.NewReader("x"), files.CreateOpts{}); err == nil {
		t.Fatal("duplicate without overwrite")
	}
	tmp, _ := os.CreateTemp(v.svc.TempDir(), "x-*")
	tmp.WriteString("committed")
	tmp.Close()
	c, err := v.svc.CommitFile(ctx(), pb, home, "", "c.txt", tmp.Name(), files.CreateOpts{ModTime: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil || c.Size != 9 || !c.ModTime.Equal(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)) || exists(tmp.Name()) {
		t.Fatalf("CommitFile %v %v", c, err)
	}
	if _, err := v.svc.CommitFile(ctx(), pb, home, "", "x.txt", "/etc/passwd", files.CreateOpts{}); err == nil {
		t.Fatal("CommitFile accepted a path outside TempDir")
	}
	got, err := v.svc.SetMeta(ctx(), pb, c.ID, strp("music"), nil, &[]string{"t"})
	if err != nil || got.Category != "music" || got.Tags[0] != "t" {
		t.Fatalf("SetMeta %v %v", got, err)
	}
	g, err := v.svc.Get(ctx(), pb, c.ID)
	if err != nil || g.Category != "music" {
		t.Fatalf("Get %v %v", g, err)
	}
	_, carol := v.user("carol", 0)
	if _, err := v.svc.Get(ctx(), principal(t, v, carol), c.ID); err == nil {
		t.Fatal("Get leaked across users")
	}
	sub, _ := v.svc.SubtreeIDs(ctx(), d.ID)
	if len(sub) != 1 || sub[0] != e.ID {
		t.Fatalf("SubtreeIDs %v", sub)
	}
}

func strp(s string) *string { return &s }
