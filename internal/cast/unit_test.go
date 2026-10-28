package cast

import (
	"bytes"
	"encoding/binary"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"ludworks/internal/cast/mdns"
	"ludworks/internal/cast/renderer"
	"ludworks/internal/httpx"
)

func testItems(n int) []*castItem {
	out := make([]*castItem, n)
	for i := range out {
		out[i] = &castItem{Kind: KindMedia, ID: string(rune('a' + i))}
	}
	return out
}

// seq returns a deterministic "random" source cycling through values.
func seq(vals ...int) func(int) int {
	i := 0
	return func(n int) int {
		v := vals[i%len(vals)] % n
		i++
		return v
	}
}

func TestQueueNavigation(t *testing.T) {
	q := newQueue(testItems(3), 1, "", false, seq(0))
	if q.current() != 1 || q.repeat != RepeatOff {
		t.Fatalf("start: %d %s", q.current(), q.repeat)
	}
	if err := q.next(); err != nil || q.current() != 2 {
		t.Fatalf("next: %v %d", err, q.current())
	}
	if err := q.next(); err != errNoNext {
		t.Fatalf("next at the end: %v", err)
	}
	if q.advance() {
		t.Fatal("advance past the end without repeat")
	}
	q.repeat = RepeatOne
	if !q.advance() || q.current() != 2 {
		t.Fatal("repeat one replays")
	}
	if err := q.next(); err != errNoNext {
		t.Fatal("repeat one does not wrap a user next")
	}
	q.repeat = RepeatAll
	if err := q.next(); err != nil || q.current() != 0 {
		t.Fatalf("repeat all wraps: %d", q.current())
	}
	q.previous()
	if q.current() != 2 {
		t.Fatalf("previous wraps with repeat all: %d", q.current())
	}
	q.repeat = RepeatOff
	q.jump(0)
	q.previous()
	if q.current() != 0 {
		t.Fatal("previous at the start stays")
	}
	q.jump(2)
	if q.current() != 2 {
		t.Fatal("jump")
	}
}

func TestQueueShuffle(t *testing.T) {
	q := newQueue(testItems(5), 2, RepeatAll, true, seq(1, 3, 0, 2))
	if q.current() != 2 || q.pos != 0 {
		t.Fatalf("shuffle starts with the current item: %v pos %d", q.order, q.pos)
	}
	perm := slices.Clone(q.order)
	slices.Sort(perm)
	if !slices.Equal(perm, []int{0, 1, 2, 3, 4}) {
		t.Fatalf("not a permutation: %v", q.order)
	}
	seen := map[int]bool{q.current(): true}
	for range 4 {
		if err := q.next(); err != nil {
			t.Fatal(err)
		}
		seen[q.current()] = true
	}
	if len(seen) != 5 {
		t.Fatalf("shuffle did not visit every item: %v", seen)
	}
	last := q.current()
	if !q.advance() || q.pos != 0 || q.order[0] == last {
		t.Fatalf("repeat all reshuffles without repeating the last item first: %v", q.order)
	}
	cur := q.current()
	q.setShuffle(false)
	if q.current() != cur || !slices.Equal(q.order, []int{0, 1, 2, 3, 4}) {
		t.Fatalf("unshuffle: %v %d", q.order, q.current())
	}
}

func TestQueueInsertAndReplace(t *testing.T) {
	q := newQueue(testItems(3), 1, "", false, nil)
	q.insert(testItems(2), 0)
	if len(q.items) != 5 || q.current() != 3 || q.items[q.current()].ID != "b" {
		t.Fatalf("insert before: %v cur %d", q.order, q.current())
	}
	q.insert(testItems(1), 99)
	if len(q.items) != 6 || q.items[5].ID != "a" || q.items[q.current()].ID != "b" {
		t.Fatal("append")
	}
	// Shuffled: inserted items play next, the current item stays.
	q = newQueue(testItems(4), 0, "", true, seq(0))
	cur := q.current()
	q.insert([]*castItem{{Kind: KindTrack, ID: "new1"}, {Kind: KindTrack, ID: "new2"}}, 1)
	if q.items[q.current()] != q.items[q.order[q.pos]] || q.current() != shiftIndex(cur, 1, 2) {
		t.Fatalf("current changed: %v", q.order)
	}
	if err := q.next(); err != nil || q.items[q.current()].ID != "new1" {
		t.Fatalf("inserted items do not play next: %v", q.order)
	}
	q.replace(testItems(2), 9)
	if len(q.items) != 2 || q.current() > 1 || !q.shuffle {
		t.Fatalf("replace clamps and keeps shuffle: %v", q.order)
	}
}

func shiftIndex(i, at, n int) int {
	if i >= at {
		return i + n
	}
	return i
}

func TestDevicesFromInstances(t *testing.T) {
	insts := []mdns.Instance{
		chromecastInstance("0123456789ABCDEF0123456789abcdef", "Living Room TV", "192.0.2.10"),
		{Service: renderer.ServiceGoogleCast, Name: "Speaker group", Host: "g.local", Port: 32111,
			Addrs: mustAddrs("192.0.2.11"), TXT: map[string]string{"fn": "", "ca": "2084"}},
		{Service: renderer.ServiceRAOP, Name: "AABBCCDDEEFF@Living Room", Host: "lr.local", Port: 7000,
			Addrs: mustAddrs("2001:db8::30", "192.0.2.30"), TXT: map[string]string{"am": "AppleTV11,1"}},
		{Service: renderer.ServiceAirPlay, Name: "Living Room", Host: "lr.local", Port: 7000,
			Addrs: mustAddrs("192.0.2.30"), TXT: map[string]string{"deviceid": "aa:bb:cc:dd:ee:ff", "model": "AppleTV11,1"}},
		{Service: renderer.ServiceRAOP, Name: "112233445566@Kitchen", Host: "k.local", Port: 7000,
			Addrs: mustAddrs("192.0.2.40"), TXT: map[string]string{"am": "AudioAccessory5,1"}},
		{Service: renderer.ServiceRAOP, Name: "no-mac-here", Host: "odd.local", Port: 5000, Addrs: mustAddrs("192.0.2.50")},
		{Service: renderer.ServiceAirPlay, Name: "No addresses", Host: "x.local", Port: 7000},
	}
	got := devicesFromInstances(insts)
	if len(got) != 5 {
		t.Fatalf("devices: %d", len(got))
	}
	byID := map[string]*deviceEntry{}
	for _, e := range got {
		if !httpx.ValidID(e.dev.ID) {
			t.Errorf("invalid id %q", e.dev.ID)
		}
		byID[e.dev.ID] = e
	}
	cc := byID["chromecast-0123456789abcdef0123456789abcdef"]
	if cc == nil || cc.dev.Name != "Living Room TV" || cc.dev.Model != "Chromecast" || cc.dev.Endpoints[0].Host != "192.0.2.10" {
		t.Fatalf("chromecast: %+v", cc)
	}
	group := byID[kindID(renderer.KindChromecast, "", "Speaker group")]
	if group == nil || group.dev.Name != "Speaker group" || group.dev.Endpoints[0].Port != 32111 {
		t.Fatalf("group without id: %+v", byID)
	}
	apple := byID["airplay-aabbccddeeff"]
	if apple == nil || apple.dev.Name != "Living Room" || apple.dev.Model != "AppleTV11,1" || len(apple.dev.Endpoints) != 2 ||
		apple.dev.Endpoints[0].Service != renderer.ServiceAirPlay || !slices.Contains(apple.addrs, netip.MustParseAddr("2001:db8::30")) {
		t.Fatalf("merged apple device: %+v", apple)
	}
	kitchen := byID["airplay-112233445566"]
	if kitchen == nil || kitchen.dev.Name != "Kitchen" || kitchen.dev.Model != "AudioAccessory5,1" {
		t.Fatalf("raop-only device: %+v", kitchen)
	}
	if odd := byID[kindID(renderer.KindAirPlay, "", "odd.local")]; odd == nil || odd.dev.Name != "no-mac-here" {
		t.Fatalf("device without identifier: %+v", byID)
	}

	for _, c := range []struct{ host, want string }{
		{"192.0.2.5", "manual-192-0-2-5-8009"},
		{"fd00::5", "manual-fd00--5-8009"},
		{"tv.example.lan", "manual-tv-example-lan-8009"},
	} {
		if got := manualID(c.host, 8009); got != c.want {
			t.Errorf("manualID(%s) = %s", c.host, got)
		}
	}
	long := manualID(strings.Repeat("a", 70)+".example", 8009)
	if !httpx.ValidID(long) || !strings.HasPrefix(long, "manual-") {
		t.Errorf("long manual id %q", long)
	}
	if macID("AA:BB:CC:DD:EE:FF") != "aabbccddeeff" || macID("zz:bb:cc:dd:ee:ff") != "" || macID("aabb") != "" {
		t.Error("macID")
	}
	m := manualEntry(ManualDevice{Kind: renderer.KindAirPlay, Host: "10.0.0.9"})
	if len(m.dev.Endpoints) != 2 || m.dev.Endpoints[0].Port != 7000 || !m.dev.Manual || m.dev.Name != "10.0.0.9" || len(m.addrs) != 1 {
		t.Fatalf("manual airplay: %+v", m)
	}
}

func TestPlayability(t *testing.T) {
	for _, c := range []struct {
		container, v, a string
		bp              bool
		want            bool
	}{
		{"mp4", "h264", "aac", false, true},
		{"webm", "vp9", "opus", false, true},
		{"mov", "h264", "", false, true},
		{"mkv", "h264", "aac", true, false},
		{"mp4", "hevc", "aac", false, false},
		{"mp4", "h264", "ac3", false, false},
		{"", "", "", true, true}, // not probed: the browser verdict
		{"", "", "", false, false},
	} {
		if got := chromecastVideoOK(c.container, c.v, c.a, c.bp); got != c.want {
			t.Errorf("chromecast %v = %v", c, got)
		}
	}
	if !airplayVideoOK("mp4", "hevc", "eac3") || airplayVideoOK("webm", "vp9", "opus") || airplayVideoOK("mkv", "h264", "aac") {
		t.Error("airplay video rules")
	}
	for _, c := range []struct {
		container, codec, mime string
		want                   bool
	}{
		{"mp3", "mp3", "audio/mpeg", true},
		{"m4a", "aac", "audio/mp4", true},
		{"flac", "flac", "audio/flac", true},
		{"ogg", "vorbis", "audio/ogg", true},
		{"ogg", "opus", "audio/ogg", true},
		{"wav", "pcm_s16le", "audio/wav", true},
		{"m4a", "alac", "audio/mp4", false},
		{"", "wmav2", "audio/x-ms-wma", false},
		{"", "", "audio/x-ms-wma", false},
		{"", "", "audio/flac", true},
	} {
		if got := chromecastAudioOK(c.container, c.codec, c.mime); got != c.want {
			t.Errorf("audio %v = %v", c, got)
		}
	}
}

// wav builds a RIFF/WAVE file.
func wav(format, channels uint16, rate uint32, bits uint16, data []byte, extra ...[]byte) []byte {
	var b bytes.Buffer
	le := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
	b.WriteString("RIFF")
	le(uint32(0))
	b.WriteString("WAVE")
	for _, x := range extra {
		b.Write(x)
	}
	b.WriteString("fmt ")
	le(uint32(16))
	le(format)
	le(channels)
	le(rate)
	le(rate * uint32(channels) * uint32(bits/8))
	le(channels * bits / 8)
	le(bits)
	b.WriteString("data")
	le(uint32(len(data)))
	b.Write(data)
	return b.Bytes()
}

func TestParseWAV(t *testing.T) {
	samples := make([]byte, 4*pcmRate) // one second
	for i := range samples {
		samples[i] = byte(i)
	}
	list := append([]byte("LIST\x05\x00\x00\x00abcde"), 0) // odd size, padded
	good := wav(1, 2, 44100, 16, samples, list)
	lay, ok := parseWAV(bytes.NewReader(good), int64(len(good)))
	if !ok || lay.size != int64(len(samples)) || !bytes.Equal(good[lay.offset:lay.offset+4], samples[:4]) {
		t.Fatalf("good wav: %+v %v", lay, ok)
	}
	for name, b := range map[string][]byte{
		"mono":      wav(1, 1, 44100, 16, samples),
		"48k":       wav(1, 2, 48000, 16, samples),
		"24-bit":    wav(1, 2, 44100, 24, samples),
		"float":     wav(3, 2, 44100, 16, samples),
		"not riff":  append([]byte("RIFX"), good[4:]...),
		"truncated": good[:30],
		"no fmt":    append(append([]byte("RIFF\x00\x00\x00\x00WAVE"), []byte("data\x04\x00\x00\x00abcd")...), make([]byte, 40)...),
	} {
		if _, ok := parseWAV(bytes.NewReader(b), int64(len(b))); ok {
			t.Errorf("%s accepted", name)
		}
	}
	// A data size larger than the file (streaming writers) is clamped.
	huge := wav(1, 2, 44100, 16, samples[:400])
	binary.LittleEndian.PutUint32(huge[40:44], 0xFFFFFFF0)
	if lay, ok := parseWAV(bytes.NewReader(huge), int64(len(huge))); !ok || lay.size != 400 {
		t.Fatalf("clamped data size: %+v %v", lay, ok)
	}
	// Chunk walking is bounded (a chain of tiny chunks never reaches "data").
	var chain bytes.Buffer
	chain.WriteString("RIFF\x00\x00\x00\x00WAVE")
	for range wavMaxChunks + 5 {
		chain.WriteString("junk\x00\x00\x00\x00")
	}
	chain.Write(wav(1, 2, 44100, 16, samples[:8])[12:])
	if _, ok := parseWAV(bytes.NewReader(chain.Bytes()), int64(chain.Len())); ok {
		t.Error("unbounded chunk walk")
	}

	// openWAV starts at the requested second, frame-aligned.
	dir := t.TempDir()
	p := filepath.Join(dir, "a.wav")
	two := append(slices.Clone(samples), samples...)
	if err := os.WriteFile(p, wav(1, 2, 44100, 16, two), 0o600); err != nil {
		t.Fatal(err)
	}
	rc, ok := openWAV(p, 1.0)
	if !ok {
		t.Fatal("openWAV failed")
	}
	defer rc.Close()
	var got bytes.Buffer
	if _, err := got.ReadFrom(rc); err != nil {
		t.Fatal(err)
	}
	if got.Len() != len(samples) || !bytes.Equal(got.Bytes()[:8], samples[:8]) {
		t.Fatalf("openWAV from 1 s: %d bytes", got.Len())
	}
}

func FuzzParseWAV(f *testing.F) {
	f.Add(wav(1, 2, 44100, 16, make([]byte, 64)))
	f.Add([]byte("RIFF\x00\x00\x00\x00WAVEfmt \xff\xff\xff\xff"))
	f.Fuzz(func(t *testing.T, b []byte) {
		lay, ok := parseWAV(bytes.NewReader(b), int64(len(b)))
		if ok && (lay.offset < 0 || lay.size <= 0 || lay.offset+lay.size > int64(len(b)) || lay.size%pcmFrameBytes != 0) {
			t.Fatalf("bad layout %+v for %d bytes", lay, len(b))
		}
	})
}
