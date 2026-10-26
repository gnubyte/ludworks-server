package probe

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Sidecar subtitle discovery: files next to the video sharing its base name,
// e.g. "Movie.srt", "Movie.en.srt", "Movie.eng.forced.ass", "Movie.pt-BR.sdh.vtt".

// maxDirEntries bounds FindSidecars on huge directories.
const maxDirEntries = 20_000

var sidecarCodec = map[string]string{".srt": "subrip", ".vtt": "webvtt", ".ass": "ass", ".ssa": "ass"}

// MatchSidecars returns the sidecar subtitles for videoName among the names
// of the files in the same directory (siblings may include the video
// itself). Matching is case-insensitive; results are sorted by file name and
// numbered from 0.
func MatchSidecars(videoName string, siblings []string) []SubtitleTrack {
	base := strings.TrimSuffix(videoName, filepath.Ext(videoName))
	lb := strings.ToLower(base)
	if lb == "" {
		return nil
	}
	names := append([]string(nil), siblings...)
	sort.Strings(names)
	var out []SubtitleTrack
	for _, n := range names {
		if strings.ContainsAny(n, `/\`) || n == videoName {
			continue
		}
		ext := strings.ToLower(filepath.Ext(n))
		codec, ok := sidecarCodec[ext]
		if !ok {
			continue
		}
		ln := strings.ToLower(n)
		stem := ln[:len(ln)-len(ext)]
		var middle string
		switch {
		case stem == lb:
		case strings.HasPrefix(stem, lb+"."):
			middle = n[len(lb)+1 : len(n)-len(ext)]
		default:
			continue
		}
		t := SubtitleTrack{Index: len(out), Codec: codec, File: n}
		parseSidecarTags(&t, middle)
		t.Label = trackLabel(t)
		out = append(out, t)
	}
	return out
}

// parseSidecarTags interprets dot-separated qualifiers: language codes or
// names, "forced", "sdh"/"cc"/"hi", "default". Unrecognised words become the
// label (e.g. "Movie.Director Commentary.srt").
func parseSidecarTags(t *SubtitleTrack, middle string) {
	if middle == "" {
		return
	}
	var extra []string
	for _, w := range strings.Split(middle, ".") {
		lw := strings.ToLower(strings.TrimSpace(w))
		switch lw {
		case "":
			continue
		case "forced", "foreign":
			t.Forced = true
			continue
		case "sdh", "cc", "hi", "hoh":
			t.HearingImpaired = true
			continue
		case "default":
			t.Default = true
			continue
		}
		if t.Language == "" {
			// Known codes/names, plus any other two-letter code (ISO 639-1 is
			// larger than our name table; three-letter words are too ambiguous).
			if code, ok := normalizeLanguage(lw); ok || len(code) == 2 && isAlpha(code) {
				t.Language = code
				continue
			}
		}
		if !isDigitsOnly(lw) {
			extra = append(extra, w)
		}
	}
	if len(extra) > 0 {
		t.Label = strings.Join(extra, " ")
	}
}

func isAlpha(s string) bool {
	for _, r := range s {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return s != ""
}

func isDigitsOnly(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// FindSidecars lists the directory containing videoPath and returns its
// sidecar subtitles (see MatchSidecars).
func FindSidecars(videoPath string) ([]SubtitleTrack, error) {
	dir := filepath.Dir(videoPath)
	f, err := os.Open(dir)
	if err != nil {
		return nil, fmt.Errorf("probe: %w", err)
	}
	defer f.Close()
	names, err := f.Readdirnames(maxDirEntries)
	if err != nil && len(names) == 0 {
		return nil, nil // empty directory (io.EOF) or unreadable
	}
	return MatchSidecars(filepath.Base(videoPath), names), nil
}

// trackLabel builds "English (Forced, SDH)" style labels.
func trackLabel(t SubtitleTrack) string {
	label := t.Label
	if label == "" {
		label = languageName(t.Language)
	}
	if label == "" {
		label = "Unknown"
	}
	var flags []string
	if t.Forced && !strings.Contains(strings.ToLower(label), "forced") {
		flags = append(flags, "Forced")
	}
	if t.HearingImpaired && !strings.Contains(strings.ToLower(label), "sdh") {
		flags = append(flags, "SDH")
	}
	if len(flags) > 0 {
		label += " (" + strings.Join(flags, ", ") + ")"
	}
	return label
}

// Language handling -------------------------------------------------------

type langEntry struct{ code, name string }

var languages = func() map[string]langEntry {
	m := map[string]langEntry{}
	add := func(code, name string, aliases ...string) {
		e := langEntry{code, name}
		m[code] = e
		m[strings.ToLower(name)] = e
		for _, a := range aliases {
			m[a] = e
		}
	}
	add("en", "English", "eng")
	add("fr", "French", "fre", "fra", "francais", "français")
	add("de", "German", "ger", "deu", "deutsch")
	add("es", "Spanish", "spa", "espanol", "español", "castellano")
	add("it", "Italian", "ita", "italiano")
	add("pt", "Portuguese", "por", "portugues", "português")
	add("nl", "Dutch", "dut", "nld", "nederlands")
	add("sv", "Swedish", "swe", "svenska")
	add("no", "Norwegian", "nor", "nob", "nb", "nno", "nn", "norsk")
	add("da", "Danish", "dan", "dansk")
	add("fi", "Finnish", "fin", "suomi")
	add("is", "Icelandic", "ice", "isl")
	add("pl", "Polish", "pol", "polski")
	add("cs", "Czech", "cze", "ces")
	add("sk", "Slovak", "slo", "slk")
	add("hu", "Hungarian", "hun", "magyar")
	add("ro", "Romanian", "rum", "ron")
	add("bg", "Bulgarian", "bul")
	add("el", "Greek", "gre", "ell")
	add("ru", "Russian", "rus")
	add("uk", "Ukrainian", "ukr")
	add("sr", "Serbian", "srp")
	add("hr", "Croatian", "hrv")
	add("bs", "Bosnian", "bos")
	add("sl", "Slovenian", "slv")
	add("mk", "Macedonian", "mac", "mkd")
	add("tr", "Turkish", "tur")
	add("ar", "Arabic", "ara")
	add("he", "Hebrew", "heb", "iw")
	add("fa", "Persian", "per", "fas", "farsi")
	add("hi", "Hindi", "hin")
	add("bn", "Bengali", "ben")
	add("ta", "Tamil", "tam")
	add("te", "Telugu", "tel")
	add("ml", "Malayalam", "mal")
	add("ur", "Urdu", "urd")
	add("th", "Thai", "tha")
	add("vi", "Vietnamese", "vie")
	add("id", "Indonesian", "ind")
	add("ms", "Malay", "may", "msa")
	add("tl", "Filipino", "tgl", "fil", "tagalog")
	add("zh", "Chinese", "chi", "zho", "chs", "cht", "mandarin")
	add("ja", "Japanese", "jpn", "jp")
	add("ko", "Korean", "kor")
	add("et", "Estonian", "est")
	add("lv", "Latvian", "lav")
	add("lt", "Lithuanian", "lit")
	add("ca", "Catalan", "cat")
	add("eu", "Basque", "baq", "eus")
	add("gl", "Galician", "glg")
	add("cy", "Welsh", "wel", "cym")
	add("ga", "Irish", "gle")
	add("la", "Latin", "lat")
	add("af", "Afrikaans", "afr")
	add("sq", "Albanian", "alb", "sqi")
	add("hy", "Armenian", "arm", "hye")
	add("ka", "Georgian", "geo", "kat")
	add("kk", "Kazakh", "kaz")
	add("sw", "Swahili", "swa")
	add("eo", "Esperanto", "epo")
	return m
}()

// normalizeLanguage maps a language tag (ISO 639-1/2, BCP 47 with region,
// or an English language name) to (code, true), e.g. "eng" → "en",
// "pt-br" → "pt-BR", "zh_Hant" → "zh-Hant". Unknown 2–3 letter codes are
// returned lower-cased with ok=false; "und"/"unk"/"" give "".
func normalizeLanguage(tag string) (string, bool) {
	t := strings.TrimSpace(strings.ReplaceAll(tag, "_", "-"))
	lt := strings.ToLower(t)
	switch lt {
	case "", "und", "unk", "zxx", "mis", "mul", "qaa", "unknown", "none":
		return "", false
	}
	if e, ok := languages[lt]; ok {
		return e.code, true
	}
	if base, region, ok := strings.Cut(t, "-"); ok {
		if e, ok := languages[strings.ToLower(base)]; ok && region != "" && len(region) <= 8 && isAlnum(region) {
			switch {
			case len(region) == 2:
				region = strings.ToUpper(region)
			case len(region) == 4:
				region = strings.ToUpper(region[:1]) + strings.ToLower(region[1:])
			}
			return e.code + "-" + region, true
		}
	}
	if (len(lt) == 2 || len(lt) == 3) && isAlnum(lt) {
		return lt, false
	}
	return "", false
}

func isAlnum(s string) bool {
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return s != ""
}

// languageName returns the English name for a normalised code ("pt-BR" →
// "Portuguese (BR)").
func languageName(code string) string {
	if code == "" {
		return ""
	}
	base, region, _ := strings.Cut(code, "-")
	e, ok := languages[base]
	if !ok {
		return strings.ToUpper(code)
	}
	if region != "" {
		return e.name + " (" + region + ")"
	}
	return e.name
}
