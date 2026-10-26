package packaging

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func readWorkflow(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", ".github", "workflows", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// workflowJob returns the text of one job (from "  <name>:" to the next job).
func workflowJob(t *testing.T, wf, name string) string {
	t.Helper()
	start := strings.Index(wf, "\n  "+name+":\n")
	if start < 0 {
		t.Fatalf("no job %q", name)
	}
	rest := wf[start+1:]
	next := regexp.MustCompile(`\n  [a-z][a-z0-9_-]*:\n`).FindStringIndex(rest[1:])
	if next == nil {
		return rest
	}
	return rest[:next[0]+1]
}

var usesRE = regexp.MustCompile(`(?m)^\s*(?:-\s+)?uses:\s*(\S+)(.*)$`)

// Every action is pinned to a full commit SHA with the tag in a comment (Dependabot keeps both
// current); a moving tag lets whoever controls it change what the workflow runs.
func TestWorkflowActionsArePinned(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", ".github", "workflows", "*.yml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no workflows: %v", err)
	}
	pinned := regexp.MustCompile(`^[\w.-]+/[\w./-]+@[0-9a-f]{40}$`)
	comment := regexp.MustCompile(`^\s+# v\d+(\.\d+)*$`)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range usesRE.FindAllStringSubmatch(string(b), -1) {
			if !pinned.MatchString(m[1]) || !comment.MatchString(m[2]) {
				t.Errorf("%s: %q is not pinned to a commit SHA with a version comment", filepath.Base(f), strings.TrimSpace(m[0]))
			}
		}
	}
}

// Release assets carry provenance that does not live next to them, a release is created once
// and never altered, publishing waits for a maintainer, the build toolchain is pinned by hash,
// and the reproducibility the docs promise is enforced.
func TestReleaseWorkflowSupplyChain(t *testing.T) {
	wf := readWorkflow(t, "release.yml")
	if strings.Contains(wf, "--clobber") || strings.Contains(wf, "gh release upload") {
		t.Error("release.yml must never replace or add assets of an existing release")
	}
	if strings.Contains(wf, "choco ") {
		t.Error("release.yml must not install build tools from the Chocolatey community feed")
	}
	if !regexp.MustCompile(`(?m)^permissions:\n  contents: read\n`).MatchString(wf) {
		t.Error("release.yml must default to read-only permissions")
	}

	publish := workflowJob(t, wf, "publish")
	for _, want := range []string{
		"\n    environment: release\n",
		"\n      contents: write",
		"\n      id-token: write",
		"\n      attestations: write",
		"gh release view \"$TAG\"",
		"uses: actions/attest@",
		"dist/ludworks_*\n            dist/SHA256SUMS\n",
		"gh attestation verify",
		"--signer-workflow ${GITHUB_REPOSITORY}/.github/workflows/release.yml --source-ref refs/tags/${TAG}",
	} {
		if !strings.Contains(publish, want) {
			t.Errorf("publish job lacks %q", want)
		}
	}
	if view, create := strings.Index(publish, "gh release view"), strings.Index(publish, "gh release create"); view < 0 || create < view {
		t.Error("publish must refuse an existing release before creating one")
	}
	// Only the gated publish job may write or mint OIDC tokens.
	for _, job := range []string{"packages", "installer"} {
		body := workflowJob(t, wf, job)
		if strings.Contains(body, "permissions:") || strings.Contains(body, "id-token") {
			t.Errorf("job %s must not widen permissions", job)
		}
	}

	packages := workflowJob(t, wf, "packages")
	if !strings.Contains(packages, `GOCACHE="$RUNNER_TEMP/gocache-rebuild"`) || !strings.Contains(packages, `diff -u "$first/SHA256SUMS" dist/SHA256SUMS`) {
		t.Error("the packages job must rebuild with a fresh cache and compare the checksums")
	}

	installer := workflowJob(t, wf, "installer")
	for _, want := range []string{
		"https://github.com/jrsoftware/issrc/releases/download/is-6_7_1/innosetup-6.7.1.exe",
		"Get-FileHash -LiteralPath $file -Algorithm SHA256",
		"Get-AuthenticodeSignature",
		"& $env:ISCC ",
		// Smoke test: custom folder refused, folder ACL, service path and firewall scope.
		"'/DIR=C:\\Ludworks'",
		"if ($p.ExitCode -eq 0)",
		"'S-1-1-0', 'S-1-5-11', 'S-1-5-32-545', 'S-1-5-4', 'S-1-5-32-546'",
		"HKLM:\\SYSTEM\\CurrentControlSet\\Services\\Ludworks",
		"Get-NetFirewallAddressFilter",
		"'LocalSubnet'",
	} {
		if !strings.Contains(installer, want) {
			t.Errorf("installer job lacks %q", want)
		}
	}
	if !regexp.MustCompile(`INNO_SHA256: [0-9a-f]{64}\n`).MatchString(installer) {
		t.Error("the Inno Setup download must be pinned to a SHA-256")
	}
}
