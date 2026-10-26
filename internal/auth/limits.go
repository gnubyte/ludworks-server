package auth

import (
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/httpx"
	"ludworks/internal/settings"
)

// rateLimited renders 429 with Retry-After (whole seconds, rounded up).
func rateLimited(w http.ResponseWriter, d time.Duration) error {
	secs := max(int(math.Ceil(d.Seconds())), 1)
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	return core.ErrRateLimited
}

// throttle checks an exponential lockout (when lock is non-nil) and takes a token from a
// bucket (when l is non-nil), answering 429 when either refuses.
func (m *Module) throttle(w http.ResponseWriter, l *httpx.KeyedLimiter, limitKey string, lock *httpx.Lockout, lockKey string) error {
	if lock != nil {
		if d := lock.Check(lockKey); d > 0 {
			return rateLimited(w, d)
		}
	}
	if l != nil {
		if ok, wait := l.Allow(limitKey); !ok {
			return rateLimited(w, wait)
		}
	}
	return nil
}

var errCrossSite = core.Forbidden("Cross-origin request rejected")

// checkBrowserOrigin rejects cookie-establishing requests (login, register) coming from
// another site, which would otherwise allow login CSRF. Requests without browser
// provenance headers (SDKs, curl) pass; they cannot be forged by a third-party page.
func checkBrowserOrigin(r *http.Request, g settings.General) error {
	if strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
		return errCrossSite
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return nil
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return errCrossSite // includes the opaque "null" origin
	}
	meta, _ := core.RequestMetaFrom(r.Context())
	if meta.Secure && u.Scheme != "https" {
		return errCrossSite
	}
	if strings.EqualFold(u.Host, r.Host) {
		return nil
	}
	if mu, err := url.Parse(meta.Origin); err == nil && mu.Host != "" && strings.EqualFold(u.Host, mu.Host) {
		return nil
	}
	if g.PublicURL != "" {
		if pu, err := url.Parse(g.PublicURL); err == nil && strings.EqualFold(pu.Host, u.Host) && strings.EqualFold(pu.Scheme, u.Scheme) {
			return nil
		}
	}
	return errCrossSite
}
