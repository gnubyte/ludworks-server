package packaging

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"ludworks/internal/config"
)

func noEnv(string) string { return "" }

// The packaged /etc/ludworks/config.json must pass internal/config's strict parser and
// produce the documented defaults.
func TestDebConfigParsesStrictly(t *testing.T) {
	cfg, err := config.LoadWith(config.Flags{ConfigFile: filepath.Join("deb", "config.json")},
		config.Environment{GOOS: "linux", Getenv: noEnv})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != "/var/lib/ludworks" || cfg.Listen != ":8443" || cfg.TLS.Mode != config.TLSSelfSigned ||
		cfg.Log.Level != "info" || cfg.Log.Format != "text" || len(cfg.TrustedProxies) != 0 || cfg.TLS.RedirectListen != "" {
		t.Errorf("unexpected config: %+v", cfg)
	}
}

func TestWindowsExampleConfigParsesStrictly(t *testing.T) {
	cfg, err := config.LoadWith(config.Flags{ConfigFile: filepath.Join("windows", "config.example.json")},
		config.Environment{GOOS: "windows", Getenv: noEnv, ProgramData: `C:\ProgramData`})
	if err != nil {
		t.Fatal(err)
	}
	// No data_dir in the example: the service passes --data, the default is %ProgramData%.
	if cfg.DataDir != `C:\ProgramData\Ludworks` || cfg.Listen != ":8443" || cfg.TLS.Mode != config.TLSSelfSigned {
		t.Errorf("unexpected config: %+v", cfg)
	}
}

// parseUnit reads a systemd unit into section -> key -> values (in order).
func parseUnit(t *testing.T, path string) map[string]map[string][]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := map[string]map[string][]string{}
	section := ""
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";"):
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			section = line[1 : len(line)-1]
			if out[section] == nil {
				out[section] = map[string][]string{}
			}
		default:
			k, v, ok := strings.Cut(line, "=")
			if !ok || section == "" {
				t.Fatalf("%s:%d: malformed line %q", path, n, line)
			}
			out[section][k] = append(out[section][k], v)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSystemdUnitHardening(t *testing.T) {
	u := parseUnit(t, filepath.Join("deb", "ludworks.service"))
	want := map[string]string{
		"Type":                    "exec",
		"User":                    "ludworks",
		"Group":                   "ludworks",
		"ExecStart":               "/usr/bin/ludworks serve --config /etc/ludworks/config.json",
		"Restart":                 "on-failure",
		"UMask":                   "0077",
		"LimitNOFILE":             "65536",
		"NoNewPrivileges":         "true",
		"ProtectSystem":           "strict",
		"ProtectHome":             "true",
		"ReadWritePaths":          "/var/lib/ludworks",
		"PrivateTmp":              "true",
		"PrivateDevices":          "true",
		"ProtectKernelTunables":   "true",
		"ProtectKernelModules":    "true",
		"ProtectKernelLogs":       "true",
		"ProtectControlGroups":    "true",
		"ProtectClock":            "true",
		"ProtectHostname":         "true",
		"RestrictAddressFamilies": "AF_INET AF_INET6 AF_UNIX AF_NETLINK",
		"RestrictNamespaces":      "true",
		"RestrictRealtime":        "true",
		"RestrictSUIDSGID":        "true",
		"LockPersonality":         "true",
		"MemoryDenyWriteExecute":  "true",
		"SystemCallArchitectures": "native",
		"SystemCallFilter":        "@system-service",
		"SystemCallErrorNumber":   "EPERM",
		"CapabilityBoundingSet":   "",
		"AmbientCapabilities":     "",
	}
	svc := u["Service"]
	for k, v := range want {
		got, ok := svc[k]
		if !ok || len(got) != 1 || got[0] != v {
			t.Errorf("[Service] %s = %q, want exactly %q", k, got, v)
		}
	}
	if got := u["Install"]["WantedBy"]; len(got) != 1 || got[0] != "multi-user.target" {
		t.Errorf("[Install] WantedBy = %q", got)
	}
	if got := u["Unit"]["After"]; len(got) != 1 || got[0] != "network-online.target" {
		t.Errorf("[Unit] After = %q", got)
	}
}

func TestBindPrivilegedPortDropIn(t *testing.T) {
	u := parseUnit(t, filepath.Join("deb", "bind-privileged-port.conf"))
	svc := u["Service"]
	if len(u) != 1 || len(svc) != 2 ||
		strings.Join(svc["AmbientCapabilities"], ",") != "CAP_NET_BIND_SERVICE" ||
		strings.Join(svc["CapabilityBoundingSet"], ",") != "CAP_NET_BIND_SERVICE" {
		t.Errorf("drop-in grants %v, want exactly CAP_NET_BIND_SERVICE", u)
	}
}

// The scripts are run by Windows PowerShell 5.1, which reads BOM-less files in the ANSI code
// page, and shown in Notepad: keep them ASCII.
func TestWindowsFilesAreASCII(t *testing.T) {
	for _, name := range []string{"install-service.ps1", "uninstall-service.ps1", "README-windows.txt", "ludworks.iss", "config.example.json"} {
		b, err := os.ReadFile(filepath.Join("windows", name))
		if err != nil {
			t.Fatal(err)
		}
		for i, c := range b {
			if c >= 0x80 || c < 0x20 && c != '\n' && c != '\r' && c != '\t' {
				t.Errorf("%s: non-ASCII byte %#x at offset %d", name, c, i)
				break
			}
		}
	}
}

func TestWindowsScriptsContract(t *testing.T) {
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join("windows", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	install := read("install-service.ps1")
	for _, want := range []string{"WindowsBuiltInRole]::Administrator", `Join-Path $env:ProgramFiles 'Ludworks'`,
		"service install", "service start", "[switch]$AddFirewallRule", "New-NetFirewallRule", "setup-token.txt",
		"$env:ProgramData"} {
		if !strings.Contains(install, want) {
			t.Errorf("install-service.ps1 lacks %q", want)
		}
	}
	uninstall := read("uninstall-service.ps1")
	for _, want := range []string{"WindowsBuiltInRole]::Administrator", "service uninstall", "Remove-NetFirewallRule", "KEPT"} {
		if !strings.Contains(uninstall, want) {
			t.Errorf("uninstall-service.ps1 lacks %q", want)
		}
	}
	if strings.Contains(uninstall, "$DataDir -Recurse") || strings.Contains(uninstall, "Remove-Item -LiteralPath $DataDir") {
		t.Error("uninstall-service.ps1 must never delete the data directory")
	}
	iss := read("ludworks.iss")
	for _, want := range []string{`DefaultDirName={autopf}\Ludworks`, `Parameters: "service install"`, `Parameters: "service start"`,
		"[UninstallRun]", `Parameters: "service uninstall"`, `Name: "firewall"`, "PrivilegesRequired=admin",
		"ArchitecturesInstallIn64BitMode=x64compatible", "SetupIconFile=ludworks.ico"} {
		if !strings.Contains(iss, want) {
			t.Errorf("ludworks.iss lacks %q", want)
		}
	}
	if strings.Contains(iss, "[UninstallDelete]") {
		t.Error("the installer must keep user data on uninstall")
	}
}

// ---------------------------------------------------------------------------------------
// Maintainer scripts, executed by a POSIX shell against a fake root with stub commands.

type sandbox struct {
	t    *testing.T
	root string
	sh   string
}

const stubLib = `log() { printf '%s\n' "$*" >>"$ROOT/log"; }
`

var stubs = map[string]string{
	"getent": `log getent "$@"
[ -e "$ROOT/state/$1-$2" ]`,
	"addgroup": `log addgroup "$@"
for a; do last=$a; done
: >"$ROOT/state/group-$last"`,
	"adduser": `log adduser "$@"
for a; do last=$a; done
: >"$ROOT/state/passwd-$last"`,
	"deluser": `log deluser "$@"
for a; do last=$a; done
rm -f "$ROOT/state/passwd-$last"`,
	"delgroup": `log delgroup "$@"
for a; do last=$a; done
rm -f "$ROOT/state/group-$last"`,
	"usermod": `log usermod "$@"`,
	"systemctl": `log systemctl "$@"
case "$1" in
is-active) [ -e "$ROOT/state/active" ] ;;
*) exit 0 ;;
esac`,
	"install": `log install "$@"
for a; do last=$a; done
mkdir -p "$last"`,
	"chown": `log chown "$@"`,
	"stat":  `if [ -e "$ROOT/state/owner" ]; then cat "$ROOT/state/owner"; else echo ludworks; fi`,
	"dpkg-statoverride": `log dpkg-statoverride "$@"
case "$1" in
--list) [ -e "$ROOT/state/statoverride" ] ;;
--remove) rm -f "$ROOT/state/statoverride" ;;
*) : >"$ROOT/state/statoverride" ;;
esac`,
	"uname": `echo testhost`,
}

func newSandbox(t *testing.T) *sandbox {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("maintainer scripts need a POSIX shell")
	}
	sh, err := exec.LookPath("dash")
	if err != nil {
		if sh, err = exec.LookPath("sh"); err != nil {
			t.Skip("no POSIX shell")
		}
	}
	s := &sandbox{t: t, root: t.TempDir(), sh: sh}
	for _, d := range []string{"bin", "state", "run/systemd/system", "var/lib", "etc/ludworks", "usr/sbin",
		"etc/systemd/system/multi-user.target.wants"} {
		if err := os.MkdirAll(filepath.Join(s.root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range stubs {
		s.write("bin/"+name, "#!/bin/sh\n"+stubLib+body+"\n", 0o755)
	}
	s.write("etc/ludworks/config.json", "{}\n", 0o640)
	return s
}

func (s *sandbox) write(rel, content string, mode os.FileMode) {
	s.t.Helper()
	p := filepath.Join(s.root, rel)
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		s.t.Fatal(err)
	}
}

func (s *sandbox) path(rel string) string { return filepath.Join(s.root, rel) }

func (s *sandbox) exists(rel string) bool {
	_, err := os.Lstat(s.path(rel))
	return err == nil
}

// run executes a maintainer script with its absolute paths redirected into the sandbox.
func (s *sandbox) run(script string, args ...string) (stdout, stderr string, log []string, code int) {
	s.t.Helper()
	src, err := os.ReadFile(filepath.Join("deb", script))
	if err != nil {
		s.t.Fatal(err)
	}
	body := strings.NewReplacer(
		"/var/lib/ludworks", s.root+"/var/lib/ludworks",
		"/etc/ludworks", s.root+"/etc/ludworks",
		"/etc/systemd/system", s.root+"/etc/systemd/system",
		"/run/", s.root+"/run/",
		"/usr/sbin/policy-rc.d", s.root+"/usr/sbin/policy-rc.d",
	).Replace(string(src))
	p := s.path(script + ".sh")
	s.write(script+".sh", body, 0o755)
	_ = os.Remove(s.path("log"))
	cmd := exec.Command(s.sh, append([]string{p}, args...)...)
	cmd.Env = []string{"PATH=" + s.path("bin") + ":/usr/bin:/bin", "ROOT=" + s.root, "LC_ALL=C"}
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err = cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		s.t.Fatal(err)
	}
	if b, err := os.ReadFile(s.path("log")); err == nil {
		log = strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	}
	return o.String(), e.String(), log, code
}

func (s *sandbox) r(p string) string { return strings.ReplaceAll(p, "@", s.root) }

func hasLine(log []string, line string) bool {
	for _, l := range log {
		if l == line {
			return true
		}
	}
	return false
}

func hasPrefix(log []string, prefix string) bool {
	for _, l := range log {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}

func TestMaintainerScriptsSyntax(t *testing.T) {
	s := newSandbox(t)
	for _, name := range []string{"postinst", "prerm", "postrm"} {
		b, err := os.ReadFile(filepath.Join("deb", name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.HasPrefix(b, []byte("#!/bin/sh\n")) || !bytes.Contains(b, []byte("\nset -e\n")) {
			t.Errorf("%s must be a /bin/sh script with set -e", name)
		}
		if bytes.Contains(b, []byte("\r")) {
			t.Errorf("%s has CRLF line endings", name)
		}
		for _, bashism := range []string{"[[", "function ", "$'", "<<<", "local ", "source ", "echo -e", "==", "${!", "pipefail"} {
			if bytes.Contains(b, []byte(bashism)) {
				t.Errorf("%s uses the non-POSIX construct %q", name, bashism)
			}
		}
		if out, err := exec.Command(s.sh, "-n", filepath.Join("deb", name)).CombinedOutput(); err != nil {
			t.Errorf("%s -n %s: %v\n%s", s.sh, name, err, out)
		}
	}
}

func TestPostinstFreshInstall(t *testing.T) {
	s := newSandbox(t)
	out, errOut, log, code := s.run("postinst", "configure", "")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{
		"addgroup --system --quiet ludworks",
		s.r("adduser --system --quiet --ingroup ludworks --home @/var/lib/ludworks --no-create-home --shell /usr/sbin/nologin ludworks"),
		s.r("install -d -m 0750 -o ludworks -g ludworks @/var/lib/ludworks"),
		s.r("dpkg-statoverride --update --add root ludworks 0640 @/etc/ludworks/config.json"),
		"systemctl daemon-reload",
		"systemctl enable ludworks.service",
		"systemctl start ludworks.service",
	} {
		if !hasLine(log, want) {
			t.Errorf("missing %q in\n%s", want, strings.Join(log, "\n"))
		}
	}
	for _, want := range []string{s.r("sudo cat @/var/lib/ludworks/setup-token.txt"), "https://testhost:8443/", "journalctl -u ludworks"} {
		if !strings.Contains(out, want) {
			t.Errorf("first-run notice lacks %q:\n%s", want, out)
		}
	}

	// Second configure = upgrade without a marker: no account changes, try-restart only.
	out, errOut, log, code = s.run("postinst", "configure", "0.1.0")
	if code != 0 {
		t.Fatalf("upgrade exit %d\n%s\n%s", code, out, errOut)
	}
	if hasPrefix(log, "adduser") || hasPrefix(log, "addgroup") || hasPrefix(log, "install") || hasPrefix(log, "usermod") ||
		hasPrefix(log, "chown") || hasPrefix(log, "dpkg-statoverride --update") || hasLine(log, "systemctl enable ludworks.service") {
		t.Errorf("upgrade repeated install-time work:\n%s", strings.Join(log, "\n"))
	}
	if !hasLine(log, "systemctl try-restart ludworks.service") {
		t.Errorf("upgrade did not try-restart:\n%s", strings.Join(log, "\n"))
	}
}

func TestUpgradeCycleRestartsRunningService(t *testing.T) {
	s := newSandbox(t)
	if _, _, _, code := s.run("postinst", "configure", ""); code != 0 {
		t.Fatal("install failed")
	}
	s.write("state/active", "", 0o644)
	_, _, log, code := s.run("prerm", "upgrade", "0.2.0")
	if code != 0 || !hasLine(log, "systemctl stop ludworks.service") {
		t.Fatalf("prerm upgrade: exit %d\n%s", code, strings.Join(log, "\n"))
	}
	if !s.exists("run/ludworks.dpkg-restart") {
		t.Fatal("prerm did not remember that the service was running")
	}
	_, _, log, code = s.run("postinst", "configure", "0.1.0")
	if code != 0 || !hasLine(log, "systemctl restart ludworks.service") || hasLine(log, "systemctl try-restart ludworks.service") {
		t.Fatalf("postinst after upgrade: exit %d\n%s", code, strings.Join(log, "\n"))
	}
	if s.exists("run/ludworks.dpkg-restart") {
		t.Error("restart marker left behind")
	}
	// A stopped service stays stopped across an upgrade.
	_ = os.Remove(s.path("state/active"))
	_, _, log, _ = s.run("prerm", "upgrade", "0.2.0")
	if s.exists("run/ludworks.dpkg-restart") || !hasLine(log, "systemctl stop ludworks.service") {
		t.Errorf("prerm of a stopped service: marker=%v log=%v", s.exists("run/ludworks.dpkg-restart"), log)
	}
	_, _, log, _ = s.run("postinst", "configure", "0.1.0")
	if hasLine(log, "systemctl restart ludworks.service") || hasLine(log, "systemctl start ludworks.service") {
		t.Errorf("stopped service was started:\n%s", strings.Join(log, "\n"))
	}
}

func TestPostinstWithoutSystemd(t *testing.T) {
	s := newSandbox(t)
	if err := os.RemoveAll(s.path("run/systemd")); err != nil {
		t.Fatal(err)
	}
	out, _, log, code := s.run("postinst", "configure", "")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !hasLine(log, "systemctl enable ludworks.service") || hasLine(log, "systemctl start ludworks.service") ||
		hasLine(log, "systemctl daemon-reload") {
		t.Errorf("without systemd: %v", log)
	}
	if !strings.Contains(out, "systemd is not running") {
		t.Errorf("no hint printed: %s", out)
	}
}

func TestPostinstHonoursPolicyRcD(t *testing.T) {
	s := newSandbox(t)
	s.write("usr/sbin/policy-rc.d", "#!/bin/sh\nexit 101\n", 0o755)
	_, _, log, code := s.run("postinst", "configure", "")
	if code != 0 || hasLine(log, "systemctl start ludworks.service") || !hasLine(log, "systemctl enable ludworks.service") {
		t.Errorf("policy-rc.d 101: exit %d log %v", code, log)
	}
}

func TestPostinstDataDirOwnership(t *testing.T) {
	s := newSandbox(t)
	if err := os.MkdirAll(s.path("var/lib/ludworks/storage"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Left over from a purged install: the old uid no longer resolves.
	s.write("state/owner", "UNKNOWN\n", 0o644)
	_, _, log, code := s.run("postinst", "configure", "")
	if code != 0 || !hasLine(log, s.r("chown -R ludworks:ludworks @/var/lib/ludworks")) {
		t.Errorf("orphaned data dir not adopted: exit %d log %v", code, log)
	}
	if !hasLine(log, "systemctl start ludworks.service") {
		t.Errorf("adopted data dir: service not started: %v", log)
	}
	// Owned by another existing account (e.g. one that inherited the uid an older package's
	// purge freed): never chown, and never enable or start the service on that data.
	s.write("state/owner", "bob\n", 0o644)
	_, errOut, log, code := s.run("postinst", "configure", "")
	if code != 0 || hasPrefix(log, "chown") || !strings.Contains(errOut, "owned by the account 'bob'") ||
		!strings.Contains(errOut, "NOT started") {
		t.Errorf("foreign owner: exit %d log %v stderr %q", code, log, errOut)
	}
	if hasLine(log, "systemctl enable ludworks.service") || hasLine(log, "systemctl start ludworks.service") {
		t.Errorf("foreign owner: service enabled or started:\n%s", strings.Join(log, "\n"))
	}
	// Same on an upgrade whose prerm stopped a running service: it stays stopped.
	s.write("run/ludworks.dpkg-restart", "", 0o644)
	_, _, log, code = s.run("postinst", "configure", "0.1.0")
	if code != 0 || hasPrefix(log, "chown") || hasLine(log, "systemctl restart ludworks.service") ||
		hasLine(log, "systemctl try-restart ludworks.service") || s.exists("run/ludworks.dpkg-restart") {
		t.Errorf("foreign owner on upgrade: exit %d marker=%v log:\n%s", code, s.exists("run/ludworks.dpkg-restart"),
			strings.Join(log, "\n"))
	}
}

func TestPrermRemove(t *testing.T) {
	s := newSandbox(t)
	_, _, log, code := s.run("prerm", "remove")
	if code != 0 || !hasLine(log, "systemctl stop ludworks.service") || s.exists("run/ludworks.dpkg-restart") {
		t.Errorf("prerm remove: exit %d log %v", code, log)
	}
	if _, _, _, code := s.run("prerm", "bogus"); code != 1 {
		t.Errorf("unknown argument: exit %d", code)
	}
}

func TestPostrmRemoveKeepsEverything(t *testing.T) {
	s := newSandbox(t)
	if _, _, _, code := s.run("postinst", "configure", ""); code != 0 {
		t.Fatal("install failed")
	}
	_, _, log, code := s.run("postrm", "remove")
	if code != 0 || !hasLine(log, "systemctl daemon-reload") || hasPrefix(log, "deluser") {
		t.Errorf("postrm remove: exit %d log %v", code, log)
	}
	if !s.exists("etc/ludworks/config.json") || !s.exists("var/lib/ludworks") || !s.exists("state/passwd-ludworks") {
		t.Error("remove deleted configuration, data or the account")
	}
}

func TestPostrmPurgeKeepsData(t *testing.T) {
	s := newSandbox(t)
	if _, _, _, code := s.run("postinst", "configure", ""); code != 0 {
		t.Fatal("install failed")
	}
	s.write("var/lib/ludworks/ludworks.db", "precious", 0o600)
	if err := os.Symlink("/usr/lib/systemd/system/ludworks.service",
		s.path("etc/systemd/system/multi-user.target.wants/ludworks.service")); err != nil {
		t.Fatal(err)
	}
	s.write("run/ludworks.dpkg-restart", "", 0o644)
	out, _, log, code := s.run("postrm", "purge")
	if code != 0 {
		t.Fatalf("purge exit %d", code)
	}
	for _, want := range []string{s.r("dpkg-statoverride --remove @/etc/ludworks/config.json"),
		"usermod --lock --expiredate 1 --shell /usr/sbin/nologin ludworks", "systemctl daemon-reload"} {
		if !hasLine(log, want) {
			t.Errorf("missing %q in %v", want, log)
		}
	}
	// While the data stays, so does the account: deleting it would free its uid/gid for the
	// next system account (adduser hands out the lowest free id), which would then own the data.
	if hasPrefix(log, "deluser") || hasPrefix(log, "delgroup") || hasPrefix(log, "userdel") || hasPrefix(log, "groupdel") ||
		!s.exists("state/passwd-ludworks") || !s.exists("state/group-ludworks") {
		t.Errorf("purge with kept data removed the account:\n%s", strings.Join(log, "\n"))
	}
	if !strings.Contains(out, "deluser --system ludworks") {
		t.Errorf("purge notice does not say how to remove the kept account:\n%s", out)
	}
	if s.exists("etc/ludworks") || s.exists("etc/systemd/system/multi-user.target.wants/ludworks.service") ||
		s.exists("run/ludworks.dpkg-restart") {
		t.Error("purge left configuration, the boot symlink or the restart marker behind")
	}
	if b, err := os.ReadFile(s.path("var/lib/ludworks/ludworks.db")); err != nil || string(b) != "precious" {
		t.Fatal("purge touched the data directory")
	}
	if !strings.Contains(out, "KEPT") || !strings.Contains(out, s.r("@/var/lib/ludworks")) {
		t.Errorf("no data-kept notice:\n%s", out)
	}
	if _, _, _, code := s.run("postrm", "bogus"); code != 1 {
		t.Errorf("unknown argument: exit %d", code)
	}
}

func TestPostrmPurgeWithoutDataRemovesAccount(t *testing.T) {
	s := newSandbox(t)
	if _, _, _, code := s.run("postinst", "configure", ""); code != 0 {
		t.Fatal("install failed")
	}
	// The admin deleted the data first: nothing can inherit, so the account goes too.
	if err := os.RemoveAll(s.path("var/lib/ludworks")); err != nil {
		t.Fatal(err)
	}
	out, _, log, code := s.run("postrm", "purge")
	if code != 0 || !hasLine(log, "deluser --system --quiet ludworks") || !hasLine(log, "delgroup --system --quiet ludworks") ||
		hasPrefix(log, "usermod") {
		t.Errorf("purge without data: exit %d log:\n%s", code, strings.Join(log, "\n"))
	}
	if s.exists("state/passwd-ludworks") || s.exists("state/group-ludworks") {
		t.Error("purge without data kept the account")
	}
	if strings.Contains(out, "KEPT") {
		t.Errorf("data-kept notice without data:\n%s", out)
	}
}

// After a purge that kept data and account, a reinstall reuses the account (same uid, so the
// data is still its own) and re-enables it, without adopting anything by chown.
func TestPurgeThenReinstallReusesAccount(t *testing.T) {
	s := newSandbox(t)
	if _, _, _, code := s.run("postinst", "configure", ""); code != 0 {
		t.Fatal("install failed")
	}
	s.write("var/lib/ludworks/ludworks.db", "precious", 0o600)
	if _, _, _, code := s.run("postrm", "purge"); code != 0 {
		t.Fatal("purge failed")
	}
	// dpkg unpacks the conffile again.
	if err := os.MkdirAll(s.path("etc/ludworks"), 0o755); err != nil {
		t.Fatal(err)
	}
	s.write("etc/ludworks/config.json", "{}\n", 0o640)
	_, errOut, log, code := s.run("postinst", "configure", "")
	if code != 0 {
		t.Fatalf("reinstall exit %d: %s", code, errOut)
	}
	if hasPrefix(log, "adduser") || hasPrefix(log, "addgroup") || hasPrefix(log, "chown") || hasPrefix(log, "install") {
		t.Errorf("reinstall recreated the account or adopted the data:\n%s", strings.Join(log, "\n"))
	}
	for _, want := range []string{"usermod --expiredate  ludworks", "systemctl enable ludworks.service", "systemctl start ludworks.service"} {
		if !hasLine(log, want) {
			t.Errorf("missing %q in\n%s", want, strings.Join(log, "\n"))
		}
	}
}
