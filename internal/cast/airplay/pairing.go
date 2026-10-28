package airplay

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"

	"golang.org/x/crypto/chacha20poly1305"

	"ludworks/internal/cast/renderer"
)

// HAP pairing over AirPlay (HTTP on the AirPlay port):
//
//	pair-setup with PIN  POST /pair-pin-start (the TV shows a PIN), then POST /pair-setup
//	                     M1–M6 (header X-Apple-HKP: 3): SRP-6a (srp.go) proves the PIN,
//	                     then both sides exchange their Ed25519 long-term keys, encrypted
//	                     with ChaCha20-Poly1305 under keys HKDF-SHA512-derived from the SRP
//	                     session key and signed over HKDF-derived "X" values.
//	transient pair-setup M1–M4 only, with Flags = transient and the fixed PIN 3939 (header
//	                     X-Apple-HKP: 4); the SRP session key is the connection secret.
//	pair-verify          POST /pair-verify M1–M4: an X25519 exchange whose ephemeral keys
//	                     both sides sign with their long-term keys; the shared secret is the
//	                     connection secret.
//
// The connection secret derives the HAP framing keys (secure.go) with salt "Control-Salt".

const (
	pairUserAgent = "AirPlay/550.10"
	pairUser      = "Pair-Setup"
	transientPIN  = "3939"
	flagTransient = 0x10

	hkpPairing   = "3"
	hkpTransient = "4"
)

// credentials are the result of a PIN pairing: our controller identity and the device's.
type credentials struct {
	id       []byte             // our pairing identifier
	key      ed25519.PrivateKey // our long-term key
	deviceID []byte             // the device's pairing identifier
	devicePK ed25519.PublicKey  // the device's long-term public key
}

const credsVersion = 1

var errBadCredentials = errors.New("airplay: invalid pairing credentials")

// marshal serialises credentials: version byte, then length-prefixed (one byte) fields:
// our identifier, our Ed25519 seed, the device identifier, the device public key.
func (c *credentials) marshal() []byte {
	out := []byte{credsVersion}
	for _, f := range [][]byte{c.id, c.key.Seed(), c.deviceID, c.devicePK} {
		out = append(out, byte(len(f)))
		out = append(out, f...)
	}
	return out
}

func parseCredentials(b []byte) (*credentials, error) {
	if len(b) == 0 || b[0] != credsVersion {
		return nil, errBadCredentials
	}
	b = b[1:]
	var f [4][]byte
	for i := range f {
		if len(b) < 1 || int(b[0]) > len(b)-1 {
			return nil, errBadCredentials
		}
		f[i], b = bytes.Clone(b[1:1+int(b[0])]), b[1+int(b[0]):]
	}
	if len(b) != 0 || len(f[0]) == 0 || len(f[1]) != ed25519.SeedSize || len(f[2]) == 0 ||
		len(f[3]) != ed25519.PublicKeySize {
		return nil, errBadCredentials
	}
	return &credentials{id: f[0], key: ed25519.NewKeyFromSeed(f[1]), deviceID: f[2], devicePK: f[3]}, nil
}

// pairHeaders are the headers of pairing requests.
func pairHeaders(hkp string) []hdr {
	return []hdr{
		{"User-Agent", pairUserAgent},
		{"Connection", "keep-alive"},
		{"X-Apple-HKP", hkp},
		{"Content-Type", "application/octet-stream"},
	}
}

// pairError maps a pairing TLV error to a driver error.
func pairError(step string, tlv map[byte][]byte) error {
	code, ok := tlv[tlvError]
	if !ok {
		return nil
	}
	var c byte
	if len(code) > 0 {
		c = code[0]
	}
	switch c {
	case tlvErrAuthentication:
		return fmt.Errorf("airplay: %s: %w", step, renderer.ErrWrongPIN)
	case tlvErrBackoff, tlvErrMaxTries:
		return fmt.Errorf("airplay: %s: the device refuses more pairing attempts for now (error %d)", step, c)
	case tlvErrMaxPeers:
		return fmt.Errorf("airplay: %s: the device cannot store more pairings", step)
	case tlvErrBusy, tlvErrUnavailable:
		return fmt.Errorf("airplay: %s: the device is busy pairing (error %d)", step, c)
	}
	return fmt.Errorf("airplay: %s: pairing error %d", step, c)
}

// pairExchange posts one pairing message and returns the decoded TLV response.
func pairExchange(ctx context.Context, c *msgConn, path, hkp, step string, items ...tlvItem) (map[byte][]byte, error) {
	resp, err := c.do(ctx, "POST", path, "HTTP/1.1", pairHeaders(hkp), encodeTLV(items...))
	if err != nil {
		return nil, err
	}
	if resp.status != 200 {
		return nil, fmt.Errorf("airplay: %s: status %d: %w", step, resp.status, errPairStatus(resp.status))
	}
	tlv, err := decodeTLV(resp.body)
	if err != nil {
		return nil, fmt.Errorf("airplay: %s: %w", step, err)
	}
	if err := pairError(step, tlv); err != nil {
		return nil, err
	}
	return tlv, nil
}

// errPairStatus classifies a non-200 pairing response.
func errPairStatus(status int) error {
	switch status {
	case 401, 403, 470:
		return renderer.ErrPairingRequired
	case 404, 501:
		return renderer.ErrPairingUnsupported
	}
	return errProtocol
}

func expectState(tlv map[byte][]byte, state byte) error {
	if s := tlv[tlvState]; len(s) != 1 || s[0] != state {
		return fmt.Errorf("%w: unexpected pairing state", errProtocol)
	}
	return nil
}

// pinPairing is an in-progress PIN pairing (renderer.Pairing).
type pinPairing struct {
	conn *msgConn

	mu    sync.Mutex
	salt  []byte
	pubB  []byte
	used  bool
	close sync.Once
}

// startPinPairing asks the device to show a PIN and runs pair-setup M1/M2.
func startPinPairing(ctx context.Context, conn *msgConn) (*pinPairing, error) {
	resp, err := conn.do(ctx, "POST", "/pair-pin-start", "HTTP/1.1", pairHeaders(hkpPairing), nil)
	if err != nil {
		return nil, err
	}
	if resp.status != 200 {
		return nil, fmt.Errorf("airplay: pair-pin-start: status %d: %w", resp.status, errPairStatus(resp.status))
	}
	tlv, err := pairExchange(ctx, conn, "/pair-setup", hkpPairing, "pair-setup M1",
		tlvItem{tlvMethod, []byte{0}}, tlvItem{tlvState, []byte{1}})
	if err != nil {
		return nil, err
	}
	if err := expectState(tlv, 2); err != nil {
		return nil, err
	}
	if len(tlv[tlvSalt]) == 0 || len(tlv[tlvPublicKey]) == 0 {
		return nil, fmt.Errorf("%w: pair-setup M2 without salt or key", errProtocol)
	}
	return &pinPairing{conn: conn, salt: tlv[tlvSalt], pubB: tlv[tlvPublicKey]}, nil
}

// Confirm runs pair-setup M3–M6 with pin and returns the serialised credentials.
func (p *pinPairing) Confirm(ctx context.Context, pin string) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.used {
		return nil, errors.New("airplay: pairing already used; start a new one")
	}
	p.used = true
	pin = strings.TrimSpace(pin)
	if len(pin) < 4 || len(pin) > 8 || strings.Trim(pin, "0123456789") != "" {
		return nil, fmt.Errorf("airplay: PIN must be 4–8 digits: %w", renderer.ErrWrongPIN)
	}
	creds, err := finishPairSetup(ctx, p.conn, p.salt, p.pubB, pin)
	if err != nil {
		return nil, err
	}
	return creds.marshal(), nil
}

func (p *pinPairing) Close() error {
	p.close.Do(func() { p.conn.close() })
	return nil
}

// srpProof runs the SRP half of pair-setup (M3/M4) and returns the session key.
func srpProof(ctx context.Context, conn *msgConn, hkp string, salt, pubB []byte, pin string) ([]byte, error) {
	srp, err := newRandomSRPClient(hapSRP, []byte(pairUser), []byte(pin), rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := srp.compute(salt, pubB); err != nil {
		return nil, err
	}
	tlv, err := pairExchange(ctx, conn, "/pair-setup", hkp, "pair-setup M3",
		tlvItem{tlvState, []byte{3}}, tlvItem{tlvPublicKey, srp.publicA()}, tlvItem{tlvProof, srp.M1})
	if err != nil {
		return nil, err
	}
	if err := expectState(tlv, 4); err != nil {
		return nil, err
	}
	if !srp.verifyM2(tlv[tlvProof]) {
		return nil, fmt.Errorf("airplay: pair-setup M4: device proof mismatch: %w", errSRP)
	}
	return srp.K, nil
}

// finishPairSetup runs M3–M6 and returns new credentials.
func finishPairSetup(ctx context.Context, conn *msgConn, salt, pubB []byte, pin string) (*credentials, error) {
	K, err := srpProof(ctx, conn, hkpPairing, salt, pubB, pin)
	if err != nil {
		return nil, err
	}
	encKey := hkdfKey(K, "Pair-Setup-Encrypt-Salt", "Pair-Setup-Encrypt-Info")
	aead, err := chacha20poly1305.New(encKey)
	if err != nil {
		return nil, err
	}

	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	id := []byte(newUUID())
	x := hkdfKey(K, "Pair-Setup-Controller-Sign-Salt", "Pair-Setup-Controller-Sign-Info")
	pub := key.Public().(ed25519.PublicKey)
	sig := ed25519.Sign(key, concat(x, id, pub))
	sub := encodeTLV(tlvItem{tlvIdentifier, id}, tlvItem{tlvPublicKey, pub}, tlvItem{tlvSignature, sig})
	tlv, err := pairExchange(ctx, conn, "/pair-setup", hkpPairing, "pair-setup M5",
		tlvItem{tlvState, []byte{5}}, tlvItem{tlvEncryptedData, aead.Seal(nil, labelNonce("PS-Msg05"), sub, nil)})
	if err != nil {
		return nil, err
	}
	if err := expectState(tlv, 6); err != nil {
		return nil, err
	}
	plain, err := aead.Open(nil, labelNonce("PS-Msg06"), tlv[tlvEncryptedData], nil)
	if err != nil {
		return nil, fmt.Errorf("%w: pair-setup M6 does not decrypt", errProtocol)
	}
	dev, err := decodeTLV(plain)
	if err != nil {
		return nil, err
	}
	devID, devPK, devSig := dev[tlvIdentifier], dev[tlvPublicKey], dev[tlvSignature]
	if len(devID) == 0 || len(devID) > 255 || len(devPK) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%w: pair-setup M6 without device identity", errProtocol)
	}
	devX := hkdfKey(K, "Pair-Setup-Accessory-Sign-Salt", "Pair-Setup-Accessory-Sign-Info")
	if !ed25519.Verify(devPK, concat(devX, devID, devPK), devSig) {
		return nil, fmt.Errorf("%w: pair-setup M6 signature does not verify", errProtocol)
	}
	return &credentials{id: id, key: key, deviceID: devID, devicePK: devPK}, nil
}

// transientPairSetup runs transient pair-setup and returns the connection secret.
func transientPairSetup(ctx context.Context, conn *msgConn) ([]byte, error) {
	// Some devices want the PIN start even though no PIN is shown; tolerate any answer.
	if resp, err := conn.do(ctx, "POST", "/pair-pin-start", "HTTP/1.1", pairHeaders(hkpTransient), nil); err != nil {
		return nil, err
	} else if resp.status == 401 || resp.status == 403 || resp.status == 470 {
		return nil, fmt.Errorf("airplay: transient pairing refused (status %d): %w", resp.status, renderer.ErrPairingRequired)
	}
	tlv, err := pairExchange(ctx, conn, "/pair-setup", hkpTransient, "transient pair-setup M1",
		tlvItem{tlvMethod, []byte{0}}, tlvItem{tlvState, []byte{1}}, tlvItem{tlvFlags, []byte{flagTransient}})
	if err != nil {
		return nil, err
	}
	if err := expectState(tlv, 2); err != nil {
		return nil, err
	}
	return srpProof(ctx, conn, hkpTransient, tlv[tlvSalt], tlv[tlvPublicKey], transientPIN)
}

// pairVerify proves both identities on conn and returns the connection secret.
func pairVerify(ctx context.Context, conn *msgConn, creds *credentials) ([]byte, error) {
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	ourPub := eph.PublicKey().Bytes()
	tlv, err := pairExchange(ctx, conn, "/pair-verify", hkpPairing, "pair-verify M1",
		tlvItem{tlvState, []byte{1}}, tlvItem{tlvPublicKey, ourPub})
	if err != nil {
		return nil, err
	}
	if err := expectState(tlv, 2); err != nil {
		return nil, err
	}
	devPub, err := ecdh.X25519().NewPublicKey(tlv[tlvPublicKey])
	if err != nil {
		return nil, fmt.Errorf("%w: pair-verify M2 key: %w", errProtocol, err)
	}
	shared, err := eph.ECDH(devPub)
	if err != nil {
		return nil, fmt.Errorf("%w: pair-verify M2 key: %w", errProtocol, err)
	}
	aead, err := chacha20poly1305.New(hkdfKey(shared, "Pair-Verify-Encrypt-Salt", "Pair-Verify-Encrypt-Info"))
	if err != nil {
		return nil, err
	}
	plain, err := aead.Open(nil, labelNonce("PV-Msg02"), tlv[tlvEncryptedData], nil)
	if err != nil {
		return nil, fmt.Errorf("airplay: pair-verify M2 does not decrypt: %w", renderer.ErrPairingRequired)
	}
	dev, err := decodeTLV(plain)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(dev[tlvIdentifier], creds.deviceID) ||
		!ed25519.Verify(creds.devicePK, concat(devPub.Bytes(), creds.deviceID, ourPub), dev[tlvSignature]) {
		// Another device, or it was reset: the stored pairing is useless.
		return nil, fmt.Errorf("airplay: pair-verify: the device does not match the pairing: %w", renderer.ErrPairingRequired)
	}
	sig := ed25519.Sign(creds.key, concat(ourPub, creds.id, devPub.Bytes()))
	sub := encodeTLV(tlvItem{tlvIdentifier, creds.id}, tlvItem{tlvSignature, sig})
	tlv, err = pairExchange(ctx, conn, "/pair-verify", hkpPairing, "pair-verify M3",
		tlvItem{tlvState, []byte{3}}, tlvItem{tlvEncryptedData, aead.Seal(nil, labelNonce("PV-Msg03"), sub, nil)})
	if err != nil {
		if errors.Is(err, renderer.ErrWrongPIN) { // authentication error: it no longer knows us
			return nil, fmt.Errorf("airplay: pair-verify: the device rejected the pairing: %w", renderer.ErrPairingRequired)
		}
		return nil, err
	}
	if err := expectState(tlv, 4); err != nil {
		return nil, err
	}
	return shared, nil
}

// encryptControl switches conn to HAP framing with the control channel keys of secret.
func encryptControl(conn *msgConn, secret []byte) error {
	return conn.encrypt(
		hkdfKey(secret, "Control-Salt", "Control-Write-Encryption-Key"),
		hkdfKey(secret, "Control-Salt", "Control-Read-Encryption-Key"))
}

// newUUID returns a random (version 4) UUID in upper case, the form Apple senders use for
// identifiers.
func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0F | 0x40
	b[8] = b[8]&0x3F | 0x80
	h := strings.ToUpper(hex.EncodeToString(b[:]))
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
