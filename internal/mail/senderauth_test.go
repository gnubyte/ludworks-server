package mail

import (
	"context"
	"strings"
	"testing"

	"ludworks/internal/mail/mailtest"
)

// trustedMX is the receiving server the mail tests trust (see newMailEnv).
const trustedMX = "mx.example.net"

// arPass is the Authentication-Results header a trusted receiving server adds for a
// message from addr whose DMARC check passed (unquoted base64 in header.b, as real
// servers write it).
func arPass(addr string) string {
	dom := addr[strings.LastIndexByte(addr, '@')+1:]
	dom = strings.TrimSuffix(dom, ">")
	return "Authentication-Results: " + trustedMX + "; spf=pass (" + trustedMX + ": domain of " + addr +
		" designates 192.0.2.1 as permitted sender) smtp.mailfrom=" + dom + "; dkim=pass header.d=" + dom +
		" header.s=s1 header.b=Ab1+/x=; dmarc=pass (p=REJECT sp=REJECT dis=NONE) header.from=" + dom + "\n"
}

// withoutAuthResults drops the leading Authentication-Results line of a test message.
func withoutAuthResults(raw []byte) []byte {
	s := string(raw)
	if !strings.HasPrefix(s, "Authentication-Results:") {
		panic("no leading Authentication-Results")
	}
	return []byte(s[strings.Index(s, "\r\n")+2:])
}

func rawMail(headers, subject string) []byte {
	return crlf(headers + "From: alice@example.com\nSubject: " + subject + "\n\nbody of " + subject + "\n")
}

// TestMailInRequiresAuthenticatedSender is the regression test for the fail-open From
// check: a message is imported only when a trusted receiving server reports dmarc=pass for
// the From domain; everything else (no header, non-pass verdicts, verdicts from other
// servers, forged lower headers) is skipped, and no attachment is written.
func TestMailInRequiresAuthenticatedSender(t *testing.T) {
	e := newMailEnv(t)
	alice := e.user(t, "alice", true, true, false)
	srv := mailtest.StartIMAP(t, mailtest.IMAPOptions{User: "inbox", Pass: testPass})
	spoofs := map[string][]byte{
		"no header":       rawMail("", "no header"),
		"dmarc=none":      rawMail("Authentication-Results: mx.example.net; spf=none; dkim=none; dmarc=none header.from=example.com\n", "none"),
		"temperror":       rawMail("Authentication-Results: mx.example.net; dmarc=temperror header.from=example.com\n", "temperror"),
		"permerror":       rawMail("Authentication-Results: mx.example.net; dmarc=permerror header.from=example.com\n", "permerror"),
		"bestguesspass":   rawMail("Authentication-Results: mx.example.net; dmarc=bestguesspass header.from=example.com\n", "bestguess"),
		"no dmarc result": rawMail("Authentication-Results: mx.example.net; spf=pass smtp.mailfrom=example.com; dkim=pass header.d=example.com\n", "no dmarc"),
		// The receiving server did not stamp: the attacker's own header is topmost.
		"untrusted pass": rawMail("Authentication-Results: attacker.example; dmarc=pass header.from=example.com\n", "untrusted"),
		"lookalike id":   rawMail("Authentication-Results: mx.example.net.evil.example; dmarc=pass header.from=example.com\n", "lookalike"),
		// A forged trusted-looking header below the first untrusted one is ignored.
		"forged below": rawMail("Authentication-Results: relay.example.org; dmarc=pass header.from=example.com\n"+
			"Authentication-Results: mx.example.net; dmarc=pass header.from=example.com\n", "forged below"),
		// The trusted server's verdict wins over a forged pass it failed to strip.
		"none over forged pass": rawMail("Authentication-Results: mx.example.net; dmarc=none header.from=example.com\n"+
			"Authentication-Results: mx.example.net; dmarc=pass header.from=example.com\n", "none over pass"),
		// Results split over several headers: a fail in the second one counts.
		"split fail": rawMail("Authentication-Results: mx.example.net; dkim=pass header.d=example.com\n"+
			"Authentication-Results: mx.example.net; dmarc=fail (p=reject) header.from=example.com\n", "split"),
		"pass for another domain":  rawMail("Authentication-Results: mx.example.net; dmarc=pass header.from=attacker.example\n", "other domain"),
		"pass without header.from": rawMail("Authentication-Results: mx.example.net; dmarc=pass\n", "no header.from"),
		"malformed trusted":        rawMail("Authentication-Results: mx.example.net; dmarc=pass (unclosed header.from=example.com\n", "malformed"),
		"duplicate property":       rawMail("Authentication-Results: mx.example.net; dmarc=pass header.from=attacker.example header.from=example.com\n", "dup"),
		"two From fields": crlf(arPass("alice@example.com") + "From: alice@example.com\nFrom: mallory@attacker.example\n" +
			"Subject: two froms\n\nx\n"),
		"spoof with attachments": withoutAuthResults(withAttachment("alice@example.com", "Report")),
	}
	uids := map[string]uint32{}
	for name, raw := range spoofs {
		uids[name] = srv.Add("INBOX", raw)
	}
	good := srv.Add("INBOX", eml("Alice <alice@EXAMPLE.com>", "genuine", "hello"))

	res := e.poll(t, e.cfg(srv))
	if res.imported != 1 || res.skipped != len(spoofs) {
		t.Fatalf("result %+v (want 1 imported, %d skipped)", res, len(spoofs))
	}
	notes := e.notes.all()
	if len(notes) != 1 || notes[0].userID != alice || notes[0].title != "genuine" {
		t.Fatalf("imported %+v", notes)
	}
	if names := e.files.names(); len(names) != 0 {
		t.Fatalf("attachments of an unauthenticated message were saved: %v", names)
	}
	got := e.processed(t)
	for name, uid := range uids {
		if got[uid] != "skipped" {
			t.Errorf("%s: status %q, want skipped", name, got[uid])
		}
	}
	if got[good] != "done" {
		t.Fatalf("genuine message: %q", got[good])
	}
}

// TestMailInFailsClosedWhenUnconfigured: without a trusted server (and without the explicit
// opt-out) mail-in does not even open the mailbox, so nothing is lost: the messages are
// imported once the admin configures the check.
func TestMailInFailsClosedWhenUnconfigured(t *testing.T) {
	e := newMailEnv(t)
	e.Patch(MailInSettingsNS, map[string]any{"trusted_authserv_ids": []string{}})
	e.user(t, "alice", true, true, false)
	srv := mailtest.StartIMAP(t, mailtest.IMAPOptions{User: "inbox", Pass: testPass})
	uid := srv.Add("INBOX", eml("alice@example.com", "waiting", "x"))
	if r := e.poll(t, e.cfg(srv)); r != (pollResult{}) || len(srv.Commands()) != 0 || srv.Seen("INBOX", uid) {
		t.Fatalf("unconfigured poll: %+v, %d commands", r, len(srv.Commands()))
	}
	e.Patch(MailInSettingsNS, map[string]any{"trusted_authserv_ids": []string{trustedMX}})
	if r := e.poll(t, e.cfg(srv)); r.imported != 1 {
		t.Fatalf("after configuring: %+v", r)
	}
}

// TestMailInAllowUnauthenticatedSenders keeps the old behaviour behind an explicit opt-out:
// only an explicit topmost dmarc=fail is skipped.
func TestMailInAllowUnauthenticatedSenders(t *testing.T) {
	e := newMailEnv(t)
	e.Patch(MailInSettingsNS, map[string]any{"trusted_authserv_ids": []string{}, "allow_unauthenticated_senders": true})
	e.user(t, "alice", true, true, false)
	srv := mailtest.StartIMAP(t, mailtest.IMAPOptions{User: "inbox", Pass: testPass})
	srv.Add("INBOX", rawMail("", "no header"))
	srv.Add("INBOX", rawMail("Authentication-Results: mx; dmarc=fail\n", "failed"))
	if r := e.poll(t, e.cfg(srv)); r.imported != 1 || r.skipped != 1 {
		t.Fatalf("%+v", r)
	}
}

func TestMailInTrustedIDsAndSettings(t *testing.T) {
	e := newMailEnv(t)
	alice := e.user(t, "alice", true, true, false)
	// Wildcards match subdomains (providers stamp from several MX hosts), not the apex.
	e.Patch(MailInSettingsNS, map[string]any{"trusted_authserv_ids": []string{" *.MessagingEngine.com. ", "*.messagingengine.com", "mx.google.com"}})
	var s MailInSettings
	if err := e.Deps.Settings.Get(context.Background(), MailInSettingsNS, &s); err != nil {
		t.Fatal(err)
	}
	if strings.Join(s.TrustedAuthservIDs, ",") != "*.messagingengine.com,mx.google.com" {
		t.Fatalf("normalised ids %q", s.TrustedAuthservIDs)
	}
	srv := mailtest.StartIMAP(t, mailtest.IMAPOptions{User: "inbox", Pass: testPass})
	srv.Add("INBOX", rawMail("Authentication-Results: mx6.messagingengine.com; dmarc=pass (p=none) header.from=example.com\n", "fastmail"))
	// Comments, quoted strings, a version number and odd case are all fine.
	srv.Add("INBOX", rawMail(`Authentication-Results: (received from x) "MX.Google.COM" 1; dkim=pass header.i=@example.com header.b="Ab1+/x=";`+
		` dmarc = pass (p=REJECT; "sp" (nested) =REJECT) header.from=Example.COM.`+"\n", "gmail"))
	srv.Add("INBOX", rawMail("Authentication-Results: messagingengine.com; dmarc=pass header.from=example.com\n", "apex"))
	srv.Add("INBOX", rawMail("Authentication-Results: evilmessagingengine.com; dmarc=pass header.from=example.com\n", "suffix"))
	if r := e.poll(t, e.cfg(srv)); r.imported != 2 || r.skipped != 2 {
		t.Fatalf("%+v", r)
	}
	for _, n := range e.notes.all() {
		if n.userID != alice || (n.title != "fastmail" && n.title != "gmail") {
			t.Fatalf("imported %+v", n)
		}
	}

	for _, bad := range [][]string{{"mx.example.net; dmarc=pass"}, {"*"}, {"*.com"}, {"*.example..net"}, {"exa mple.net"}, {""}, {"mx_1.example.net"}} {
		if err := e.Deps.Settings.Update(context.Background(), MailInSettingsNS, []byte(`{"trusted_authserv_ids":`+jsonList(bad)+`}`)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func jsonList(v []string) string {
	q := make([]string, len(v))
	for i, s := range v {
		q[i] = `"` + s + `"`
	}
	return "[" + strings.Join(q, ",") + "]"
}

func TestParseAuthResults(t *testing.T) {
	id, res, err := parseAuthResults(`mx.google.com;
       dkim=pass header.i=@example.com header.s=20230601 header.b=VHZtqQ1+;
       spf=pass (google.com: domain of a@example.com designates 2001:db8::1 as permitted sender) smtp.mailfrom=a@example.com;
       dmarc=pass (p=REJECT sp=REJECT dis=NONE) header.from=example.com`)
	if err != nil || id != "mx.google.com" || len(res) != 3 {
		t.Fatalf("%q %+v %v", id, res, err)
	}
	if r := res[2]; r.method != "dmarc" || r.result != "pass" || r.props["header.from"] != "example.com" {
		t.Fatalf("%+v", r)
	}
	if r := res[0]; r.props["header.b"] != "VHZtqQ1+" || r.props["header.i"] != "@example.com" {
		t.Fatalf("%+v", r)
	}
	if id, res, err := parseAuthResults("mail.example.net 1; none"); err != nil || id != "mail.example.net" || len(res) != 0 {
		t.Fatalf("none: %q %v %v", id, res, err)
	}
	if _, res, err := parseAuthResults(`mx; auth/1 = pass reason="ok; really" smtp.auth=x`); err != nil || res[0].method != "auth" || res[0].props["reason"] != "ok; really" {
		t.Fatalf("method version / quoted reason: %+v %v", res, err)
	}
	for _, bad := range []string{"", "mx", "mx;", "mx; dmarc", "mx; dmarc=", "mx; dmarc=pass header.from", "mx; dmarc=pass (x",
		"mx; dmarc=pass x)", `mx; dmarc=pass reason="open`, "mx two; dmarc=pass", strings.Repeat("x", 20000)} {
		if _, _, err := parseAuthResults(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	// The id is reported even when the results are malformed (so the caller knows whose
	// header it is).
	if id, _, err := parseAuthResults("mx.example.net; dmarc"); err == nil || id != "mx.example.net" {
		t.Fatalf("%q %v", id, err)
	}
}
