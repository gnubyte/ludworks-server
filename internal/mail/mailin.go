package mail

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/files"
	"ludworks/internal/settings"
)

// Mail-in: unread messages in the configured IMAP mailbox whose From address is a user's
// verified email (with mail_in_enabled) become notes in that user's account; attachments
// are saved to "Mail Attachments/<YYYY-MM-DD>/" in their home space.

const (
	pollTick        = time.Minute      // scheduler granularity; poll_minutes decides when to poll
	pollSlack       = 15 * time.Second // tolerated jitter of the scheduler
	pollTimeout     = 10 * time.Minute // one complete poll
	imapTestTimeout = 60 * time.Second
	maxPerPoll      = 50 // messages imported per poll (the rest wait for the next one)
	maxAttempts     = 5  // transient failures before a message is given up
	maxNameTries    = 20 // "name (2).ext" … when an attachment name is taken
	attachmentsDir  = "Mail Attachments"
)

// noteCreator and fileStore are the parts of the notes and files services mail-in uses
// (interfaces so tests can substitute fakes).
type noteCreator interface {
	CreateNote(ctx context.Context, userID, title, body string) (string, error)
}

type fileStore interface {
	HomeSpaceID(ctx context.Context, userID string) (string, error)
	EnsureDir(ctx context.Context, p *core.Principal, spaceID, path string) (*files.Entry, error)
	CreateFromReader(ctx context.Context, p *core.Principal, spaceID, dirPath, name string, r io.Reader, o files.CreateOpts) (*files.Entry, error)
}

// IMAPTestResult is the admin "test" button result.
type IMAPTestResult struct {
	OK        bool     `json:"ok"`
	Mailboxes []string `json:"mailboxes"`
	Unseen    int      `json:"unseen"`
}

// imapFailed renders an IMAP error as 502 imap_failed, never echoing the password.
func imapFailed(ctx context.Context, err error, password string) error {
	msg := err.Error()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		msg = "imap: the server did not answer in time"
	}
	return &core.Error{Status: http.StatusBadGateway, Code: "imap_failed", Message: clip(scrub(msg, password), 500)}
}

// scrub removes the password from a message (a server could echo it back).
func scrub(msg, password string) string {
	if len(password) >= 3 {
		msg = strings.ReplaceAll(msg, password, "***")
	}
	return msg
}

// TestIMAP logs in with the saved IMAP settings (even while mail-in is disabled), lists the
// mailboxes and counts unseen messages in the configured mailbox, read-only (EXAMINE).
func (m *Module) TestIMAP(ctx context.Context) (any, error) {
	cfg, err := m.d.Settings.IMAP(ctx)
	if err != nil {
		return nil, err
	}
	fields := map[string]string{}
	if strings.TrimSpace(cfg.Host) == "" {
		fields["imap.host"] = "required"
	}
	if cfg.Username == "" {
		fields["imap.username"] = "required"
	}
	if len(fields) > 0 {
		return nil, core.Validation(fields)
	}
	ctx, cancel := context.WithTimeout(ctx, imapTestTimeout)
	defer cancel()
	c, err := dialIMAP(ctx, cfg, m.imapOptions())
	if err != nil {
		return nil, imapFailed(ctx, err, cfg.Password)
	}
	defer c.logout()
	names, err := c.listMailboxes()
	if err != nil {
		return nil, imapFailed(ctx, err, cfg.Password)
	}
	if _, err := c.selectMailbox(cfg.Mailbox, true); err != nil {
		return nil, imapFailed(ctx, err, cfg.Password)
	}
	uids, err := c.searchUnseen()
	if err != nil {
		return nil, imapFailed(ctx, err, cfg.Password)
	}
	return IMAPTestResult{OK: true, Mailboxes: names, Unseen: len(uids)}, nil
}

// pollTickJob runs every minute and polls when mail-in is enabled and poll_minutes have
// passed since the last poll, so settings changes apply without rescheduling.
func (m *Module) pollTickJob(ctx context.Context) error {
	cfg, err := m.d.Settings.IMAP(ctx)
	if err != nil || !cfg.Enabled {
		return err
	}
	interval := time.Duration(max(cfg.PollMinutes, 1)) * time.Minute
	now := m.now()
	m.mu.Lock()
	if !m.lastPoll.IsZero() && now.Sub(m.lastPoll) < interval-pollSlack {
		m.mu.Unlock()
		return nil
	}
	m.lastPoll = now
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()
	res, err := m.pollOnce(ctx, cfg)
	if err != nil {
		m.log.Warn("mail-in poll failed", "err", clip(scrub(err.Error(), cfg.Password), 300))
		return nil
	}
	if res.imported+res.skipped+res.failed > 0 {
		m.log.Info("mail-in poll", "imported", res.imported, "skipped", res.skipped, "failed", res.failed, "pending", res.pending)
	}
	return nil
}

// pollResult counts what one poll did.
type pollResult struct{ imported, skipped, failed, retry, pending int }

// procRow is a row of mail_in_processed.
type procRow struct {
	status   string
	attempts int
}

func accountKey(cfg settings.IMAP) string {
	h := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(cfg.Host)) + "\x00" + strconv.Itoa(cfg.Port) +
		"\x00" + cfg.Username + "\x00" + cfg.Mailbox))
	return hex.EncodeToString(h[:16])
}

// pollOnce imports up to maxPerPoll unseen messages.
func (m *Module) pollOnce(ctx context.Context, cfg settings.IMAP) (pollResult, error) {
	var res pollResult
	notes, _ := m.ingestTargets()
	if notes == nil {
		if m.warnedNoIngest.CompareAndSwap(false, true) {
			m.log.Warn("mail-in is enabled but the notes service is not available")
		}
		return res, nil
	}
	auth, err := m.mailInSettings(ctx)
	if err != nil {
		return res, err
	}
	if !auth.senderCheckConfigured() {
		// Fail closed without touching the mailbox: messages stay unread and are imported
		// once the admin names the trusted receiving server (senderauth.go).
		if m.warnedNoTrust.CompareAndSwap(false, true) {
			m.log.Warn("mail-in is enabled but no sender authentication is configured; nothing is imported until " +
				"mail_in.trusted_authserv_ids lists the receiving mail server (or mail_in.allow_unauthenticated_senders is set)")
		}
		return res, nil
	}
	m.warnedNoTrust.Store(false)
	c, err := dialIMAP(ctx, cfg, m.imapOptions())
	if err != nil {
		return res, err
	}
	defer c.logout()
	mb, err := c.selectMailbox(cfg.Mailbox, false)
	if err != nil {
		return res, err
	}
	unseen, err := c.searchUnseen()
	if err != nil {
		return res, err
	}
	key := accountKey(cfg)
	state, err := m.loadProcessed(ctx, key, mb.uidValidity, unseen)
	if err != nil {
		return res, err
	}
	var todo []uint32
	for _, uid := range unseen {
		if st, ok := state[uid]; ok && st.status != "retry" {
			continue
		}
		if len(todo) == maxPerPoll {
			res.pending++
			continue
		}
		todo = append(todo, uid)
	}
	if len(todo) == 0 {
		return res, nil
	}
	sizes, err := c.fetchSizes(todo)
	if err != nil {
		return res, err
	}
	finish := func(uid uint32, status string, attempts int, noteID string) error {
		if err := m.record(ctx, key, mb.uidValidity, uid, status, attempts, noteID); err != nil {
			return err
		}
		if status == "retry" || !cfg.MarkSeen {
			return nil
		}
		if err := c.markSeen(uid); err != nil {
			var ie *imapError
			if errors.As(err, &ie) { // NO/BAD: the record above prevents a duplicate import
				m.log.Warn("mail-in: could not mark message as seen", "uid", uid, "err", err)
				return nil
			}
			return err
		}
		return nil
	}
	for _, uid := range todo {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		size, ok := sizes[uid]
		if !ok {
			continue // expunged meanwhile
		}
		if size > m.maxMsg {
			m.log.Info("mail-in: message too large, skipped", "uid", uid, "bytes", size)
			res.skipped++
			if err := finish(uid, "skipped", 1, ""); err != nil {
				return res, err
			}
			continue
		}
		raw, err := c.fetchMessage(uid, m.maxMsg)
		switch {
		case errors.Is(err, errMessageGone):
			continue
		case errors.Is(err, errIMAPLiteral):
			// The server sent more than it announced: never retry this message. The
			// connection is out of sync, so the poll ends here.
			res.skipped++
			_ = m.record(ctx, key, mb.uidValidity, uid, "skipped", 1, "")
			return res, err
		case err != nil:
			return res, err
		}
		out := m.ingest(ctx, cfg, auth, raw)
		attempts := 1
		if out.status == "retry" {
			attempts = state[uid].attempts + 1
			if attempts >= maxAttempts {
				out.status = "failed"
			}
		}
		switch out.status {
		case "done":
			res.imported++
		case "skipped":
			res.skipped++
		case "failed":
			res.failed++
			m.log.Warn("mail-in: message could not be imported", "uid", uid, "err", out.err)
		case "retry":
			res.retry++
			m.log.Warn("mail-in: import failed, will retry", "uid", uid, "attempt", attempts, "err", out.err)
		}
		if err := finish(uid, out.status, attempts, out.noteID); err != nil {
			return res, err
		}
	}
	return res, nil
}

// loadProcessed returns the bookkeeping rows for the current UIDVALIDITY after pruning rows
// of an old UIDVALIDITY and rows of messages that are no longer unseen.
func (m *Module) loadProcessed(ctx context.Context, key string, uidValidity uint32, unseen []uint32) (map[uint32]procRow, error) {
	inUnseen := make(map[uint32]bool, len(unseen))
	for _, u := range unseen {
		inUnseen[u] = true
	}
	var out map[uint32]procRow
	err := m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		out = map[uint32]procRow{}
		if _, err := tx.ExecContext(ctx, `DELETE FROM mail_in_processed WHERE account_key = ? AND uid_validity <> ?`,
			key, int64(uidValidity)); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT uid, status, attempts FROM mail_in_processed
			WHERE account_key = ? AND uid_validity = ?`, key, int64(uidValidity))
		if err != nil {
			return err
		}
		var stale []int64
		for rows.Next() {
			var uid int64
			var r procRow
			if err := rows.Scan(&uid, &r.status, &r.attempts); err != nil {
				rows.Close()
				return err
			}
			if uid <= 0 || uid > 1<<32-1 || !inUnseen[uint32(uid)] {
				stale = append(stale, uid)
				continue
			}
			out[uint32(uid)] = r
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, uid := range stale {
			if _, err := tx.ExecContext(ctx, `DELETE FROM mail_in_processed WHERE account_key = ? AND uid_validity = ? AND uid = ?`,
				key, int64(uidValidity), uid); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

func (m *Module) record(ctx context.Context, key string, uidValidity, uid uint32, status string, attempts int, noteID string) error {
	var note any
	if noteID != "" {
		note = noteID
	}
	return m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO mail_in_processed
			(account_key, uid_validity, uid, status, attempts, note_id, processed_at) VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (account_key, uid_validity, uid) DO UPDATE SET status = excluded.status,
				attempts = excluded.attempts, note_id = excluded.note_id, processed_at = excluded.processed_at`,
			key, int64(uidValidity), int64(uid), status, attempts, note, db.FormatTime(m.now()))
		return err
	})
}

// ingestOutcome is the result of importing one message.
type ingestOutcome struct {
	status string // done | skipped | failed | retry
	noteID string
	err    error
}

// mailUser is the account a message is imported into.
type mailUser struct{ id, username string }

// maskAddress hides the local part of an address for logs ("*@example.com").
func maskAddress(addr string) string {
	if i := strings.LastIndexByte(addr, '@'); i >= 0 {
		return "*" + addr[i:]
	}
	return "*"
}

// findRecipientUser maps a From address to the enabled account with that verified email
// and mail-in switched on.
func (m *Module) findRecipientUser(ctx context.Context, addr string) (*mailUser, error) {
	var u mailUser
	err := m.d.DB.SQL.QueryRowContext(ctx, `SELECT id, username FROM users
		WHERE email = ? AND email_verified = 1 AND mail_in_enabled = 1 AND disabled = 0`, addr).Scan(&u.id, &u.username)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// ingest imports one raw message.
func (m *Module) ingest(ctx context.Context, cfg settings.IMAP, auth MailInSettings, raw []byte) ingestOutcome {
	notes, store := m.ingestTargets()
	in, err := parseInbound(raw)
	if err != nil {
		m.log.Debug("mail-in: unparseable message skipped", "err", err)
		return ingestOutcome{status: "skipped"}
	}
	// The From address selects the account written to, so it must be authenticated by a
	// trusted receiving server before it is looked up (fails closed, senderauth.go).
	if ok, why := auth.checkSender(in); !ok {
		m.log.Info("mail-in: sender not authenticated, skipped", "from", maskAddress(in.From), "reason", why)
		return ingestOutcome{status: "skipped"}
	}
	u, err := m.findRecipientUser(ctx, in.From)
	if err != nil {
		return ingestOutcome{status: "retry", err: err}
	}
	if u == nil {
		m.log.Debug("mail-in: sender is not a mail-in user, skipped", "from", maskAddress(in.From))
		return ingestOutcome{status: "skipped"}
	}
	if notes == nil {
		return ingestOutcome{status: "retry", err: errors.New("notes service not available")}
	}
	p := &core.Principal{UserID: u.id, Username: u.username, Role: core.RoleUser,
		Scopes: []string{"files:read", "files:write", "notes:write"}, Kind: core.KindToken, SessionID: "mail-in"}
	var saved []savedAttachment
	if len(in.Attachments) > 0 {
		saved = m.saveAttachments(ctx, cfg, store, p, in)
	}
	title := in.Subject
	if title == "" {
		title = "(no subject)"
	}
	noteID, err := notes.CreateNote(ctx, u.id, title, composeBody(in, saved, cfg.SaveAttachments && store != nil))
	if err != nil {
		var ce *core.Error
		if errors.As(err, &ce) && ce.Status >= 400 && ce.Status < 500 {
			return ingestOutcome{status: "failed", err: err}
		}
		return ingestOutcome{status: "retry", err: err}
	}
	if m.d.Audit != nil {
		m.d.Audit.Record(core.WithPrincipal(ctx, p), "mail_in.note_created", "note", noteID,
			map[string]any{"attachments": countSaved(saved)})
	}
	m.log.Debug("mail-in: note created", "from", maskAddress(in.From), "attachments", countSaved(saved))
	return ingestOutcome{status: "done", noteID: noteID}
}

// savedAttachment is one attachment's fate.
type savedAttachment struct {
	name  string
	size  int
	id    string // file id when saved
	issue string // why it was not saved
}

func countSaved(s []savedAttachment) int {
	n := 0
	for _, a := range s {
		if a.id != "" {
			n++
		}
	}
	return n
}

// attachmentDate is the folder date: the message's Date when plausible, else today (UTC).
func (m *Module) attachmentDate(d time.Time) string {
	now := m.now().UTC()
	if d.IsZero() || d.After(now.Add(24*time.Hour)) || d.Year() < 1990 {
		d = now
	}
	return d.UTC().Format("2006-01-02")
}

// saveAttachments stores the attachments in the user's home space. Failures are reported
// per attachment in the note instead of failing the import.
func (m *Module) saveAttachments(ctx context.Context, cfg settings.IMAP, store fileStore, p *core.Principal, in *inboundMail) []savedAttachment {
	out := make([]savedAttachment, 0, len(in.Attachments))
	for _, a := range in.Attachments {
		out = append(out, savedAttachment{name: a.Name, size: len(a.Data)})
	}
	if !cfg.SaveAttachments || store == nil {
		return out
	}
	fail := func(issue string) []savedAttachment {
		for i := range out {
			out[i].issue = issue
		}
		return out
	}
	spaceID, err := store.HomeSpaceID(ctx, p.UserID)
	if err != nil {
		m.log.Warn("mail-in: home space unavailable", "err", err)
		return fail("could not be saved")
	}
	day := m.attachmentDate(in.Date)
	dir := attachmentsDir + "/" + day
	if _, err := store.EnsureDir(ctx, p, spaceID, dir); err != nil {
		m.log.Warn("mail-in: creating the attachments folder failed", "err", err)
		return fail("could not be saved")
	}
	mod, _ := time.Parse("2006-01-02", day)
	if !in.Date.IsZero() && in.Date.UTC().Format("2006-01-02") == day {
		mod = in.Date.UTC()
	}
	for i, a := range in.Attachments {
		if len(a.Data) > MaxMessageBytes {
			out[i].issue = "too large"
			continue
		}
		e, err := createUnique(ctx, store, p, spaceID, dir, a.Name, a.Data, files.CreateOpts{ModTime: mod})
		if err != nil {
			m.log.Warn("mail-in: saving an attachment failed", "err", err)
			out[i].issue = "could not be saved"
			var ce *core.Error
			if errors.As(err, &ce) && ce.Status == http.StatusInsufficientStorage {
				out[i].issue = "not saved: storage quota exceeded"
			}
			continue
		}
		out[i].id, out[i].name = e.ID, e.Name
	}
	return out
}

// createUnique creates dir/name, trying "name (2).ext", "name (3).ext"… while the name is
// taken.
func createUnique(ctx context.Context, store fileStore, p *core.Principal, spaceID, dir, name string, data []byte, o files.CreateOpts) (*files.Entry, error) {
	ext := path.Ext(name)
	if len(ext) > 16 || ext == name {
		ext = ""
	}
	base := strings.TrimSuffix(name, ext)
	var lastErr error
	for n := 1; n <= maxNameTries; n++ {
		candidate := name
		if n > 1 {
			candidate = base + " (" + strconv.Itoa(n) + ")" + ext
		}
		e, err := store.CreateFromReader(ctx, p, spaceID, dir, candidate, bytes.NewReader(data), o)
		if err == nil {
			return e, nil
		}
		var ce *core.Error
		if !(errors.As(err, &ce) && ce.Status == http.StatusConflict) {
			return nil, err
		}
		lastErr = err
	}
	return nil, lastErr
}

// composeBody appends the attachment list (links to the saved files) to the message text.
func composeBody(in *inboundMail, saved []savedAttachment, saving bool) string {
	var b strings.Builder
	b.WriteString(in.Body)
	if len(saved) == 0 && in.Dropped == 0 {
		if b.Len() == 0 {
			return "_(This email had no text.)_"
		}
		return b.String()
	}
	if b.Len() > 0 {
		b.WriteString("\n\n---\n\n")
	}
	b.WriteString("**Attachments**\n")
	for _, a := range saved {
		label := escapeLinkText(a.name)
		switch {
		case a.id != "":
			fmt.Fprintf(&b, "\n- [%s](/api/v1/files/%s/content) (%s)", label, a.id, humanBytes(int64(a.size)))
		case !saving:
			fmt.Fprintf(&b, "\n- %s (%s, not saved)", label, humanBytes(int64(a.size)))
		default:
			fmt.Fprintf(&b, "\n- %s (%s, %s)", label, humanBytes(int64(a.size)), a.issue)
		}
	}
	if in.Dropped > 0 {
		fmt.Fprintf(&b, "\n- %d more attachments were not imported", in.Dropped)
	}
	return b.String()
}

// humanBytes formats a size in binary units like the web UI ("1.5 MiB").
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return strconv.FormatFloat(float64(n)/float64(div), 'f', 1, 64) + " " + string("KMGTP"[exp]) + "iB"
}
