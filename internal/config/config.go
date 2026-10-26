// Package config loads the server configuration.
//
// Precedence (lowest to highest): OS defaults < JSON config file < LUDWORKS_* environment
// variables < command-line flags. The JSON file is parsed strictly (unknown keys are an
// error) so that a typo in a security-relevant key such as "trusted_proxies" fails loudly
// instead of being silently ignored.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// TLS modes.
const (
	TLSSelfSigned = "self-signed"
	TLSFiles      = "files"
	TLSACME       = "acme"
	TLSOff        = "off"
)

// Config is the process configuration. It holds only bootstrap settings; everything an
// admin can change at runtime lives in the settings store instead.
type Config struct {
	DataDir        string    `json:"data_dir"`
	Listen         string    `json:"listen"`
	TLS            TLSConfig `json:"tls"`
	TrustedProxies []string  `json:"trusted_proxies"` // CIDRs or single IPs
	Log            LogConfig `json:"log"`

	// ConfigFile is the path of the JSON file that was loaded ("" if none).
	ConfigFile string `json:"-"`
}

// TLSConfig selects how the HTTPS listener obtains its certificate.
type TLSConfig struct {
	Mode           string   `json:"mode"` // self-signed | files | acme | off
	CertFile       string   `json:"cert_file"`
	KeyFile        string   `json:"key_file"`
	ACMEDomains    []string `json:"acme_domains"`
	ACMEEmail      string   `json:"acme_email"`
	RedirectListen string   `json:"redirect_listen"` // e.g. ":8080"; "" disables the HTTP->HTTPS redirect listener
}

// LogConfig controls slog output.
type LogConfig struct {
	Level  string `json:"level"`  // debug | info | warn | error
	Format string `json:"format"` // text | json
}

// Flags holds command-line overrides. Empty strings mean "not set".
type Flags struct {
	DataDir    string
	Listen     string
	TLSMode    string
	ConfigFile string
	LogLevel   string
}

// Register binds the standard serve flags (--data --listen --tls --config --log-level).
func (f *Flags) Register(fs *flag.FlagSet) {
	fs.StringVar(&f.DataDir, "data", "", "data directory (default: OS specific)")
	fs.StringVar(&f.Listen, "listen", "", "listen address, e.g. :8443 or 127.0.0.1:18080")
	fs.StringVar(&f.TLSMode, "tls", "", "TLS mode: self-signed | files | acme | off")
	fs.StringVar(&f.ConfigFile, "config", "", "path to JSON config file")
	fs.StringVar(&f.LogLevel, "log-level", "", "log level: debug | info | warn | error")
}

// Env lists the environment variables that are honoured.
var Env = []string{
	"LUDWORKS_CONFIG", "LUDWORKS_DATA_DIR", "LUDWORKS_LISTEN", "LUDWORKS_TLS_MODE",
	"LUDWORKS_TLS_CERT_FILE", "LUDWORKS_TLS_KEY_FILE", "LUDWORKS_TLS_ACME_DOMAINS",
	"LUDWORKS_TLS_ACME_EMAIL", "LUDWORKS_TLS_REDIRECT_LISTEN", "LUDWORKS_TRUSTED_PROXIES",
	"LUDWORKS_LOG_LEVEL", "LUDWORKS_LOG_FORMAT",
}

// Environment abstracts the process environment for tests.
type Environment struct {
	GOOS        string
	Getenv      func(string) string
	HomeDir     func() (string, error)
	ProgramData string // Windows %ProgramData%
}

// OSEnvironment returns the real process environment.
func OSEnvironment() Environment {
	return Environment{
		GOOS:        runtime.GOOS,
		Getenv:      os.Getenv,
		HomeDir:     os.UserHomeDir,
		ProgramData: os.Getenv("ProgramData"),
	}
}

// Load resolves the configuration from the real environment.
func Load(f Flags) (*Config, error) { return LoadWith(f, OSEnvironment()) }

// LoadWith resolves the configuration using the given environment (testable).
func LoadWith(f Flags, e Environment) (*Config, error) {
	if e.Getenv == nil {
		e.Getenv = func(string) string { return "" }
	}
	c := Defaults(e)

	// The data dir used to locate the default config file can itself come from env/flags.
	dataForConfig := c.DataDir
	if v := e.Getenv("LUDWORKS_DATA_DIR"); v != "" {
		dataForConfig = v
	}
	if f.DataDir != "" {
		dataForConfig = f.DataDir
	}

	path, explicit := f.ConfigFile, f.ConfigFile != ""
	if !explicit {
		if v := e.Getenv("LUDWORKS_CONFIG"); v != "" {
			path, explicit = v, true
		}
	}
	if !explicit {
		path = defaultConfigPath(e, dataForConfig)
	}
	if path != "" {
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			// On Windows the default location is inside the data directory, which any user
			// may have created first: never act on a file another account could have written.
			if err := checkConfigFile(path); err != nil {
				return nil, err
			}
			if err := mergeJSON(c, data); err != nil {
				return nil, fmt.Errorf("config %s: %w", path, err)
			}
			c.ConfigFile = path
		case errors.Is(err, os.ErrNotExist) && !explicit:
			// The default config file is optional.
		default:
			return nil, fmt.Errorf("config: read %s: %w", path, err)
		}
	}

	applyEnv(c, e.Getenv)

	if f.DataDir != "" {
		c.DataDir = f.DataDir
	}
	if f.Listen != "" {
		c.Listen = f.Listen
	}
	if f.TLSMode != "" {
		c.TLS.Mode = f.TLSMode
	}
	if f.LogLevel != "" {
		c.Log.Level = f.LogLevel
	}

	if err := c.normalize(e); err != nil {
		return nil, err
	}
	return c, nil
}

// Defaults returns the OS-specific defaults.
func Defaults(e Environment) *Config {
	return &Config{
		DataDir: defaultDataDir(e),
		Listen:  ":8443",
		TLS:     TLSConfig{Mode: TLSSelfSigned},
		Log:     LogConfig{Level: "info", Format: "text"},
	}
}

func defaultDataDir(e Environment) string {
	switch e.GOOS {
	case "linux":
		return "/var/lib/ludworks"
	case "windows":
		pd := e.ProgramData
		if pd == "" {
			pd = `C:\ProgramData`
		}
		return pd + `\Ludworks`
	default:
		if e.HomeDir != nil {
			if h, err := e.HomeDir(); err == nil && h != "" {
				return filepath.Join(h, ".ludworks")
			}
		}
		return ".ludworks"
	}
}

func defaultConfigPath(e Environment, dataDir string) string {
	switch e.GOOS {
	case "linux":
		return "/etc/ludworks/config.json"
	case "windows":
		return strings.TrimRight(dataDir, `\/`) + `\config.json`
	default:
		return filepath.Join(dataDir, "config.json")
	}
}

func mergeJSON(c *Config, data []byte) error {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")) // tolerate a UTF-8 BOM (Notepad)
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(c); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data after JSON object")
	}
	return nil
}

func applyEnv(c *Config, getenv func(string) string) {
	set := func(key string, dst *string) {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			*dst = v
		}
	}
	list := func(key string, dst *[]string) {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			*dst = splitList(v)
		}
	}
	set("LUDWORKS_DATA_DIR", &c.DataDir)
	set("LUDWORKS_LISTEN", &c.Listen)
	set("LUDWORKS_TLS_MODE", &c.TLS.Mode)
	set("LUDWORKS_TLS_CERT_FILE", &c.TLS.CertFile)
	set("LUDWORKS_TLS_KEY_FILE", &c.TLS.KeyFile)
	list("LUDWORKS_TLS_ACME_DOMAINS", &c.TLS.ACMEDomains)
	set("LUDWORKS_TLS_ACME_EMAIL", &c.TLS.ACMEEmail)
	// redirect_listen may be deliberately disabled with the value "off".
	set("LUDWORKS_TLS_REDIRECT_LISTEN", &c.TLS.RedirectListen)
	list("LUDWORKS_TRUSTED_PROXIES", &c.TrustedProxies)
	set("LUDWORKS_LOG_LEVEL", &c.Log.Level)
	set("LUDWORKS_LOG_FORMAT", &c.Log.Format)
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (c *Config) normalize(e Environment) error {
	var errs []string
	if c.DataDir == "" {
		errs = append(errs, "data_dir must not be empty")
	} else if e.GOOS == runtime.GOOS {
		abs, err := filepath.Abs(c.DataDir)
		if err != nil {
			errs = append(errs, "data_dir: "+err.Error())
		} else {
			c.DataDir = abs
		}
	}
	if _, port, err := net.SplitHostPort(c.Listen); err != nil || port == "" {
		errs = append(errs, fmt.Sprintf("listen %q: expected host:port", c.Listen))
	}
	c.TLS.Mode = strings.ToLower(strings.TrimSpace(c.TLS.Mode))
	switch c.TLS.Mode {
	case TLSSelfSigned, TLSOff:
	case TLSFiles:
		if c.TLS.CertFile == "" || c.TLS.KeyFile == "" {
			errs = append(errs, "tls mode files requires tls.cert_file and tls.key_file")
		}
	case TLSACME:
		if len(c.TLS.ACMEDomains) == 0 {
			errs = append(errs, "tls mode acme requires tls.acme_domains")
		}
		for _, d := range c.TLS.ACMEDomains {
			if !validHostname(d) {
				errs = append(errs, fmt.Sprintf("tls.acme_domains: invalid hostname %q", d))
			}
		}
	default:
		errs = append(errs, fmt.Sprintf("tls.mode %q: expected self-signed, files, acme or off", c.TLS.Mode))
	}
	if strings.EqualFold(c.TLS.RedirectListen, "off") {
		c.TLS.RedirectListen = ""
	}
	if c.TLS.RedirectListen != "" {
		if c.TLS.Mode == TLSOff {
			errs = append(errs, "tls.redirect_listen requires TLS to be enabled")
		} else if _, _, err := net.SplitHostPort(c.TLS.RedirectListen); err != nil {
			errs = append(errs, fmt.Sprintf("tls.redirect_listen %q: expected host:port", c.TLS.RedirectListen))
		}
	}
	if _, err := ParseCIDRs(c.TrustedProxies); err != nil {
		errs = append(errs, "trusted_proxies: "+err.Error())
	}
	c.Log.Level = strings.ToLower(c.Log.Level)
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Sprintf("log.level %q: expected debug, info, warn or error", c.Log.Level))
	}
	c.Log.Format = strings.ToLower(c.Log.Format)
	switch c.Log.Format {
	case "text", "json":
	default:
		errs = append(errs, fmt.Sprintf("log.format %q: expected text or json", c.Log.Format))
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid configuration: %s", strings.Join(errs, "; "))
	}
	return nil
}

// ParseCIDRs parses CIDRs or bare IPs (treated as single-host prefixes).
func ParseCIDRs(in []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if strings.Contains(s, "/") {
			p, err := netip.ParsePrefix(s)
			if err != nil {
				return nil, fmt.Errorf("%q: %w", s, err)
			}
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(s)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", s, err)
		}
		a = a.Unmap()
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

// TrustedProxyPrefixes returns the parsed trusted proxy CIDRs (already validated by Load).
func (c *Config) TrustedProxyPrefixes() []netip.Prefix {
	p, _ := ParseCIDRs(c.TrustedProxies)
	return p
}

func validHostname(h string) bool {
	if len(h) == 0 || len(h) > 253 {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if len(label) == 0 || len(label) > 63 {
			return false
		}
		for i, r := range label {
			ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' && i > 0 && i < len(label)-1
			if !ok {
				return false
			}
		}
	}
	return true
}

// Data directory layout.

// DBPath is the SQLite database file.
func (c *Config) DBPath() string { return filepath.Join(c.DataDir, "ludworks.db") }

// SecretKeyPath is the 32-byte server secret used for at-rest encryption and MAC keys.
func (c *Config) SecretKeyPath() string { return filepath.Join(c.DataDir, "secret.key") }

// SetupTokenPath holds the one-time first-run setup token.
func (c *Config) SetupTokenPath() string { return filepath.Join(c.DataDir, "setup-token.txt") }

// StorageDir is the root of all user file spaces.
func (c *Config) StorageDir() string { return filepath.Join(c.DataDir, "storage") }

// TmpDir holds partial uploads and scratch files; it is on the same volume as StorageDir so
// commits can be atomic renames.
func (c *Config) TmpDir() string { return filepath.Join(c.DataDir, "tmp") }

// ThumbsDir caches generated thumbnails and posters.
func (c *Config) ThumbsDir() string { return filepath.Join(c.DataDir, "thumbs") }

// BackupsDir receives database backups (VACUUM INTO).
func (c *Config) BackupsDir() string { return filepath.Join(c.DataDir, "backups") }

// TLSDir holds generated certificates and the ACME cache.
func (c *Config) TLSDir() string { return filepath.Join(c.DataDir, "tls") }

// EnsureDirs creates the data directory tree with owner-only permissions. On Windows the data
// directory is created with a protected DACL (SYSTEM, Administrators, the current account and
// the Ludworks service account) in one step, because %ProgramData% lets every user read and
// create folders; an existing data directory, and every file in it the server trusts (keys,
// database, configuration, logs, the storage/backups/TLS directories), must be owned by and
// accessible to those principals only, or EnsureDirs fails (see perm_windows.go).
func (c *Config) EnsureDirs() error {
	if err := ensureDataDir(c.DataDir); err != nil {
		return err
	}
	if err := checkDataEntries(c); err != nil {
		return err
	}
	for _, d := range []string{c.StorageDir(), c.TmpDir(), c.ThumbsDir(), c.BackupsDir(), c.TLSDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", d, err)
		}
	}
	return nil
}

// WriteFileAtomic writes data to path with mode 0600 via a temp file + rename so readers
// never observe a partially written file.
func WriteFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename
	if err := f.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
