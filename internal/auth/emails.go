package auth

import (
	"context"
	"database/sql"
	"fmt"
	"html"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"ludworks/internal/core"
)

const (
	resetTokenTTL  = time.Hour
	verifyTokenTTL = 48 * time.Hour
	// verifyResendGap throttles verification emails triggered by login attempts.
	verifyResendGap = 10 * time.Minute
)

// linkBase returns the origin used in emailed links. general.public_url wins; otherwise the
// request's own origin is used only when its host demonstrably belongs to this server, so
// a forged Host header cannot make the server email a reset link pointing elsewhere.
func (m *Module) linkBase(ctx context.Context) (string, bool) {
	if g := m.general(ctx); g.PublicURL != "" {
		return g.PublicURL, true
	}
	meta, ok := core.RequestMetaFrom(ctx)
	if !ok || meta.Origin == "" {
		return "", false
	}
	u, err := url.Parse(meta.Origin)
	if err != nil || u.Host == "" || !m.isOwnHost(u.Hostname()) {
		return "", false
	}
	return u.Scheme + "://" + u.Host, true
}

// isOwnHost reports whether h names this machine: localhost or a *.localhost name (RFC 6761
// reserves both for loopback, so a forged one can only point a link back at the clicker's own
// machine), its hostname, one of its interface addresses, or a name in the served TLS certificate.
func (m *Module) isOwnHost(h string) bool {
	h = strings.ToLower(strings.TrimSuffix(h, "."))
	if h == "" {
		return false
	}
	if h == "localhost" || (strings.HasSuffix(h, ".localhost") && len(h) > len(".localhost")) {
		return true
	}
	if hn, err := os.Hostname(); err == nil && hn != "" {
		hn = strings.ToLower(hn)
		if h == hn || h == hn+".local" {
			return true
		}
	}
	if ip := net.ParseIP(h); ip != nil {
		if ip.IsLoopback() {
			return true
		}
		if addrs, err := net.InterfaceAddrs(); err == nil {
			for _, a := range addrs {
				if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
					return true
				}
			}
		}
	}
	if c := m.d.Info.CurrentTLS(); c != nil {
		for _, d := range c.DNSNames {
			if strings.EqualFold(d, h) {
				return true
			}
		}
		for _, s := range c.IPAddresses {
			if ip := net.ParseIP(s); ip != nil && ip.Equal(net.ParseIP(h)) {
				return true
			}
		}
	}
	return false
}

// createEmailToken stores a new single-use token for u's current address, replacing earlier
// unused ones of the same purpose, and returns the plaintext.
func (m *Module) createEmailToken(ctx context.Context, u *userRow, purpose string, ttl time.Duration) (string, error) {
	return m.createEmailTokenFor(ctx, u.ID, purpose, u.Email, ttl)
}

// createEmailTokenFor is createEmailToken for an explicit address (the new address of an
// email change).
func (m *Module) createEmailTokenFor(ctx context.Context, userID, purpose, email string, ttl time.Duration) (string, error) {
	tok := randomToken()
	now := m.now()
	err := m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM email_tokens WHERE user_id = ? AND purpose = ?`, userID, purpose); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO email_tokens (id, user_id, purpose, token_hash, email, created_at, expires_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, core.NewID(), userID, purpose, hashToken(tok), email, fmtTime(now), fmtTime(now.Add(ttl)))
		return err
	})
	return tok, err
}

// requestEmailChange runs the account-dependent part of a self-service email change in the
// background, so neither the response nor its timing depends on whether newEmail is taken:
// the current address is told about the request, and the new address gets a confirmation
// link only when no other account uses it (the redeemer re-checks that).
func (m *Module) requestEmailChange(ctx context.Context, u *userRow, newEmail, base string) {
	meta, _ := core.RequestMetaFrom(ctx)
	user := *u
	actor := &core.Principal{UserID: u.ID, Username: u.Username, Role: u.Role}
	m.d.Jobs.Go("auth.email-change", func(jctx context.Context) error {
		jctx = core.WithRequestMeta(jctx, meta)
		taken, err := emailTaken(jctx, m.d.DB.SQL, newEmail, user.ID)
		if err != nil {
			return err
		}
		m.audit(jctx, actor, "account.email_change_requested", "user", user.ID, map[string]any{"address_in_use": taken})
		server := m.general(jctx).ServerName
		if err := m.d.Mailer.Send(jctx, emailChangeNoticeMessage(server, &user, newEmail)); err != nil {
			m.log.Warn("email change notice not sent", "user_id", user.ID, "err", err)
		}
		if taken {
			return nil
		}
		tok, err := m.createEmailTokenFor(jctx, user.ID, "email-change", newEmail, verifyTokenTTL)
		if err != nil {
			return err
		}
		link := base + "/verify-email?token=" + url.QueryEscape(tok)
		return m.d.Mailer.Send(jctx, emailChangeConfirmMessage(server, &user, newEmail, link))
	})
}

// notifyAccountExists tells the owner of email that someone tried to register a new account
// with it (the registration itself was answered like a successful one; see register). At
// most a few notices per address are sent, silently, so this cannot be used to flood a mailbox.
func (m *Module) notifyAccountExists(ctx context.Context, email string) {
	if ok, _ := m.resetMail.Allow("exists|" + strings.ToLower(email)); !ok {
		return
	}
	meta, _ := core.RequestMetaFrom(ctx)
	base, _ := m.linkBase(ctx)
	m.d.Jobs.Go("auth.register-conflict", func(jctx context.Context) error {
		jctx = core.WithRequestMeta(jctx, meta)
		if !m.d.Mailer.Enabled(jctx) {
			return nil
		}
		u, err := scanUser(m.d.DB.SQL.QueryRowContext(jctx, `SELECT `+userCols+` FROM users WHERE email = ?`, email))
		if isNoRows(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if u.Disabled {
			return nil
		}
		return m.d.Mailer.Send(jctx, accountExistsMessage(m.general(jctx).ServerName, u, base))
	})
}

// sendVerification emails a verification link in the background (no-op without SMTP or a
// trustworthy link origin).
func (m *Module) sendVerification(ctx context.Context, u *userRow) {
	base, ok := m.linkBase(ctx)
	if !ok {
		m.log.Warn("verification email not sent: set general.public_url so links can be built", "user_id", u.ID)
		return
	}
	meta, _ := core.RequestMetaFrom(ctx)
	user := *u
	m.d.Jobs.Go("auth.verify-email", func(jctx context.Context) error {
		jctx = core.WithRequestMeta(jctx, meta)
		if !m.d.Mailer.Enabled(jctx) {
			m.log.Warn("verification email not sent: SMTP is not configured", "user_id", user.ID)
			return nil
		}
		tok, err := m.createEmailToken(jctx, &user, "verify", verifyTokenTTL)
		if err != nil {
			return err
		}
		link := base + "/verify-email?token=" + url.QueryEscape(tok)
		return m.d.Mailer.Send(jctx, verifyMessage(m.general(jctx).ServerName, &user, link))
	})
}

// resendVerification re-sends the verification email after a login attempt by an
// unverified account, at most once per verifyResendGap.
func (m *Module) resendVerification(ctx context.Context, u *userRow) {
	var n int
	err := m.d.DB.SQL.QueryRowContext(ctx, `SELECT count(*) FROM email_tokens WHERE user_id = ? AND purpose = 'verify'
		AND used_at IS NULL AND created_at > ?`, u.ID, fmtTime(m.now().Add(-verifyResendGap))).Scan(&n)
	if err != nil || n > 0 {
		return
	}
	m.sendVerification(ctx, u)
}

// processResetRequest runs in the background for POST /auth/password-reset/request.
func (m *Module) processResetRequest(ctx context.Context, email, base string) error {
	u, err := scanUser(m.d.DB.SQL.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE email = ?`, email))
	if isNoRows(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if u.Disabled {
		return nil
	}
	tok, err := m.createEmailToken(ctx, u, "reset", resetTokenTTL)
	if err != nil {
		return err
	}
	link := base + "/reset-password?token=" + url.QueryEscape(tok)
	if err := m.d.Mailer.Send(ctx, resetMessage(m.general(ctx).ServerName, u, link)); err != nil {
		return fmt.Errorf("auth: send reset email: %w", err)
	}
	m.audit(ctx, &core.Principal{UserID: u.ID, Username: u.Username}, "auth.password_reset_requested", "user", u.ID, nil)
	return nil
}

func greetingName(u *userRow) string {
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return u.Username
}

func resetMessage(server string, u *userRow, link string) core.MailMessage {
	text := fmt.Sprintf(`Hello %s,

Someone (hopefully you) asked to reset the password of your %s account "%s".
To choose a new password, open this link within one hour:

%s

If you did not ask for this, ignore this email: your password stays unchanged.
`, greetingName(u), server, u.Username, link)
	htm := fmt.Sprintf(`<!doctype html><html><body style="font-family:sans-serif">
<p>Hello %s,</p>
<p>Someone (hopefully you) asked to reset the password of your %s account &ldquo;%s&rdquo;.
To choose a new password, open this link within one hour:</p>
<p><a href="%s">Reset my password</a></p>
<p>If you did not ask for this, ignore this email: your password stays unchanged.</p>
</body></html>`, html.EscapeString(greetingName(u)), html.EscapeString(server), html.EscapeString(u.Username), html.EscapeString(link))
	return core.MailMessage{To: []string{u.Email}, Subject: "Reset your " + server + " password", Text: text, HTML: htm}
}

func emailChangeConfirmMessage(server string, u *userRow, newEmail, link string) core.MailMessage {
	text := fmt.Sprintf(`Hello %s,

Please confirm that this address should become the email address of your %s account "%s"
by opening this link:

%s

The link is valid for 48 hours. Until then your account keeps its current address. If you
did not ask for this, ignore this email.
`, greetingName(u), server, u.Username, link)
	htm := fmt.Sprintf(`<!doctype html><html><body style="font-family:sans-serif">
<p>Hello %s,</p>
<p>Please confirm that this address should become the email address of your %s account &ldquo;%s&rdquo;:</p>
<p><a href="%s">Confirm my new email address</a></p>
<p>The link is valid for 48 hours. Until then your account keeps its current address. If you did not ask for this, ignore this email.</p>
</body></html>`, html.EscapeString(greetingName(u)), html.EscapeString(server), html.EscapeString(u.Username), html.EscapeString(link))
	return core.MailMessage{To: []string{newEmail}, Subject: "Confirm your new email address for " + server, Text: text, HTML: htm}
}

func emailChangeNoticeMessage(server string, u *userRow, newEmail string) core.MailMessage {
	text := fmt.Sprintf(`Hello %s,

Someone signed in to your %s account "%s" asked to change its email address to
%s. The change only happens once that address confirms it.

If this was not you, change your password and sign out your other sessions right away.
`, greetingName(u), server, u.Username, newEmail)
	htm := fmt.Sprintf(`<!doctype html><html><body style="font-family:sans-serif">
<p>Hello %s,</p>
<p>Someone signed in to your %s account &ldquo;%s&rdquo; asked to change its email address to
%s. The change only happens once that address confirms it.</p>
<p>If this was not you, change your password and sign out your other sessions right away.</p>
</body></html>`, html.EscapeString(greetingName(u)), html.EscapeString(server), html.EscapeString(u.Username), html.EscapeString(newEmail))
	return core.MailMessage{To: []string{u.Email}, Subject: "Email change requested for your " + server + " account", Text: text, HTML: htm}
}

func accountExistsMessage(server string, u *userRow, base string) core.MailMessage {
	where := "the sign-in page"
	whereHTML := where
	if base != "" {
		where = base + "/login"
		whereHTML = fmt.Sprintf(`<a href="%s">the sign-in page</a>`, html.EscapeString(where))
	}
	text := fmt.Sprintf(`Hello %s,

Someone tried to create a new %s account with this email address, but it already belongs
to your account "%s", so no new account was created.

If that was you, sign in at %s instead; if you forgot your password, you can reset it there.
Otherwise you can ignore this email.
`, greetingName(u), server, u.Username, where)
	htm := fmt.Sprintf(`<!doctype html><html><body style="font-family:sans-serif">
<p>Hello %s,</p>
<p>Someone tried to create a new %s account with this email address, but it already belongs
to your account &ldquo;%s&rdquo;, so no new account was created.</p>
<p>If that was you, sign in at %s instead; if you forgot your password, you can reset it there.
Otherwise you can ignore this email.</p>
</body></html>`, html.EscapeString(greetingName(u)), html.EscapeString(server), html.EscapeString(u.Username), whereHTML)
	return core.MailMessage{To: []string{u.Email}, Subject: "Sign-up attempt with your email address on " + server, Text: text, HTML: htm}
}

func verifyMessage(server string, u *userRow, link string) core.MailMessage {
	text := fmt.Sprintf(`Hello %s,

Please confirm that this is the email address of your %s account "%s" by opening this link:

%s

The link is valid for 48 hours. If you did not create this account, ignore this email.
`, greetingName(u), server, u.Username, link)
	htm := fmt.Sprintf(`<!doctype html><html><body style="font-family:sans-serif">
<p>Hello %s,</p>
<p>Please confirm that this is the email address of your %s account &ldquo;%s&rdquo;:</p>
<p><a href="%s">Confirm my email address</a></p>
<p>The link is valid for 48 hours. If you did not create this account, ignore this email.</p>
</body></html>`, html.EscapeString(greetingName(u)), html.EscapeString(server), html.EscapeString(u.Username), html.EscapeString(link))
	return core.MailMessage{To: []string{u.Email}, Subject: "Confirm your email address for " + server, Text: text, HTML: htm}
}
