package airplay

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math/big"
	"net"
	"strconv"
	"strings"
)

// RAOP message bodies: the ANNOUNCE SDP, RSA/AES stream encryption, DMAP metadata and
// text/parameters values.

// airportKeyModulus is the RSA-2048 public key (exponent 65537) of the AirPort Express,
// which every RAOP receiver offering encryption type 1 holds the private half of. The
// sender wraps its per-session AES key with it.
const airportKeyModulus = "59dE8qLieItsH1WgjrcFRKj6eUWqi+bGLOX1HL3U3GhC/j0Qg90u3sG/1CUtwC5vOYvfDmFI6oSFXi5ELabWJmT2dKHz" +
	"BJKa3k9ok+8t9ucRqMd6DZHJ2YCCLlDRKSKv6kDqnw4UwPdpOMXziC/AMj3Z/lUVX1G7WSHCAWKf1zNS1eLvqr+boEjX" +
	"uBOitnZ/bDzPHrTOZz0Dew0uowxf/+sG+NCK3eQJVxqcaJ/vEHKIVd2M+5qL71yJQ+87X6oV3eaYvt3zWZYD6z5vYTcr" +
	"tij2VZ9Zmni/UAaHqn9JdsBWLUEpVviYnhimNVvYFZeCXg/IdTQ+x4IRdiXNv5hEew=="

func airportKey() *rsa.PublicKey {
	n, err := base64.StdEncoding.DecodeString(airportKeyModulus)
	if err != nil {
		panic(err)
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: 65537}
}

// streamCipher is the AES-128-CBC encryption of RAOP payloads (encryption type 1): each
// payload is encrypted on its own from the session IV; a trailing partial block stays in
// the clear.
type streamCipher struct {
	block cipher.Block
	key   []byte
	iv    []byte
}

func newStreamCipher() (*streamCipher, error) {
	key := make([]byte, 16)
	iv := make([]byte, 16)
	rand.Read(key)
	rand.Read(iv)
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return &streamCipher{block: b, key: key, iv: iv}, nil
}

// seal encrypts the whole 16-byte blocks of payload in place.
func (s *streamCipher) seal(payload []byte) {
	n := len(payload) &^ (aes.BlockSize - 1)
	if n > 0 {
		cipher.NewCBCEncrypter(s.block, s.iv).CryptBlocks(payload[:n], payload[:n])
	}
}

// sdpAttrs returns the rsaaeskey and aesiv SDP values: the AES key RSA-OAEP(SHA-1)
// wrapped with pub, and the IV, both base64 without padding.
func (s *streamCipher) sdpAttrs(pub *rsa.PublicKey) (key, iv string, err error) {
	wrapped, err := rsa.EncryptOAEP(sha1.New(), rand.Reader, pub, s.key, nil)
	if err != nil {
		return "", "", err
	}
	return base64.RawStdEncoding.EncodeToString(wrapped), base64.RawStdEncoding.EncodeToString(s.iv), nil
}

// buildSDP is the ANNOUNCE body for an ALAC 44.1 kHz 16-bit stereo stream.
func buildSDP(sessionID uint32, local, remote net.IP, rsaKey, iv string) []byte {
	var b strings.Builder
	b.WriteString("v=0\r\n")
	fmt.Fprintf(&b, "o=iTunes %d 0 IN %s %s\r\n", sessionID, ipFamily(local), local)
	b.WriteString("s=iTunes\r\n")
	fmt.Fprintf(&b, "c=IN %s %s\r\n", ipFamily(remote), remote)
	b.WriteString("t=0 0\r\n")
	b.WriteString("m=audio 0 RTP/AVP 96\r\n")
	b.WriteString("a=rtpmap:96 AppleLossless\r\n")
	fmt.Fprintf(&b, "a=fmtp:96 %d 0 16 40 10 14 2 255 0 0 %d\r\n", rtpFramesPerPacket, rtpSampleRate)
	if rsaKey != "" {
		fmt.Fprintf(&b, "a=rsaaeskey:%s\r\n", rsaKey)
		fmt.Fprintf(&b, "a=aesiv:%s\r\n", iv)
	}
	return []byte(b.String())
}

func ipFamily(ip net.IP) string {
	if ip.To4() == nil {
		return "IP6"
	}
	return "IP4"
}

// dmapItem appends one DMAP item: 4-byte tag, 4-byte big-endian length, value.
func dmapItem(b []byte, tag string, val []byte) []byte {
	b = append(b, tag[:4]...)
	b = binary.BigEndian.AppendUint32(b, uint32(len(val)))
	return append(b, val...)
}

// dmapMetadata is the SET_PARAMETER application/x-dmap-tagged body: an mlit container
// with the title (minm), artist (asar) and album (asal) present.
func dmapMetadata(title, artist, album string) []byte {
	var items []byte
	for _, f := range []struct{ tag, v string }{{"minm", title}, {"asar", artist}, {"asal", album}} {
		if f.v != "" {
			items = dmapItem(items, f.tag, []byte(clip(f.v, 1024)))
		}
	}
	return dmapItem(nil, "mlit", items)
}

// clip shortens s to at most n bytes without splitting a UTF-8 sequence.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

// volumeDB maps a 0–1 level to the RAOP volume in dB: −30 (quietest) to 0, −144 = mute.
func volumeDB(level float64, muted bool) float64 {
	if muted || level <= 0 {
		return -144
	}
	return -30 + 30*min(level, 1)
}

// parseTransport reads the key=value parameters of a SETUP Transport header.
func parseTransport(v string) map[string]string {
	out := map[string]string{}
	for _, p := range strings.Split(v, ";") {
		if k, val, ok := strings.Cut(strings.TrimSpace(p), "="); ok {
			out[strings.ToLower(k)] = val
		}
	}
	return out
}

func parsePort(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	return n, err == nil && n > 0 && n < 65536
}
