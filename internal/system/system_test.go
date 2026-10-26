package system

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ludworks/internal/auth"
	"ludworks/internal/auth/authtest"
	"ludworks/internal/core"
	"ludworks/internal/httpx"
	"ludworks/internal/settings"
)

// filesSchema is the ARCHITECTURE.md §3.3 schema of the tables the files module owns. The
// system module only reads them, so the tests create them as fixtures.
const filesSchema = `
CREATE TABLE spaces(id TEXT PRIMARY KEY, kind TEXT CHECK(kind IN ('home','library')), name TEXT,
  owner_id TEXT NULL REFERENCES users(id) ON DELETE SET NULL, root_dir TEXT, created_at TEXT);
CREATE TABLE space_members(space_id TEXT, user_id TEXT, access TEXT CHECK(access IN ('read','write')),
  PRIMARY KEY(space_id, user_id));
CREATE TABLE files(id TEXT PRIMARY KEY, space_id TEXT REFERENCES spaces(id) ON DELETE CASCADE, parent_id TEXT NULL,
  path TEXT, name TEXT, is_dir INTEGER, size INTEGER, mime TEXT, mod_time TEXT, category TEXT DEFAULT '',
  subcategory TEXT DEFAULT '', tags TEXT DEFAULT '[]', sha256 TEXT DEFAULT '', owner_id TEXT, trashed_at TEXT NULL,
  trash_origin TEXT NULL, created_at TEXT, updated_at TEXT, UNIQUE(space_id, path));`

type env struct {
	*authtest.Env
	sys   *Module
	admin *authtest.Client
}

func newEnv(t *testing.T) *env {
	t.Helper()
	var sys *Module
	e := authtest.New(t, authtest.Options{Modules: func(e *authtest.Env) []httpx.Module {
		sys = New(e.Deps, nil)
		return []httpx.Module{sys}
	}})
	adm := e.Setup()
	return &env{Env: e, sys: sys, admin: adm}
}

func (e *env) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := e.Deps.DB.SQL.ExecContext(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func (e *env) createUser(t *testing.T, name string, quota int64) (string, *authtest.Client) {
	t.Helper()
	u, err := e.Auth.CreateUser(context.Background(), auth.CreateUserInput{
		Username: name, Email: name + "@example.com", Password: authtest.AdminPassword, Role: "user", QuotaBytes: &quota,
	})
	if err != nil {
		t.Fatal(err)
	}
	return u.ID, e.TokenLogin(name, authtest.AdminPassword)
}

func (e *env) adminID(t *testing.T) string {
	t.Helper()
	var id string
	if err := e.Deps.DB.SQL.QueryRow(`SELECT id FROM users WHERE username = ?`, authtest.AdminUser).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func storage(t *testing.T, c *authtest.Client) (Storage, map[string]any) {
	t.Helper()
	r := c.Do("GET", "/api/v1/system/storage", nil).Expect(200)
	var st Storage
	r.JSON(&st)
	return st, r.Map()
}

func TestStatDiskOnTempDir(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	a, err := statDisk(dir)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == "" || a.Total == 0 || a.Free > a.Total || a.Avail > a.Total {
		t.Fatalf("implausible statfs result: %+v", a)
	}
	b, err := statDisk(sub)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID || a.Total != b.Total {
		t.Fatalf("same filesystem reported differently: %+v vs %+v", a, b)
	}
	if _, err := statDisk(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing directory should fail")
	}
}

func TestSaturatingHelpers(t *testing.T) {
	if mulSat(1<<40, 1<<40) != ^uint64(0) || mulSat(0, 5) != 0 || mulSat(3, 4) != 12 {
		t.Fatal("mulSat")
	}
	if nonNeg(int64(-5)) != 0 || nonNeg(int32(7)) != 7 || nonNeg(uint32(9)) != 9 {
		t.Fatal("nonNeg")
	}
	if toInt64(^uint64(0)) != 1<<63-1 || addSat(1<<62, 1<<62) != 1<<63-1 || addSat(2, 3) != 5 {
		t.Fatal("int64 saturation")
	}
}

func TestStorageWithoutFilesTables(t *testing.T) {
	e := newEnv(t)
	st, raw := storage(t, e.admin)
	if st.User.UsageBytes != 0 || st.User.TrashBytes != 0 || len(st.User.ByCategory) != 0 || len(st.Spaces) != 0 {
		t.Fatalf("expected empty usage: %+v", st)
	}
	if len(st.Users) != 1 || st.Users[0].Username != authtest.AdminUser {
		t.Fatalf("users: %+v", st.Users)
	}
	// data and storage live on the same filesystem in the test, so they merge.
	if len(st.Volumes) != 1 || st.Volumes[0].Label != "data, storage" || st.Volumes[0].Path != e.Deps.Config.DataDir ||
		st.Volumes[0].TotalBytes <= 0 || st.Volumes[0].UsedBytes+st.Volumes[0].FreeBytes > st.Volumes[0].TotalBytes {
		t.Fatalf("volumes: %+v", st.Volumes)
	}
	if raw["uploads_pending_bytes"] != float64(0) || raw["data_dir"] != e.Deps.Config.DataDir {
		t.Fatalf("admin fields: %v", raw)
	}
	for _, k := range []string{"user", "volumes", "spaces", "users"} {
		if _, ok := raw[k]; !ok {
			t.Fatalf("missing %q in %v", k, raw)
		}
	}
	e.NewClient().Do("GET", "/api/v1/system/storage", nil).ExpectError(401, "unauthorized")
}

func TestStorageAggregation(t *testing.T) {
	e := newEnv(t)
	aliceID, alice := e.createUser(t, "alice", 1000)
	bobID, _ := e.createUser(t, "bob", 0)
	adminID := e.adminID(t)
	e.exec(t, filesSchema)
	libDir := t.TempDir()
	for _, s := range [][]any{
		{"home_alice", "home", "Home", aliceID, "homes/alice"},
		{"home_bob", "home", "Home", bobID, "homes/bob"},
		{"lib_movies", "library", "Movies", nil, libDir},
		{"lib_secret", "library", "Secret", nil, "libraries/secret"},
		{"lib_public", "library", "Public", nil, "../outside"},
	} {
		e.exec(t, `INSERT INTO spaces(id, kind, name, owner_id, root_dir, created_at) VALUES (?, ?, ?, ?, ?, '2026-01-01T00:00:00.000Z')`, s...)
	}
	e.exec(t, `INSERT INTO space_members VALUES ('lib_movies', ?, 'read'), ('lib_public', '*', 'read')`, aliceID)
	n := 0
	file := func(space, owner string, isDir bool, size int64, cat string, trashed bool) {
		n++
		var tr any
		if trashed {
			tr = "2026-02-01T00:00:00.000Z"
		}
		dir := 0
		if isDir {
			dir = 1
		}
		e.exec(t, `INSERT INTO files(id, space_id, path, name, is_dir, size, category, owner_id, trashed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, "file"+itoa(n), space, "p"+itoa(n), "n", dir, size, cat, owner, tr)
	}
	file("home_alice", aliceID, false, 100, "movie", false)
	file("home_alice", aliceID, false, 50, "tv", false)
	file("home_alice", aliceID, false, 25, "", false)
	file("home_alice", aliceID, true, 4096, "", false) // directories never count
	file("home_alice", aliceID, false, 40, "movie", true)
	file("lib_movies", aliceID, false, 200, "movie", false)
	file("home_bob", bobID, false, 70, "music", false)
	file("lib_public", bobID, false, 30, "image", false)
	file("lib_secret", adminID, false, 1, "other", false)

	st, raw := storage(t, alice)
	want := UserUsage{UsageBytes: 375, QuotaBytes: 1000, TrashBytes: 40, ByCategory: map[string]int64{"movie": 300, "tv": 50, "": 25}}
	if got, _ := json.Marshal(st.User); string(got) != string(must(json.Marshal(want))) {
		t.Fatalf("alice usage %s, want %+v", got, want)
	}
	if got := spaceSummary(st.Spaces); got != "home_alice:175:3 lib_movies:200:1 lib_public:30:1" {
		t.Fatalf("alice spaces: %s", got)
	}
	if len(st.Volumes) != 0 || len(st.Users) != 0 {
		t.Fatalf("non-admin got admin data: %+v", st)
	}
	for _, k := range []string{"uploads_pending_bytes", "data_dir"} {
		if _, ok := raw[k]; ok {
			t.Fatalf("non-admin response has %q", k)
		}
	}

	// Pending uploads: only regular *.part files count.
	up := filepath.Join(e.Deps.Config.TmpDir(), "uploads")
	if err := os.MkdirAll(filepath.Join(up, "dir.part"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, size := range map[string]int{"a.part": 10, "b.part": 5, "c.info": 7} {
		if err := os.WriteFile(filepath.Join(up, name), make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink(filepath.Join(libDir), filepath.Join(up, "link.part")); err != nil {
			t.Fatal(err)
		}
	}

	st, raw = storage(t, e.admin)
	if got := spaceSummary(st.Spaces); got != "lib_movies:200:1 lib_public:30:1 lib_secret:1:1" {
		t.Fatalf("admin spaces: %s", got)
	}
	var users []string
	for _, u := range st.Users {
		users = append(users, u.Username+":"+itoa(int(u.UsageBytes))+":"+itoa(int(u.QuotaBytes)))
	}
	if got := strings.Join(users, " "); got != "admin:1:0 alice:375:1000 bob:100:0" {
		t.Fatalf("admin users: %s", got)
	}
	if raw["uploads_pending_bytes"] != float64(15) {
		t.Fatalf("uploads_pending_bytes = %v", raw["uploads_pending_bytes"])
	}
	// Movies lives in another temp dir; on the same filesystem it merges into one volume.
	if len(st.Volumes) == 0 || !strings.HasPrefix(st.Volumes[0].Label, "data, storage") {
		t.Fatalf("volumes: %+v", st.Volumes)
	}

	// An admin's personal access token without the admin scope gets the user view.
	_, tok, err := auth.IssueToken(context.Background(), e.Deps, adminID, "ci", []string{"files:read"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	st, raw = storage(t, e.WithBearer(tok))
	if len(st.Users) != 0 || len(st.Volumes) != 0 || raw["data_dir"] != nil {
		t.Fatalf("token without admin scope saw admin data: %v", raw)
	}
	if st.User.UsageBytes != 1 {
		t.Fatalf("admin own usage: %+v", st.User)
	}
}

func spaceSummary(s []SpaceUsage) string {
	var parts []string
	for _, x := range s {
		parts = append(parts, x.ID+":"+itoa(int(x.UsageBytes))+":"+itoa(int(x.FileCount)))
	}
	return strings.Join(parts, " ")
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

func TestVolumesMergeAndSkip(t *testing.T) {
	e := newEnv(t)
	cfg := e.Deps.Config
	abs := filepath.Join(t.TempDir(), "media")
	ids := map[string]string{
		cfg.DataDir: "A", cfg.StorageDir(): "A", abs: "B",
		filepath.Join(cfg.StorageDir(), "l1"): "A", filepath.Join(cfg.StorageDir(), "l2"): "A",
		filepath.Join(cfg.StorageDir(), "l3"): "A",
	}
	var calls []string
	e.sys.statDisk = func(p string) (diskStat, error) {
		calls = append(calls, p)
		id, ok := ids[p]
		if !ok {
			return diskStat{}, errors.New("no such dir")
		}
		if id == "B" {
			return diskStat{ID: id, Total: 1000, Free: 400, Avail: 300}, nil
		}
		return diskStat{ID: id, Total: 100, Free: 120, Avail: 150}, nil // inconsistent: clamped
	}
	vols := e.sys.volumes(context.Background(), []libraryRoot{
		{"Films", abs}, {"Escape", "../../etc"}, {"Gone", "missing"}, {"", ""},
		{"One", "l1"}, {"Two", "l2"}, {"Three", "l3"},
	})
	if len(vols) != 2 {
		t.Fatalf("volumes: %+v", vols)
	}
	if v := vols[0]; v.Path != cfg.DataDir || v.Label != "data, storage, library: One +2" || v.TotalBytes != 100 || v.UsedBytes != 0 || v.FreeBytes != 100 {
		t.Fatalf("merged volume: %+v", v)
	}
	if v := vols[1]; v.Path != abs || v.Label != "library: Films" || v.TotalBytes != 1000 || v.UsedBytes != 600 || v.FreeBytes != 300 {
		t.Fatalf("library volume: %+v", v)
	}
	for _, c := range calls {
		if strings.Contains(c, "etc") {
			t.Fatalf("measured a path outside storage: %s", c)
		}
	}
}

func TestStatTimeout(t *testing.T) {
	e := newEnv(t)
	block := make(chan struct{})
	defer close(block)
	e.sys.statDisk = func(string) (diskStat, error) { <-block; return diskStat{}, nil }
	e.sys.statTimeout = 50 * time.Millisecond
	start := time.Now()
	if _, err := e.sys.statWithTimeout("/x"); err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("expected a timeout error, got %v after %v", err, time.Since(start))
	}
}

type mediaNS struct {
	FFmpegPath  string `json:"ffmpeg_path"`
	FFprobePath string `json:"ffprobe_path"`
}

func TestInfo(t *testing.T) {
	e := newEnv(t)
	e.Deps.Settings.Register("media", mediaNS{}, nil, nil)
	var calls atomic.Int32
	var lastPath atomic.Value
	e.sys.detect = func(ff, fp string) (string, string, string) {
		calls.Add(1)
		lastPath.Store(ff)
		if ff == "" {
			return "", "", ""
		}
		return ff, "", "7.1"
	}
	now := time.Now()
	e.sys.now = func() time.Time { return now }
	notAfter := time.Date(2029, 1, 28, 12, 0, 0, 0, time.UTC)
	e.Deps.Info.TLSMode = "self-signed"
	e.Deps.Info.TLSCert = func() *core.TLSCertInfo {
		return &core.TLSCertInfo{FingerprintSHA256: "AB:CD", NotAfter: notAfter}
	}

	get := func() Info {
		var info Info
		e.admin.Do("GET", "/api/v1/system/info", nil).Expect(200).JSON(&info)
		return info
	}
	info := get()
	if info.Version == "" || info.GoVersion != runtime.Version() || info.OS != runtime.GOOS || info.Arch != runtime.GOARCH {
		t.Fatalf("build info: %+v", info)
	}
	if info.UserCount != 1 || info.FileCount != 0 || info.DBSizeBytes <= 0 || info.UptimeSeconds < 0 || info.StartedAt == "" {
		t.Fatalf("counts: %+v", info)
	}
	if info.FFmpeg.Available || info.FFmpeg.Version != "" {
		t.Fatalf("ffmpeg should be unavailable: %+v", info.FFmpeg)
	}
	if info.TLS.Mode != "self-signed" || info.TLS.FingerprintSHA256 == nil || *info.TLS.FingerprintSHA256 != "AB:CD" ||
		info.TLS.NotAfter == nil || *info.TLS.NotAfter != "2029-01-28T12:00:00.000Z" {
		t.Fatalf("tls: %+v", info.TLS)
	}

	// Cached for 5 minutes...
	get()
	if calls.Load() != 1 {
		t.Fatalf("detect ran %d times, want 1 (cached)", calls.Load())
	}
	// ...but a settings change is picked up at once.
	e.Patch("media", map[string]any{"ffmpeg_path": "/opt/ffmpeg/bin/ffmpeg"})
	info = get()
	if calls.Load() != 2 || !info.FFmpeg.Available || info.FFmpeg.Version != "7.1" || lastPath.Load() != "/opt/ffmpeg/bin/ffmpeg" {
		t.Fatalf("after settings change: calls=%d info=%+v", calls.Load(), info.FFmpeg)
	}
	now = now.Add(toolCacheTTL + time.Second)
	get()
	if calls.Load() != 3 {
		t.Fatalf("cache should expire after the TTL (calls=%d)", calls.Load())
	}

	// TLS off: nulls.
	e.Deps.Info.TLSMode = "off"
	e.Deps.Info.TLSCert = nil
	raw := e.admin.Do("GET", "/api/v1/system/info", nil).Expect(200).Map()
	tls := raw["tls"].(map[string]any)
	if tls["mode"] != "off" || tls["fingerprint_sha256"] != nil || tls["not_after"] != nil {
		t.Fatalf("tls off: %v", tls)
	}
	if _, ok := tls["fingerprint_sha256"]; !ok {
		t.Fatal("fingerprint_sha256 must be present (null)")
	}

	// Files are counted once the files table exists (non-trashed, non-directories).
	e.exec(t, filesSchema)
	e.exec(t, `INSERT INTO spaces(id, kind, name) VALUES ('s1', 'library', 'L')`)
	e.exec(t, `INSERT INTO files(id, space_id, path, is_dir, size) VALUES ('f1','s1','a',0,1), ('f2','s1','b',1,0)`)
	e.exec(t, `INSERT INTO files(id, space_id, path, is_dir, size, trashed_at) VALUES ('f3','s1','c',0,1,'2026-01-01T00:00:00.000Z')`)
	if got := get(); got.FileCount != 1 {
		t.Fatalf("file_count = %d", got.FileCount)
	}

	// Authorization: users and admin tokens without the admin scope get 403.
	_, bob := e.createUser(t, "bob", 0)
	bob.Do("GET", "/api/v1/system/info", nil).ExpectError(403, "forbidden")
	_, tok, err := auth.IssueToken(context.Background(), e.Deps, e.adminID(t), "ci", []string{"files:read"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.WithBearer(tok).Do("GET", "/api/v1/system/info", nil).ExpectError(403, "forbidden")
	e.NewClient().Do("GET", "/api/v1/system/info", nil).ExpectError(401, "unauthorized")
}

// TestInfoRealDetection runs probe.DetectTools against a fake ffmpeg script.
func TestInfoRealDetection(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake ffmpeg")
	}
	e := newEnv(t)
	e.Deps.Settings.Register("media", mediaNS{}, nil, settings.Validator(func(*mediaNS) error { return nil }))
	dir := t.TempDir()
	ff := filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(ff, []byte("#!/bin/sh\necho 'ffmpeg version 6.1.1-test Copyright (c) 2000-2024'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.Patch("media", map[string]any{"ffmpeg_path": ff})
	var info Info
	e.admin.Do("GET", "/api/v1/system/info", nil).Expect(200).JSON(&info)
	if !info.FFmpeg.Available || info.FFmpeg.Version != "6.1.1-test" {
		t.Fatalf("ffmpeg: %+v", info.FFmpeg)
	}
	// A configured path that does not exist means "not available" (no PATH fallback).
	e.Patch("media", map[string]any{"ffmpeg_path": filepath.Join(dir, "nope")})
	e.admin.Do("GET", "/api/v1/system/info", nil).Expect(200).JSON(&info)
	if info.FFmpeg.Available {
		t.Fatalf("missing configured ffmpeg reported available: %+v", info.FFmpeg)
	}
}
