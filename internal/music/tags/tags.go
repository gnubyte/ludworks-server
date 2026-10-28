// Package tags reads the descriptive metadata of audio files with pure-Go parsers: ID3v1 and
// ID3v2.2–2.4 (MP3, and the ID3 chunk of WAV/AIFF), FLAC metadata blocks (STREAMINFO,
// VORBIS_COMMENT, PICTURE), Ogg Vorbis/Opus/FLAC/Speex comment headers (including
// METADATA_BLOCK_PICTURE), MP4/M4A ilst atoms, WAV LIST/INFO and AIFF text chunks. It also
// reports the technical values these structures carry (sample rate, channels, bit depth,
// duration, audio payload size) and parses LRC lyrics.
//
// Every parser treats its input as hostile (ARCHITECTURE.md §2.16): reads go through a byte
// budget (MaxRead), structure counts and nesting depth are capped, text values, lyrics and
// pictures have size limits (an embedded picture over MaxPictureBytes is skipped without being
// read), and nothing proportional to the file size is ever read — the audio payload is
// skipped by its declared size.
package tags

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

// Limits applied to every Read.
const (
	// MaxRead is the total number of bytes one Read may read.
	MaxRead = 48 << 20
	// MaxPictureBytes is the largest embedded picture that is read (larger ones are skipped).
	MaxPictureBytes = 16 << 20
	// maxPictures caps the pictures kept per file; maxPictureTotal their combined size.
	maxPictures     = 4
	maxPictureTotal = 32 << 20
	// maxTextBytes caps one text value (tag frame, comment field) that is read.
	maxTextBytes = 1 << 20
	// maxLyricsBytes caps embedded lyrics.
	maxLyricsBytes = 512 << 10
	// maxValueRunes truncates descriptive values.
	maxValueRunes = 1000
	// maxValues caps the values of one multi-value field.
	maxValues = 64
	// maxStructures caps frames, blocks, atoms, chunks and pages visited by one Read.
	maxStructures = 100_000
	// maxDepth caps MP4 atom nesting.
	maxDepth = 12
)

var (
	// ErrUnknownFormat means the data is not a format this package reads.
	ErrUnknownFormat = errors.New("tags: unknown audio format")
	// ErrCorrupt means a structure is invalid or truncated.
	ErrCorrupt = errors.New("tags: corrupt or truncated metadata")
	// errBudget means the read budget is exhausted.
	errBudget = errors.New("tags: read budget exhausted")
)

// Picture types (ID3v2 APIC / FLAC PICTURE).
const (
	PictureOther      = 0
	PictureFrontCover = 3
	PictureBackCover  = 4
)

// Picture is an embedded image.
type Picture struct {
	Type        int    // ID3v2/FLAC picture type (3 = front cover)
	MIME        string // as declared (may be empty or wrong; sniff the data)
	Description string
	Data        []byte
}

// LyricLine is one line of lyrics; Time is in seconds (negative = no time stamp).
type LyricLine struct {
	Time float64
	Text string
}

// Lyrics are embedded or sidecar lyrics. Synced lyrics have a time stamp on every line.
type Lyrics struct {
	Synced bool
	Lines  []LyricLine
}

// Text joins the lines without time stamps.
func (l *Lyrics) Text() string {
	parts := make([]string, len(l.Lines))
	for i, ln := range l.Lines {
		parts[i] = ln.Text
	}
	return strings.Join(parts, "\n")
}

// ReplayGain values (dB / linear peak). nil = absent.
type ReplayGain struct {
	TrackGain, TrackPeak, AlbumGain, AlbumPeak *float64
}

// Metadata is what Read found. Zero values mean "not present".
type Metadata struct {
	// Format names the tag structure that was read: "id3v2.3", "flac", "vorbis", "opus",
	// "mp4", "riff", "aiff", "id3v1" (several may contribute; this is the main one).
	Format string

	Title, Artist, Album, AlbumArtist, Composer, Comment string
	// Artists are the individual artists of a multi-value artist tag (or the ARTISTS tag);
	// Artist stays the display string. Genres are the individual genres.
	Artists, Genres                                                 []string
	TrackNumber, TrackTotal, DiscNumber, DiscTotal                  int
	Year                                                            int
	Compilation                                                     bool
	SortTitle, SortArtist, SortAlbum, SortAlbumArtist, SortComposer string
	ReplayGain                                                      ReplayGain

	Pictures []Picture
	Lyrics   *Lyrics // embedded lyrics (synced when time stamps were found)

	// Technical values carried by the parsed structures (0 / "" = unknown).
	Container       string // "mp3", "flac", "ogg", "mp4"/"m4a", "wav", "aiff"
	Codec           string // ffprobe-style names: "mp3", "flac", "vorbis", "opus", "aac", "alac", "pcm", …
	DurationSeconds float64
	SampleRate      int
	Channels        int
	BitDepth        int
	BitrateKbps     int   // as declared by the container (nominal); prefer AudioBytes when known
	AudioBytes      int64 // size of the audio payload (file minus metadata), for bitrate estimates
}

// Read identifies r (size bytes) and reads its tags. name is only a hint (its extension
// selects raw MP3 when the data has no signature). A file without tags but of a known format
// returns a Metadata with only technical values and no error.
func Read(ctx context.Context, r io.ReaderAt, size int64, name string) (*Metadata, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s := &src{ctx: ctx, r: r, size: size, budget: MaxRead}
	md := &Metadata{}
	head, err := s.upTo(0, 64)
	if err != nil || len(head) < 4 {
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		return nil, ErrUnknownFormat
	}
	ext := strings.ToLower(strings.TrimPrefix(path.Ext(name), "."))
	switch {
	case string(head[:4]) == "fLaC":
		err = readFLAC(s, 0, md)
	case string(head[:4]) == "OggS":
		err = readOgg(s, md)
	case len(head) >= 12 && (string(head[:4]) == "RIFF" || string(head[:4]) == "RF64") && string(head[8:12]) == "WAVE":
		err = readWAV(s, md)
	case len(head) >= 12 && string(head[:4]) == "FORM" && (string(head[8:12]) == "AIFF" || string(head[8:12]) == "AIFC"):
		err = readAIFF(s, md)
	case isMP4(head):
		err = readMP4(s, md)
	case string(head[:3]) == "ID3":
		err = readMPEG(s, md)
	case ext == "mp3" || ext == "mp2" || ext == "mpa" || looksLikeMPEGFrame(head):
		err = readMPEG(s, md)
	default:
		if err := readID3v1(s, md); err == nil && md.Format != "" {
			return md, nil
		}
		return nil, ErrUnknownFormat
	}
	if cerr := ctx.Err(); cerr != nil {
		return nil, cerr
	}
	md.finish()
	if err != nil {
		// What was read before the damage is still returned (a truncated download keeps its
		// tags); callers decide whether partial metadata is good enough.
		if errors.Is(err, errBudget) {
			err = fmt.Errorf("%w: read budget exhausted", ErrCorrupt)
		}
		return md, err
	}
	return md, nil
}

// readMPEG reads an MP3 (or any stream starting with ID3v2 tags): the leading ID3v2 tag,
// then a FLAC stream behind it if present, and the trailing ID3v1 tag for missing fields.
func readMPEG(s *src, md *Metadata) error {
	end, err := readID3v2At(s, 0, md)
	if err != nil && !errors.Is(err, ErrUnknownFormat) {
		return err
	}
	// Chained tags: skip further ID3v2 headers (their frames are ignored).
	for range 3 {
		b, err := s.upTo(end, 10)
		if err != nil || len(b) < 10 || string(b[:3]) != "ID3" {
			break
		}
		n, ok := syncsafe(b[6:10])
		if !ok {
			break
		}
		end += 10 + int64(n)
		if b[5]&0x10 != 0 {
			end += 10
		}
	}
	if b, err := s.upTo(end, 4); err == nil && len(b) == 4 && string(b) == "fLaC" {
		return readFLAC(s, end, md)
	}
	md.Container, md.Codec = "mp3", "mp3"
	audioEnd := s.size
	if t, err := s.at(s.size-128, 3); err == nil && string(t) == "TAG" {
		audioEnd -= 128
	}
	if v1 := (&Metadata{}); readID3v1(s, v1) == nil && v1.Format != "" {
		if md.Format == "" {
			md.Format = "id3v1"
		}
		md.fillFrom(v1)
	}
	if audioEnd > end {
		md.AudioBytes = audioEnd - end
	}
	return nil
}

// fillFrom copies the descriptive values md lacks from o (ID3v1 behind ID3v2, the ID3 chunk
// of WAV/AIFF next to their native text chunks).
func (md *Metadata) fillFrom(o *Metadata) {
	fill := func(dst *string, v string) {
		if *dst == "" {
			*dst = v
		}
	}
	fill(&md.Title, o.Title)
	fill(&md.Artist, o.Artist)
	fill(&md.Album, o.Album)
	fill(&md.AlbumArtist, o.AlbumArtist)
	fill(&md.Composer, o.Composer)
	fill(&md.Comment, o.Comment)
	fill(&md.SortTitle, o.SortTitle)
	fill(&md.SortArtist, o.SortArtist)
	fill(&md.SortAlbum, o.SortAlbum)
	fill(&md.SortAlbumArtist, o.SortAlbumArtist)
	fill(&md.SortComposer, o.SortComposer)
	if len(md.Artists) == 0 {
		md.Artists = o.Artists
	}
	if len(md.Genres) == 0 {
		md.Genres = o.Genres
	}
	if md.TrackNumber == 0 {
		md.TrackNumber, md.TrackTotal = o.TrackNumber, max(md.TrackTotal, o.TrackTotal)
	}
	if md.DiscNumber == 0 {
		md.DiscNumber, md.DiscTotal = o.DiscNumber, max(md.DiscTotal, o.DiscTotal)
	}
	if md.Year == 0 {
		md.Year = o.Year
	}
	md.Compilation = md.Compilation || o.Compilation
	rg := &md.ReplayGain
	if rg.TrackGain == nil {
		rg.TrackGain, rg.TrackPeak = o.ReplayGain.TrackGain, o.ReplayGain.TrackPeak
	}
	if rg.AlbumGain == nil {
		rg.AlbumGain, rg.AlbumPeak = o.ReplayGain.AlbumGain, o.ReplayGain.AlbumPeak
	}
	for _, p := range o.Pictures {
		md.addPicture(p)
	}
	if md.Lyrics == nil {
		md.Lyrics = o.Lyrics
	}
}

// addPicture keeps a picture within the count and size limits.
func (md *Metadata) addPicture(p Picture) {
	if len(p.Data) == 0 || len(p.Data) > MaxPictureBytes || len(md.Pictures) >= maxPictures {
		return
	}
	total := len(p.Data)
	for _, q := range md.Pictures {
		total += len(q.Data)
	}
	if total > maxPictureTotal {
		return
	}
	md.Pictures = append(md.Pictures, p)
}

// BestPicture returns the front cover, else the first picture (nil when there is none).
func (md *Metadata) BestPicture() *Picture {
	for i := range md.Pictures {
		if md.Pictures[i].Type == PictureFrontCover {
			return &md.Pictures[i]
		}
	}
	if len(md.Pictures) > 0 {
		return &md.Pictures[0]
	}
	return nil
}

// finish normalises the collected values.
func (md *Metadata) finish() {
	for _, p := range []*string{&md.Title, &md.Artist, &md.Album, &md.AlbumArtist, &md.Composer, &md.SortTitle,
		&md.SortArtist, &md.SortAlbum, &md.SortAlbumArtist, &md.SortComposer} {
		*p = cleanValue(*p)
	}
	md.Comment = cleanMultiline(md.Comment, 4000)
	md.Artists = cleanList(md.Artists, false)
	md.Genres = cleanList(md.Genres, true)
	if md.Artist == "" && len(md.Artists) > 0 {
		md.Artist = strings.Join(md.Artists, "; ")
	}
	for _, v := range []*int{&md.TrackNumber, &md.TrackTotal, &md.DiscNumber, &md.DiscTotal} {
		if *v < 0 || *v > 9999 {
			*v = 0
		}
	}
	if md.Year < 0 || md.Year > 9999 {
		md.Year = 0
	}
	if md.DurationSeconds < 0 || md.DurationSeconds > 1e8 || md.DurationSeconds != md.DurationSeconds {
		md.DurationSeconds = 0
	}
	if md.SampleRate < 0 || md.SampleRate > 1_000_000 {
		md.SampleRate = 0
	}
	if md.Channels < 0 || md.Channels > 255 {
		md.Channels = 0
	}
	if md.BitDepth < 0 || md.BitDepth > 64 {
		md.BitDepth = 0
	}
	if md.BitrateKbps < 0 || md.BitrateKbps > 100_000 {
		md.BitrateKbps = 0
	}
	if md.AudioBytes < 0 {
		md.AudioBytes = 0
	}
	md.ReplayGain.clean()
	if md.Lyrics != nil && len(md.Lyrics.Lines) == 0 {
		md.Lyrics = nil
	}
}

func (rg *ReplayGain) clean() {
	ok := func(p **float64, lo, hi float64) {
		if *p != nil && (**p != **p || **p < lo || **p > hi) {
			*p = nil
		}
	}
	ok(&rg.TrackGain, -100, 100)
	ok(&rg.AlbumGain, -100, 100)
	ok(&rg.TrackPeak, 0, 100)
	ok(&rg.AlbumPeak, 0, 100)
}

func isMP4(h []byte) bool {
	if len(h) < 8 {
		return false
	}
	switch string(h[4:8]) {
	case "ftyp", "moov", "mdat", "free", "skip", "wide":
		return true
	}
	return false
}

// looksLikeMPEGFrame reports an MPEG audio frame sync with a valid layer III header.
func looksLikeMPEGFrame(h []byte) bool {
	if len(h) < 4 || h[0] != 0xFF || h[1]&0xE0 != 0xE0 {
		return false
	}
	version, layer := h[1]>>3&3, h[1]>>1&3
	bitrate, rate := h[2]>>4, h[2]>>2&3
	return version != 1 && layer == 1 && bitrate != 0 && bitrate != 15 && rate != 3
}

/* ------------------------------------------------------------- budgeted reader */

// src is a budgeted io.ReaderAt view of the file being read.
type src struct {
	ctx    context.Context
	r      io.ReaderAt
	size   int64
	read   int64
	budget int64
	n      int // structures visited
}

// at reads exactly n bytes at off. Reads beyond the end fail with io.ErrUnexpectedEOF,
// reads over the budget with errBudget.
func (s *src) at(off int64, n int) ([]byte, error) {
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	if n < 0 || off < 0 || off > s.size || int64(n) > s.size-off {
		return nil, io.ErrUnexpectedEOF
	}
	if s.read+int64(n) > s.budget {
		return nil, errBudget
	}
	b := make([]byte, n)
	got, err := s.r.ReadAt(b, off)
	s.read += int64(got)
	if got == n {
		return b, nil
	}
	if err == nil || err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return nil, err
}

// upTo reads up to n bytes at off (fewer near the end of the file).
func (s *src) upTo(off int64, n int) ([]byte, error) {
	if off >= s.size || off < 0 {
		return nil, io.ErrUnexpectedEOF
	}
	return s.at(off, int(min(int64(n), s.size-off)))
}

// count enforces the structure cap (and checks the context now and then).
func (s *src) count() error {
	s.n++
	if s.n > maxStructures {
		return fmt.Errorf("%w: too many structures", ErrCorrupt)
	}
	if s.n&255 == 0 {
		return s.ctx.Err()
	}
	return nil
}
