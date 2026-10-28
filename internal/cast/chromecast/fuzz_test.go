package chromecast

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"

	"ludworks/internal/cast/renderer"
)

// FuzzDecodeMessage feeds arbitrary bytes to the CastMessage decoder (as the body of a frame
// and as a whole stream): it must never panic, and whatever it accepts must survive a
// re-encode unchanged.
func FuzzDecodeMessage(f *testing.F) {
	f.Add(golden)
	f.Add([]byte{})
	f.Add([]byte{0x80})
	f.Add([]byte{0x43, 0x00})
	f.Add((&Message{SourceID: "a", Namespace: NamespaceMedia, PayloadType: PayloadBinary, PayloadBinary: []byte{1}}).Marshal())
	f.Add(append(append([]byte(nil), golden...), 0xfa, 0xff, 0xff, 0xff, 0x0f, 0x01, 'z'))
	f.Fuzz(func(t *testing.T, b []byte) {
		m, err := UnmarshalMessage(b)
		if err == nil {
			again, err := UnmarshalMessage(m.Marshal())
			if err != nil {
				t.Fatalf("re-decode: %v", err)
			}
			if again.SourceID != m.SourceID || again.DestinationID != m.DestinationID || again.Namespace != m.Namespace ||
				again.PayloadType != m.PayloadType {
				t.Fatalf("re-encode changed the message: %+v vs %+v", again, m)
			}
			if m.PayloadType == PayloadBinary && !bytes.Equal(again.PayloadBinary, m.PayloadBinary) ||
				m.PayloadType == PayloadString && again.PayloadUTF8 != m.PayloadUTF8 {
				t.Fatalf("re-encode changed the payload")
			}
		}
		// As a framed stream: a bogus length must never make the reader allocate past the cap.
		r := bytes.NewReader(b)
		for i := 0; i < 8; i++ {
			if _, err := ReadMessage(r); err != nil {
				break
			}
		}
		framed := binary.BigEndian.AppendUint32(nil, uint32(len(b)))
		_, _ = ReadMessage(bytes.NewReader(append(framed, b...)))
	})
}

// FuzzDeviceMessages feeds arbitrary payloads on every namespace to a player (no network):
// device messages must never panic it or break its invariants.
func FuzzDeviceMessages(f *testing.F) {
	for _, s := range []string{
		`{"type":"RECEIVER_STATUS","requestId":0,"status":{"applications":[{"appId":"CC1AD845","sessionId":"s","transportId":"t"}],"volume":{"level":0.5,"muted":true}}}`,
		`{"type":"MEDIA_STATUS","status":[{"mediaSessionId":1,"playerState":"PLAYING","currentTime":12.5,"playbackRate":1,"media":{"duration":100}}]}`,
		`{"type":"MEDIA_STATUS","status":[{"mediaSessionId":1,"playerState":"IDLE","idleReason":"FINISHED"}]}`,
		`{"type":"MEDIA_STATUS","status":[{"mediaSessionId":1,"playerState":"PLAYING","currentTime":1e300,"playbackRate":-5}]}`,
		`{"type":"RECEIVER_STATUS","status":{"volume":{"level":1e308}}}`,
		`{"type":"CLOSE"}`,
		`{"type":"PING"}`,
		`[]`, `null`, `{"type":7}`, `{`,
	} {
		for ns := 0; ns < 4; ns++ {
			f.Add(ns, "t", s)
		}
	}
	namespaces := []string{NamespaceReceiver, NamespaceMedia, NamespaceConnection, NamespaceHeartbeat}
	f.Fuzz(func(t *testing.T, ns int, src, payload string) {
		p := newPlayer(New(Options{}), renderer.Device{Name: "fuzz"}, "127.0.0.1", 8009)
		defer p.cancel()
		p.appSession, p.transport, p.mediaSession = "s", "t", 1
		p.st.State = renderer.StatePlaying
		l := newLink(nopConn{}, "sender-0", 1, func(*Message) {})
		if ns < 0 {
			ns = -ns
		}
		m := &Message{SourceID: src, DestinationID: "sender-0", Namespace: namespaces[ns%len(namespaces)], PayloadUTF8: payload}
		for i := 0; i < 3; i++ {
			p.onMessage(l, m)
			if src == "t" || src == ReceiverID {
				p.onMessage(l, &Message{SourceID: ReceiverID, Namespace: NamespaceReceiver, PayloadUTF8: payload})
				p.onMessage(l, &Message{SourceID: "t", Namespace: NamespaceMedia, PayloadUTF8: payload})
			}
		}
		st := p.Status()
		if math.IsNaN(st.Position) || math.IsInf(st.Position, 0) || st.Position < 0 || st.Position > maxSeconds*maxPlaybackRate*2 {
			t.Fatalf("position %v", st.Position)
		}
		if st.Volume < 0 || st.Volume > 1 || math.IsNaN(st.Volume) {
			t.Fatalf("volume %v", st.Volume)
		}
		if st.Duration < 0 || st.Duration > maxSeconds {
			t.Fatalf("duration %v", st.Duration)
		}
	})
}
