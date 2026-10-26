package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func env(m map[string]string, goos string) Environment {
	return Environment{
		GOOS:        goos,
		Getenv:      func(k string) string { return m[k] },
		HomeDir:     func() (string, error) { return "/home/u", nil },
		ProgramData: `D:\PD`,
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOSDefaults(t *testing.T) {
	d := Defaults(env(nil, "linux"))
	if d.DataDir != "/var/lib/ludworks" || d.Listen != ":8443" || d.TLS.Mode != TLSSelfSigned || d.Log.Level != "info" {
		t.Fatalf("%+v", d)
	}
	if w := Defaults(env(nil, "windows")); w.DataDir != `D:\PD\Ludworks` {
		t.Fatalf("windows: %s", w.DataDir)
	}
	if o := Defaults(env(nil, "darwin")); o.DataDir != filepath.Join("/home/u", ".ludworks") {
		t.Fatalf("other: %s", o.DataDir)
	}
	if p := defaultConfigPath(env(nil, "windows"), `D:\PD\Ludworks`); p != `D:\PD\Ludworks\config.json` {
		t.Fatalf("windows config path: %s", p)
	}
	if p := defaultConfigPath(env(nil, "linux"), "/x"); p != "/etc/ludworks/config.json" {
		t.Fatalf("linux config path: %s", p)
	}
}

func TestPrecedence(t *testing.T) {
	dir := t.TempDir()
	cfgFile := writeConfig(t, `{"data_dir":"`+filepath.ToSlash(filepath.Join(dir, "fromfile"))+`","listen":":9000",
		"tls":{"mode":"off"},"log":{"level":"warn","format":"json"},"trusted_proxies":["10.0.0.0/8"]}`)

	// File over defaults.
	c, err := LoadWith(Flags{ConfigFile: cfgFile}, env(nil, runtime.GOOS))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9000" || c.TLS.Mode != TLSOff || c.Log.Level != "warn" || c.Log.Format != "json" ||
		!strings.HasSuffix(filepath.ToSlash(c.DataDir), "fromfile") || c.ConfigFile != cfgFile {
		t.Fatalf("file: %+v", c)
	}
	if p := c.TrustedProxyPrefixes(); len(p) != 1 || p[0].String() != "10.0.0.0/8" {
		t.Fatalf("proxies %v", p)
	}

	// Env over file.
	e := map[string]string{"LUDWORKS_LISTEN": ":9100", "LUDWORKS_LOG_LEVEL": "debug", "LUDWORKS_TRUSTED_PROXIES": "192.168.0.1, fd00::/8"}
	c, err = LoadWith(Flags{ConfigFile: cfgFile}, env(e, runtime.GOOS))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9100" || c.Log.Level != "debug" || c.Log.Format != "json" || len(c.TrustedProxies) != 2 {
		t.Fatalf("env: %+v", c)
	}
	if p := c.TrustedProxyPrefixes(); p[0].String() != "192.168.0.1/32" {
		t.Fatalf("single IP prefix: %v", p)
	}

	// Flags over env.
	flagData := filepath.Join(dir, "fromflag")
	c, err = LoadWith(Flags{ConfigFile: cfgFile, Listen: "127.0.0.1:18080", DataDir: flagData, TLSMode: "self-signed", LogLevel: "error"}, env(e, runtime.GOOS))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:18080" || c.DataDir != flagData || c.TLS.Mode != TLSSelfSigned || c.Log.Level != "error" {
		t.Fatalf("flags: %+v", c)
	}

	// LUDWORKS_CONFIG locates the file when --config is absent.
	c, err = LoadWith(Flags{}, env(map[string]string{"LUDWORKS_CONFIG": cfgFile}, runtime.GOOS))
	if err != nil || c.Listen != ":9000" {
		t.Fatalf("LUDWORKS_CONFIG: %+v %v", c, err)
	}
}

func TestDefaultConfigFileOptionalButExplicitRequired(t *testing.T) {
	data := t.TempDir()
	// Non-linux: default config path is <data>/config.json; missing is fine.
	if _, err := LoadWith(Flags{DataDir: data}, env(nil, "darwin")); err != nil && runtime.GOOS == "darwin" {
		t.Fatal(err)
	}
	if _, err := LoadWith(Flags{ConfigFile: filepath.Join(data, "missing.json")}, env(nil, runtime.GOOS)); err == nil {
		t.Fatal("missing explicit config accepted")
	}
	// On Windows the default config file lives in the data dir selected by flag/env.
	os.WriteFile(filepath.Join(data, "config.json"), []byte(`{"listen":":7777"}`), 0o600)
	if runtime.GOOS == "windows" {
		c, err := LoadWith(Flags{DataDir: data}, env(nil, "windows"))
		if err != nil || c.Listen != ":7777" {
			t.Fatalf("%+v %v", c, err)
		}
	}
}

func TestStrictAndValidation(t *testing.T) {
	data := t.TempDir()
	bad := []string{
		`{"listn":":1"}`,               // typo → unknown field
		`{"trusted_proxies":["nope"]}`, // bad CIDR
		`{"tls":{"mode":"files"}}`,     // missing cert/key
		`{"tls":{"mode":"acme"}}`,      // missing domains
		`{"tls":{"mode":"acme","acme_domains":["bad domain"]}}`,
		`{"tls":{"mode":"weird"}}`,
		`{"listen":"8443"}`,
		`{"log":{"level":"loud"}}`,
		`{"log":{"format":"xml"}}`,
		`{"tls":{"mode":"off","redirect_listen":":80"}}`,
		`{"listen":":1"} {"x":1}`,
	}
	for _, b := range bad {
		if _, err := LoadWith(Flags{ConfigFile: writeConfig(t, b), DataDir: data}, env(nil, runtime.GOOS)); err == nil {
			t.Errorf("accepted %s", b)
		}
	}
	good := "\xef\xbb\xbf" + `{"tls":{"mode":"acme","acme_domains":["nas.example.com"],"acme_email":"a@b.c","redirect_listen":":80"}}`
	c, err := LoadWith(Flags{ConfigFile: writeConfig(t, good), DataDir: data}, env(nil, runtime.GOOS))
	if err != nil || c.TLS.RedirectListen != ":80" {
		t.Fatalf("BOM/acme config: %v", err)
	}
	c, err = LoadWith(Flags{DataDir: data}, env(map[string]string{"LUDWORKS_TLS_REDIRECT_LISTEN": "off", "LUDWORKS_TLS_MODE": "OFF"}, runtime.GOOS))
	if err != nil || c.TLS.RedirectListen != "" || c.TLS.Mode != TLSOff {
		t.Fatalf("redirect off: %+v %v", c, err)
	}
}

func TestLayoutAndEnsureDirs(t *testing.T) {
	c := &Config{DataDir: filepath.Join(t.TempDir(), "d")}
	if err := c.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{c.DataDir, c.StorageDir(), c.TmpDir(), c.ThumbsDir(), c.BackupsDir(), c.TLSDir()} {
		st, err := os.Stat(p)
		if err != nil || !st.IsDir() {
			t.Fatalf("%s: %v", p, err)
		}
		if runtime.GOOS != "windows" && st.Mode().Perm() != 0o700 {
			t.Errorf("%s: mode %v", p, st.Mode().Perm())
		}
	}
	if filepath.Base(c.DBPath()) != "ludworks.db" || filepath.Base(c.SecretKeyPath()) != "secret.key" ||
		filepath.Base(c.SetupTokenPath()) != "setup-token.txt" {
		t.Fatal("layout names")
	}
	p := filepath.Join(c.DataDir, "f.txt")
	if err := WriteFileAtomic(p, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "hello" {
		t.Fatal("content")
	}
	if st, _ := os.Stat(p); runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v", st.Mode().Perm())
	}
	if m, _ := filepath.Glob(filepath.Join(c.DataDir, ".tmp-*")); len(m) != 0 {
		t.Fatalf("temp files left: %v", m)
	}
}
