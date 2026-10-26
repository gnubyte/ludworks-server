package files

import (
	"context"
	"time"
)

// Test hooks (compiled only into tests).

// SetClock replaces the service clock.
func (s *Service) SetClock(fn func() time.Time) { s.now = fn }

// SetFreeSpace replaces the free-space probe.
func (s *Service) SetFreeSpace(fn func(string) (int64, error)) { s.freeSpace = fn }

// PurgeExpiredTrash runs the retention job synchronously.
func (s *Service) PurgeExpiredTrash(ctx context.Context) error { return s.purgeExpiredTrash(ctx) }

// PurgeUploads runs the upload expiry job synchronously.
func (s *Service) PurgeUploads(ctx context.Context) error { return s.purgeUploads(ctx) }

// HashNow runs the SHA-256 job synchronously.
func (s *Service) HashNow(ctx context.Context) error { s.hashDirty.Store(true); return s.hashJob(ctx) }

// SetFFmpeg overrides the detected ffmpeg binary.
func (s *Service) SetFFmpeg(path string) {
	s.tools.mu.Lock()
	s.tools.ffmpeg, s.tools.autoThumb, s.tools.checked = path, true, true
	s.tools.mu.Unlock()
}

// UserDeleted / UserDeleteFiles run the event handlers synchronously.
func (s *Service) UserDeleted(id string)     { s.onUserDeleted(id) }
func (s *Service) UserDeleteFiles(id string) { s.onUserDeleteFiles(id) }

// Exported internals for table tests.
var (
	CheckName      = checkName
	CleanPath      = cleanPath
	NumberedName   = numberedName
	NormalizeTags  = normalizeTags
	DetectMime     = detectMime
	InlineSafe     = inlineSafe
	ServedType     = servedContentType
	ParseMetadata  = parseMetadata
	ParseChecksum  = parseChecksum
	SubtreeBounds  = subtreeBounds
	EscapeHdrPath  = escapeHeaderPath
	ValidMimeHint  = validMimeHint
	ContentDispos  = contentDisposition
	SplitExt       = splitExt
	ValidTaxonomy  = validateTaxonomy
	CursorFingerpr = cursorFingerprint
)

// CloseRoot closes the long-lived data-root handle (Windows cannot delete a test's temp
// directory while it is open).
func (s *Service) CloseRoot() {
	if s.root != nil {
		_ = s.root.Close()
	}
}
