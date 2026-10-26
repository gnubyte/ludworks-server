package auth

import "context"

// RunCleanup exposes the periodic cleanup job to the external tests.
func RunCleanup(m *Module, ctx context.Context) error { return m.cleanup(ctx) }

// RunSetupTokenRotation exposes the periodic setup-token rotation job to the external tests.
func RunSetupTokenRotation(m *Module, ctx context.Context) error { return m.rotateSetupToken(ctx) }

// HoldHashSlots occupies every password-hashing slot until the returned function is called,
// so tests can make concurrent password checks overlap deterministically: requests admitted
// past the brute-force checks block in Verify while refused ones return at once.
func HoldHashSlots() (release func()) {
	for i := 0; i < cap(hashSem); i++ {
		hashSem <- struct{}{}
	}
	return func() {
		for i := 0; i < cap(hashSem); i++ {
			<-hashSem
		}
	}
}
