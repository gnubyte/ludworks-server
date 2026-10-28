package airplay

import (
	"crypto/hmac"
	"crypto/sha512"
	"errors"
	"hash"
	"io"
	"math/big"
)

// SRP-6a (RFC 5054) as HAP pair-setup uses it: the 3072-bit group with g = 5, SHA-512,
// username "Pair-Setup" and the PIN as password. Byte conventions follow Apple's HomeKit
// ADK: k = H(N | PAD(g)), x = H(salt | H(I ":" P)), u = H(PAD(A) | PAD(B)),
// K = H(S without leading zero bytes), M1 = H(H(N) xor H(g) | H(I) | salt | A | B | K) with
// A and B stripped of leading zero bytes, and the server proof M2 = H(PAD(A) | M1 | K).

// srpGroup is an SRP group with its hash.
type srpGroup struct {
	N    *big.Int
	g    *big.Int
	size int // byte length of N
	hash func() hash.Hash
}

// rfc5054N3072 is the 3072-bit prime of RFC 5054 Appendix A (generator 5).
const rfc5054N3072 = "FFFFFFFFFFFFFFFFC90FDAA22168C234C4C6628B80DC1CD129024E088A67CC74020BBEA63B139B22514A08798E3404DD" +
	"EF9519B3CD3A431B302B0A6DF25F14374FE1356D6D51C245E485B576625E7EC6F44C42E9A637ED6B0BFF5CB6F406B7ED" +
	"EE386BFB5A899FA5AE9F24117C4B1FE649286651ECE45B3DC2007CB8A163BF0598DA48361C55D39A69163FA8FD24CF5F" +
	"83655D23DCA3AD961C62F356208552BB9ED529077096966D670C354E4ABC9804F1746C08CA18217C32905E462E36CE3B" +
	"E39E772C180E86039B2783A2EC07A28FB5C55DF06F4C52C9DE2BCBF6955817183995497CEA956AE515D2261898FA0510" +
	"15728E5A8AAAC42DAD33170D04507A33A85521ABDF1CBA64ECFB850458DBEF0A8AEA71575D060C7DB3970F85A6E1E4C7" +
	"ABF5AE8CDB0933D71E8C94E04A25619DCEE3D2261AD2EE6BF12FFA06D98A0864D87602733EC86A64521F2B18177B200C" +
	"BBE117577A615D6C770988C0BAD946E208E24FA074E5AB3143DB5BFCE0FD108E4B82D120A93AD2CAFFFFFFFFFFFFFFFF"

// hapSRP is the group HAP pair-setup uses.
var hapSRP = newSRPGroup(rfc5054N3072, 5, sha512.New)

func newSRPGroup(nHex string, g int64, h func() hash.Hash) *srpGroup {
	n, ok := new(big.Int).SetString(nHex, 16)
	if !ok {
		panic("airplay: bad SRP prime")
	}
	return &srpGroup{N: n, g: big.NewInt(g), size: (n.BitLen() + 7) / 8, hash: h}
}

var errSRP = errors.New("airplay: SRP exchange failed")

func (grp *srpGroup) pad(x *big.Int) []byte {
	b := make([]byte, grp.size)
	return x.FillBytes(b)
}

func (grp *srpGroup) h(parts ...[]byte) []byte {
	h := grp.hash()
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}

func (grp *srpGroup) hInt(parts ...[]byte) *big.Int {
	return new(big.Int).SetBytes(grp.h(parts...))
}

// k is the SRP-6a multiplier H(N | PAD(g)).
func (grp *srpGroup) k() *big.Int { return grp.hInt(grp.pad(grp.N), grp.pad(grp.g)) }

// x is the private key derived from the password: H(salt | H(I ":" P)).
func (grp *srpGroup) x(salt, user, pass []byte) *big.Int {
	inner := grp.h(user, []byte(":"), pass)
	return grp.hInt(salt, inner)
}

// u is the scrambling parameter H(PAD(A) | PAD(B)).
func (grp *srpGroup) u(A, B *big.Int) *big.Int { return grp.hInt(grp.pad(A), grp.pad(B)) }

// srpClient is the client side of one SRP exchange.
type srpClient struct {
	grp        *srpGroup
	user, pass []byte
	a, A       *big.Int

	// Set by compute.
	S      *big.Int
	K      []byte // session key H(S)
	M1     []byte // client proof
	wantM2 []byte // expected server proof
}

// newSRPClient starts an exchange with the private value a (big-endian bytes).
func newSRPClient(grp *srpGroup, user, pass, a []byte) *srpClient {
	c := &srpClient{grp: grp, user: user, pass: pass, a: new(big.Int).SetBytes(a)}
	c.A = new(big.Int).Exp(grp.g, c.a, grp.N)
	return c
}

// newRandomSRPClient picks a 256-bit private value whose public value A has no leading zero
// byte, so that the padded and the stripped encodings of A (both appear in the proofs) are
// the same bytes on every implementation.
func newRandomSRPClient(grp *srpGroup, user, pass []byte, rnd io.Reader) (*srpClient, error) {
	a := make([]byte, 32)
	for range 64 {
		if _, err := io.ReadFull(rnd, a); err != nil {
			return nil, err
		}
		c := newSRPClient(grp, user, pass, a)
		if c.A.BitLen() > 8*(grp.size-1) {
			return c, nil
		}
	}
	return nil, errSRP
}

// publicA returns A padded to the group size.
func (c *srpClient) publicA() []byte { return c.grp.pad(c.A) }

// compute derives the premaster secret, the session key and both proofs from the server's
// salt and public value B.
func (c *srpClient) compute(salt, bBytes []byte) error {
	grp := c.grp
	if len(bBytes) == 0 || len(bBytes) > grp.size {
		return errSRP
	}
	B := new(big.Int).SetBytes(bBytes)
	if new(big.Int).Mod(B, grp.N).Sign() == 0 {
		return errSRP
	}
	u := grp.u(c.A, B)
	if u.Sign() == 0 {
		return errSRP
	}
	x := grp.x(salt, c.user, c.pass)
	// S = (B - k*g^x) ^ (a + u*x) mod N
	kgx := new(big.Int).Exp(grp.g, x, grp.N)
	kgx.Mul(kgx, grp.k())
	base := new(big.Int).Sub(B, kgx)
	base.Mod(base, grp.N)
	exp := new(big.Int).Mul(u, x)
	exp.Add(exp, c.a)
	c.S = new(big.Int).Exp(base, exp, grp.N)
	c.K = grp.h(c.S.Bytes())

	hn, hg := grp.h(grp.N.Bytes()), grp.h(grp.g.Bytes())
	for i := range hn {
		hn[i] ^= hg[i]
	}
	c.M1 = grp.h(hn, grp.h(c.user), salt, c.A.Bytes(), B.Bytes(), c.K)
	c.wantM2 = grp.h(grp.pad(c.A), c.M1, c.K)
	return nil
}

// verifyM2 checks the server's proof.
func (c *srpClient) verifyM2(m2 []byte) bool {
	return c.wantM2 != nil && hmac.Equal(m2, c.wantM2)
}
