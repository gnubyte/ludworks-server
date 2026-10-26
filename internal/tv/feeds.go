package tv

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/xml"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"ludworks/internal/auth"
	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/httpx"
	"ludworks/internal/tv/schedule"
)

// Feed token scopes (API.md §7: a dedicated lwp_ token with tv:read only). Every feed,
// program, live.ts and HDHomeRun route needs nothing more: program bytes are served with the
// channel owner's permissions (ownerPrincipal), not the caller's media scope. media:read is
// deliberately absent, so a feed URL leaked from an IPTV app or a proxy log cannot list or
// stream the library outside the user's channels.
var feedScopes = []string{"tv:read"}

// hasFeedScopes reports whether p may mint or reveal the feed token (which must not carry
// more than the credential asking for it).
func hasFeedScopes(p *core.Principal) bool {
	for _, s := range feedScopes {
		if !p.Has(s) {
			return false
		}
	}
	return true
}

// isFeedScopes reports whether a token carries exactly feedScopes.
func isFeedScopes(scopes []string) bool {
	if len(scopes) != len(feedScopes) {
		return false
	}
	for i := range scopes {
		if scopes[i] != feedScopes[i] {
			return false
		}
	}
	return true
}

const feedTokenName = "TV feeds (M3U, XMLTV, HDHomeRun)"

func feedAAD(userID, tokenID string) string { return "tv-feed:" + userID + ":" + tokenID }

// Feeds is the GET /tv/feeds response.
type Feeds struct {
	M3UURL   string  `json:"m3u_url"`
	XMLTVURL string  `json:"xmltv_url"`
	HDHRURL  *string `json:"hdhr_url"`
}

// errFeedScope: minting or revealing a feed token (which carries tv:read) must not widen what
// the caller's own credential may do.
var errFeedScope = core.Forbidden("Feed URLs need a credential with the tv:read scope")

// feedToken returns the user's feed token, minting a new one when there is none, when it was
// revoked (e.g. from the account's token list), when its scopes are not exactly feedScopes
// (migration 0002 narrows older media:read tokens in place) or when rotate is set (the old
// one is revoked).
func (m *Module) feedToken(ctx context.Context, userID string, rotate bool) (string, error) {
	m.feedMu.Lock()
	defer m.feedMu.Unlock()
	cipher := m.d.Settings.Cipher()
	var tokenID, enc string
	err := m.d.DB.SQL.QueryRowContext(ctx, `SELECT token_id, token_enc FROM tv_feed_tokens WHERE user_id = ?`, userID).Scan(&tokenID, &enc)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		tokenID = ""
	case err != nil:
		return "", err
	case !rotate:
		if pt, err := cipher.Open(enc, feedAAD(userID, tokenID)); err == nil {
			p, err := auth.VerifyToken(ctx, m.d, string(pt))
			if err == nil && p != nil && p.UserID == userID && p.SessionID == tokenID && isFeedScopes(p.Scopes) {
				return string(pt), nil
			}
		}
	}
	if tokenID != "" {
		if err := auth.RevokeToken(ctx, m.d, userID, tokenID); err != nil {
			var ce *core.Error
			if !errors.As(err, &ce) || ce.Status != http.StatusNotFound {
				return "", err
			}
		}
	}
	info, plain, err := auth.IssueToken(ctx, m.d, userID, feedTokenName, feedScopes, nil)
	if err != nil {
		return "", err
	}
	sealed, err := cipher.Seal([]byte(plain), feedAAD(userID, info.ID))
	if err == nil {
		_, err = m.d.DB.SQL.ExecContext(ctx, `INSERT INTO tv_feed_tokens (user_id, token_id, token_enc, created_at)
			VALUES (?, ?, ?, ?) ON CONFLICT (user_id) DO UPDATE SET token_id = excluded.token_id,
			token_enc = excluded.token_enc, created_at = excluded.created_at`,
			userID, info.ID, sealed, db.FormatTime(m.now()))
	}
	if err != nil {
		_ = auth.RevokeToken(context.WithoutCancel(ctx), m.d, userID, info.ID)
		return "", err
	}
	return plain, nil
}

// baseURL is the absolute origin used in feed URLs: general.public_url when configured,
// otherwise the origin the request reached us on.
func (m *Module) baseURL(ctx context.Context) string {
	if g, err := m.d.Settings.General(ctx); err == nil && g.PublicURL != "" {
		if u, err := url.Parse(g.PublicURL); err == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https") {
			return strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/")
		}
	}
	meta, _ := core.RequestMetaFrom(ctx)
	return meta.Origin
}

func (m *Module) tvSettings(ctx context.Context) Settings {
	s := DefaultSettings
	if err := m.d.Settings.Get(ctx, "tv", &s); err != nil {
		m.log.Warn("reading tv settings failed", "err", err)
		return DefaultSettings
	}
	return s
}

func (m *Module) feedsFor(ctx context.Context, tok string) Feeds {
	base := m.baseURL(ctx)
	q := "?access_token=" + url.QueryEscape(tok)
	f := Feeds{M3UURL: base + "/api/v1/tv/channels.m3u" + q, XMLTVURL: base + "/api/v1/tv/xmltv.xml" + q}
	if m.tvSettings(ctx).HDHREnabled {
		u := base + "/api/v1/hdhr/" + url.PathEscape(tok)
		f.HDHRURL = &u
	}
	return f
}

func (m *Module) getFeeds(w http.ResponseWriter, r *http.Request) error {
	p := core.MustPrincipal(r.Context())
	if !hasFeedScopes(p) {
		return errFeedScope
	}
	tok, err := m.feedToken(r.Context(), p.UserID, false)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, m.feedsFor(r.Context(), tok))
}

func (m *Module) rotateFeeds(w http.ResponseWriter, r *http.Request) error {
	p := core.MustPrincipal(r.Context())
	if !hasFeedScopes(p) {
		return errFeedScope
	}
	tok, err := m.feedToken(r.Context(), p.UserID, true)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, m.feedsFor(r.Context(), tok))
}

// embedToken picks the token to embed in a playlist's URLs: the query token the request
// itself used (it already proved it can read these feeds), otherwise the user's feed token.
func (m *Module) embedToken(r *http.Request, p *core.Principal) (string, error) {
	if tok := r.URL.Query().Get("access_token"); tok != "" && r.Header.Get("Authorization") == "" && p.Kind == core.KindToken {
		return tok, nil
	}
	if !hasFeedScopes(p) {
		return "", errFeedScope
	}
	return m.feedToken(r.Context(), p.UserID, false)
}

// m3uAttr makes a value safe inside an M3U attribute (no quotes, no line breaks).
func m3uAttr(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '"':
			return '\''
		case r == '\r' || r == '\n' || r < 0x20 || r == 0x7f:
			return ' '
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}

// streamURL is the URL external players use for a channel.
func streamURL(base, channelID, tokQuery string, ffmpeg bool) string {
	if ffmpeg {
		return base + "/api/v1/tv/channels/" + channelID + "/live.ts" + tokQuery
	}
	return base + "/api/v1/tv/channels/" + channelID + "/play" + tokQuery
}

func (m *Module) m3u(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	tok, err := m.embedToken(r, p)
	if err != nil {
		return err
	}
	rows, err := visibleChannels(ctx, m.d.DB.SQL, p.UserID)
	if err != nil {
		return err
	}
	base := m.baseURL(ctx)
	tq := "?access_token=" + url.QueryEscape(tok)
	ff := m.ffmpeg(ctx) != ""
	var b bytes.Buffer
	xmltv := base + "/api/v1/tv/xmltv.xml" + tq
	b.WriteString("#EXTM3U url-tvg=\"" + xmltv + "\" x-tvg-url=\"" + xmltv + "\"\n")
	for _, c := range rows {
		b.WriteString(`#EXTINF:-1 tvg-id="` + c.ID + `" tvg-chno="` + strconv.Itoa(c.Number) + `" tvg-name="` + m3uAttr(c.Name) + `"`)
		if c.LogoVersion != "" {
			b.WriteString(` tvg-logo="` + base + "/api/v1/tv/channels/" + c.ID + "/logo?v=" + c.LogoVersion + "&access_token=" + url.QueryEscape(tok) + `"`)
		}
		group := c.Group
		if group == "" {
			group = "Ludworks"
		}
		b.WriteString(` group-title="` + m3uAttr(group) + `",` + strconv.Itoa(c.Number) + " " + m3uAttr(c.Name) + "\n")
		b.WriteString(streamURL(base, c.ID, tq, ff) + "\n")
	}
	h := w.Header()
	h.Set("Content-Type", "audio/x-mpegurl; charset=utf-8")
	h.Set("Content-Disposition", `inline; filename="ludworks.m3u"`)
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	_, err = w.Write(b.Bytes())
	return err
}

// xmlText escapes s as XML character data (invalid characters become U+FFFD).
func xmlText(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// xmltvTime formats t as an XMLTV timestamp ("20261026120000 +0000").
func xmltvTime(t time.Time) string { return t.UTC().Format("20060102150405") + " +0000" }

func (m *Module) xmltv(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	tok, err := m.embedToken(r, p)
	if err != nil {
		return err
	}
	rows, err := visibleChannels(ctx, m.d.DB.SQL, p.UserID)
	if err != nil {
		return err
	}
	days := m.tvSettings(ctx).GuideDays
	now := m.now()
	from := now.Truncate(time.Hour)
	to := from.Add(time.Duration(days) * 24 * time.Hour)
	base := m.baseURL(ctx)
	tq := "&access_token=" + url.QueryEscape(tok)

	h := w.Header()
	h.Set("Content-Type", "application/xml; charset=utf-8")
	h.Set("Content-Disposition", `inline; filename="ludworks-xmltv.xml"`)
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	bw := bufio.NewWriterSize(w, 64<<10)
	bw.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<!DOCTYPE tv SYSTEM "xmltv.dtd">` + "\n")
	bw.WriteString(`<tv generator-info-name="Ludworks" source-info-name="Ludworks TV">` + "\n")
	for _, c := range rows {
		bw.WriteString(`  <channel id="` + xmlText(c.ID) + `">` + "\n")
		bw.WriteString(`    <display-name>` + xmlText(c.Name) + `</display-name>` + "\n")
		bw.WriteString(`    <display-name>` + strconv.Itoa(c.Number) + `</display-name>` + "\n")
		if c.LogoVersion != "" {
			bw.WriteString(`    <icon src="` + xmlText(base+"/api/v1/tv/channels/"+c.ID+"/logo?v="+c.LogoVersion+tq) + `"/>` + "\n")
		}
		bw.WriteString("  </channel>\n")
	}
	for _, c := range rows {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		exp, err := m.expansionFor(ctx, c.LineupID, c.OwnerID)
		if err != nil {
			return err
		}
		slots := schedule.Guide(exp.items, scheduleChannel(c), from, to)
		if len(slots) > MaxXMLTVPerChannel {
			slots = slots[:MaxXMLTVPerChannel]
		}
		for _, s := range slots {
			f := exp.byID[s.FileID]
			if f == nil {
				continue
			}
			bw.WriteString(`  <programme start="` + xmltvTime(s.Start) + `" stop="` + xmltvTime(s.End) + `" channel="` + xmlText(c.ID) + `">` + "\n")
			title := f.Title
			if f.ShowTitle != "" {
				title = f.ShowTitle
			}
			bw.WriteString(`    <title lang="en">` + xmlText(title) + `</title>` + "\n")
			sub := ""
			if f.ShowTitle != "" {
				sub = episodeSubtitle(f)
			}
			if sub != "" {
				bw.WriteString(`    <sub-title lang="en">` + xmlText(sub) + `</sub-title>` + "\n")
			}
			bw.WriteString(`    <length units="seconds">` + strconv.FormatInt(int64(s.End.Sub(s.Start).Seconds()), 10) + `</length>` + "\n")
			if f.Season != nil && f.Episode != nil && *f.Season >= 0 && *f.Episode >= 1 {
				if *f.Season >= 1 { // xmltv_ns is zero-based; specials (season 0) have no xmltv_ns form
					bw.WriteString(`    <episode-num system="xmltv_ns">` + strconv.Itoa(*f.Season-1) + "." + strconv.Itoa(*f.Episode-1) + ".</episode-num>\n")
				}
				bw.WriteString(`    <episode-num system="onscreen">S` + pad2(*f.Season) + "E" + pad2(*f.Episode) + "</episode-num>\n")
			}
			if f.ShowTitle == "" {
				bw.WriteString("    <category lang=\"en\">Movie</category>\n")
			}
			bw.WriteString("  </programme>\n")
		}
	}
	bw.WriteString("</tv>\n")
	return bw.Flush()
}

func pad2(n int) string {
	if n >= 0 && n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}
