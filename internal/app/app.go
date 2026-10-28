// Package app wires the server: it opens the data dir and database, builds core.Deps,
// constructs every module in dependency order, mounts the HTTP routes (API, OpenAPI/Swagger,
// web UI) and runs the HTTPS (and optional redirect) listeners with graceful shutdown.
package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"ludworks/internal/admin"
	"ludworks/internal/apidocs"
	"ludworks/internal/audit"
	"ludworks/internal/auth"
	"ludworks/internal/browser"
	"ludworks/internal/cast"
	"ludworks/internal/config"
	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/events"
	"ludworks/internal/files"
	"ludworks/internal/httpx"
	"ludworks/internal/jobs"
	"ludworks/internal/kanban"
	"ludworks/internal/mail"
	"ludworks/internal/media"
	"ludworks/internal/music"
	"ludworks/internal/notes"
	"ludworks/internal/settings"
	"ludworks/internal/sheets"
	"ludworks/internal/system"
	"ludworks/internal/tlsutil"
	"ludworks/internal/tv"
	"ludworks/internal/vault"
	"ludworks/internal/version"
	"ludworks/internal/webui"
	"ludworks/web"
)

// ShutdownTimeout is how long in-flight requests get to finish on shutdown.
const ShutdownTimeout = 20 * time.Second

// App is a fully wired server.
type App struct {
	cfg     *config.Config
	log     *slog.Logger
	infra   *Infra
	router  *httpx.Router
	modules []httpx.Module

	tlsConfig   *tls.Config
	redirect    http.Handler // handler for the plain-HTTP listener (nil = none)
	certSummary string

	mu       sync.Mutex
	servers  []*http.Server
	addr     net.Addr
	runCtx   context.Context
	cancel   context.CancelFunc
	serveErr chan error
	started  bool
	stopped  bool
}

// Infra is the module-independent foundation (also used by CLI admin commands).
type Infra struct {
	DB       *db.DB
	Settings *settings.Store
	Audit    *audit.Log
	Events   *events.Bus
	Jobs     *jobs.Runner
	Deps     *core.Deps
	users    *usersProxy
	mailer   *mailerProxy
}

// OpenInfra prepares the data dir, secret key, database, settings, audit log, event bus and
// job runner, and returns Deps whose Mailer/Users are proxies to be bound later.
func OpenInfra(ctx context.Context, cfg *config.Config, log *slog.Logger) (*Infra, error) {
	if err := cfg.EnsureDirs(); err != nil {
		return nil, err
	}
	key, err := settings.LoadOrCreateKey(cfg.SecretKeyPath())
	if err != nil {
		return nil, err
	}
	if err := settings.CheckKeyFile(cfg.SecretKeyPath()); err != nil {
		// On Windows a key that is not private may have been planted by another user (who
		// then knows every secret derived from it): refuse to start. On POSIX systems the
		// data directory (0700) still protects it, so warn.
		if runtime.GOOS == "windows" {
			return nil, fmt.Errorf("secret key: %w", err)
		}
		log.Warn("secret key file is not private", "err", err)
	}
	d, err := db.Open(ctx, cfg.DBPath())
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Infra, error) { d.Close(); return nil, err }
	st, err := settings.New(ctx, d, key)
	if err != nil {
		return fail(err)
	}
	al, err := audit.New(ctx, d, log)
	if err != nil {
		return fail(err)
	}
	httpx.SetCursorKey(st.DeriveKey("cursor-v1"))
	in := &Infra{
		DB: d, Settings: st, Audit: al,
		Events: events.New(log.With("component", "events")),
		Jobs:   jobs.New(log.With("component", "jobs"), 4),
		users:  &usersProxy{}, mailer: &mailerProxy{},
	}
	in.Deps = &core.Deps{
		DB: d, Config: cfg, Settings: st, Log: log, Jobs: in.Jobs, Events: in.Events,
		Mailer: in.mailer, Audit: al, Users: in.users,
		Info: &core.ServerInfo{StartedAt: core.Now(), TLSMode: cfg.TLS.Mode},
	}
	return in, nil
}

// BindUsers / BindMailer point the Deps proxies at the real implementations.
func (in *Infra) BindUsers(u core.UserLookup) { in.users.set(u) }
func (in *Infra) BindMailer(m core.Mailer)    { in.mailer.set(m) }

// Close stops background work and closes the database.
func (in *Infra) Close(ctx context.Context) error {
	var errs []error
	if err := in.Jobs.Stop(ctx); err != nil {
		errs = append(errs, fmt.Errorf("jobs: %w", err))
	}
	if err := in.Events.Close(ctx); err != nil {
		errs = append(errs, fmt.Errorf("events: %w", err))
	}
	if err := in.DB.Close(); err != nil {
		errs = append(errs, fmt.Errorf("db: %w", err))
	}
	return errors.Join(errs...)
}

// OpenCLI opens the infrastructure plus the auth module (migrated) for admin CLI commands.
func OpenCLI(ctx context.Context, cfg *config.Config, log *slog.Logger) (*core.Deps, func(), error) {
	in, err := OpenInfra(ctx, cfg, log)
	if err != nil {
		return nil, nil, err
	}
	a := auth.New(in.Deps)
	in.BindUsers(a.Users())
	if err := in.DB.Migrate(ctx, a.Name(), a.Migrations()); err != nil {
		in.Close(ctx)
		return nil, nil, err
	}
	return in.Deps, func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		in.Close(c)
	}, nil
}

// New builds the application. Nothing listens until Start.
func New(ctx context.Context, cfg *config.Config, log *slog.Logger) (*App, error) {
	in, err := OpenInfra(ctx, cfg, log)
	if err != nil {
		return nil, err
	}
	a := &App{cfg: cfg, log: log, infra: in}
	if err := a.build(ctx); err != nil {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		in.Close(c)
		return nil, err
	}
	return a, nil
}

func (a *App) build(ctx context.Context) error {
	d := a.infra.Deps
	if err := a.setupTLS(); err != nil {
		return err
	}

	// Construction order (auth → mail → admin → files → media → tv → music → notes → vault → browser →
	// sheets → kanban → cast → system): admin needs the auth and mail modules, mail-in needs
	// notes and files, browser sync ties its records to the vault key, cast plays media, TV
	// channels and music.
	authM := auth.New(d)
	a.infra.BindUsers(authM.Users())
	mailM := mail.New(d)
	a.infra.BindMailer(mailM.Mailer())
	adminM := admin.New(d, authM, mailM)
	filesM := files.New(d)
	mediaM := media.New(d, filesM.Service())
	tvM := tv.New(d, filesM.Service(), mediaM.Service())
	musicM := music.New(d, filesM.Service(), mediaM.Service())
	notesM := notes.New(d)
	mailM.SetIngest(notesM.Service(), filesM.Service())
	vaultM := vault.New(d)
	browserM := browser.New(d, vaultM)
	sheetsM := sheets.New(d, filesM.Service())
	kanbanM := kanban.New(d, filesM.Service())
	castM := cast.New(d, filesM.Service(), mediaM.Service(), tvM, musicM.Service())
	systemM := system.New(d, filesM.Service())
	a.modules = []httpx.Module{authM, mailM, adminM, filesM, mediaM, tvM, musicM, notesM, vaultM, browserM, sheetsM, kanbanM, castM, systemM}

	for _, m := range a.modules {
		if err := d.DB.Migrate(ctx, m.Name(), m.Migrations()); err != nil {
			return err
		}
	}

	a.router = httpx.NewRouter(httpx.Options{
		Log:            a.log.With("component", "http"),
		Auth:           authM.Authenticator(),
		SetupRequired:  authM.SetupRequired,
		TrustedProxies: a.cfg.TrustedProxyPrefixes(),
		Settings:       d.Settings,
	})
	a.router.Handle("GET /api/v1/health", httpx.Public(), a.health)
	for _, m := range a.modules {
		if err := registerRoutes(m, a.router); err != nil {
			return err
		}
	}

	frags := make([]apidocs.Fragment, 0, len(a.modules))
	for _, m := range a.modules {
		frags = append(frags, apidocs.Fragment{Name: m.Name(), JSON: m.OpenAPI()})
	}
	doc, err := apidocs.Build(apidocs.Base(), version.Version, frags)
	if err != nil {
		return err
	}
	if missing, _ := apidocs.UnresolvedRefs(doc); len(missing) > 0 {
		a.log.Warn("OpenAPI document has unresolved $refs", "refs", strings.Join(missing, ", "))
	}
	apidocs.Register(a.router, doc)

	ui, err := webui.New(web.FS())
	if err != nil {
		return err
	}
	a.router.SetFallback(ui)
	return nil
}

func registerRoutes(m httpx.Module, r *httpx.Router) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("module %s: route registration: %v", m.Name(), p)
		}
	}()
	m.Routes(r)
	return nil
}

func (a *App) health(w http.ResponseWriter, r *http.Request) error {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	status, code := "ok", http.StatusOK
	if err := a.infra.DB.SQL.PingContext(ctx); err != nil {
		a.log.Error("health: database unavailable", "err", err)
		status, code = "error", http.StatusServiceUnavailable
	}
	return httpx.WriteJSON(w, code, map[string]string{
		"status": status, "version": version.Version, "time": core.FormatTime(core.Now()),
	})
}

// Handler returns the root HTTP handler (for tests and embedding).
func (a *App) Handler() http.Handler { return a.router }

// Deps returns the shared dependencies.
func (a *App) Deps() *core.Deps { return a.infra.Deps }

// setupTLS prepares the certificate source for the configured mode.
func (a *App) setupTLS() error {
	cfg := a.cfg
	info := a.infra.Deps.Info
	_, httpsPort, _ := net.SplitHostPort(cfg.Listen)
	switch cfg.TLS.Mode {
	case config.TLSOff:
		a.log.Warn("TLS is disabled: only run like this behind a TLS-terminating reverse proxy (set trusted_proxies)")
		return nil
	case config.TLSSelfSigned:
		dns, ips := tlsHosts(cfg.Listen)
		cert, generated, err := tlsutil.EnsureSelfSigned(cfg.TLSDir(), dns, ips, time.Now())
		if err != nil {
			return fmt.Errorf("tls: self-signed certificate: %w", err)
		}
		if generated {
			a.log.Info("generated self-signed TLS certificate", "dir", cfg.TLSDir())
		}
		a.tlsConfig = tlsutil.ServerConfig(func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return cert, nil })
		info.TLSCert = func() *core.TLSCertInfo { return certInfo(cert) }
	case config.TLSFiles:
		rl, err := tlsutil.NewFileReloader(cfg.TLS.CertFile, cfg.TLS.KeyFile, func(err error) {
			a.log.Error("tls: certificate reload failed; keeping previous certificate", "err", err)
		})
		if err != nil {
			return fmt.Errorf("tls: %w", err)
		}
		a.tlsConfig = tlsutil.ServerConfig(rl.GetCertificate)
		info.TLSCert = func() *core.TLSCertInfo { return certInfo(rl.Current()) }
	case config.TLSACME:
		cacheDir := filepath.Join(cfg.TLSDir(), "acme")
		if err := os.MkdirAll(cacheDir, 0o700); err != nil {
			return err
		}
		m := tlsutil.ACME(cfg.TLS.ACMEDomains, cfg.TLS.ACMEEmail, cacheDir)
		a.tlsConfig = tlsutil.ACMEConfig(m)
		if cfg.TLS.RedirectListen != "" {
			a.redirect = m.HTTPHandler(tlsutil.RedirectHandler(httpsPort)) // serves http-01 challenges too
		}
		domain := cfg.TLS.ACMEDomains[0]
		info.TLSCert = func() *core.TLSCertInfo { return certInfo(tlsutil.ACMECachedCert(cacheDir, domain)) }
	}
	if a.redirect == nil && cfg.TLS.RedirectListen != "" {
		a.redirect = tlsutil.RedirectHandler(httpsPort)
	}
	if c := info.CurrentTLS(); c != nil {
		a.certSummary = c.FingerprintSHA256
	}
	return nil
}

// tlsHosts returns the SANs for the self-signed certificate: the defaults plus the listen
// host when it is a specific name or address.
func tlsHosts(listen string) ([]string, []net.IP) {
	dns, ips := tlsutil.DefaultHosts()
	host, _, _ := net.SplitHostPort(listen)
	if host == "" {
		return dns, ips
	}
	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsUnspecified() {
			ips = appendIP(ips, ip)
		}
		return dns, ips
	}
	for _, d := range dns {
		if strings.EqualFold(d, host) {
			return dns, ips
		}
	}
	return append(dns, strings.ToLower(host)), ips
}

func appendIP(ips []net.IP, ip net.IP) []net.IP {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	for _, x := range ips {
		if x.Equal(ip) {
			return ips
		}
	}
	return append(ips, ip)
}

// Start binds the listeners, starts background jobs and modules, and begins serving.
func (a *App) Start(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.started {
		return errors.New("app: already started")
	}
	a.started = true
	a.runCtx, a.cancel = context.WithCancel(context.WithoutCancel(ctx))

	ln, err := net.Listen("tcp", a.cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", a.cfg.Listen, err)
	}
	a.addr = ln.Addr()
	var redirectLn net.Listener
	if a.redirect != nil {
		if redirectLn, err = net.Listen("tcp", a.cfg.TLS.RedirectListen); err != nil {
			ln.Close()
			return fmt.Errorf("listen %s: %w", a.cfg.TLS.RedirectListen, err)
		}
	}

	a.infra.Jobs.Start()
	for _, m := range a.modules {
		if err := m.Start(a.runCtx); err != nil {
			ln.Close()
			if redirectLn != nil {
				redirectLn.Close()
			}
			return fmt.Errorf("module %s: start: %w", m.Name(), err)
		}
	}

	errLog := newServerErrorLog(a.log)
	main := &http.Server{
		Handler:           a.router,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          errLog,
		TLSConfig:         a.tlsConfig,
		BaseContext:       func(net.Listener) context.Context { return a.runCtx },
		// No ReadTimeout/WriteTimeout: uploads and streams are long-lived and enforce their
		// own limits.
		HTTP2: http2Config(),
	}
	a.serveErr = make(chan error, 2)
	a.servers = []*http.Server{main}
	go func() {
		var err error
		if a.tlsConfig != nil {
			err = main.ServeTLS(ln, "", "")
		} else {
			err = main.Serve(ln)
		}
		if !errors.Is(err, http.ErrServerClosed) {
			a.serveErr <- err
		}
	}()
	if redirectLn != nil {
		rs := &http.Server{
			Handler:           a.redirect,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       60 * time.Second,
			MaxHeaderBytes:    16 << 10,
			ErrorLog:          errLog,
		}
		a.servers = append(a.servers, rs)
		go func() {
			if err := rs.Serve(redirectLn); !errors.Is(err, http.ErrServerClosed) {
				a.serveErr <- err
			}
		}()
	}
	a.logStartup(redirectLn)
	return nil
}

// Addr returns the bound address of the main listener (after Start).
func (a *App) Addr() net.Addr {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.addr
}

// Run starts the app and blocks until ctx is cancelled or a listener fails, then shuts down
// gracefully (ShutdownTimeout).
func (a *App) Run(ctx context.Context) error {
	if err := a.Start(ctx); err != nil {
		a.Shutdown(context.Background())
		return err
	}
	var runErr error
	select {
	case <-ctx.Done():
		a.log.Info("shutting down", "timeout", ShutdownTimeout)
	case runErr = <-a.serveErr:
		a.log.Error("server failed", "err", runErr)
	}
	sctx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
	defer cancel()
	err := errors.Join(runErr, a.Shutdown(sctx))
	a.log.Info("stopped")
	return err
}

// Shutdown drains HTTP servers (forcing close when ctx expires), stops modules' background
// work and closes the database. It is idempotent.
func (a *App) Shutdown(ctx context.Context) error {
	a.mu.Lock()
	if a.stopped {
		a.mu.Unlock()
		return nil
	}
	a.stopped = true
	servers := a.servers
	a.mu.Unlock()

	var (
		errs  []error
		errMu sync.Mutex
		wg    sync.WaitGroup
	)
	for _, s := range servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Shutdown(ctx); err != nil {
				s.Close() // drain timed out: drop remaining (streaming) connections
				if !errors.Is(err, context.DeadlineExceeded) {
					errMu.Lock()
					errs = append(errs, err)
					errMu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	if a.cancel != nil {
		a.cancel()
	}
	cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.infra.Close(cctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (a *App) logStartup(redirectLn net.Listener) {
	scheme := "https"
	if a.tlsConfig == nil {
		scheme = "http"
	}
	host, port, _ := net.SplitHostPort(a.addr.String())
	var urls []string
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		urls = append(urls, fmt.Sprintf("%s://localhost:%s", scheme, port))
		if h, err := os.Hostname(); err == nil {
			urls = append(urls, fmt.Sprintf("%s://%s:%s", scheme, strings.ToLower(h), port))
		}
		_, ips := tlsutil.DefaultHosts()
		for _, ip := range ips {
			if ip.IsLoopback() {
				continue
			}
			urls = append(urls, fmt.Sprintf("%s://%s", scheme, net.JoinHostPort(ip.String(), port)))
		}
	} else {
		urls = append(urls, fmt.Sprintf("%s://%s", scheme, net.JoinHostPort(host, port)))
	}
	a.log.Info("Ludworks started", "version", version.Version, "data_dir", a.cfg.DataDir, "tls", a.cfg.TLS.Mode,
		"listen", a.addr.String())
	for _, u := range urls {
		a.log.Info("listening", "url", u)
	}
	if a.certSummary != "" {
		a.log.Info("TLS certificate", "sha256_fingerprint", a.certSummary)
	} else if a.cfg.TLS.Mode == config.TLSACME {
		a.log.Info("TLS certificate will be obtained via ACME on first connection", "domains", strings.Join(a.cfg.TLS.ACMEDomains, ","))
	}
	if redirectLn != nil {
		a.log.Info("HTTP->HTTPS redirect listener", "listen", redirectLn.Addr().String())
	}
	a.log.Info("API docs", "url", urls[0]+"/api/docs")
}

// HTTP/2 connection limits. Per-request write deadlines (http.ResponseController) bound how
// long a handler waits for one stream, but over HTTP/2 a client that stops reading the TCP
// connection stalls every stream on it in the shared connection writer, so streams and
// transcodes (which hold limited slots) could be pinned by a stalled peer. These close such
// connections, and detect dead peers that send nothing at all.
const (
	// HTTP2WriteByteTimeout closes a connection when data is waiting to be written and no
	// byte has gone out for this long. Matches the media transcode drop-after-60s rule.
	HTTP2WriteByteTimeout = 60 * time.Second
	// HTTP2SendPingTimeout sends a PING after this long without any frame from the peer;
	// HTTP2PingTimeout closes the connection when the PING is not answered in time.
	HTTP2SendPingTimeout = 60 * time.Second
	HTTP2PingTimeout     = 15 * time.Second
)

func http2Config() *http.HTTP2Config {
	return &http.HTTP2Config{
		WriteByteTimeout: HTTP2WriteByteTimeout,
		SendPingTimeout:  HTTP2SendPingTimeout,
		PingTimeout:      HTTP2PingTimeout,
	}
}
