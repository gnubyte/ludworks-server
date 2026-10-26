// Package httpx is the HTTP layer shared by all modules: the Router (access rules,
// authentication, CSRF, CORS for browser extensions, setup gate, rate limiting, security
// headers, logging, panic recovery), JSON helpers, error rendering, signed cursors and the
// Module interface.
package httpx

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/settings"
)

// Module is implemented by every feature module and wired by internal/app.
type Module interface {
	Name() string                    // "files", "tv", ...
	Migrations() fs.FS               // NNNN_name.sql files at the FS root (may be nil)
	Routes(r *Router)                // register HTTP routes
	Start(ctx context.Context) error // start background workers; return quickly
	OpenAPI() []byte                 // {"tags":[],"paths":{},"components":{...}} fragment; may be nil
}

// AuthResult is what an Authenticator resolved for a request.
type AuthResult struct {
	Principal *core.Principal // nil = anonymous
	ViaCookie bool            // credential came from the session cookie (CSRF applies)
	CSRFToken string          // the session's CSRF token (cookie sessions only)
}

// Authenticator resolves credentials (implemented by the auth module).
//
// Contract: if an Authorization header is present it is the only credential considered (no
// fallback to the cookie). ?access_token= is consulted only when allowQueryToken is true.
// Return a zero AuthResult and nil error for "no / invalid / expired credentials"; return a
// *core.Error to send a specific response (e.g. 401 with a message, 403 for a disabled
// account); any other error is treated as an internal error (500).
type Authenticator interface {
	Authenticate(r *http.Request, allowQueryToken bool) (AuthResult, error)
}

// Access declares who may call a route.
type Access struct {
	Public          bool   // no auth required
	Scope           string // required scope, e.g. "files:read"; "" with !Public means "any authenticated"
	AltScope        string // with Scope: a narrower scope that is also accepted (e.g. "tv:feed" on feed/stream routes)
	Admin           bool   // requires Role admin (and scope "admin" for tokens)
	AllowQueryToken bool   // accept ?access_token= (streaming/feeds only)
	MaxBody         int64  // 0 → default 1 MiB; -1 → unlimited (handler enforces its own limit)
	NoCSRF          bool   // only for endpoints that are safe without it (e.g. login, setup)
}

// Public returns Access for unauthenticated routes.
func Public() Access { return Access{Public: true} }

// Authenticated returns Access for any authenticated principal ([auth] in API.md).
func Authenticated() Access { return Access{} }

// Scope returns Access requiring scope s.
func Scope(s string) Access { return Access{Scope: s} }

// AdminOnly returns Access requiring the admin role (and scope "admin" for tokens).
func AdminOnly() Access { return Access{Admin: true} }

// WithMaxBody returns a copy with the body limit set (-1 = unlimited).
func (a Access) WithMaxBody(n int64) Access { a.MaxBody = n; return a }

// WithQueryToken returns a copy that also accepts ?access_token=.
func (a Access) WithQueryToken() Access { a.AllowQueryToken = true; return a }

// WithAltScope returns a copy that also admits principals holding scope s instead of Scope.
// It lets a narrow scope (one that grants nothing else) reach a few routes of a wider one.
func (a Access) WithAltScope(s string) Access { a.AltScope = s; return a }

// WithoutCSRF returns a copy that skips CSRF checks.
func (a Access) WithoutCSRF() Access { a.NoCSRF = true; return a }

// HandlerFunc is a handler that returns an error: *core.Error is rendered as JSON; other
// errors become a logged 500 with a generic message.
type HandlerFunc func(w http.ResponseWriter, r *http.Request) error

// Options configures a Router.
type Options struct {
	Log            *slog.Logger
	Auth           Authenticator                  // nil: every non-public route answers 401
	SetupRequired  func(ctx context.Context) bool // nil: never
	TrustedProxies []netip.Prefix
	Settings       *settings.Store // extension-origin allow-list and public_url; nil → defaults
	RateLimit      RateLimitOptions
}

// Generic API rate limits (ARCHITECTURE.md §2.6). They only stop runaway or abusive clients;
// bulk work by a signed-in user (syncing, uploading, scrolling large libraries) must fit. The
// auth endpoints have their own, much stricter limiters and lockouts (internal/auth).
const (
	// AuthRateLimitRPS / AuthRateLimitBurst apply per authenticated principal (user).
	AuthRateLimitRPS   = 200
	AuthRateLimitBurst = 1000
	// AnonRateLimitRPS / AnonRateLimitBurst apply per client IP to unauthenticated requests.
	AnonRateLimitRPS   = 30
	AnonRateLimitBurst = 120
)

// RateLimitOptions configures the generic API limiter: per authenticated principal, else per
// client IP. Zero values select the constants above.
type RateLimitOptions struct {
	Disabled  bool
	AuthRPS   float64
	AuthBurst int
	AnonRPS   float64
	AnonBurst int
}

// rateLimitExempt lists the routes whose requests skip the generic limiter because each is
// one piece of a larger transfer that is bounded by the route's own locks and concurrency
// caps: tus upload chunks (PATCH, and the HEAD offset queries clients send to resume), and
// Range requests for file, media and recorded-program content (players and download managers
// issue many). The predicate decides per request; authentication and access rules still apply.
var rateLimitExempt = map[string]func(r *http.Request) bool{
	"PATCH /api/v1/uploads/{id}":                             always,
	"HEAD /api/v1/uploads/{id}":                              always,
	"GET /api/v1/files/{id}/content":                         isRangeGET,
	"GET /api/v1/tv/channels/{id}/programs/{file_id}/stream": isRangeGET,
	"GET /api/v1/media/{file_id}/{asset}": func(r *http.Request) bool {
		return r.PathValue("asset") == "stream" && isRangeGET(r) // not posters or transcodes
	},
}

// RateLimitExemptPatterns returns the route patterns rateLimitExempt names (for tests that
// check they are all registered).
func RateLimitExemptPatterns() []string {
	out := make([]string, 0, len(rateLimitExempt))
	for p := range rateLimitExempt {
		out = append(out, p)
	}
	return out
}

func always(*http.Request) bool { return true }

// isRangeGET reports a GET (not HEAD) asking for a byte range.
func isRangeGET(r *http.Request) bool {
	return r.Method == http.MethodGet && strings.HasPrefix(r.Header.Get("Range"), "bytes=")
}

// Router wraps http.ServeMux with the Ludworks access rules.
type Router struct {
	mux         *http.ServeMux
	log         *slog.Logger
	auth        Authenticator
	setup       func(ctx context.Context) bool
	proxies     []netip.Prefix
	settings    *settings.Store
	authLimiter *KeyedLimiter // per principal
	anonLimiter *KeyedLimiter // per client IP, unauthenticated requests
	fallback    http.Handler
}

// NewRouter creates a Router.
func NewRouter(o Options) *Router {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	rt := &Router{mux: http.NewServeMux(), log: o.Log, auth: o.Auth, setup: o.SetupRequired,
		proxies: o.TrustedProxies, settings: o.Settings}
	if rl := o.RateLimit; !rl.Disabled {
		orDefault := func(v, def float64) float64 {
			if v <= 0 {
				return def
			}
			return v
		}
		rt.authLimiter = NewKeyedLimiter(orDefault(rl.AuthRPS, AuthRateLimitRPS), int(orDefault(float64(rl.AuthBurst), AuthRateLimitBurst)))
		rt.anonLimiter = NewKeyedLimiter(orDefault(rl.AnonRPS, AnonRateLimitRPS), int(orDefault(float64(rl.AnonBurst), AnonRateLimitBurst)))
	}
	return rt
}

// RouteFor returns the registered pattern that would serve r ("" if none).
func (rt *Router) RouteFor(r *http.Request) string {
	_, p := rt.mux.Handler(r)
	return p
}

// SetFallback sets the handler for requests outside /api/ that match no route (the web UI).
func (rt *Router) SetFallback(h http.Handler) { rt.fallback = h }

// Handle registers h for pattern (Go 1.22 syntax including the method, e.g.
// "GET /api/v1/files/{id}"). Registration errors (missing method, duplicate or conflicting
// pattern) panic at startup.
func (rt *Router) Handle(pattern string, a Access, h HandlerFunc) {
	rt.HandleRaw(pattern, a, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			WriteError(w, r, err)
		}
	}))
}

// HandleRaw registers a plain http.Handler with the same access rules (no error wrapping).
func (rt *Router) HandleRaw(pattern string, a Access, h http.Handler) {
	method, path, ok := strings.Cut(strings.TrimSpace(pattern), " ")
	if !ok || method == "" || !strings.HasPrefix(strings.TrimSpace(path), "/") || strings.ToUpper(method) != method {
		panic(fmt.Sprintf("httpx: pattern %q must be \"METHOD /path\"", pattern))
	}
	if a.Public && (a.Admin || a.Scope != "" || a.AltScope != "") || (a.AltScope != "" && a.Scope == "") {
		panic(fmt.Sprintf("httpx: pattern %q: Public cannot be combined with Scope/Admin, and AltScope needs Scope", pattern))
	}
	rt.mux.Handle(pattern, rt.guard(a, rateLimitExempt[method+" "+strings.TrimSpace(path)], h))
}

type authKey struct{}

// AuthFrom returns the AuthResult the router resolved for this request (e.g. so /auth/session
// can echo the CSRF token).
func AuthFrom(ctx context.Context) (AuthResult, bool) {
	a, ok := ctx.Value(authKey{}).(AuthResult)
	return a, ok
}

func isUnsafe(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return false
	}
	return true
}

// guard applies body limit, authentication, rate limit, access rules and CSRF for one route.
// exempt (may be nil) reports requests the generic rate limiter skips.
func (rt *Router) guard(a Access, exempt func(*http.Request) bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		wrapBody(r) // inside MaxBytesReader, so a too-large body still surfaces as 413
		if a.MaxBody >= 0 {
			limit := a.MaxBody
			if limit == 0 {
				limit = DefaultMaxBody
			}
			if r.ContentLength > limit {
				WriteError(w, r, core.ErrPayloadTooLarge)
				return
			}
			base := w // the real writer, so the server can close the connection on overflow
			if rc, ok := w.(*recorder); ok {
				base = rc.ResponseWriter
			}
			r.Body = http.MaxBytesReader(base, r.Body, limit)
			ctx = context.WithValue(ctx, maxBodyKey{}, limit)
		}

		var res AuthResult
		if rt.auth != nil {
			var err error
			res, err = rt.auth.Authenticate(r, a.AllowQueryToken)
			if err != nil {
				if !a.Public {
					WriteError(w, r, err)
					return
				}
				var ce *core.Error
				if !errors.As(err, &ce) {
					rt.log.Warn("authentication error on public route", "request_id", core.RequestID(ctx), "err", err)
				}
				res = AuthResult{}
			}
		}
		p := res.Principal

		if rt.authLimiter != nil && (exempt == nil || !exempt(r)) {
			l, key := rt.anonLimiter, RateKeyForIP(ClientIP(r))
			if p != nil {
				l, key = rt.authLimiter, "u:"+p.UserID
			}
			if ok, wait := l.Allow(key); !ok {
				w.Header().Set("Retry-After", strconv.Itoa(int(wait/time.Second)+1))
				WriteError(w, r, core.ErrRateLimited)
				return
			}
		}

		if p != nil && res.ViaCookie && isUnsafe(r.Method) && !a.NoCSRF {
			if err := rt.checkCSRF(r, res); err != nil {
				if !a.Public {
					WriteError(w, r, err)
					return
				}
				// Public route: never use ambient cookie authority without CSRF proof.
				p, res = nil, AuthResult{}
			}
		}

		// Cookies are never accepted cross-origin (ARCHITECTURE.md §2.9), reads included: a
		// browser extension holding a host permission (or any page whose request skips CORS)
		// could otherwise read the response with the user's ambient session. Browsers send no
		// Origin on same-origin GET/HEAD, and a same-origin Origin is accepted.
		if p != nil && res.ViaCookie {
			if origin := r.Header.Get("Origin"); origin != "" && !rt.sameOrigin(r, origin) {
				if !a.Public {
					WriteError(w, r, errCrossOriginCookie)
					return
				}
				p, res = nil, AuthResult{}
			}
		}

		if !a.Public {
			if p == nil {
				WriteError(w, r, core.ErrUnauthorized)
				return
			}
			if a.Admin && !(p.IsAdmin() && p.Has(core.ScopeAdmin)) {
				WriteError(w, r, core.Forbidden("Administrator access required"))
				return
			}
			if a.Scope != "" && !p.Has(a.Scope) && (a.AltScope == "" || !p.Has(a.AltScope)) {
				WriteError(w, r, core.Forbidden("Missing required scope: "+a.Scope))
				return
			}
		}

		ctx = context.WithValue(ctx, authKey{}, res)
		if p != nil {
			ctx = core.WithPrincipal(ctx, p)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// checkCSRF enforces the double-submit token and, when present, a same-origin Origin header.
func (rt *Router) checkCSRF(r *http.Request, res AuthResult) error {
	tok := r.Header.Get("X-CSRF-Token")
	if res.CSRFToken == "" || tok == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(res.CSRFToken)) != 1 {
		return core.Forbidden("CSRF token missing or invalid")
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		if !rt.sameOrigin(r, origin) {
			return core.Forbidden("Cross-origin request rejected")
		}
	} else if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return core.Forbidden("Cross-origin request rejected")
	}
	return nil
}

// sameOrigin reports whether origin designates this server: its host must equal the request
// Host (or a trusted X-Forwarded-Host, or general.public_url). A request known to be HTTPS
// only accepts an https origin; for plain HTTP either scheme is accepted because a
// TLS-terminating proxy that is not configured as trusted would otherwise break every write.
func (rt *Router) sameOrigin(r *http.Request, origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.Path != "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return false
	}
	meta, _ := core.RequestMetaFrom(r.Context())
	schemeOK := u.Scheme == "https" || !meta.Secure
	if schemeOK && strings.EqualFold(u.Host, r.Host) {
		return true
	}
	if schemeOK && meta.Origin != "" {
		if mu, err := url.Parse(meta.Origin); err == nil && strings.EqualFold(u.Host, mu.Host) {
			return true
		}
	}
	if rt.settings != nil {
		if g, err := rt.settings.General(r.Context()); err == nil && g.PublicURL != "" {
			if pu, err := url.Parse(g.PublicURL); err == nil &&
				strings.EqualFold(pu.Scheme, u.Scheme) && strings.EqualFold(pu.Host, u.Host) {
				return true
			}
		}
	}
	return false
}

// Security headers (ARCHITECTURE.md §2.8). Handlers may override Content-Security-Policy for
// their own pages (Swagger UI does).
const DefaultCSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data: blob:; " +
	"media-src 'self' blob:; connect-src 'self'; font-src 'self'; object-src 'none'; base-uri 'none'; " +
	"frame-ancestors 'none'; form-action 'self'"

func setSecurityHeaders(h http.Header, secure bool) {
	h.Set("Content-Security-Policy", DefaultCSP)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	if secure {
		h.Set("Strict-Transport-Security", "max-age=31536000")
	}
}

// ServeHTTP is the outer middleware chain shared by every request.
func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &recorder{ResponseWriter: w, status: http.StatusOK}

	ip, secure, fwdHost := resolveClient(r, rt.proxies)
	scheme := "http"
	if secure {
		scheme = "https"
	}
	host := r.Host
	if fwdHost != "" {
		host = fwdHost
	}
	ua := r.UserAgent()
	if len(ua) > 512 {
		ua = ua[:512]
	}
	meta := core.RequestMeta{ID: core.NewID(), IP: ip.String(), UserAgent: ua, Secure: secure, Origin: scheme + "://" + host}
	ctx := core.WithRequestMeta(r.Context(), meta)
	ctx = context.WithValue(ctx, loggerKey{}, rt.log)
	r = r.WithContext(ctx)

	h := rec.Header()
	setSecurityHeaders(h, secure)
	h.Set("X-Request-Id", meta.ID)

	defer func() {
		if p := recover(); p != nil {
			if p == http.ErrAbortHandler {
				panic(p)
			}
			rt.log.Error("panic in handler", "request_id", meta.ID, "method", r.Method, "path", r.URL.Path,
				"panic", p, "stack", string(debug.Stack()))
			if !rec.wroteHeader {
				WriteError(rec, r, core.ErrInternal)
			}
		}
		rt.logRequest(r, rec, start, meta)
	}()

	path := r.URL.Path
	isAPI := path == "/api" || strings.HasPrefix(path, "/api/")
	if isAPI {
		h.Set("Cache-Control", "no-store")
	}
	if strings.HasPrefix(path, "/api/v1/") {
		if rt.cors(rec, r) {
			return
		}
		if rt.setup != nil && !setupExempt(r) && rt.setup(ctx) {
			WriteError(rec, r, core.ErrSetupRequired)
			return
		}
	}

	if _, pattern := rt.mux.Handler(r); pattern != "" {
		rt.mux.ServeHTTP(rec, r)
		return
	}
	if isAPI || rt.fallback == nil {
		rt.notFound(rec, r)
		return
	}
	rt.fallback.ServeHTTP(rec, r)
}

func setupExempt(r *http.Request) bool {
	get := r.Method == http.MethodGet || r.Method == http.MethodHead
	switch r.URL.Path {
	case "/api/v1/health", "/api/v1/setup/status":
		return get
	case "/api/v1/setup":
		return r.Method == http.MethodPost
	}
	return false
}

// notFound renders 404 or 405 as JSON, keeping the Allow header the mux computed.
func (rt *Router) notFound(w http.ResponseWriter, r *http.Request) {
	probe := &statusProbe{header: http.Header{}}
	rt.mux.ServeHTTP(probe, r) // the mux's own 404/405 decision, discarded
	if probe.status == http.StatusMethodNotAllowed {
		if allow := probe.header.Get("Allow"); allow != "" {
			w.Header().Set("Allow", allow)
		}
		WriteError(w, r, core.NewError(http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed"))
		return
	}
	WriteError(w, r, core.ErrNotFound)
}

type statusProbe struct {
	header http.Header
	status int
}

func (p *statusProbe) Header() http.Header         { return p.header }
func (p *statusProbe) Write(b []byte) (int, error) { return len(b), nil }
func (p *statusProbe) WriteHeader(s int)           { p.status = s }

func (rt *Router) logRequest(r *http.Request, rec *recorder, start time.Time, meta core.RequestMeta) {
	level := slog.LevelInfo
	if strings.HasPrefix(r.URL.Path, "/assets/") || strings.HasPrefix(r.URL.Path, "/api/docs/") {
		level = slog.LevelDebug
	}
	if !rt.log.Enabled(r.Context(), level) {
		return
	}
	rt.log.LogAttrs(r.Context(), level, "http",
		slog.String("method", r.Method),
		slog.String("path", redactPath(r.URL.Path, r.Pattern)),
		slog.Int("status", rec.status),
		slog.Int64("bytes", rec.bytes),
		slog.Duration("duration", time.Since(start).Round(time.Microsecond)),
		slog.String("ip", meta.IP),
		slog.String("request_id", meta.ID),
	)
}

// redactPath hides path segments bound to wildcards whose name mentions "token" (e.g.
// /hdhr/{feed_token}/discover.json), since paths are logged but credentials must not be.
func redactPath(path, pattern string) string {
	if !strings.Contains(pattern, "token") {
		return path
	}
	_, pp, ok := strings.Cut(pattern, " ")
	if !ok {
		pp = pattern
	}
	ps, segs := strings.Split(pp, "/"), strings.Split(path, "/")
	for i := 0; i < len(ps) && i < len(segs); i++ {
		if strings.HasPrefix(ps[i], "{") && strings.Contains(ps[i], "token") {
			segs[i] = "[redacted]"
		}
	}
	return strings.Join(segs, "/")
}

// CORS (ARCHITECTURE.md §2.9): only browser-extension origins, only Bearer requests, never
// credentials. Returns true when the request was fully handled (preflight).

var corsAllowHeaders = "Authorization, Content-Type, Tus-Resumable, Upload-Length, Upload-Offset, " +
	"Upload-Metadata, Upload-Checksum, Upload-Defer-Length, If-Match, If-None-Match, Range"

var corsExposeHeaders = "Location, Retry-After, Content-Disposition, Content-Range, ETag, X-Request-Id, " +
	"Tus-Resumable, Tus-Version, Tus-Extension, Tus-Max-Size, Tus-Checksum-Algorithm, Upload-Offset, " +
	"Upload-Length, Upload-Expires, Ludworks-File-Id, Ludworks-File-Path"

func isExtensionOrigin(o string) bool {
	return strings.HasPrefix(o, "chrome-extension://") || strings.HasPrefix(o, "moz-extension://")
}

func (rt *Router) extensionOriginAllowed(ctx context.Context, origin string) bool {
	o := strings.ToLower(origin)
	if !settings.ValidExtensionOrigin(o) {
		return false
	}
	if rt.settings == nil {
		return true
	}
	sec, err := rt.settings.Security(ctx)
	if err != nil {
		return false
	}
	if len(sec.AllowedExtensionOrigins) == 0 {
		return true
	}
	for _, a := range sec.AllowedExtensionOrigins {
		if a == o {
			return true
		}
	}
	return false
}

var (
	errCrossOriginCookie   = core.Forbidden("Cross-origin request rejected: session cookies are only accepted from this server's own pages")
	errExtensionNotAllowed = core.Forbidden("This browser extension is not allowed to use this server")
)

func (rt *Router) cors(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" || !isExtensionOrigin(strings.ToLower(origin)) {
		return false
	}
	h := w.Header()
	h.Add("Vary", "Origin")
	allowed := rt.extensionOriginAllowed(r.Context(), origin)
	if !allowed && !(r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "") {
		// The allow-list is enforced on every request, not only through CORS headers: an
		// extension with a host permission for this server skips CORS entirely, so a missing
		// Access-Control-Allow-Origin would not stop it from reading the response.
		WriteError(w, r, errExtensionNotAllowed)
		return true
	}
	if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
		h.Add("Vary", "Access-Control-Request-Method")
		h.Add("Vary", "Access-Control-Request-Headers")
		if !allowed || !requestsHeader(r.Header.Get("Access-Control-Request-Headers"), "authorization") {
			WriteError(w, r, core.Forbidden("CORS is only available to allowed browser extensions using Bearer tokens"))
			return true
		}
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Access-Control-Allow-Methods", "GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS")
		h.Set("Access-Control-Allow-Headers", corsAllowHeaders)
		h.Set("Access-Control-Max-Age", "600")
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	if allowed && hasBearer(r) {
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Access-Control-Expose-Headers", corsExposeHeaders)
	}
	return false
}

func hasBearer(r *http.Request) bool {
	a := r.Header.Get("Authorization")
	return len(a) > 7 && strings.EqualFold(a[:7], "bearer ")
}

func requestsHeader(list, name string) bool {
	for _, p := range strings.Split(list, ",") {
		if strings.EqualFold(strings.TrimSpace(p), name) {
			return true
		}
	}
	return false
}
