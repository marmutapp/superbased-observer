package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/marmutapp/superbased-observer/internal/orgserver/auth"
)

type reqIDKey struct{}

// RequestID assigns each request a stable id (honouring an inbound
// X-Request-Id), echoes it in the response header, and stores it in context.
func RequestID() auth.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get("X-Request-Id")
			if id == "" {
				b := make([]byte, 8)
				_, _ = rand.Read(b)
				id = hex.EncodeToString(b)
			}
			w.Header().Set("X-Request-Id", id)
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), reqIDKey{}, id)))
		})
	}
}

// statusRecorder captures the status code for logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Logging logs one line per request at DEBUG (method/path/status/duration),
// elevating to WARN for 4xx and ERROR for 5xx.
func Logging(logger *slog.Logger) auth.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			level := slog.LevelDebug
			switch {
			case rec.status >= 500:
				level = slog.LevelError
			case rec.status >= 400:
				level = slog.LevelWarn
			}
			id, _ := r.Context().Value(reqIDKey{}).(string)
			logger.LogAttrs(
				r.Context(), level, "http",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.status),
				slog.Duration("dur", time.Since(start)),
				slog.String("request_id", id),
			)
		})
	}
}

// rateLimiter is a small per-client token bucket. It is intentionally
// dependency-free; for the M1 single-instance server this is sufficient. A
// multi-instance deployment would move this to a shared store (M5 hardening).
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64 // tokens per second
	burst   float64
	maxKeys int
	nowFn   func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// RateLimit returns a middleware allowing `rps` requests/second per client IP
// with the given burst. On exhaustion it returns 429 with a Retry-After.
func RateLimit(rps, burst float64) auth.Middleware {
	rl := &rateLimiter{
		buckets: make(map[string]*bucket),
		rate:    rps,
		burst:   burst,
		maxKeys: 10000,
		nowFn:   time.Now,
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if rps <= 0 {
				next.ServeHTTP(w, r)
				return
			}
			if !rl.allow(clientIP(r)) {
				w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(rps)))
				auth.WriteError(w, http.StatusTooManyRequests, "rate_limited", "too many requests")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func (rl *rateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := rl.nowFn()
	b, ok := rl.buckets[key]
	if !ok {
		if len(rl.buckets) >= rl.maxKeys {
			// Crude overflow guard: drop the whole table rather than grow
			// unbounded. Acceptable for a single-instance M1 server.
			rl.buckets = make(map[string]*bucket)
		}
		rl.buckets[key] = &bucket{tokens: rl.burst - 1, last: now}
		return true
	}
	elapsed := now.Sub(b.last).Seconds()
	b.tokens += elapsed * rl.rate
	if b.tokens > rl.burst {
		b.tokens = rl.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func retryAfterSeconds(rps float64) int {
	if rps <= 0 {
		return 1
	}
	s := int(1.0/rps + 0.999)
	if s < 1 {
		s = 1
	}
	return s
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
