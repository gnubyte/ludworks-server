package httpx

import (
	"math"
	"sync"
	"time"
)

// KeyedLimiter is a token-bucket rate limiter keyed by an arbitrary string (principal, IP,
// account name...). Memory is bounded: idle (full) buckets are swept, and if the table still
// exceeds MaxKeys it is reset (fail-open for the limiter, never for authentication).
type KeyedLimiter struct {
	rate    float64 // tokens per second
	burst   float64
	maxKeys int
	now     func() time.Time

	mu        sync.Mutex
	buckets   map[string]*bucket
	lastSweep time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewKeyedLimiter allows rps requests per second per key with the given burst.
func NewKeyedLimiter(rps float64, burst int) *KeyedLimiter {
	if rps <= 0 {
		rps = 1
	}
	if burst < 1 {
		burst = 1
	}
	return &KeyedLimiter{rate: rps, burst: float64(burst), maxKeys: 100_000, now: time.Now, buckets: map[string]*bucket{}}
}

// Allow takes one token for key. When denied it returns the time until a token is available.
func (l *KeyedLimiter) Allow(key string) (bool, time.Duration) { return l.AllowN(key, 1) }

// AllowN takes n tokens for key.
func (l *KeyedLimiter) AllowN(key string, n float64) (bool, time.Duration) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.maybeSweep(now)
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	} else if el := now.Sub(b.last).Seconds(); el > 0 {
		b.tokens = math.Min(l.burst, b.tokens+el*l.rate)
		b.last = now
	}
	if b.tokens >= n {
		b.tokens -= n
		return true, 0
	}
	wait := time.Duration((n - b.tokens) / l.rate * float64(time.Second))
	return false, max(wait, time.Millisecond)
}

func (l *KeyedLimiter) maybeSweep(now time.Time) {
	since := now.Sub(l.lastSweep)
	if since < time.Minute && !(len(l.buckets) > l.maxKeys && since >= time.Second) {
		return
	}
	l.lastSweep = now
	full := time.Duration(l.burst / l.rate * float64(time.Second))
	for k, b := range l.buckets {
		if now.Sub(b.last) >= full {
			delete(l.buckets, k) // would be full again: identical to a fresh bucket
		}
	}
	if len(l.buckets) > l.maxKeys {
		l.buckets = map[string]*bucket{}
	}
}

// Len returns the number of tracked keys (for tests/metrics).
func (l *KeyedLimiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// Lockout implements exponential lockout after repeated failures: the first Free failures
// are free, then each further failure locks the key for Base*2^n (n = failures-Free), capped
// at Max. State is forgotten Forget after the last failure, or on Success.
// Defaults (NewLockout): 5 free failures, 2^n seconds, max 15 minutes, forget after 24h.
//
// Check/Failure/Success are separate critical sections, so they only enforce the limits for
// attempts that do not overlap. Callers that verify a guessable secret use Begin instead: it
// decides and reserves under one lock, counting attempts still in flight as if they had
// already failed, so a simultaneous burst cannot get more verifications than a sequence.
type Lockout struct {
	Free   int
	Base   time.Duration
	Max    time.Duration
	Forget time.Duration

	now     func() time.Time
	maxKeys int

	mu        sync.Mutex
	state     map[string]*lockState
	lastSweep time.Time
}

type lockState struct {
	failures    int
	inflight    int // attempts admitted by Begin that have not finished yet
	lockedUntil time.Time
	lastFailure time.Time
}

// LockoutRetryInFlight is the Retry-After for attempts refused because earlier attempts on
// the same key are still being verified and could engage the lock.
const LockoutRetryInFlight = time.Second

// NewLockout returns a Lockout with the contract defaults.
func NewLockout() *Lockout {
	return &Lockout{Free: 5, Base: time.Second, Max: 15 * time.Minute, Forget: 24 * time.Hour,
		now: time.Now, maxKeys: 200_000, state: map[string]*lockState{}}
}

// LockoutDelay is the lock duration after the given number of consecutive failures.
func LockoutDelay(failures, free int, base, maxDelay time.Duration) time.Duration {
	n := failures - free
	if n <= 0 {
		return 0
	}
	if n >= 32 {
		return maxDelay
	}
	d := base << uint(n)
	if d <= 0 || d > maxDelay {
		return maxDelay
	}
	return d
}

// current returns key's state, dropping it when it has been forgotten. Call with l.mu held.
func (l *Lockout) current(key string, now time.Time) *lockState {
	s, ok := l.state[key]
	if !ok {
		return nil
	}
	if s.inflight == 0 && (s.failures == 0 || now.Sub(s.lastFailure) >= l.Forget) {
		delete(l.state, key)
		return nil
	}
	if s.failures > 0 && now.Sub(s.lastFailure) >= l.Forget {
		s.failures, s.lockedUntil = 0, time.Time{}
	}
	return s
}

// Check returns how long key remains locked (0 = not locked).
func (l *Lockout) Check(key string) time.Duration {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.current(key, now)
	if s == nil {
		return 0
	}
	if d := s.lockedUntil.Sub(now); d > 0 {
		return d
	}
	return 0
}

// Failure records a failed attempt and returns the resulting lock duration (0 while free).
func (l *Lockout) Failure(key string) time.Duration {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(now)
	return l.failLocked(key, now)
}

func (l *Lockout) failLocked(key string, now time.Time) time.Duration {
	s := l.current(key, now)
	if s == nil {
		s = &lockState{}
		l.state[key] = s
	}
	s.failures++
	s.lastFailure = now
	d := LockoutDelay(s.failures, l.Free, l.Base, l.Max)
	if d > 0 {
		s.lockedUntil = now.Add(d)
	}
	return d
}

// Success clears key's failure history. Attempts still in flight stay reserved.
func (l *Lockout) Success(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.succeedLocked(key)
}

func (l *Lockout) succeedLocked(key string) {
	s, ok := l.state[key]
	if !ok {
		return
	}
	if s.inflight == 0 {
		delete(l.state, key)
		return
	}
	s.failures, s.lockedUntil, s.lastFailure = 0, time.Time{}, time.Time{}
}

// Failures returns the current consecutive failure count for key.
func (l *Lockout) Failures(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if s, ok := l.state[key]; ok {
		return s.failures
	}
	return 0
}

// Attempt is an in-flight attempt admitted by Begin. Exactly one of Fail, Succeed or Release
// takes effect (later calls are no-ops), so callers can defer Release and report the outcome
// on the paths that have one.
type Attempt struct {
	l    *Lockout
	key  string
	once sync.Once
}

// Begin admits one attempt for key, or refuses it with the time to wait. An attempt is
// refused while key is locked, and while the failures recorded so far plus the attempts
// still in flight already reach the point where a failure engages the lock (each in-flight
// attempt counts as a failure until it reports otherwise). Without concurrency this is
// exactly Check; with it, at most Free+1 attempts are verified before the lock engages,
// as if they had arrived one after another.
func (l *Lockout) Begin(key string) (*Attempt, time.Duration) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(now)
	s := l.current(key, now)
	if s != nil {
		if d := s.lockedUntil.Sub(now); d > 0 {
			return nil, d
		}
		if s.inflight > 0 && LockoutDelay(s.failures+s.inflight, l.Free, l.Base, l.Max) > 0 {
			return nil, LockoutRetryInFlight
		}
	} else {
		s = &lockState{}
		l.state[key] = s
	}
	s.inflight++
	return &Attempt{l: l, key: key}, 0
}

func (a *Attempt) finish(f func(now time.Time)) {
	if a == nil {
		return
	}
	a.once.Do(func() {
		now := a.l.now()
		a.l.mu.Lock()
		defer a.l.mu.Unlock()
		if s, ok := a.l.state[a.key]; ok && s.inflight > 0 {
			s.inflight--
		}
		f(now)
		// Drop a state that holds nothing any more (no failures, nothing in flight).
		if s, ok := a.l.state[a.key]; ok && s.inflight == 0 && s.failures == 0 {
			delete(a.l.state, a.key)
		}
	})
}

// Fail records the attempt as a failure and returns the resulting lock duration.
func (a *Attempt) Fail() time.Duration {
	var d time.Duration
	a.finish(func(now time.Time) { d = a.l.failLocked(a.key, now) })
	return d
}

// Succeed clears the key's failure history (like Lockout.Success).
func (a *Attempt) Succeed() { a.finish(func(time.Time) { a.l.succeedLocked(a.key) }) }

// Release ends the attempt without an outcome (e.g. it stopped before the secret was
// checked, or failed for a reason that is not a wrong guess).
func (a *Attempt) Release() { a.finish(func(time.Time) {}) }

func (l *Lockout) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < time.Minute && len(l.state) <= l.maxKeys {
		return
	}
	l.lastSweep = now
	for k := range l.state {
		l.current(k, now) // drops forgotten states
	}
	if len(l.state) > l.maxKeys {
		// Keep locked keys and attempts in flight (security relevant), drop the rest.
		for k, s := range l.state {
			if s.inflight == 0 && !s.lockedUntil.After(now) {
				delete(l.state, k)
			}
		}
	}
}

// SetClock replaces the time source (tests).
func (l *KeyedLimiter) SetClock(now func() time.Time) {
	l.mu.Lock()
	l.now = now
	l.mu.Unlock()
}

// SetClock replaces the time source (tests).
func (l *Lockout) SetClock(now func() time.Time) {
	l.mu.Lock()
	l.now = now
	l.mu.Unlock()
}
