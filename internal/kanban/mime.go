package kanban

import (
	"mime"
	"net/http"
	"path"
	"strings"
	"unicode"
)

// Attachment content types follow the Files rules (API.md §5, internal/files/mime.go): the
// stored type comes from the extension when it is specific and from a content sniff
// otherwise; only browser-displayable raster images are ever served inline (with their own
// type); every other response is an application/octet-stream attachment, so no stored type
// can make a response run as script or style from our origin.

// extMimes maps extensions to types for the formats people attach to cards. Unknown
// extensions fall back to the content sniff.
var extMimes = map[string]string{
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".jpe": "image/jpeg", ".png": "image/png", ".gif": "image/gif",
	".webp": "image/webp", ".bmp": "image/bmp", ".ico": "image/x-icon", ".avif": "image/avif", ".svg": "image/svg+xml",
	".tif": "image/tiff", ".tiff": "image/tiff", ".heic": "image/heic", ".heif": "image/heif", ".psd": "image/vnd.adobe.photoshop",
	".pdf": "application/pdf", ".txt": "text/plain", ".log": "text/plain", ".md": "text/markdown", ".markdown": "text/markdown",
	".csv": "text/csv", ".tsv": "text/tab-separated-values", ".json": "application/json", ".xml": "application/xml",
	".html": "text/html", ".htm": "text/html", ".css": "text/css", ".js": "text/javascript", ".mjs": "text/javascript",
	".yaml": "application/yaml", ".yml": "application/yaml", ".rtf": "application/rtf",
	".doc": "application/msword", ".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xls": "application/vnd.ms-excel", ".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".ppt": "application/vnd.ms-powerpoint", ".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".odt": "application/vnd.oasis.opendocument.text", ".ods": "application/vnd.oasis.opendocument.spreadsheet",
	".odp": "application/vnd.oasis.opendocument.presentation", ".epub": "application/epub+zip",
	".zip": "application/zip", ".7z": "application/x-7z-compressed", ".rar": "application/vnd.rar", ".gz": "application/gzip",
	".tgz": "application/gzip", ".tar": "application/x-tar", ".bz2": "application/x-bzip2", ".xz": "application/x-xz",
	".mp3": "audio/mpeg", ".m4a": "audio/mp4", ".wav": "audio/wav", ".ogg": "audio/ogg", ".flac": "audio/flac", ".opus": "audio/opus",
	".mp4": "video/mp4", ".m4v": "video/x-m4v", ".mov": "video/quicktime", ".webm": "video/webm", ".mkv": "video/x-matroska",
	".avi": "video/x-msvideo",
}

// detectMime combines the extension with a sniff of the first bytes; the result never
// carries parameters.
func detectMime(name string, head []byte) string {
	ext := strings.ToLower(path.Ext(name))
	if m, ok := extMimes[ext]; ok {
		return m
	}
	if len(head) > 0 {
		if mt, _, err := mime.ParseMediaType(http.DetectContentType(head)); err == nil && mt != "" {
			return mt
		}
	}
	return "application/octet-stream"
}

// imageTypes are the raster images browsers display (SVG is deliberately absent: it can
// carry script). Only these are served inline and offered as card covers.
var imageTypes = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true, "image/bmp": true,
	"image/avif": true, "image/x-icon": true, "image/vnd.microsoft.icon": true,
}

func inlineImage(m string) bool { return imageTypes[m] }

// thumbable reports whether internal/imaging can thumbnail this type.
func thumbable(m string) bool { return m == "image/jpeg" || m == "image/png" || m == "image/gif" }

// servedContentType is the Content-Type of an attachment response: the stored type only for an
// inline image, application/octet-stream for everything else.
func servedContentType(m string, inline bool) string {
	if inline && inlineImage(m) {
		return m
	}
	return "application/octet-stream"
}

// contentCSP is sent with every attachment response (as for Files content): the bytes can
// never run script or navigate as our origin even if a browser decides to render them.
const contentCSP = "sandbox; default-src 'none'; img-src 'self' data:; media-src 'self'; style-src 'unsafe-inline'; frame-ancestors 'none'"

// contentDisposition builds an RFC 6266 header with an RFC 5987 UTF-8 filename.
func contentDisposition(kind, name string) string {
	if v := mime.FormatMediaType(kind, map[string]string{"filename": name}); v != "" {
		return v
	}
	return kind
}

// cleanFilename keeps the base name of a client-supplied file name and refuses names that are
// empty, too long or contain control characters (they are stored as metadata only; the bytes
// live under a server-generated name).
func cleanFilename(name string) (string, string) {
	name = strings.ToValidUTF8(name, "�")
	name = strings.ReplaceAll(name, `\`, "/")
	name = strings.TrimSpace(path.Base(strings.TrimSpace(name)))
	switch {
	case name == "" || name == "." || name == ".." || name == "/":
		return "", "is required"
	case strings.ContainsFunc(name, unicode.IsControl):
		return "", "must not contain control characters"
	case len(name) > 255:
		return "", "must be at most 255 bytes"
	}
	return name, ""
}
