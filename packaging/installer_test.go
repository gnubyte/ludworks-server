package packaging

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func readWindows(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("windows", name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// issSection returns the lines of one [Section] of an Inno Setup script, comments dropped.
func issSection(iss, name string) []string {
	var out []string
	in := false
	for _, line := range strings.Split(iss, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") && !strings.Contains(trimmed, " ") {
			in = strings.EqualFold(trimmed, "["+name+"]")
			continue
		}
		if in && trimmed != "" && !strings.HasPrefix(trimmed, ";") {
			out = append(out, trimmed)
		}
	}
	return out
}

// pascalFunc returns the body of a [Code] function, from its header to the matching "end;".
func pascalFunc(t *testing.T, iss, name string) string {
	t.Helper()
	start := strings.Index(iss, "function "+name+"(")
	if start < 0 {
		t.Fatalf("ludworks.iss has no function %s", name)
	}
	rest := iss[start:]
	end := strings.Index(rest, "\nend;")
	if end < 0 {
		t.Fatalf("function %s has no end", name)
	}
	return rest[:end]
}

// The service runs ludworks.exe from the install folder (and the uninstaller runs elevated
// from it), so the installer must only ever use Program Files, which only administrators can
// change: no folder page, no reuse of an earlier custom folder, /DIR= and services registered
// for a program elsewhere refused before anything is stopped or copied.
func TestInstallerConfinedToProgramFiles(t *testing.T) {
	iss := readWindows(t, "ludworks.iss")
	setup := map[string]string{}
	for _, line := range issSection(iss, "Setup") {
		if k, v, ok := strings.Cut(line, "="); ok {
			setup[strings.ToLower(k)] = v
		}
	}
	for k, want := range map[string]string{
		"defaultdirname":     `{autopf}\Ludworks`,
		"disabledirpage":     "yes",
		"usepreviousappdir":  "no",
		"privilegesrequired": "admin",
	} {
		if got := setup[k]; !strings.EqualFold(got, want) {
			t.Errorf("[Setup] %s=%q, want %q", k, got, want)
		}
	}
	if _, ok := setup["privilegesrequiredoverridesallowed"]; ok {
		t.Error("the installer must not allow a per-user install (its folder would be user-writable)")
	}
	// Inno's Permissions: only add allow entries and cannot remove inherited write access.
	if regexp.MustCompile(`(?im)^[^;\n]*\bPermissions:`).MatchString(iss) {
		t.Error("ludworks.iss must not rely on Permissions: to harden a folder")
	}

	problem := pascalFunc(t, iss, "InstallLocationProblem")
	for _, want := range []string{
		`ExpandConstant('{app}')`, `ExpandConstant('{autopf}\Ludworks')`, "CompareText(AddBackslash(AppDir), AddBackslash(Wanted)) <> 0",
		`'SYSTEM\CurrentControlSet\Services\{#ServiceName}', 'ImagePath'`, "ImagePathProgram(ImagePath)",
		"AddBackslash(Wanted) + 'ludworks.exe'", "sc.exe delete",
	} {
		if !strings.Contains(problem, want) {
			t.Errorf("InstallLocationProblem lacks %q", want)
		}
	}
	prepare := pascalFunc(t, iss, "PrepareToInstall")
	guard := strings.Index(prepare, "Result := InstallLocationProblem();")
	exit := strings.Index(prepare, "Exit;")
	stop := strings.Index(prepare, "Stop-Service")
	if guard < 0 || exit < guard || stop < 0 || stop < exit {
		t.Error("PrepareToInstall must refuse a bad location (and return) before it stops the service")
	}
}

// ImagePathProgram's contract, mirrored by Get-ServiceProgram in install-service.ps1.
func TestServiceProgramParsers(t *testing.T) {
	iss := readWindows(t, "ludworks.iss")
	body := pascalFunc(t, iss, "ImagePathProgram")
	for _, want := range []string{"Copy(S, 1, 1) = '\"'", "Pos('\"', S)", "Pos(' ', S)"} {
		if !strings.Contains(body, want) {
			t.Errorf("ImagePathProgram lacks %q", want)
		}
	}
	ps := readWindows(t, "install-service.ps1")
	for _, want := range []string{"function Get-ServiceProgram", `$p.StartsWith('"')`, `HKLM:\SYSTEM\CurrentControlSet\Services\$ServiceName`,
		"if ($registered -ine $exe)", "sc.exe delete $ServiceName"} {
		if !strings.Contains(ps, want) {
			t.Errorf("install-service.ps1 lacks %q", want)
		}
	}
	// The check runs before the existing service is stopped for the upgrade.
	if check, stop := strings.Index(ps, "if ($registered -ine $exe)"), strings.Index(ps, "Stop-Service -Name $ServiceName"); check < 0 || stop < check {
		t.Error("install-service.ps1 must check the registered program before stopping the service")
	}
}

// The firewall rule admits only the local subnet (the LAN's IPv4 subnet and on-link IPv6
// prefixes), not every address that reaches a private-profile interface, and upgrades
// narrow rules left by older versions.
func TestFirewallRuleLimitedToLocalSubnet(t *testing.T) {
	iss := readWindows(t, "ludworks.iss")
	adds := 0
	narrows := 0
	for _, line := range issSection(iss, "Run") {
		if strings.Contains(line, "firewall add rule") {
			adds++
			if !strings.Contains(line, "remoteip=localsubnet") || !strings.Contains(line, "Tasks: firewall;") {
				t.Errorf("unscoped firewall rule: %s", line)
			}
		}
		if strings.Contains(line, "firewall set rule") {
			narrows++
			if !strings.Contains(line, "new remoteip=localsubnet") || !strings.Contains(line, "Tasks: not firewall;") {
				t.Errorf("upgrade narrowing entry: %s", line)
			}
		}
	}
	if adds != 1 || narrows != 1 {
		t.Errorf("ludworks.iss has %d add-rule and %d set-rule entries, want 1 each", adds, narrows)
	}
	if strings.Contains(iss, "private networks to connect") {
		t.Error("the firewall task must not promise more than the local subnet")
	}

	ps := readWindows(t, "install-service.ps1")
	newRule := ps[strings.Index(ps, "New-NetFirewallRule"):]
	newRule = newRule[:strings.Index(newRule, "Out-Null")]
	if !strings.Contains(newRule, "-RemoteAddress LocalSubnet") {
		t.Errorf("New-NetFirewallRule without -RemoteAddress LocalSubnet:\n%s", newRule)
	}
	if !strings.Contains(ps, "$rules | Set-NetFirewallRule -RemoteAddress LocalSubnet") {
		t.Error("install-service.ps1 does not narrow an existing rule on upgrade")
	}
	readme := readWindows(t, "README-windows.txt")
	if !strings.Contains(readme, "local subnet") || !strings.Contains(readme, "Ludworks (HTTPS 8443, VPN)") {
		t.Error("README-windows.txt does not document the scope and how to widen it with a separate rule")
	}
}
