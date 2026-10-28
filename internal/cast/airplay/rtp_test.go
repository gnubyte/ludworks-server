package airplay

import (
	"bytes"
	"encoding/hex"
	"testing"
	"time"
)

func mustHex(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestNTPConversions(t *testing.T) {
	cases := []struct {
		t    time.Time
		want ntpTime
	}{
		{time.Unix(0, 0), ntpTime(2208988800) << 32},
		{time.Unix(1, 500_000_000), ntpTime(2208988801)<<32 | 0x80000000},
		{time.Unix(0, 250_000_000), ntpTime(2208988800)<<32 | 0x40000000},
		{time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC), ntpTime(0xEE8B10C0) << 32}, // 4002091200 s after 1900
	}
	for _, c := range cases {
		if got := ntpFromTime(c.t); got != c.want {
			t.Errorf("ntpFromTime(%v) = %#x, want %#x", c.t, uint64(got), uint64(c.want))
		}
		if back := c.want.time(); !back.Equal(c.t) {
			t.Errorf("%#x.time() = %v, want %v", uint64(c.want), back, c.t)
		}
	}
	now := time.Now()
	if d := ntpFromTime(now).time().Sub(now); d > time.Nanosecond || d < -time.Nanosecond {
		t.Errorf("round trip off by %v", d)
	}
}

func TestAudioHeader(t *testing.T) {
	b := make([]byte, 12)
	putAudioHeader(b, true, 0x1234, 0xDEADBEEF, 0x01020304)
	if want := mustHex(t, "80e01234deadbeef01020304"); !bytes.Equal(b, want) {
		t.Errorf("marker header % x", b)
	}
	putAudioHeader(b, false, 0xFFFF, 1, 2)
	if want := mustHex(t, "8060ffff0000000100000002"); !bytes.Equal(b, want) {
		t.Errorf("header % x", b)
	}
}

func TestSyncPacket(t *testing.T) {
	now := ntpTime(0xE2E1D2C1_80000000)
	got := syncPacket(true, now, 100000, 11025)
	// 0x90 (extension bit) 0xD4 0x0007 | 100000-11025 = 88975 | NTP | 100000
	want := mustHex(t, "90d40007"+"00015b8f"+"e2e1d2c180000000"+"000186a0")
	if !bytes.Equal(got, want) {
		t.Errorf("first sync % x\nwant       % x", got, want)
	}
	got = syncPacket(false, now, 5, 11025) // RTP time wraps below zero: 2^32 - 11020
	if got[0] != 0x80 || !bytes.Equal(got[4:8], mustHex(t, "ffffd4f4")) {
		t.Errorf("sync % x", got)
	}
}

func TestTimingReply(t *testing.T) {
	req := mustHex(t, "80d20007"+"00000000"+"0000000000000000"+"0000000000000000"+"83aa7e8012345678")
	got, ok := timingReply(req, 0x1111111122222222, 0x3333333344444444)
	want := mustHex(t, "80d30007"+"00000000"+"83aa7e8012345678"+"1111111122222222"+"3333333344444444")
	if !ok || !bytes.Equal(got, want) {
		t.Errorf("reply % x\nwant  % x", got, want)
	}
	for _, bad := range [][]byte{req[:31], append(bytes.Clone(req), 0), mustHex(t, "80d30007"+string(bytes.Repeat([]byte("00"), 28)))} {
		if _, ok := timingReply(bad, 0, 0); ok {
			t.Errorf("answered % x", bad)
		}
	}
}

func TestResendPackets(t *testing.T) {
	first, count, ok := parseResendRequest(mustHex(t, "80d50001fffe0003"))
	if !ok || first != 0xFFFE || count != 3 {
		t.Errorf("parsed %d %d %v", first, count, ok)
	}
	if _, _, ok := parseResendRequest(mustHex(t, "80d50001fffe00")); ok {
		t.Error("short request accepted")
	}
	if _, _, ok := parseResendRequest(mustHex(t, "80d40001fffe0003")); ok {
		t.Error("sync packet taken for a resend request")
	}
	pkt := mustHex(t, "8060abcd0000000100000002aabbcc")
	if got := resendReply(pkt); !bytes.Equal(got, append(mustHex(t, "80d6abcd"), pkt...)) {
		t.Errorf("resend reply % x", got)
	}
}

func TestPacketRing(t *testing.T) {
	r := newPacketRing()
	start := uint16(65000)
	for i := range 2 * ringPackets {
		r.put(sentPacket{seq: start + uint16(i), frames: i}) // wraps through 0
	}
	last := start + uint16(2*ringPackets-1)
	if p, ok := r.get(last); !ok || p.frames != 2*ringPackets-1 {
		t.Errorf("newest packet missing: %+v %v", p, ok)
	}
	if _, ok := r.get(last - ringPackets); ok {
		t.Error("overwritten packet still found")
	}
	if p, ok := r.get(last - ringPackets + 1); !ok || p.frames != ringPackets {
		t.Errorf("oldest kept packet: %+v %v", p, ok)
	}
	r.reset()
	if _, ok := r.get(last); ok {
		t.Error("reset kept packets")
	}
}
