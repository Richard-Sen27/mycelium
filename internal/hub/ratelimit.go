package hub

import (
	"net"
	"sync"
	"time"
)

// Enrol rate limit per remote IP.
const (
	enrolRatePerMinute = 10
	enrolBurst         = 10
)

// rateLimiter is an in-memory token bucket per key (remote IP). Idle buckets
// are pruned so the map cannot grow without bound.
type rateLimiter struct {
	mu        sync.Mutex
	rate      float64 // tokens per second
	burst     float64
	buckets   map[string]*bucket
	lastPrune time.Time
	now       func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(perMinute, burst int) *rateLimiter {
	return &rateLimiter{
		rate: float64(perMinute) / 60, burst: float64(burst),
		buckets: map[string]*bucket{}, now: time.Now,
	}
}

// allow takes one token for key and reports whether one was available.
func (l *rateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if now.Sub(l.lastPrune) > time.Minute {
		l.prune(now)
	}
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// prune drops buckets that have refilled completely; they carry no state.
func (l *rateLimiter) prune(now time.Time) {
	l.lastPrune = now
	for k, b := range l.buckets {
		if b.tokens+now.Sub(b.last).Seconds()*l.rate >= l.burst {
			delete(l.buckets, k)
		}
	}
}

// remoteIP strips the port from a peer address.
func remoteIP(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}
