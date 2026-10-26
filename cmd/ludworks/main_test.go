package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ludworks/internal/version"
)

func TestVersionAndUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"version"}, &out, &errb); code != 0 || !strings.Contains(out.String(), version.Version) {
		t.Fatalf("version: %d %q", code, out.String())
	}
	if code := run(nil, &out, &errb); code != 2 {
		t.Fatalf("no args: %d", code)
	}
	if code := run([]string{"bogus"}, &out, &errb); code != 2 {
		t.Fatalf("unknown: %d", code)
	}
	if code := run([]string{"serve", "--tls", "nonsense", "--data", t.TempDir()}, &out, &errb); code != 2 {
		t.Fatalf("bad flag value: %d", code)
	}
}

func TestParseFlagsInterspersed(t *testing.T) {
	for _, args := range [][]string{
		{"--data", "/x", "bob", "--log-level", "debug"},
		{"bob", "--data", "/x", "--log-level=debug"},
		{"--data=/x", "--log-level", "debug", "bob"},
	} {
		f, pos, err := parseFlags("t", args, io.Discard)
		if err != nil || f.DataDir != "/x" || f.LogLevel != "debug" || len(pos) != 1 || pos[0] != "bob" {
			t.Errorf("%v: %+v %v %v", args, f, pos, err)
		}
	}
	if _, _, err := parseFlags("t", []string{"--nope"}, io.Discard); err == nil {
		t.Error("unknown flag accepted")
	}
}

func TestReadNewPasswordFromPipe(t *testing.T) {
	p := filepath.Join(t.TempDir(), "in")
	os.WriteFile(p, []byte("correct horse battery staple\r\nsecond line\n"), 0o600)
	f, _ := os.Open(p)
	defer f.Close()
	pw, err := readNewPassword(f, io.Discard)
	if err != nil || pw != "correct horse battery staple" {
		t.Fatalf("%q %v", pw, err)
	}
	rest, _ := io.ReadAll(f)
	if string(rest) != "second line\n" {
		t.Fatalf("over-read: %q", rest)
	}
	os.WriteFile(p, []byte("\n"), 0o600)
	f2, _ := os.Open(p)
	defer f2.Close()
	if _, err := readNewPassword(f2, io.Discard); err == nil {
		t.Fatal("empty password accepted")
	}
	os.WriteFile(p, []byte(strings.Repeat("x", 2000)), 0o600)
	f3, _ := os.Open(p)
	defer f3.Close()
	if _, err := readNewPassword(f3, io.Discard); err == nil {
		t.Fatal("over-long password accepted")
	}
}

func TestServiceCommandOnThisOS(t *testing.T) {
	if isWindowsService() {
		t.Skip()
	}
	var out, errb bytes.Buffer
	code := run([]string{"service", "install", "--data", t.TempDir()}, &out, &errb)
	if code == 0 && !strings.Contains(out.String(), "Service") {
		t.Fatalf("unexpected success: %s", out.String())
	}
}
