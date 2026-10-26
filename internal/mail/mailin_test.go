package mail

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"ludworks/internal/auth"
	"ludworks/internal/auth/authtest"
	"ludworks/internal/core"
	"ludworks/internal/files"
	"ludworks/internal/httpx"
	"ludworks/internal/mail/mailtest"
	"ludworks/internal/settings"
)

// noStart keeps the background poller out of the way so tests drive polls themselves.
type noStart struct{ *Module }

func (noStart) Start(context.Context) error { return nil }

type fakeNote struct{ userID, title, body string }

type fakeNotes struct {
	mu    sync.Mutex
	notes []fakeNote
	errs  []error // returned (and consumed) before succeeding
}

func (f *fakeNotes) CreateNote(_ context.Context, userID, title, body string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		return "", err
	}
	f.notes = append(f.notes, fakeNote{userID, title, body})
	return fmt.Sprintf("note%08d", len(f.notes)), nil
}

func (f *fakeNotes) all() []fakeNote {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeNote(nil), f.notes...)
}

type fakeFiles struct {
	mu        sync.Mutex
	dirs      map[string]bool
	files     map[string][]byte // space/dir/name
	ids       int
	failQuota bool
}

func newFakeFiles() *fakeFiles {
	return &fakeFiles{dirs: map[string]bool{}, files: map[string][]byte{}}
}

func (f *fakeFiles) HomeSpaceID(_ context.Context, userID string) (string, error) {
	return "home-" + userID, nil
}

func (f *fakeFiles) EnsureDir(_ context.Context, p *core.Principal, spaceID, path string) (*files.Entry, error) {
	if spaceID != "home-"+p.UserID {
		return nil, core.ErrNotFound
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dirs[spaceID+"/"+path] = true
	return &files.Entry{ID: "dir00001", SpaceID: spaceID, Path: path, IsDir: true}, nil
}

func (f *fakeFiles) CreateFromReader(_ context.Context, p *core.Principal, spaceID, dir, name string, r io.Reader, o files.CreateOpts) (*files.Entry, error) {
	if spaceID != "home-"+p.UserID || !p.Has("files:write") || p.IsAdmin() {
		return nil, core.ErrNotFound
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failQuota {
		return nil, core.NewError(http.StatusInsufficientStorage, "quota_exceeded", "quota exceeded")
	}
	if !f.dirs[spaceID+"/"+dir] {
		return nil, core.NotFound("no such directory")
	}
	key := spaceID + "/" + dir + "/" + name
	if _, ok := f.files[key]; ok {
		return nil, core.Conflict("name taken")
	}
	f.files[key] = data
	f.ids++
	return &files.Entry{ID: fmt.Sprintf("file%04d", f.ids), SpaceID: spaceID, Path: dir + "/" + name, Name: name,
		Size: int64(len(data)), ModTime: o.ModTime}, nil
}

func (f *fakeFiles) names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for k := range f.files {
		out = append(out, k)
	}
	return out
}

type mailEnv struct {
	*authtest.Env
	m     *Module
	notes *fakeNotes
	files *fakeFiles
}

func newMailEnv(t *testing.T) *mailEnv {
	t.Helper()
	var m *Module
	e := authtest.New(t, authtest.Options{Modules: func(e *authtest.Env) []httpx.Module {
		m = New(e.Deps)
		return []httpx.Module{noStart{m}}
	}})
	me := &mailEnv{Env: e, m: m, notes: &fakeNotes{}, files: newFakeFiles()}
	m.setIngest(me.notes, me.files)
	m.SetIMAPTimeout(3 * time.Second)
	e.Patch(MailInSettingsNS, map[string]any{"trusted_authserv_ids": []string{trustedMX}})
	return me
}

func (e *mailEnv) patchIMAP(t *testing.T, v map[string]any) {
	t.Helper()
	e.Patch("imap", v)
}

// user creates an account with the given mail-in state and returns its id.
func (e *mailEnv) user(t *testing.T, name string, verified, mailIn, disabled bool) string {
	t.Helper()
	u, err := e.Auth.CreateUser(context.Background(), auth.CreateUserInput{
		Username: name, Email: name + "@example.com", Password: authtest.AdminPassword, Role: "user"})
	if err != nil {
		t.Fatal(err)
	}
	b := func(v bool) int {
		if v {
			return 1
		}
		return 0
	}
	if _, err := e.Deps.DB.SQL.Exec(`UPDATE users SET email_verified = ?, mail_in_enabled = ?, disabled = ? WHERE id = ?`,
		b(verified), b(mailIn), b(disabled), u.ID); err != nil {
		t.Fatal(err)
	}
	return u.ID
}

func (e *mailEnv) cfg(srv *mailtest.IMAPServer) settings.IMAP {
	c := imapCfg(srv, "none")
	c.Enabled = true
	return c
}

func (e *mailEnv) poll(t *testing.T, cfg settings.IMAP) pollResult {
	t.Helper()
	res, err := e.m.pollOnce(context.Background(), cfg)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	return res
}

func (e *mailEnv) processed(t *testing.T) map[uint32]string {
	t.Helper()
	rows, err := e.Deps.DB.SQL.Query(`SELECT uid, status FROM mail_in_processed`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[uint32]string{}
	for rows.Next() {
		var uid uint32
		var st string
		if err := rows.Scan(&uid, &st); err != nil {
			t.Fatal(err)
		}
		out[uid] = st
	}
	return out
}

// eml and withAttachment build messages as a trusted receiving server delivers them: with
// an Authentication-Results header reporting dmarc=pass for the sender's domain.
func eml(from, subject, body string) []byte {
	return crlf(arPass(from) + "From: " + from + "\nTo: notes@example.com\nSubject: " + subject +
		"\nDate: Mon, 26 Oct 2026 09:30:00 +0000\nContent-Type: text/plain; charset=utf-8\n\n" + body + "\n")
}

func withAttachment(from, subject string) []byte {
	return crlf(arPass(from) + `From: ` + from + `
Subject: ` + subject + `
Date: Sun, 25 Oct 2026 23:30:00 -0200
Content-Type: multipart/mixed; boundary="b"

--b
Content-Type: text/plain

See the attached report.
--b
Content-Type: application/pdf
Content-Disposition: attachment; filename="report [final].pdf"
Content-Transfer-Encoding: base64

JVBERi0xLjQK
--b
Content-Type: text/csv; name="../data.csv"
Content-Disposition: attachment

a,b
--b--
`)
}

func TestMailInMapsVerifiedSendersOnly(t *testing.T) {
	e := newMailEnv(t)
	alice := e.user(t, "alice", true, true, false)
	e.user(t, "bob", false, true, false)   // not verified
	e.user(t, "carol", true, false, false) // mail-in off
	e.user(t, "dave", true, true, true)    // disabled
	srv := mailtest.StartIMAP(t, mailtest.IMAPOptions{User: "inbox", Pass: testPass})
	u1 := srv.Add("INBOX", eml("Alice <ALICE@example.com>", "Groceries", "- milk\n- eggs"))
	u2 := srv.Add("INBOX", eml("bob@example.com", "from bob", "x"))
	u3 := srv.Add("INBOX", eml("carol@example.com", "from carol", "x"))
	u4 := srv.Add("INBOX", eml("dave@example.com", "from dave", "x"))
	u5 := srv.Add("INBOX", eml("mallory@evil.example", "spam", "x"))
	u6 := srv.Add("INBOX", withAttachment("alice@example.com", "Report"))
	u7 := srv.Add("INBOX", crlf("Authentication-Results: mx; dmarc=fail\nFrom: alice@example.com\nSubject: forged\n\nx\n"))
	u8 := srv.Add("INBOX", []byte("not a message at all"))
	u9 := srv.Add("INBOX", eml("alice@example.com", "", "untitled"))

	res := e.poll(t, e.cfg(srv))
	if res.imported != 3 || res.skipped != 6 || res.failed != 0 {
		t.Fatalf("result %+v", res)
	}
	notes := e.notes.all()
	if len(notes) != 3 {
		t.Fatalf("notes %+v", notes)
	}
	if n := notes[0]; n.userID != alice || n.title != "Groceries" || n.body != "- milk\n- eggs" {
		t.Fatalf("note 1 %+v", n)
	}
	if n := notes[2]; n.title != "(no subject)" || n.body != "untitled" {
		t.Fatalf("note 3 %+v", n)
	}
	// Attachments land in the sender's home under the message date (UTC).
	body := notes[1].body
	if !strings.HasPrefix(body, "See the attached report.\n\n---\n\n**Attachments**\n") ||
		!strings.Contains(body, `- [report \[final\].pdf](/api/v1/files/file0001/content) (9 B)`) ||
		!strings.Contains(body, "- [data.csv](/api/v1/files/file0002/content) (3 B)") {
		t.Fatalf("note body:\n%s", body)
	}
	names := strings.Join(e.files.names(), " ")
	for _, want := range []string{"home-" + alice + "/Mail Attachments/2026-10-26/report [final].pdf", "home-" + alice + "/Mail Attachments/2026-10-26/data.csv"} {
		if !strings.Contains(names, want) {
			t.Fatalf("files %s missing %s", names, want)
		}
	}
	// Every handled message is \Seen and recorded.
	for _, u := range []uint32{u1, u2, u3, u4, u5, u6, u7, u8, u9} {
		if !srv.Seen("INBOX", u) {
			t.Fatalf("uid %d not marked seen", u)
		}
	}
	// Audit trail with the user as actor.
	var n int
	if err := e.Deps.DB.SQL.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = 'mail_in.note_created' AND actor_id = ?`, alice).Scan(&n); err != nil || n != 3 {
		t.Fatalf("audit entries %d %v", n, err)
	}
	// Nothing left: a second poll is a no-op.
	if res := e.poll(t, e.cfg(srv)); res != (pollResult{}) {
		t.Fatalf("second poll %+v", res)
	}
}

func TestMailInIdempotentWithoutSeen(t *testing.T) {
	e := newMailEnv(t)
	e.user(t, "alice", true, true, false)
	// STORE fails: the bookkeeping must still prevent a second import.
	srv := mailtest.StartIMAP(t, mailtest.IMAPOptions{User: "inbox", Pass: testPass, FailStore: true})
	uid := srv.Add("INBOX", eml("alice@example.com", "once", "x"))
	cfg := e.cfg(srv)
	e.poll(t, cfg)
	e.poll(t, cfg)
	if n := len(e.notes.all()); n != 1 || srv.Seen("INBOX", uid) {
		t.Fatalf("notes %d, seen %v", n, srv.Seen("INBOX", uid))
	}
	if got := e.processed(t); got[uid] != "done" {
		t.Fatalf("processed %v", got)
	}

	// mark_seen off: same guarantee, and nothing is flagged.
	srv2 := mailtest.StartIMAP(t, mailtest.IMAPOptions{User: "inbox", Pass: testPass})
	u2 := srv2.Add("INBOX", eml("alice@example.com", "twice?", "x"))
	cfg2 := e.cfg(srv2)
	cfg2.MarkSeen = false
	e.poll(t, cfg2)
	e.poll(t, cfg2)
	if n := len(e.notes.all()); n != 2 || srv2.Seen("INBOX", u2) {
		t.Fatalf("notes %d", n)
	}
	if cmds := commandLines(srv2); strings.Contains(cmds, "STORE") {
		t.Fatalf("STORE sent with mark_seen off:\n%s", cmds)
	}

	// A message marked seen elsewhere is pruned from the bookkeeping; a new UIDVALIDITY
	// drops the old rows entirely.
	srv2.SetSeen("INBOX", u2, true)
	e.poll(t, cfg2)
	if got := e.processed(t); len(got) != 1 { // only the first server's row remains
		t.Fatalf("after prune %v", got)
	}
	srv.SetUIDValidity(7)
	e.poll(t, cfg)
	var stale int
	_ = e.Deps.DB.SQL.QueryRow(`SELECT COUNT(*) FROM mail_in_processed WHERE uid_validity = 1 AND account_key = ?`, accountKey(cfg)).Scan(&stale)
	if stale != 0 {
		t.Fatal("rows of the old UIDVALIDITY survived")
	}
}

func TestMailInRetriesTransientFailures(t *testing.T) {
	e := newMailEnv(t)
	e.user(t, "alice", true, true, false)
	srv := mailtest.StartIMAP(t, mailtest.IMAPOptions{User: "inbox", Pass: testPass})
	uid := srv.Add("INBOX", eml("alice@example.com", "flaky", "x"))
	cfg := e.cfg(srv)
	e.notes.errs = []error{errors.New("database is locked"), core.ErrInternal}
	if r := e.poll(t, cfg); r.retry != 1 || srv.Seen("INBOX", uid) {
		t.Fatalf("first %+v", r)
	}
	if r := e.poll(t, cfg); r.retry != 1 {
		t.Fatalf("second %+v", r)
	}
	if r := e.poll(t, cfg); r.imported != 1 || !srv.Seen("INBOX", uid) {
		t.Fatalf("third %+v", r)
	}

	// Giving up after maxAttempts.
	u2 := srv.Add("INBOX", eml("alice@example.com", "always failing", "x"))
	for i := 0; i < maxAttempts; i++ {
		e.notes.errs = append(e.notes.errs, errors.New("boom"))
	}
	var last pollResult
	for i := 0; i < maxAttempts; i++ {
		last = e.poll(t, cfg)
	}
	if last.failed != 1 || !srv.Seen("INBOX", u2) || e.processed(t)[u2] != "failed" {
		t.Fatalf("after %d attempts: %+v %v", maxAttempts, last, e.processed(t))
	}

	// A 4xx from the notes service is permanent at once.
	u3 := srv.Add("INBOX", eml("alice@example.com", "invalid", "x"))
	e.notes.errs = []error{core.Validation(map[string]string{"body": "too long"})}
	if r := e.poll(t, cfg); r.failed != 1 || e.processed(t)[u3] != "failed" {
		t.Fatalf("validation failure: %+v", r)
	}
}

func TestMailInSizeLimits(t *testing.T) {
	e := newMailEnv(t)
	e.user(t, "alice", true, true, false)
	small := eml("alice@example.com", "small", "fits")
	e.m.maxMsg = int64(len(small)) + 100
	srv := mailtest.StartIMAP(t, mailtest.IMAPOptions{User: "inbox", Pass: testPass})
	big := srv.Add("INBOX", eml("alice@example.com", "big", strings.Repeat("x", 400)))
	ok := srv.Add("INBOX", small)
	r := e.poll(t, e.cfg(srv))
	if r.skipped != 1 || r.imported != 1 || !srv.Seen("INBOX", big) || e.processed(t)[big] != "skipped" || e.processed(t)[ok] != "done" {
		t.Fatalf("%+v %v", r, e.processed(t))
	}

	// A server announcing a small size but sending a huge literal: the message is never
	// retried and the poll stops.
	liar := mailtest.StartIMAP(t, mailtest.IMAPOptions{User: "inbox", Pass: testPass,
		Override: func(tag, verb, line string) (string, bool, bool) {
			if verb == "UID FETCH" && strings.Contains(line, "BODY.PEEK[]") {
				return "* 1 FETCH (UID 1 BODY[] {10000000}\r\n", true, true
			}
			return "", false, false
		}})
	liar.Add("INBOX", eml("alice@example.com", "liar", "x"))
	if _, err := e.m.pollOnce(context.Background(), e.cfg(liar)); !errors.Is(err, errIMAPLiteral) {
		t.Fatalf("liar: %v", err)
	}
	var st string
	if err := e.Deps.DB.SQL.QueryRow(`SELECT status FROM mail_in_processed WHERE account_key = ? AND uid = 1`,
		accountKey(e.cfg(liar))).Scan(&st); err != nil || st != "skipped" {
		t.Fatalf("liar bookkeeping %q %v", st, err)
	}
}

func TestMailInAttachmentOptions(t *testing.T) {
	e := newMailEnv(t)
	alice := e.user(t, "alice", true, true, false)
	srv := mailtest.StartIMAP(t, mailtest.IMAPOptions{User: "inbox", Pass: testPass})
	cfg := e.cfg(srv)

	// save_attachments off: listed, not saved.
	cfg.SaveAttachments = false
	srv.Add("INBOX", withAttachment("alice@example.com", "no save"))
	e.poll(t, cfg)
	if n := e.notes.all(); len(n) != 1 || !strings.Contains(n[0].body, "- report \\[final\\].pdf (9 B, not saved)") || len(e.files.names()) != 0 {
		t.Fatalf("%+v %v", n, e.files.names())
	}

	// Name collisions get " (2)"; quota errors are reported in the note.
	cfg.SaveAttachments = true
	srv.Add("INBOX", withAttachment("alice@example.com", "first"))
	srv.Add("INBOX", withAttachment("alice@example.com", "second"))
	e.poll(t, cfg)
	names := strings.Join(e.files.names(), "|")
	if !strings.Contains(names, "home-"+alice+"/Mail Attachments/2026-10-26/report [final] (2).pdf") {
		t.Fatalf("collision handling: %s", names)
	}
	e.files.failQuota = true
	srv.Add("INBOX", withAttachment("alice@example.com", "full"))
	e.poll(t, cfg)
	notes := e.notes.all()
	if last := notes[len(notes)-1]; !strings.Contains(last.body, "not saved: storage quota exceeded") {
		t.Fatalf("quota note: %s", last.body)
	}
}

func TestMailInWithoutNotesService(t *testing.T) {
	e := newMailEnv(t)
	e.m.setIngest(nil, nil)
	srv := mailtest.StartIMAP(t, mailtest.IMAPOptions{User: "inbox", Pass: testPass})
	srv.Add("INBOX", eml("alice@example.com", "x", "x"))
	if r := e.poll(t, e.cfg(srv)); r != (pollResult{}) || len(srv.Commands()) != 0 {
		t.Fatalf("poll without notes service: %+v", r)
	}
	// The real (possibly stub) services can be wired through SetIngest with nil values.
	e.m.SetIngest(nil, nil)
	if n, f := e.m.ingestTargets(); n != nil || f != nil {
		t.Fatal("nil services must stay nil interfaces")
	}
}

func TestPollTickSchedule(t *testing.T) {
	e := newMailEnv(t)
	e.user(t, "alice", true, true, false)
	srv := mailtest.StartIMAP(t, mailtest.IMAPOptions{User: "inbox", Pass: testPass})
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	e.m.now = func() time.Time { return now }
	ctx := context.Background()

	srv.Add("INBOX", eml("alice@example.com", "one", "x"))
	if err := e.m.pollTickJob(ctx); err != nil || len(srv.Commands()) != 0 {
		t.Fatal("disabled mail-in must not connect")
	}
	e.patchIMAP(t, map[string]any{"enabled": true, "host": srv.Host, "port": srv.Port, "security": "none",
		"username": "inbox", "password": testPass, "poll_minutes": 5})
	_ = e.m.pollTickJob(ctx)
	if len(e.notes.all()) != 1 {
		t.Fatal("first tick should poll")
	}
	srv.Add("INBOX", eml("alice@example.com", "two", "x"))
	now = now.Add(3 * time.Minute)
	_ = e.m.pollTickJob(ctx)
	if len(e.notes.all()) != 1 {
		t.Fatal("polled before poll_minutes elapsed")
	}
	now = now.Add(2*time.Minute - 10*time.Second) // within the scheduler slack
	_ = e.m.pollTickJob(ctx)
	if len(e.notes.all()) != 2 {
		t.Fatal("second poll missing")
	}
}

func TestComposeBodyAndBytes(t *testing.T) {
	in := &inboundMail{Body: ""}
	if got := composeBody(in, nil, true); got != "_(This email had no text.)_" {
		t.Fatal(got)
	}
	in = &inboundMail{Body: "", Dropped: 2}
	got := composeBody(in, []savedAttachment{{name: "a.png", size: 2048, id: "f1"}, {name: "b", size: 1, issue: "could not be saved"}}, true)
	want := "**Attachments**\n\n- [a.png](/api/v1/files/f1/content) (2.0 KiB)\n- b (1 B, could not be saved)\n- 2 more attachments were not imported"
	if got != want {
		t.Fatalf("got %q", got)
	}
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1536: "1.5 KiB", 25 << 20: "25.0 MiB", 3 << 30: "3.0 GiB"} {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q", n, got)
		}
	}
	if maskAddress("alice@example.com") != "*@example.com" || maskAddress("x") != "*" {
		t.Fatal("maskAddress")
	}
	if d := (&Module{now: time.Now}).attachmentDate(time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)); d != time.Now().UTC().Format("2006-01-02") {
		t.Fatalf("future date accepted: %s", d)
	}
}
