// Package ratelimit implements per-key RPM and TPM token buckets, held in
// memory (single-node deployment).
//
// Semantics: a request is admitted when the RPM bucket holds at least one
// token (one is consumed) and the TPM bucket balance is positive. Actual
// token consumption is only known after the provider responds, so TPM is
// debited afterwards with provider-reported usage — the balance may go
// negative, blocking subsequent requests until refill catches up. The
// gateway never estimates token counts locally. A limit of 0 means
// unlimited.
package ratelimit

import (
	"math"
	"sync"
	"time"
)

// Clock abstracts time for tests.
type Clock func() time.Time

// Limiter tracks buckets for every key it has seen.
type Limiter struct {
	mu      sync.Mutex
	clock   Clock
	buckets map[int64]*keyBuckets
}

type keyBuckets struct {
	rpm bucket
	tpm bucket
}

type bucket struct {
	level       float64
	last        time.Time
	initialized bool
}

func (b *bucket) refill(now time.Time, capacity, ratePerSec float64) {
	if !b.initialized {
		b.level = capacity
		b.last = now
		b.initialized = true
		return
	}
	if dt := now.Sub(b.last).Seconds(); dt > 0 {
		b.level = math.Min(capacity, b.level+dt*ratePerSec)
		b.last = now
	}
}

// timeUntil returns how long until the level reaches target at the given
// refill rate.
func (b *bucket) timeUntil(target, ratePerSec float64) time.Duration {
	if b.level >= target || ratePerSec <= 0 {
		return 0
	}
	return time.Duration((target - b.level) / ratePerSec * float64(time.Second))
}

// New returns a Limiter. A nil clock means time.Now.
func New(clock Clock) *Limiter {
	if clock == nil {
		clock = time.Now
	}
	return &Limiter{clock: clock, buckets: make(map[int64]*keyBuckets)}
}

// Decision is the outcome of an admission check.
type Decision struct {
	OK         bool
	RetryAfter time.Duration // how long until admission would succeed
}

// Allow admits one request for the key under the given rpm/tpm limits.
func (l *Limiter) Allow(keyID int64, rpm, tpm int) Decision {
	if rpm <= 0 && tpm <= 0 {
		return Decision{OK: true}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock()
	kb := l.bucketsFor(keyID)

	ok := true
	var retry time.Duration
	if rpm > 0 {
		rate := float64(rpm) / 60
		kb.rpm.refill(now, float64(rpm), rate)
		if kb.rpm.level < 1 {
			ok = false
			retry = max(retry, kb.rpm.timeUntil(1, rate))
		}
	}
	if tpm > 0 {
		rate := float64(tpm) / 60
		kb.tpm.refill(now, float64(tpm), rate)
		if kb.tpm.level <= 0 {
			ok = false
			retry = max(retry, kb.tpm.timeUntil(1, rate))
		}
	}
	if !ok {
		return Decision{OK: false, RetryAfter: retry}
	}
	if rpm > 0 {
		kb.rpm.level--
	}
	return Decision{OK: true}
}

// DebitTokens subtracts provider-reported token usage from the key's TPM
// bucket after a request completes.
func (l *Limiter) DebitTokens(keyID int64, tpm, tokens int) {
	if tpm <= 0 || tokens <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	kb := l.bucketsFor(keyID)
	kb.tpm.refill(l.clock(), float64(tpm), float64(tpm)/60)
	kb.tpm.level -= float64(tokens)
}

func (l *Limiter) bucketsFor(keyID int64) *keyBuckets {
	kb, ok := l.buckets[keyID]
	if !ok {
		kb = &keyBuckets{}
		l.buckets[keyID] = kb
	}
	return kb
}
