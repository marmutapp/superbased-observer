package remoteauth

import (
	"sync"
	"time"
)

// RateLimiter is a per-key token-bucket limiter for auth attempts (plan §4.8 —
// code-server-class throttling of /api/remote/pair and any credential check).
// A bucket refills at PerMinute tokens/minute up to a Burst capacity. Safe for
// concurrent use.
type RateLimiter struct {
	mu        sync.Mutex
	perMinute float64
	burst     float64
	now       func() time.Time
	buckets   map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewRateLimiter builds a limiter allowing perMinute attempts/min with a burst
// capacity (defaults to perMinute when <=0). perMinute<=0 disables limiting
// (Allow always true) — used when the operator sets rate_limit_per_min=0.
func NewRateLimiter(perMinute, burst int, now func() time.Time) *RateLimiter {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	b := float64(burst)
	if b <= 0 {
		b = float64(perMinute)
	}
	return &RateLimiter{
		perMinute: float64(perMinute),
		burst:     b,
		now:       now,
		buckets:   map[string]*bucket{},
	}
}

// Allow reports whether an attempt for key is permitted right now, consuming a
// token when it is. A disabled limiter (perMinute<=0) always allows.
func (r *RateLimiter) Allow(key string) bool {
	if r.perMinute <= 0 {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	bk, ok := r.buckets[key]
	if !ok {
		bk = &bucket{tokens: r.burst, last: now}
		r.buckets[key] = bk
	}
	// Refill proportional to elapsed time.
	elapsedMin := now.Sub(bk.last).Minutes()
	if elapsedMin > 0 {
		bk.tokens += elapsedMin * r.perMinute
		if bk.tokens > r.burst {
			bk.tokens = r.burst
		}
		bk.last = now
	}
	if bk.tokens < 1 {
		return false
	}
	bk.tokens--
	return true
}

// Reset clears a key's bucket (e.g. after a successful pairing so a legitimate
// device isn't throttled by prior failed attempts).
func (r *RateLimiter) Reset(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.buckets, key)
}
