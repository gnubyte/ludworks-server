package system

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"

	"ludworks/internal/db"
	"ludworks/internal/httpx"
	"ludworks/internal/settings"
	"ludworks/internal/version"
)

// toolCacheTTL is how long the ffmpeg detection result is reused. Detection runs
// `ffmpeg -version`, which is far too slow to do on every request.
const toolCacheTTL = 5 * time.Minute

// FFmpegInfo reports whether ffmpeg was found and its version.
type FFmpegInfo struct {
	Available bool   `json:"available"`
	Version   string `json:"version"`
}

// TLSInfo describes the served certificate.
type TLSInfo struct {
	Mode              string  `json:"mode"`
	FingerprintSHA256 *string `json:"fingerprint_sha256"`
	NotAfter          *string `json:"not_after"`
}

// Info is the GET /system/info response.
type Info struct {
	Version       string     `json:"version"`
	GoVersion     string     `json:"go_version"`
	OS            string     `json:"os"`
	Arch          string     `json:"arch"`
	Hostname      string     `json:"hostname"`
	UptimeSeconds float64    `json:"uptime_seconds"`
	StartedAt     string     `json:"started_at"`
	DBSizeBytes   int64      `json:"db_size_bytes"`
	UserCount     int64      `json:"user_count"`
	FileCount     int64      `json:"file_count"`
	FFmpeg        FFmpegInfo `json:"ffmpeg"`
	TLS           TLSInfo    `json:"tls"`
}

// toolCache memoises probe.DetectTools per configured path pair.
type toolCache struct {
	mu    sync.Mutex
	key   string
	at    time.Time
	val   FFmpegInfo
	valid bool
}

func (c *toolCache) invalidate() {
	c.mu.Lock()
	c.valid = false
	c.mu.Unlock()
}

// mediaPaths is the part of the "media" settings namespace this module reads. It is
// decoded locally so the system module does not depend on the media package.
type mediaPaths struct {
	FFmpegPath  string `json:"ffmpeg_path"`
	FFprobePath string `json:"ffprobe_path"`
}

func (m *Module) getInfo(w http.ResponseWriter, r *http.Request) error {
	info, err := m.Info(r.Context())
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, info)
}

// Info gathers server information (admin only).
func (m *Module) Info(ctx context.Context) (*Info, error) {
	out := &Info{
		Version: version.Version, GoVersion: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH,
	}
	if h, err := os.Hostname(); err == nil {
		out.Hostname = h
	}
	started := m.d.Info.StartedAt
	if started.IsZero() {
		started = m.started
	}
	out.StartedAt = db.FormatTime(started)
	out.UptimeSeconds = math.Max(0, math.Round(m.now().Sub(started).Seconds()*1000)/1000)
	out.DBSizeBytes = dbSize(m.d.DB.Path())

	err := m.d.DB.ReadTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&out.UserCount); err != nil {
			return err
		}
		t, err := existingTables(ctx, tx)
		if err != nil || !t.files {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM files WHERE is_dir = 0 AND trashed_at IS NULL`).
			Scan(&out.FileCount)
	})
	if err != nil {
		return nil, err
	}

	out.FFmpeg = m.ffmpeg(ctx)

	mode := m.d.Info.TLSMode
	if mode == "" {
		mode = m.d.Config.TLS.Mode
	}
	out.TLS = TLSInfo{Mode: mode}
	if c := m.d.Info.CurrentTLS(); c != nil {
		fp := c.FingerprintSHA256
		out.TLS.FingerprintSHA256 = &fp
		if !c.NotAfter.IsZero() {
			na := db.FormatTime(c.NotAfter)
			out.TLS.NotAfter = &na
		}
	}
	return out, nil
}

// dbSize is the size of the database file plus its write-ahead log.
func dbSize(path string) int64 {
	var total int64
	for _, p := range []string{path, path + "-wal"} {
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			total = addSat(total, st.Size())
		}
	}
	return total
}

// ffmpeg returns the cached ffmpeg detection, refreshing it when stale or when the
// configured paths changed. Concurrent callers wait for one detection instead of each
// spawning ffmpeg.
func (m *Module) ffmpeg(ctx context.Context) FFmpegInfo {
	var mp mediaPaths
	if err := m.d.Settings.Get(ctx, "media", &mp); err != nil && !errors.Is(err, settings.ErrUnknownNamespace) {
		m.log().Warn("reading media settings", "err", err)
	}
	key := mp.FFmpegPath + "\x00" + mp.FFprobePath
	c := &m.tools
	c.mu.Lock()
	defer c.mu.Unlock()
	now := m.now()
	if c.valid && c.key == key && now.Sub(c.at) < toolCacheTTL && now.Sub(c.at) >= 0 {
		return c.val
	}
	ff, _, ver := m.detect(mp.FFmpegPath, mp.FFprobePath)
	c.val = FFmpegInfo{Available: ff != "", Version: ver}
	c.key, c.at, c.valid = key, now, true
	return c.val
}
