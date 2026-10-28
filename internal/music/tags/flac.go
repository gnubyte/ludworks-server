package tags

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
)

// FLAC metadata blocks and Vorbis comments (shared with Ogg).

const (
	// maxCommentBytes caps a Vorbis comment block/packet (it may carry base64 pictures).
	maxCommentBytes = MaxPictureBytes*4/3 + 8<<20
	maxFLACBlocks   = 1024
	maxComments     = 10_000
)

// readFLAC reads the metadata blocks of a FLAC stream whose "fLaC" marker is at off.
func readFLAC(s *src, off int64, md *Metadata) error {
	if md.Format == "" {
		md.Format = "flac"
	}
	md.Container, md.Codec = "flac", "flac"
	pos := off + 4
	for range maxFLACBlocks {
		if err := s.count(); err != nil {
			return err
		}
		h, err := s.at(pos, 4)
		if err != nil {
			return fmt.Errorf("%w: truncated FLAC metadata", ErrCorrupt)
		}
		last, typ := h[0]&0x80 != 0, h[0]&0x7F
		n := int64(h[1])<<16 | int64(h[2])<<8 | int64(h[3])
		data := pos + 4
		if n > s.size-data {
			return fmt.Errorf("%w: FLAC block beyond the end", ErrCorrupt)
		}
		switch typ {
		case 0:
			if n >= 34 {
				b, err := s.at(data, 34)
				if err != nil {
					return err
				}
				streamInfo(b, md)
			}
		case 4:
			if n <= maxCommentBytes {
				b, err := s.at(data, int(n))
				if err != nil {
					return err
				}
				vc := &vorbisCollector{md: md}
				vc.parse(b)
				vc.finish()
			}
		case 6:
			// Peek at the header first: an oversized picture is skipped without being read.
			head, err := s.at(data, int(min(n, 64<<10)))
			if err != nil {
				return err
			}
			if l, ok := pictureDataLen(head); ok && l <= MaxPictureBytes {
				b := head
				if int64(len(head)) < n {
					if b, err = s.at(data, int(n)); err != nil {
						return err
					}
				}
				if p, ok := parsePictureBlock(b); ok {
					md.addPicture(p)
				}
			}
		case 127:
			return fmt.Errorf("%w: invalid FLAC block type", ErrCorrupt)
		}
		pos = data + n
		if last {
			break
		}
	}
	if pos < s.size {
		md.AudioBytes = s.size - pos
	}
	return nil
}

// streamInfo reads the 34-byte STREAMINFO block.
func streamInfo(d []byte, md *Metadata) {
	rate := int(d[10])<<12 | int(d[11])<<4 | int(d[12])>>4
	md.SampleRate = rate
	md.Channels = int(d[12]>>1&7) + 1
	md.BitDepth = (int(d[12]&1)<<4 | int(d[13]>>4)) + 1
	total := uint64(d[13]&0x0F)<<32 | uint64(binary.BigEndian.Uint32(d[14:18]))
	if rate > 0 && total > 0 {
		md.DurationSeconds = float64(total) / float64(rate)
	}
}

// pictureDataLen reads the declared image size from the start of a PICTURE block.
func pictureDataLen(b []byte) (int, bool) {
	p := 4
	for range 2 { // MIME type, description
		if p+4 > len(b) {
			return 0, false
		}
		n := int(binary.BigEndian.Uint32(b[p:]))
		if n < 0 || n > len(b)-p-4 {
			return 0, false
		}
		p += 4 + n
	}
	p += 16
	if p+4 > len(b) {
		return 0, false
	}
	return int(binary.BigEndian.Uint32(b[p:])), true
}

// parsePictureBlock decodes a FLAC PICTURE block (also the payload of a Vorbis
// METADATA_BLOCK_PICTURE comment).
func parsePictureBlock(b []byte) (Picture, bool) {
	p := 0
	u32 := func() (int, bool) {
		if p+4 > len(b) {
			return 0, false
		}
		v := binary.BigEndian.Uint32(b[p:])
		p += 4
		if v > uint32(len(b)) {
			return 0, false
		}
		return int(v), true
	}
	str := func() (string, bool) {
		n, ok := u32()
		if !ok || n > len(b)-p {
			return "", false
		}
		v := b[p : p+n]
		p += n
		return string(v), true
	}
	typ, ok := u32()
	if !ok {
		return Picture{}, false
	}
	mime, ok := str()
	if !ok {
		return Picture{}, false
	}
	desc, ok := str()
	if !ok || p+16 > len(b) {
		return Picture{}, false
	}
	p += 16 // width, height, depth, colours
	n, ok := u32()
	if !ok || n == 0 || n > len(b)-p || n > MaxPictureBytes {
		return Picture{}, false
	}
	return Picture{Type: typ, MIME: strings.ToLower(cleanValue(mime)), Description: cleanValue(utf8OrLatin1([]byte(desc))),
		Data: b[p : p+n]}, true
}

// vorbisCollector gathers Vorbis comment fields; finish() resolves precedence.
type vorbisCollector struct {
	md                      *Metadata
	artist, artists, albArt []string
	composer                []string
	date, orig              int
	comment, desc           string
	lyrics                  string
	r128Track, r128Album    *float64
	coverArt, coverArtMIME  string
	trackTotal, discTotal   int
	sawTrack, sawDisc       bool
}

// parse reads a Vorbis comment structure: vendor string, then length-prefixed fields.
func (vc *vorbisCollector) parse(b []byte) {
	if len(b) < 8 {
		return
	}
	vlen := int64(binary.LittleEndian.Uint32(b))
	if vlen > int64(len(b))-8 {
		return
	}
	p := 4 + int(vlen)
	count := binary.LittleEndian.Uint32(b[p:])
	p += 4
	for i := uint32(0); i < count && i < maxComments; i++ {
		if p+4 > len(b) {
			return
		}
		n := int64(binary.LittleEndian.Uint32(b[p:]))
		p += 4
		if n > int64(len(b)-p) {
			return
		}
		vc.field(b[p : p+int(n)])
		p += int(n)
	}
}

func (vc *vorbisCollector) field(f []byte) {
	eq := -1
	for i, c := range f {
		if c == '=' {
			eq = i
			break
		}
		if i > 64 {
			return
		}
	}
	if eq <= 0 {
		return
	}
	key := strings.ToUpper(string(f[:eq]))
	raw := f[eq+1:]
	md := vc.md
	switch key {
	case "METADATA_BLOCK_PICTURE":
		if len(raw) <= base64.StdEncoding.EncodedLen(MaxPictureBytes+64<<10) {
			if dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw))); err == nil {
				if p, ok := parsePictureBlock(dec); ok {
					md.addPicture(p)
				}
			}
		}
		return
	case "COVERART":
		if len(raw) <= base64.StdEncoding.EncodedLen(MaxPictureBytes) {
			vc.coverArt = string(raw)
		}
		return
	case "LYRICS", "UNSYNCEDLYRICS", "UNSYNCED LYRICS":
		if vc.lyrics == "" && len(raw) <= maxLyricsBytes*2 {
			vc.lyrics = utf8OrLatin1(raw)
		}
		return
	}
	if len(raw) > maxTextBytes {
		return
	}
	v := strings.TrimSpace(utf8OrLatin1(raw))
	if v == "" {
		return
	}
	switch key {
	case "TITLE":
		if md.Title == "" {
			md.Title = v
		}
	case "ARTIST":
		vc.artist = appendValues(vc.artist, v)
	case "ARTISTS":
		vc.artists = appendValues(vc.artists, v)
	case "ALBUM":
		if md.Album == "" {
			md.Album = v
		}
	case "ALBUMARTIST", "ALBUM ARTIST", "ALBUM_ARTIST":
		vc.albArt = appendValues(vc.albArt, v)
	case "TRACKNUMBER":
		if !vc.sawTrack {
			vc.sawTrack = true
			md.TrackNumber, md.TrackTotal = parseNumberPair(v)
		}
	case "TRACKTOTAL", "TOTALTRACKS":
		vc.trackTotal = atoiPrefix(v)
	case "DISCNUMBER":
		if !vc.sawDisc {
			vc.sawDisc = true
			md.DiscNumber, md.DiscTotal = parseNumberPair(v)
		}
	case "DISCTOTAL", "TOTALDISCS":
		vc.discTotal = atoiPrefix(v)
	case "DATE", "YEAR":
		if y := parseYear(v); y > 0 && vc.date == 0 {
			vc.date = y
		}
	case "ORIGINALDATE", "ORIGINALYEAR":
		if y := parseYear(v); y > 0 && vc.orig == 0 {
			vc.orig = y
		}
	case "GENRE":
		md.Genres = appendValues(md.Genres, v)
	case "COMPOSER":
		vc.composer = appendValues(vc.composer, v)
	case "COMMENT":
		if vc.comment == "" {
			vc.comment = v
		}
	case "DESCRIPTION":
		if vc.desc == "" {
			vc.desc = v
		}
	case "COMPILATION", "ITUNESCOMPILATION":
		md.Compilation = md.Compilation || parseBool(v)
	case "TITLESORT":
		md.SortTitle = v
	case "ARTISTSORT":
		md.SortArtist = v
	case "ALBUMSORT":
		md.SortAlbum = v
	case "ALBUMARTISTSORT":
		md.SortAlbumArtist = v
	case "COMPOSERSORT":
		md.SortComposer = v
	case "REPLAYGAIN_TRACK_GAIN":
		md.ReplayGain.TrackGain = parseGain(v)
	case "REPLAYGAIN_TRACK_PEAK":
		md.ReplayGain.TrackPeak = parseGain(v)
	case "REPLAYGAIN_ALBUM_GAIN":
		md.ReplayGain.AlbumGain = parseGain(v)
	case "REPLAYGAIN_ALBUM_PEAK":
		md.ReplayGain.AlbumPeak = parseGain(v)
	case "R128_TRACK_GAIN":
		vc.r128Track = r128(v)
	case "R128_ALBUM_GAIN":
		vc.r128Album = r128(v)
	case "COVERARTMIME":
		vc.coverArtMIME = strings.ToLower(v)
	}
}

// r128 converts an Opus R128 gain (Q7.8 dB relative to -23 LUFS) to a ReplayGain value
// (relative to -18 LUFS).
func r128(v string) *float64 {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < -32768 || n > 32767 {
		return nil
	}
	g := float64(n)/256 + 5
	return &g
}

func (vc *vorbisCollector) finish() {
	md := vc.md
	if len(vc.artist) > 0 {
		md.Artist = strings.Join(vc.artist, "; ")
		if len(vc.artist) > 1 {
			md.Artists = vc.artist
		}
	}
	if len(vc.artists) > 0 {
		md.Artists = vc.artists
	}
	if len(vc.albArt) > 0 {
		md.AlbumArtist = strings.Join(vc.albArt, "; ")
	}
	if len(vc.composer) > 0 {
		md.Composer = strings.Join(vc.composer, "; ")
	}
	if md.TrackTotal == 0 {
		md.TrackTotal = vc.trackTotal
	}
	if md.DiscTotal == 0 {
		md.DiscTotal = vc.discTotal
	}
	if vc.date > 0 {
		md.Year = vc.date
	} else if vc.orig > 0 {
		md.Year = vc.orig
	}
	if vc.comment != "" {
		md.Comment = vc.comment
	} else if vc.desc != "" {
		md.Comment = vc.desc
	}
	if md.ReplayGain.TrackGain == nil && vc.r128Track != nil {
		md.ReplayGain.TrackGain = vc.r128Track
	}
	if md.ReplayGain.AlbumGain == nil && vc.r128Album != nil {
		md.ReplayGain.AlbumGain = vc.r128Album
	}
	if vc.lyrics != "" && md.Lyrics == nil {
		md.Lyrics = ParseLyricsText(vc.lyrics)
	}
	if vc.coverArt != "" && len(md.Pictures) == 0 {
		if dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(vc.coverArt)); err == nil {
			md.addPicture(Picture{Type: PictureFrontCover, MIME: vc.coverArtMIME, Data: dec})
		}
	}
}
