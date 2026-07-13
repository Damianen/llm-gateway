package ratelimit

import (
	"testing"
	"time"
)

type fakeClock struct {
	t time.Time
}

func (f *fakeClock) now() time.Time          { return f.t }
func (f *fakeClock) advance(d time.Duration) { f.t = f.t.Add(d) }
func newFakeClock() *fakeClock               { return &fakeClock{t: time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)} }

func TestRPMBucket(t *testing.T) {
	clock := newFakeClock()
	l := New(clock.now)

	// rpm=2: two requests pass, the third is rejected.
	for i := range 2 {
		if dec := l.Allow(1, 2, 0); !dec.OK {
			t.Fatalf("request %d should be admitted", i)
		}
	}
	dec := l.Allow(1, 2, 0)
	if dec.OK {
		t.Fatal("third request should be rejected")
	}
	// One token refills in 30s at rpm=2.
	if dec.RetryAfter <= 0 || dec.RetryAfter > 30*time.Second {
		t.Errorf("RetryAfter = %v, want (0, 30s]", dec.RetryAfter)
	}

	clock.advance(29 * time.Second)
	if dec := l.Allow(1, 2, 0); dec.OK {
		t.Error("still rejected before a full token refills")
	}
	clock.advance(2 * time.Second)
	if dec := l.Allow(1, 2, 0); !dec.OK {
		t.Error("should be admitted after refill")
	}
}

func TestTPMBucketDebitsActualUsage(t *testing.T) {
	clock := newFakeClock()
	l := New(clock.now)

	// tpm=100: admission only requires a positive balance.
	if dec := l.Allow(1, 0, 100); !dec.OK {
		t.Fatal("first request should be admitted")
	}
	// The request turned out to use 250 tokens: balance goes to -150.
	l.DebitTokens(1, 100, 250)

	dec := l.Allow(1, 0, 100)
	if dec.OK {
		t.Fatal("negative balance must block")
	}
	// Refill rate is 100/60 tokens/s; reaching level 1 from -150 takes
	// 151 * 0.6 = 90.6s.
	if dec.RetryAfter < 90*time.Second || dec.RetryAfter > 91*time.Second {
		t.Errorf("RetryAfter = %v, want ~90.6s", dec.RetryAfter)
	}

	clock.advance(90 * time.Second)
	if dec := l.Allow(1, 0, 100); dec.OK {
		t.Error("still blocked just before balance turns positive")
	}
	clock.advance(2 * time.Second)
	if dec := l.Allow(1, 0, 100); !dec.OK {
		t.Error("should be admitted once balance is positive")
	}
}

func TestUnlimited(t *testing.T) {
	l := New(newFakeClock().now)
	for range 1000 {
		if dec := l.Allow(7, 0, 0); !dec.OK {
			t.Fatal("0/0 limits must never reject")
		}
	}
	l.DebitTokens(7, 0, 1<<30) // no TPM limit: debit is a no-op
	if dec := l.Allow(7, 0, 0); !dec.OK {
		t.Fatal("still unlimited after debit")
	}
}

func TestKeysAreIsolated(t *testing.T) {
	clock := newFakeClock()
	l := New(clock.now)
	if dec := l.Allow(1, 1, 0); !dec.OK {
		t.Fatal("key 1 first request")
	}
	if dec := l.Allow(1, 1, 0); dec.OK {
		t.Fatal("key 1 should be exhausted")
	}
	if dec := l.Allow(2, 1, 0); !dec.OK {
		t.Error("key 2 must have its own bucket")
	}
}

func TestRPMDoesNotConsumeWhenTPMBlocks(t *testing.T) {
	clock := newFakeClock()
	l := New(clock.now)
	l.Allow(1, 5, 100)
	l.DebitTokens(1, 100, 500) // TPM deep in the red

	// Several rejected attempts must not drain the RPM bucket.
	for range 3 {
		if dec := l.Allow(1, 5, 100); dec.OK {
			t.Fatal("TPM should block")
		}
	}
	// Clear the TPM debt: RPM must still have 4 of its 5 tokens.
	clock.advance(10 * time.Minute)
	admitted := 0
	for range 10 {
		if dec := l.Allow(1, 5, 100); dec.OK {
			admitted++
			l.DebitTokens(1, 100, 1)
		}
	}
	if admitted != 5 {
		t.Errorf("admitted %d after refill, want full rpm capacity 5", admitted)
	}
}
