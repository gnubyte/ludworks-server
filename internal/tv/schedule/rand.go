package schedule

import (
	"crypto/sha256"
	"encoding/binary"
	"math/bits"
)

// Seed64 derives the 64-bit PRNG seed for a channel's textual shuffle seed:
// the first 8 bytes of SHA-256(seed), read big-endian (API.md §7.1 step 4).
func Seed64(seed string) uint64 {
	sum := sha256.Sum256([]byte(seed))
	return binary.BigEndian.Uint64(sum[:8])
}

// SplitMix64 is the PRNG mandated by API.md §7.1. It is tiny, fast, has a
// full 2^64 period and — crucially — is trivially reproducible in any
// language, so the slate is a pure function of (seed, cycle).
//
// The zero value is a valid generator seeded with 0.
type SplitMix64 struct {
	state uint64
}

// NewSplitMix64 returns a generator whose first output is mix(seed+γ).
func NewSplitMix64(seed uint64) *SplitMix64 {
	return &SplitMix64{state: seed}
}

// Uint64 returns the next pseudo-random value (reference algorithm by
// Sebastiano Vigna, constants from the original public-domain C code).
func (r *SplitMix64) Uint64() uint64 {
	r.state += 0x9E3779B97F4A7C15
	z := r.state
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

// Uint64n returns a uniformly distributed value in [0, n) using Lemire's
// multiply-shift method with rejection, which is exactly unbiased (unlike a
// plain modulo). n must be > 0; n == 0 returns 0.
func (r *SplitMix64) Uint64n(n uint64) uint64 {
	if n == 0 {
		return 0
	}
	hi, lo := bits.Mul64(r.Uint64(), n)
	if lo < n {
		threshold := -n % n // (2^64 - n) mod n
		for lo < threshold {
			hi, lo = bits.Mul64(r.Uint64(), n)
		}
	}
	return hi
}

// Shuffle permutes s in place with the Fisher–Yates (Durstenfeld) algorithm
// driven by SplitMix64(seed): for i from len-1 down to 1, swap s[i] with
// s[j], j = Uint64n(i+1). Identical inputs always yield identical output.
func Shuffle[T any](s []T, seed uint64) {
	rng := NewSplitMix64(seed)
	for i := len(s) - 1; i > 0; i-- {
		j := int(rng.Uint64n(uint64(i) + 1))
		s[i], s[j] = s[j], s[i]
	}
}
