package music

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/httpx"
)

// Download renditions (API.md §12): AAC in MP4 (faststart) at 256/160/96 kbps — never above
// the source's own bitrate — produced once by ffmpeg in the background into <data>/music/cache,
// bounded by music.download_cache_bytes (least recently used first), and then served with
// Range/ETag like the original. A source that already is AAC in MP4 at or below the bitrate is
// served as is.

var qualityBitrates = map[string]int{"high": 256, "normal": 160, "low": 96}

const (
	maxRenderWorkers   = 2    // concurrent encodes
	maxRenderQueue     = 1000 // queued renditions server-wide
	maxRenderPerUser   = 200  // queued renditions per requesting user
	renderRetryAfter   = 5    // seconds (202 while preparing)
	renderFailedRetry  = 5 * time.Minute
	minRenditionKbps   = 32
	maxRenditionBytes  = 2 << 30
	renditionMinBudget = 2 * time.Minute
	renditionMaxBudget = 2 * time.Hour
)

type renderKey struct{ fileID, quality, source string }

type renderJob struct {
	key      renderKey
	user     string
	bitrate  int
	duration float64
	tmpName  string
}

// renderQueue holds the queued and running renditions.
type renderQueue struct {
	mu      sync.Mutex
	pending map[renderKey]*renderJob // queued or running
	queue   []*renderJob
	failed  map[renderKey]time.Time
	perUser map[string]int
	running map[string]bool // tmp names being written
	dirty   bool
	cacheMu sync.Mutex // serialises cache trimming and additions
}

func newRenderQueue() renderQueue {
	return renderQueue{pending: map[renderKey]*renderJob{}, failed: map[renderKey]time.Time{},
		perUser: map[string]int{}, running: map[string]bool{}}
}

var errRenderBusy = core.NewError(http.StatusTooManyRequests, "rate_limited", "Too many downloads are being prepared; try again shortly")

// enqueue queues a job unless it is already pending; it reports whether the job is (now)
// pending and an error when a limit is reached or the last attempt failed recently.
func (q *renderQueue) enqueue(j *renderJob, now time.Time) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, ok := q.pending[j.key]; ok {
		return nil
	}
	if t, ok := q.failed[j.key]; ok {
		if now.Sub(t) < renderFailedRetry {
			return core.NewError(http.StatusInternalServerError, "internal", "Preparing this download failed; try again later or download the original")
		}
		delete(q.failed, j.key)
	}
	if len(q.queue) >= maxRenderQueue || q.perUser[j.user] >= maxRenderPerUser {
		return errRenderBusy
	}
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	j.tmpName = j.key.fileID + "-" + j.key.quality + "-" + hex.EncodeToString(rnd[:]) + ".m4a"
	q.pending[j.key] = j
	q.queue = append(q.queue, j)
	q.perUser[j.user]++
	q.dirty = true
	return nil
}

func (q *renderQueue) next() *renderJob {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.dirty = false
	if len(q.queue) == 0 {
		return nil
	}
	j := q.queue[0]
	q.queue[0] = nil
	q.queue = q.queue[1:]
	q.running[j.tmpName] = true
	return j
}

func (q *renderQueue) finish(j *renderJob, failed bool, now time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.pending, j.key)
	delete(q.running, j.tmpName)
	if q.perUser[j.user]--; q.perUser[j.user] <= 0 {
		delete(q.perUser, j.user)
	}
	if failed {
		q.failed[j.key] = now
		if len(q.failed) > 10_000 {
			for k := range q.failed {
				delete(q.failed, k)
				break
			}
		}
	}
}

func (q *renderQueue) isDirty() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.dirty || len(q.queue) > 0
}

// encoding reports whether a tmp file belongs to a running encode.
func (q *renderQueue) encoding(name string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.running[name]
}

// sourceKeyOf identifies a version of a file (as stored in files.mod_time).
func sourceKeyOf(size int64, modTime time.Time) string {
	return imageSourceKey(size, db.FormatTime(modTime))
}

// targetBitrate is the rendition bitrate for a quality: never above the source's own bitrate.
func targetBitrate(quality string, source sql.NullInt64) int {
	b := qualityBitrates[quality]
	if source.Valid && source.Int64 > 0 && int(source.Int64) < b {
		b = max(int(source.Int64), minRenditionKbps)
	}
	return b
}

// serveDownload answers GET /music/tracks/{id}/download for a non-original quality.
func (s *Service) serveDownload(w http.ResponseWriter, r *http.Request, p *core.Principal, tr *track, quality string) error {
	ctx := r.Context()
	switch quality {
	case "", "original":
		return s.serveFile(w, r, p, tr, false, "original")
	case "high", "normal", "low":
	default:
		return core.BadRequest("quality: must be original, high, normal or low")
	}
	ff := s.ffmpegPath(ctx)
	if ff == "" || s.settings(ctx).DownloadCacheBytes <= 0 {
		return errFFmpegUnavailable
	}
	e, err := s.files.Get(ctx, p, tr.FileID)
	if err != nil {
		return err
	}
	bitrate := targetBitrate(quality, tr.Bitrate)
	if tr.Codec == "aac" && (tr.Container == "m4a" || tr.Container == "mp4") && tr.Bitrate.Valid && int(tr.Bitrate.Int64) <= bitrate {
		return s.serveFile(w, r, p, tr, false, quality) // already AAC in MP4 at or below the bitrate
	}
	key := renderKey{fileID: tr.FileID, quality: quality, source: sourceKeyOf(e.Size, e.ModTime)}
	if f, err := s.openRendition(ctx, key); err == nil {
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			return err
		}
		h := w.Header()
		h.Set("Content-Type", "audio/mp4")
		h.Set("Content-Disposition", contentDisposition("attachment", downloadName(e.Name, quality)))
		// The cached file's own size and time are part of the validator: a rendition encoded again
		// after eviction (or by another ffmpeg) need not be byte-identical, and If-Range resumes
		// must never splice two encodes.
		h.Set("ETag", renditionETag(e.ID, e.Size, e.ModTime,
			quality+"|"+strconv.FormatInt(st.Size(), 10)+"|"+strconv.FormatInt(st.ModTime().UnixNano(), 10)))
		h.Set("Cache-Control", "private, no-cache")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", streamCSP)
		h.Set("Accept-Ranges", "bytes")
		http.ServeContent(w, r, "", st.ModTime(), f)
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	dur := 0.0
	if tr.Duration.Valid {
		dur = tr.Duration.Float64
	}
	job := &renderJob{key: key, user: requester(r, p), bitrate: bitrate, duration: dur}
	if err := s.render.enqueue(job, s.now()); err != nil {
		var ce *core.Error
		if errors.As(err, &ce) && ce.Status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "30")
		}
		return err
	}
	s.d.Jobs.GoOnce("music.renditions", s.drainRenders)
	w.Header().Set("Retry-After", strconv.Itoa(renderRetryAfter))
	return httpx.WriteJSON(w, http.StatusAccepted, map[string]string{"status": "preparing"})
}

// openRendition opens the cached rendition for key (fs.ErrNotExist when there is none) and
// marks it recently used.
func (s *Service) openRendition(ctx context.Context, key renderKey) (*os.File, error) {
	var rel, source string
	err := s.d.DB.SQL.QueryRowContext(ctx, `SELECT path, source_key FROM music_renditions WHERE file_id = ? AND quality = ?`,
		key.fileID, key.quality).Scan(&rel, &source)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && source != key.source) {
		return nil, fs.ErrNotExist
	}
	if err != nil {
		return nil, err
	}
	if !validCacheRel(rel) {
		return nil, fs.ErrNotExist
	}
	root, err := s.musicRoot()
	if err != nil {
		return nil, err
	}
	f, err := root.Open(rel)
	if err != nil {
		_, _ = s.d.DB.SQL.ExecContext(ctx, `DELETE FROM music_renditions WHERE file_id = ? AND quality = ?`, key.fileID, key.quality)
		return nil, fs.ErrNotExist
	}
	_, _ = s.d.DB.SQL.ExecContext(ctx, `UPDATE music_renditions SET last_access = ? WHERE file_id = ? AND quality = ?`,
		db.FormatTime(s.now()), key.fileID, key.quality)
	return f, nil
}

// validCacheRel checks a stored rendition path (server-made; defence in depth).
func validCacheRel(rel string) bool {
	dir, name := filepath.Split(filepath.FromSlash(rel))
	if filepath.ToSlash(filepath.Clean(dir)) != "cache" || !strings.HasSuffix(name, ".m4a") || len(name) > 160 {
		return false
	}
	for _, r := range strings.TrimSuffix(name, ".m4a") {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// drainRenders prepares queued renditions, maxRenderWorkers at a time.
func (s *Service) drainRenders(ctx context.Context) error {
	sem := make(chan struct{}, maxRenderWorkers)
	var wg sync.WaitGroup
	defer wg.Wait()
	for ctx.Err() == nil {
		j := s.render.next()
		if j == nil {
			wg.Wait()
			if !s.render.isDirty() {
				return nil
			}
			continue
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			s.render.finish(j, false, s.now())
			return nil
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			err := s.prepareRendition(ctx, j)
			if err != nil && ctx.Err() == nil {
				s.log.Warn("music: preparing download failed", "id", j.key.fileID, "quality", j.key.quality, "err", err)
			}
			s.render.finish(j, err != nil && ctx.Err() == nil, s.now())
		}()
	}
	return nil
}

// prepareRendition encodes one rendition and adds it to the cache.
func (s *Service) prepareRendition(ctx context.Context, j *renderJob) error {
	ff := s.ffmpegPath(ctx)
	if ff == "" {
		return errFFmpegUnavailable
	}
	local, err := s.files.LocalPath(ctx, s.sys, j.key.fileID)
	if err != nil {
		return err
	}
	root, err := s.musicRoot()
	if err != nil {
		return err
	}
	tmpRel := filepath.Join("tmp", j.tmpName)
	defer root.Remove(tmpRel)
	limit := int64(maxRenditionBytes)
	budget := 30 * time.Minute
	if j.duration > 0 {
		expected := int64(j.duration*float64(j.bitrate)*1000/8) + 1<<20
		limit = min(limit, expected*3/2+4<<20)
		budget = min(max(renditionMinBudget, time.Duration(j.duration/2)*time.Second), renditionMaxBudget)
	}
	ectx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	cmd, err := renditionCommand(ectx, ff, local, filepath.Join(s.dir, "tmp", j.tmpName), j.bitrate, limit)
	if err != nil {
		return err
	}
	stderr := &capped{max: maxStderr}
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("ffmpeg: " + err.Error() + ": " + firstLine(stderr.Bytes()))
	}
	st, err := root.Stat(tmpRel)
	if err != nil || st.Size() == 0 {
		return errors.New("ffmpeg produced no output")
	}
	// The source may have changed while it was encoded.
	e, err := s.files.Get(ctx, s.sys, j.key.fileID)
	if err != nil {
		return err
	}
	if sourceKeyOf(e.Size, e.ModTime) != j.key.source {
		return nil
	}
	s.render.cacheMu.Lock()
	defer s.render.cacheMu.Unlock()
	budgetBytes := s.settings(ctx).DownloadCacheBytes
	if st.Size() > budgetBytes {
		return errors.New("the rendition is larger than the download cache")
	}
	if err := s.trimCacheLocked(ctx, st.Size()); err != nil {
		return err
	}
	rel := filepath.Join("cache", j.tmpName)
	if err := root.Rename(tmpRel, rel); err != nil {
		return err
	}
	now := db.FormatTime(s.now())
	var old string
	err = s.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT path FROM music_renditions WHERE file_id = ? AND quality = ?`,
			j.key.fileID, j.key.quality).Scan(&old); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO music_renditions (file_id, quality, source_key, path, size, bitrate_kbps,
			created_at, last_access) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (file_id, quality) DO UPDATE SET source_key = excluded.source_key, path = excluded.path,
			size = excluded.size, bitrate_kbps = excluded.bitrate_kbps, created_at = excluded.created_at,
			last_access = excluded.last_access`,
			j.key.fileID, j.key.quality, j.key.source, filepath.ToSlash(rel), st.Size(), j.bitrate, now, now)
		return err
	})
	if err != nil {
		_ = root.Remove(rel)
		if db.IsForeignKeyViolation(err) {
			return nil // the file was deleted meanwhile
		}
		return err
	}
	if old != "" && old != filepath.ToSlash(rel) && validCacheRel(old) {
		_ = root.Remove(filepath.FromSlash(old))
	}
	return nil
}

// trimCache evicts least recently used renditions until extra more bytes fit the budget.
func (s *Service) trimCache(ctx context.Context, extra int64) error {
	s.render.cacheMu.Lock()
	defer s.render.cacheMu.Unlock()
	return s.trimCacheLocked(ctx, extra)
}

func (s *Service) trimCacheLocked(ctx context.Context, extra int64) error {
	budget := s.settings(ctx).DownloadCacheBytes
	root, err := s.musicRoot()
	if err != nil {
		return err
	}
	var total int64
	if err := s.d.DB.SQL.QueryRowContext(ctx, `SELECT COALESCE(SUM(size), 0) FROM music_renditions`).Scan(&total); err != nil {
		return err
	}
	for total+extra > budget && ctx.Err() == nil {
		var (
			fileID, quality, rel string
			size                 int64
		)
		err := s.d.DB.SQL.QueryRowContext(ctx, `SELECT file_id, quality, path, size FROM music_renditions
			ORDER BY last_access, file_id LIMIT 1`).Scan(&fileID, &quality, &rel, &size)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := s.d.DB.SQL.ExecContext(ctx, `DELETE FROM music_renditions WHERE file_id = ? AND quality = ?`, fileID, quality); err != nil {
			return err
		}
		if validCacheRel(rel) {
			_ = root.Remove(filepath.FromSlash(rel))
		}
		total -= size
	}
	return ctx.Err()
}

// gcRenditions drops stale renditions (source changed or gone) and cache files without a row.
func (s *Service) gcRenditions(ctx context.Context, root *os.Root) error {
	rs, err := s.d.DB.SQL.QueryContext(ctx, `SELECT r.file_id, r.quality, r.path FROM music_renditions r JOIN files f ON f.id = r.file_id
		WHERE f.trashed_at IS NOT NULL OR r.source_key <> (f.size || '|' || f.mod_time)`)
	if err != nil {
		return err
	}
	type stale struct{ id, q, rel string }
	var drop []stale
	for rs.Next() {
		var x stale
		if err := rs.Scan(&x.id, &x.q, &x.rel); err != nil {
			rs.Close()
			return err
		}
		drop = append(drop, x)
	}
	rs.Close()
	for _, x := range drop {
		if _, err := s.d.DB.SQL.ExecContext(ctx, `DELETE FROM music_renditions WHERE file_id = ? AND quality = ?`, x.id, x.q); err != nil {
			return err
		}
		if validCacheRel(x.rel) {
			_ = root.Remove(filepath.FromSlash(x.rel))
		}
	}
	used := map[string]bool{}
	rs, err = s.d.DB.SQL.QueryContext(ctx, `SELECT path FROM music_renditions`)
	if err != nil {
		return err
	}
	for rs.Next() {
		var p string
		if err := rs.Scan(&p); err != nil {
			rs.Close()
			return err
		}
		used[filepath.Base(filepath.FromSlash(p))] = true
	}
	rs.Close()
	entries, err := fs.ReadDir(root.FS(), "cache")
	if err != nil {
		return nil
	}
	cutoff := time.Now().Add(-time.Hour)
	for _, d := range entries {
		info, err := d.Info()
		if err == nil && d.Type().IsRegular() && !used[d.Name()] && info.ModTime().Before(cutoff) {
			_ = root.Remove(filepath.Join("cache", d.Name()))
		}
	}
	return nil
}
