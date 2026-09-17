package api

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRateLimiterBoundsAndConcurrency(t *testing.T) {
	now := time.Unix(0, 0)
	limiter := newRateLimiter(10, time.Minute, 1)
	var admitted atomic.Int64
	var workers sync.WaitGroup
	for range 100 {
		workers.Go(func() {
			if limiter.allow("first", now) == 0 {
				admitted.Add(1)
			}
		})
	}
	workers.Wait()
	if admitted.Load() != 10 {
		t.Fatalf("quota race admitted %d instead of 10", admitted.Load())
	}
	if limiter.allow("second", now) == 0 || len(limiter.clients) != 1 {
		t.Fatal("full table grew or evicted an active quota")
	}
	if limiter.allow("second", now.Add(time.Minute)) != 0 || len(limiter.clients) != 1 {
		t.Fatal("expired quota was not reclaimed")
	}
}

func TestRateLimiterStaggeredExpiryAndCapacityRecovery(t *testing.T) {
	start := time.Unix(0, 0)
	limiter := newRateLimiter(1, 2*time.Second, 3)
	for index, client := range []string{"first", "second", "third"} {
		if retry := limiter.allow(client, start.Add(time.Duration(index)*250*time.Millisecond)); retry != 0 {
			t.Fatalf("initial client %s denied: %v", client, retry)
		}
	}
	// The first entry expires at 2s. Cleanup frees its slot, but retains
	// the two later expirations and the newly admitted client's active quota.
	if retry := limiter.allow("fourth", start.Add(2*time.Second)); retry != 0 {
		t.Fatalf("cleanup did not free capacity: %v", retry)
	}
	// Later entries expire between sweeps. Their slots stay occupied until
	// the next cleanup, rather than triggering a scan at each expiration.
	for _, elapsed := range []time.Duration{2250 * time.Millisecond, 2500 * time.Millisecond, 2750 * time.Millisecond} {
		want := 3*time.Second - elapsed
		if retry := limiter.allow("fifth", start.Add(elapsed)); retry != want {
			t.Fatalf("at %v: retry=%v; want next cleanup in %v", elapsed, retry, want)
		}
	}
	if retry := limiter.allow("fifth", start.Add(3*time.Second)); retry != 0 {
		t.Fatalf("next sweep did not reclaim expired entries: %v", retry)
	}
	if retry := limiter.allow("fourth", start.Add(3*time.Second)); retry != time.Second {
		t.Fatalf("cleanup changed an active quota: retry=%v", retry)
	}
}

func TestRateLimiterResetsRequestedClientBeforeCleanup(t *testing.T) {
	start := time.Unix(0, 0)
	limiter := newRateLimiter(1, 1500*time.Millisecond, 1)
	if retry := limiter.allow("client", start); retry != 0 {
		t.Fatal("initial request denied")
	}
	// Trigger a sweep at 1s while this client's window is still active.
	if retry := limiter.allow("client", start.Add(time.Second)); retry != 500*time.Millisecond {
		t.Fatalf("active window reset early: retry=%v", retry)
	}
	// The client's quota resets at 1.5s even though cleanup is due at 2s
	// and the key table is full.
	now := start.Add(1500 * time.Millisecond)
	if retry := limiter.allow("client", now); retry != 0 {
		t.Fatalf("expired client had to wait for cleanup: %v", retry)
	}
	if retry := limiter.allow("client", now); retry != 1500*time.Millisecond {
		t.Fatalf("reset window admitted more than its quota: retry=%v", retry)
	}
	if retry := limiter.allow("other", now); retry != 500*time.Millisecond {
		t.Fatalf("full table should defer new clients until cleanup: retry=%v", retry)
	}
	if retry := limiter.allow("other", start.Add(2*time.Second)); retry != time.Second {
		t.Fatalf("cleanup evicted an active quota: retry=%v", retry)
	}
	if retry := limiter.allow("other", start.Add(3*time.Second)); retry != 0 {
		t.Fatalf("capacity was not recovered at expiry: retry=%v", retry)
	}
}
