package kanban

import (
	"bytes"
	"fmt"
	"image/jpeg"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ludworks/internal/core"
	"ludworks/internal/db"
)

func TestAttachmentsContentHeadersAndThumbnails(t *testing.T) {
	v := newEnv(t)
	c := v.admin
	b := v.board(c, "Files")
	card := v.card(c, b.Columns[0].ID, "Design")
	img := pngBytes(t, 640, 400)

	var pic Attachment
	v.upload(c, card.ID, "C:\\Users\\me\\mock up.png", img).expect(t, 201).decode(t, &pic)
	if pic.Filename != "mock up.png" || pic.Mime != "image/png" || !pic.IsImage || pic.Size != int64(len(img)) ||
		pic.URL != "/api/v1/kanban/attachments/"+pic.ID || pic.ThumbnailURL == nil {
		t.Fatalf("attachment %+v", pic)
	}
	// Inline image with its own type, sandboxed.
	r := v.raw(c, "GET", pic.URL, nil, nil).expect(t, 200)
	h := r.Header
	if h.Get("Content-Type") != "image/png" || !strings.HasPrefix(h.Get("Content-Disposition"), "inline") ||
		h.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(h.Get("Content-Security-Policy"), "sandbox") ||
		h.Get("ETag") == "" || !bytes.Equal(r.Body, img) {
		t.Fatalf("inline headers %v", h)
	}
	// download=1 is always an octet-stream attachment.
	r = v.raw(c, "GET", pic.URL+"?download=1", nil, nil).expect(t, 200)
	if r.Header.Get("Content-Type") != "application/octet-stream" || !strings.HasPrefix(r.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("download headers %v", r.Header)
	}
	// Range and conditional requests.
	r = v.raw(c, "GET", pic.URL, map[string]string{"Range": "bytes=0-9"}, nil).expect(t, 206)
	if len(r.Body) != 10 {
		t.Fatalf("range body %d bytes", len(r.Body))
	}
	v.raw(c, "GET", pic.URL, map[string]string{"If-None-Match": h.Get("ETag")}, nil).expect(t, 304)

	// Active content is never served with its type, and never inline.
	for name, data := range map[string]string{
		"page.html": "<html><script>alert(1)</script></html>",
		"logo.svg":  `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`,
		"notes.txt": "plain text",
	} {
		var a Attachment
		v.upload(c, card.ID, name, []byte(data)).expect(t, 201).decode(t, &a)
		if a.IsImage || a.ThumbnailURL != nil {
			t.Errorf("%s: %+v", name, a)
		}
		r := v.raw(c, "GET", a.URL, nil, nil).expect(t, 200)
		if r.Header.Get("Content-Type") != "application/octet-stream" || !strings.HasPrefix(r.Header.Get("Content-Disposition"), "attachment") ||
			!strings.Contains(r.Header.Get("Content-Security-Policy"), "sandbox") {
			t.Errorf("%s: served as %v", name, r.Header)
		}
		if name == "notes.txt" {
			v.raw(c, "GET", a.URL+"/thumbnail", nil, nil).expect(t, 404)
		}
	}

	// Thumbnails.
	for size, edge := range map[string]int{"sm": 160, "md": 320, "": 320} {
		r := v.raw(c, "GET", pic.URL+"/thumbnail?size="+size, nil, nil).expect(t, 200)
		if r.Header.Get("Content-Type") != "image/jpeg" {
			t.Fatalf("thumbnail type %q", r.Header.Get("Content-Type"))
		}
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(r.Body))
		if err != nil || cfg.Width != edge || cfg.Height != edge*400/640 {
			t.Fatalf("thumbnail %s: %+v %v", size, cfg, err)
		}
	}
	v.raw(c, "GET", pic.URL+"/thumbnail?size=lg", nil, nil).expect(t, 422)
	if _, err := os.Stat(filepath.Join(v.m.thumbDir, pic.ID+"-sm.jpg")); err != nil {
		t.Fatalf("thumbnail not cached: %v", err)
	}
	// A broken image gets no thumbnail and is no longer offered one.
	var broken Attachment
	v.upload(c, card.ID, "broken.png", append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...)).expect(t, 201).decode(t, &broken)
	v.raw(c, "GET", broken.URL+"/thumbnail", nil, nil).expect(t, 404)
	var d CardDetail
	c.Do("GET", "/api/v1/kanban/cards/"+card.ID, nil).Expect(200).JSON(&d)
	for _, a := range d.Attachments {
		if a.ID == broken.ID && a.ThumbnailURL != nil {
			t.Fatal("broken image still offers a thumbnail")
		}
	}
	if d.AttachmentCount != 5 || len(d.Attachments) != 5 || d.Attachments[0].ID != pic.ID {
		t.Fatalf("detail attachments %d/%d", d.AttachmentCount, len(d.Attachments))
	}

	// Rename.
	var renamed Attachment
	c.Do("PATCH", pic.URL, map[string]any{"filename": "final.png"}).Expect(200).JSON(&renamed)
	if renamed.Filename != "final.png" {
		t.Fatalf("renamed %+v", renamed)
	}
	if cd := v.raw(c, "GET", pic.URL, nil, nil).Header.Get("Content-Disposition"); !strings.Contains(cd, "final.png") {
		t.Fatalf("disposition after rename %q", cd)
	}
	c.Do("PATCH", pic.URL, map[string]any{"filename": "a\x01b"}).ExpectError(422, "validation_failed")

	// Covers: only image attachments of the card; deleting the attachment clears the cover.
	var withCover Card
	c.Do("PATCH", "/api/v1/kanban/cards/"+card.ID, map[string]any{"cover_attachment_id": pic.ID}).Expect(200).JSON(&withCover)
	if withCover.CoverAttachmentID == nil || *withCover.CoverAttachmentID != pic.ID {
		t.Fatalf("cover %+v", withCover)
	}
	for _, a := range d.Attachments {
		if a.Filename == "notes.txt" {
			c.Do("PATCH", "/api/v1/kanban/cards/"+card.ID, map[string]any{"cover_attachment_id": a.ID}).ExpectError(422, "validation_failed")
		}
	}
	file := filepath.Join(v.m.attachDir, pic.ID)
	if _, err := os.Stat(file); err != nil {
		t.Fatal(err)
	}
	c.Do("DELETE", pic.URL, nil).Expect(http.StatusNoContent)
	var after Card
	c.Do("GET", "/api/v1/kanban/cards/"+card.ID, nil).Expect(200).JSON(&after)
	if after.CoverAttachmentID != nil || after.Version != withCover.Version+1 || after.AttachmentCount != 4 {
		t.Fatalf("after delete %+v", after)
	}
	eventually(t, "attachment file removal", func() bool {
		_, err1 := os.Stat(file)
		_, err2 := os.Stat(filepath.Join(v.m.thumbDir, pic.ID+"-sm.jpg"))
		return os.IsNotExist(err1) && os.IsNotExist(err2)
	})
	v.raw(c, "GET", pic.URL, nil, nil).expect(t, 404)

	// Query tokens work on the content route.
	tok := v.token(c, "kanban:read")
	var other Attachment
	v.upload(c, card.ID, "q.txt", []byte("via query token")).expect(t, 201).decode(t, &other)
	anon := v.e.NewClient()
	v.raw(anon, "GET", other.URL+"?access_token="+url.QueryEscape(tok.Bearer), nil, nil).expect(t, 200)
	v.raw(anon, "GET", other.URL, nil, nil).expect(t, 401)
}

func TestAttachmentLimitsAndPermissions(t *testing.T) {
	v := newEnv(t)
	owner := v.admin
	edID, editor := v.user("editor")
	viewerID, viewer := v.user("viewer")
	b := v.board(owner, "Limits")
	v.members(owner, b.ID, map[string]string{edID: "editor", viewerID: "viewer"})
	card := v.card(owner, b.Columns[0].ID, "Card")

	v.upload(owner, card.ID, "", []byte("x")).expect(t, 422)
	v.upload(owner, card.ID, "empty.txt", []byte{}).expect(t, 400)
	if r := v.upload(viewer, card.ID, "v.txt", []byte("viewer")); r.Status != 403 {
		t.Fatalf("viewer upload: %d", r.Status)
	}
	// The uploader may delete their attachment even after losing the editor role.
	var mine Attachment
	v.upload(editor, card.ID, "mine.txt", []byte("mine")).expect(t, 201).decode(t, &mine)
	var someone Attachment
	v.upload(owner, card.ID, "owner.txt", []byte("owner")).expect(t, 201).decode(t, &someone)
	v.members(owner, b.ID, map[string]string{edID: "viewer", viewerID: "viewer"})
	editor.Do("DELETE", someone.URL, nil).ExpectError(403, "forbidden")
	editor.Do("PATCH", mine.URL, map[string]any{"filename": "x.txt"}).ExpectError(403, "forbidden")
	editor.Do("DELETE", mine.URL, nil).Expect(http.StatusNoContent)

	// 50 MiB per attachment: a declared length over the limit is refused before reading (the
	// server may close the connection while the client still sends, so a transport error is
	// accepted there), a streamed body when it crosses the limit.
	send := func(name string, body io.Reader, length int64) (int, error) {
		req, _ := http.NewRequest("POST", v.e.URL+"/api/v1/kanban/cards/"+card.ID+"/attachments?filename="+name, body)
		req.Header.Set("X-CSRF-Token", owner.CSRF)
		req.ContentLength = length
		resp, err := owner.HTTP.Do(req)
		if err != nil {
			return 0, err
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, nil
	}
	if st, err := send("big.bin", io.LimitReader(onesReader{}, MaxAttachmentBytes+1), MaxAttachmentBytes+1); err == nil && st != http.StatusRequestEntityTooLarge {
		t.Fatalf("declared big upload: %d", st)
	}
	if st, err := send("stream.bin", io.LimitReader(onesReader{}, MaxAttachmentBytes+1), -1); err != nil || st != http.StatusRequestEntityTooLarge {
		t.Fatalf("streamed big upload: %d %v", st, err)
	}
	if st, err := send("exact.bin", io.LimitReader(onesReader{}, MaxAttachmentBytes), -1); err != nil || st != http.StatusCreated {
		t.Fatalf("upload of exactly 50 MiB: %d %v", st, err)
	}

	// 2 GiB per board (507 quota_exceeded) and 100 per card (409).
	ts := db.FormatTime(core.Now())
	sqlDB := v.e.Deps.DB.SQL
	if _, err := sqlDB.Exec(`INSERT INTO kanban_attachments (id, card_id, board_id, filename, mime, size, uploaded_by, created_at)
		VALUES (?, ?, ?, 'huge.bin', 'application/octet-stream', ?, ?, ?)`, core.NewID(), card.ID, b.ID, MaxBoardAttachBytes-10, b.OwnerID, ts); err != nil {
		t.Fatal(err)
	}
	if r := v.upload(owner, card.ID, "small.txt", []byte("more than ten bytes")); r.Status != 507 || r.code() != "quota_exceeded" {
		t.Fatalf("board cap: %d %s", r.Status, r.Body)
	}
	other := v.card(owner, b.Columns[0].ID, "Other")
	sqlDB.Exec(`DELETE FROM kanban_attachments WHERE filename = 'huge.bin'`)
	for i := 0; i < MaxAttachments; i++ {
		sqlDB.Exec(`INSERT INTO kanban_attachments (id, card_id, board_id, filename, mime, size, uploaded_by, created_at)
			VALUES (?, ?, ?, ?, 'text/plain', 1, ?, ?)`, core.NewID(), other.ID, b.ID, fmt.Sprint("f", i), b.OwnerID, ts)
	}
	if r := v.upload(owner, other.ID, "one-more.txt", []byte("x")); r.Status != 409 {
		t.Fatalf("per-card limit: %d %s", r.Status, r.Body)
	}

	// Free space: 1 GiB stays free (507 insufficient_storage).
	v.m.freeSpace = func(string) (int64, error) { return freeHeadroom + 10, nil }
	if r := v.upload(owner, card.ID, "space.txt", []byte("needs more than ten bytes")); r.Status != 507 || r.code() != "insufficient_storage" {
		t.Fatalf("free space: %d %s", r.Status, r.Body)
	}
	v.m.freeSpace = diskFree
	v.upload(owner, card.ID, "fits.txt", []byte("fits")).expect(t, 201)
	// Nothing half-written stays behind.
	entries, _ := os.ReadDir(v.m.attachDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Fatalf("temp file left: %s", e.Name())
		}
	}
}

type onesReader struct{}

func (onesReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 1
	}
	return len(p), nil
}

func TestAttachFromFilesAndSaveToFiles(t *testing.T) {
	v := newEnv(t)
	owner := v.admin
	aliceID, alice := v.user("alice")
	bobID, bob := v.user("bob")
	carolID, carol := v.user("carol")
	b := v.board(owner, "Bridge")
	v.members(owner, b.ID, map[string]string{aliceID: "editor", bobID: "viewer", carolID: "editor"})
	card := v.card(owner, b.Columns[0].ID, "Card")

	// alice uploads a file to her home space.
	img := pngBytes(t, 32, 32)
	up := v.raw(alice, "POST", "/api/v1/files/upload?name=diagram.png", map[string]string{"Content-Type": "application/octet-stream"}, img).expect(t, 201)
	var entry struct {
		ID      string `json:"id"`
		SpaceID string `json:"space_id"`
	}
	up.decode(t, &entry)

	var att Attachment
	alice.Do("POST", "/api/v1/kanban/cards/"+card.ID+"/attachments/from-file", map[string]any{"file_id": entry.ID}).Expect(201).JSON(&att)
	if att.Filename != "diagram.png" || att.Mime != "image/png" || att.Size != int64(len(img)) || att.UploadedBy != aliceID {
		t.Fatalf("from-file %+v", att)
	}
	if got := v.raw(alice, "GET", att.URL, nil, nil).expect(t, 200).Body; !bytes.Equal(got, img) {
		t.Fatal("copied bytes differ")
	}
	// Scopes and permissions.
	kOnly := v.token(alice, "kanban:read", "kanban:write")
	kOnly.Do("POST", "/api/v1/kanban/cards/"+card.ID+"/attachments/from-file", map[string]any{"file_id": entry.ID}).ExpectError(403, "forbidden")
	both := v.token(alice, "kanban:write", "files:read")
	both.Do("POST", "/api/v1/kanban/cards/"+card.ID+"/attachments/from-file", map[string]any{"file_id": entry.ID}).Expect(201)
	carol.Do("POST", "/api/v1/kanban/cards/"+card.ID+"/attachments/from-file", map[string]any{"file_id": entry.ID}).ExpectError(404, "not_found") // alice's home
	bob.Do("POST", "/api/v1/kanban/cards/"+card.ID+"/attachments/from-file", map[string]any{"file_id": entry.ID}).ExpectError(403, "forbidden")   // viewer
	alice.Do("POST", "/api/v1/kanban/cards/"+card.ID+"/attachments/from-file", map[string]any{"file_id": "nosuchfile0001"}).ExpectError(404, "not_found")

	// Save to Files: the caller's home root by default, any member may save.
	var saved map[string]any
	bob.Do("POST", att.URL+"/save-to-files", map[string]any{}).Expect(201).JSON(&saved)
	if saved["name"] != "diagram.png" || saved["size"] != float64(len(img)) || saved["path"] != "diagram.png" || saved["owner_id"] != bobID {
		t.Fatalf("saved %v", saved)
	}
	var content []byte
	content = v.raw(bob, "GET", "/api/v1/files/"+saved["id"].(string)+"/content", nil, nil).expect(t, 200).Body
	if !bytes.Equal(content, img) {
		t.Fatal("saved bytes differ")
	}
	bob.Do("POST", att.URL+"/save-to-files", map[string]any{}).ExpectError(409, "conflict")
	bob.Do("POST", att.URL+"/save-to-files", map[string]any{"overwrite": true}).Expect(201)
	bob.Do("POST", att.URL+"/save-to-files", map[string]any{"name": "copy.png", "path": "Boards/Bridge"}).Expect(201).JSON(&saved)
	if saved["path"] != "Boards/Bridge/copy.png" {
		t.Fatalf("saved into folder %v", saved)
	}
	bob.Do("POST", att.URL+"/save-to-files", map[string]any{"space_id": entry.SpaceID}).ExpectError(404, "not_found") // alice's home
	v.token(bob, "kanban:write").Do("POST", att.URL+"/save-to-files", map[string]any{"name": "x.png"}).ExpectError(403, "forbidden")
	v.token(bob, "kanban:write", "files:write").Do("POST", att.URL+"/save-to-files", map[string]any{"name": "y.png"}).Expect(201)
	// Quota applies.
	owner.Do("PATCH", "/api/v1/admin/users/"+bobID, map[string]any{"quota_bytes": 10}).Expect(200)
	bob.Do("POST", att.URL+"/save-to-files", map[string]any{"name": "z.png"}).ExpectError(507, "quota_exceeded")
}
