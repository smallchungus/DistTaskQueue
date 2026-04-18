package api

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// tokenBucket is a simple per-key (IP) token bucket rate limiter with a
// background goroutine-free design: tokens refill lazily on each Allow call.
//
// refill: one token per `interval`, up to `burst` max.
// Example: interval=1s, burst=5 -> 1 request/sec sustained, burst 5.
type tokenBucket struct {
	mu       sync.Mutex
	buckets  map[string]*bucketState
	interval time.Duration
	burst    float64
}

type bucketState struct {
	tokens   float64
	lastSeen time.Time
}

func newTokenBucket(interval time.Duration, burst int) *tokenBucket {
	return &tokenBucket{
		buckets:  map[string]*bucketState{},
		interval: interval,
		burst:    float64(burst),
	}
}

func (t *tokenBucket) Allow(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	b, ok := t.buckets[key]
	if !ok {
		t.buckets[key] = &bucketState{tokens: t.burst - 1, lastSeen: now}
		return true
	}
	elapsed := now.Sub(b.lastSeen).Seconds()
	refill := elapsed / t.interval.Seconds()
	b.tokens += refill
	if b.tokens > t.burst {
		b.tokens = t.burst
	}
	b.lastSeen = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// rateLimit wraps a handler with a per-IP token bucket. Returns 429 on bucket empty.
func (t *tokenBucket) rateLimit(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if !t.Allow(ip) {
			w.Header().Set("Retry-After", "1")
			writeJSONErr(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		next(w, r)
	}
}

func clientIP(r *http.Request) string {
	if v := r.Header.Get("CF-Connecting-IP"); v != "" {
		return v
	}
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		if i := strings.IndexByte(v, ','); i >= 0 {
			return strings.TrimSpace(v[:i])
		}
		return strings.TrimSpace(v)
	}
	if i := strings.LastIndexByte(r.RemoteAddr, ':'); i >= 0 {
		return r.RemoteAddr[:i]
	}
	return r.RemoteAddr
}
