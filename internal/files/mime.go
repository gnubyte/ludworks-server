package files

import (
	"io"
	"mime"
	"net/http"
	"path"
	"strings"
)

// DirMime is the MIME type reported for directories.
const DirMime = "inode/directory"

// extMimes is an explicit table so detection does not depend on the host's mime.types
// (which differs between Debian and Windows and can be edited by local admins).
var extMimes = map[string]string{
	// images
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".jpe": "image/jpeg", ".png": "image/png", ".gif": "image/gif",
	".webp": "image/webp", ".bmp": "image/bmp", ".tif": "image/tiff", ".tiff": "image/tiff", ".ico": "image/x-icon",
	".svg": "image/svg+xml", ".svgz": "image/svg+xml", ".heic": "image/heic", ".heif": "image/heif", ".avif": "image/avif",
	".raw": "image/x-raw", ".cr2": "image/x-canon-cr2", ".nef": "image/x-nikon-nef", ".dng": "image/x-adobe-dng", ".arw": "image/x-sony-arw",
	".psd": "image/vnd.adobe.photoshop",
	// video
	".mp4": "video/mp4", ".m4v": "video/x-m4v", ".mov": "video/quicktime", ".mkv": "video/x-matroska", ".webm": "video/webm",
	".avi": "video/x-msvideo", ".wmv": "video/x-ms-wmv", ".flv": "video/x-flv", ".mpg": "video/mpeg", ".mpeg": "video/mpeg",
	".m2ts": "video/mp2t", ".mts": "video/mp2t", ".ts": "video/mp2t", ".3gp": "video/3gpp", ".ogv": "video/ogg", ".vob": "video/dvd",
	// audio
	".mp3": "audio/mpeg", ".m4a": "audio/mp4", ".aac": "audio/aac", ".flac": "audio/flac", ".wav": "audio/wav",
	".ogg": "audio/ogg", ".oga": "audio/ogg", ".opus": "audio/opus", ".wma": "audio/x-ms-wma", ".aiff": "audio/aiff", ".aif": "audio/aiff",
	".mid": "audio/midi", ".midi": "audio/midi", ".m4b": "audio/mp4",
	// documents
	".pdf": "application/pdf", ".txt": "text/plain", ".text": "text/plain", ".log": "text/plain", ".md": "text/markdown",
	".markdown": "text/markdown", ".csv": "text/csv", ".tsv": "text/tab-separated-values", ".rtf": "application/rtf",
	".html": "text/html", ".htm": "text/html", ".xhtml": "application/xhtml+xml", ".xml": "application/xml",
	".json": "application/json", ".yaml": "application/yaml", ".yml": "application/yaml", ".toml": "application/toml",
	".ini": "text/plain", ".conf": "text/plain", ".cfg": "text/plain", ".srt": "application/x-subrip", ".vtt": "text/vtt",
	".ass": "text/x-ssa", ".ssa": "text/x-ssa", ".nfo": "text/plain",
	".doc": "application/msword", ".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xls": "application/vnd.ms-excel", ".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".ppt": "application/vnd.ms-powerpoint", ".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".odt": "application/vnd.oasis.opendocument.text", ".ods": "application/vnd.oasis.opendocument.spreadsheet",
	".odp": "application/vnd.oasis.opendocument.presentation", ".epub": "application/epub+zip", ".mobi": "application/x-mobipocket-ebook",
	".azw3": "application/vnd.amazon.ebook", ".cbz": "application/vnd.comicbook+zip", ".cbr": "application/vnd.comicbook-rar",
	// code
	".js": "text/javascript", ".mjs": "text/javascript", ".css": "text/css", ".go": "text/x-go",
	".py": "text/x-python", ".rs": "text/x-rust", ".java": "text/x-java", ".c": "text/x-c", ".h": "text/x-c",
	".cpp": "text/x-c++", ".hpp": "text/x-c++", ".cc": "text/x-c++", ".cs": "text/x-csharp", ".sh": "application/x-sh",
	".bash": "application/x-sh", ".ps1": "text/plain", ".bat": "text/plain", ".sql": "application/sql", ".rb": "text/x-ruby",
	".php": "text/x-php", ".kt": "text/x-kotlin", ".swift": "text/x-swift", ".lua": "text/x-lua", ".pl": "text/x-perl",
	".tsx": "text/plain", ".jsx": "text/javascript", ".vue": "text/plain", ".diff": "text/x-diff", ".patch": "text/x-diff",
	".dockerfile": "text/plain", ".gradle": "text/plain", ".mk": "text/plain",
	// archives & software
	".zip": "application/zip", ".7z": "application/x-7z-compressed", ".rar": "application/vnd.rar", ".gz": "application/gzip",
	".tgz": "application/gzip", ".bz2": "application/x-bzip2", ".xz": "application/x-xz", ".zst": "application/zstd",
	".tar": "application/x-tar", ".iso": "application/x-iso9660-image", ".img": "application/octet-stream",
	".dmg": "application/x-apple-diskimage", ".exe": "application/vnd.microsoft.portable-executable",
	".msi": "application/x-msi", ".deb": "application/vnd.debian.binary-package", ".rpm": "application/x-rpm",
	".apk": "application/vnd.android.package-archive", ".appimage": "application/x-executable", ".jar": "application/java-archive",
	// fonts
	".ttf": "font/ttf", ".otf": "font/otf", ".woff": "font/woff", ".woff2": "font/woff2",
	// games
	".nes": "application/x-nes-rom", ".sfc": "application/x-snes-rom", ".smc": "application/x-snes-rom",
	".gb": "application/x-gameboy-rom", ".gbc": "application/x-gameboy-color-rom", ".gba": "application/x-gba-rom",
	".n64": "application/x-n64-rom", ".z64": "application/x-n64-rom", ".nds": "application/x-nintendo-ds-rom",
	".cue": "application/x-cue", ".bin": "application/octet-stream", ".chd": "application/octet-stream",
}

// mimeByName guesses a MIME type from the file name only ("" when unknown).
func mimeByName(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if ext == "" {
		return ""
	}
	if m, ok := extMimes[ext]; ok {
		return m
	}
	if m := mime.TypeByExtension(ext); m != "" {
		mt, _, err := mime.ParseMediaType(m)
		if err == nil {
			return mt
		}
	}
	return ""
}

// detectMime combines the extension with a content sniff of the first 512 bytes. The
// extension wins when it is specific; the sniff fills in unknown or generic types. The
// result never carries parameters.
func detectMime(name string, head []byte) string {
	byName := mimeByName(name)
	if byName != "" && byName != "application/octet-stream" {
		return byName
	}
	if len(head) > 0 {
		sniffed := http.DetectContentType(head)
		if mt, _, err := mime.ParseMediaType(sniffed); err == nil && mt != "" {
			return mt
		}
	}
	if byName != "" {
		return byName
	}
	return "application/octet-stream"
}

// detectMimeReader sniffs up to 512 bytes from r.
func detectMimeReader(name string, r io.Reader) string {
	var buf [512]byte
	n, _ := io.ReadFull(r, buf[:])
	return detectMime(name, buf[:n])
}

// inlineSafe reports whether content of this MIME type may be shown inline by the browser
// (API.md §5): images (not SVG), video, audio, PDF and plain text. HTML, SVG, XML and
// everything else are always served as attachments.
func inlineSafe(m string) bool {
	switch {
	case m == "image/svg+xml":
		return false
	case strings.HasPrefix(m, "image/"), strings.HasPrefix(m, "video/"), strings.HasPrefix(m, "audio/"):
		return true
	case m == "application/pdf", m == "text/plain":
		return true
	}
	return false
}

// jsMimes are the JavaScript MIME type essences: a browser runs a same-origin response with
// any of these types as a classic script, module or worker even with nosniff (WHATWG MIME
// Sniffing, "JavaScript MIME type"), and CSP script-src 'self' allows it.
var jsMimes = map[string]bool{
	"application/ecmascript": true, "application/javascript": true, "application/x-ecmascript": true,
	"application/x-javascript": true, "text/ecmascript": true, "text/javascript": true, "text/javascript1.0": true,
	"text/javascript1.1": true, "text/javascript1.2": true, "text/javascript1.3": true, "text/javascript1.4": true,
	"text/javascript1.5": true, "text/jscript": true, "text/livescript": true, "text/x-ecmascript": true,
	"text/x-javascript": true,
}

// activeContent are types a browser would execute, style with or render with script access
// when served from our origin. They are never accepted as a client hint and never served as
// such (servedContentType). Matching is deliberately broad: any type mentioning script or
// ECMAScript, and every XML type, counts.
func activeContent(m string) bool {
	m, _, _ = strings.Cut(m, ";")
	m = strings.ToLower(strings.TrimSpace(m))
	if jsMimes[m] || strings.Contains(m, "script") || strings.Contains(m, "ecma") {
		return true
	}
	switch m {
	case "text/html", "application/xhtml+xml", "image/svg+xml", "text/xml", "application/xml", "text/css",
		"application/x-shockwave-flash", "application/vnd.wap.xhtml+xml", "multipart/x-mixed-replace", "text/xsl":
		return true
	}
	return strings.HasPrefix(m, "text/html") || strings.HasSuffix(m, "+xml") || strings.HasSuffix(m, "/xml")
}

// servedContentType returns the Content-Type for a file response. It is an allowlist: only
// an inline response of a type inlineSafe accepts (images except SVG, video, audio, PDF,
// plain text) carries the stored type. Every attachment and every other type is sent as
// application/octet-stream, so no stored type — detected, from the host's mime.types or
// hinted by a client — can make a response loadable as script or style from our origin.
func servedContentType(m string, inline bool) string {
	if !inline || !inlineSafe(m) || activeContent(m) {
		return "application/octet-stream"
	}
	if m == "text/plain" {
		return "text/plain; charset=utf-8"
	}
	return m
}

// hintMimes are the client MIME hints accepted (only when detection is inconclusive): the
// types of the extension table, minus active content. Any other hint is ignored.
var hintMimes = func() map[string]bool {
	out := map[string]bool{}
	for _, m := range extMimes {
		if !activeContent(m) {
			out[m] = true
		}
	}
	return out
}()

// compressedExts are formats that do not shrink under Deflate; ZIP stores them uncompressed.
var compressedExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true, ".heic": true, ".heif": true, ".avif": true,
	".mp4": true, ".m4v": true, ".mov": true, ".mkv": true, ".webm": true, ".avi": true, ".wmv": true, ".flv": true,
	".mpg": true, ".mpeg": true, ".m2ts": true, ".mts": true, ".ts": true, ".3gp": true, ".ogv": true,
	".mp3": true, ".m4a": true, ".aac": true, ".flac": true, ".ogg": true, ".oga": true, ".opus": true, ".wma": true, ".m4b": true,
	".zip": true, ".7z": true, ".rar": true, ".gz": true, ".tgz": true, ".bz2": true, ".xz": true, ".zst": true,
	".docx": true, ".xlsx": true, ".pptx": true, ".odt": true, ".ods": true, ".odp": true, ".epub": true, ".cbz": true, ".cbr": true,
	".jar": true, ".apk": true, ".deb": true, ".rpm": true, ".dmg": true, ".woff": true, ".woff2": true, ".pdf": true, ".chd": true,
}

func alreadyCompressed(name string) bool { return compressedExts[strings.ToLower(path.Ext(name))] }

// thumbImage reports whether internal/imaging can thumbnail this type.
func thumbImage(m string) bool { return m == "image/jpeg" || m == "image/png" || m == "image/gif" }

// thumbVideo reports whether ffmpeg may produce a thumbnail.
func thumbVideo(m string) bool { return strings.HasPrefix(m, "video/") }
