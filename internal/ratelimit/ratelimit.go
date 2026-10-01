// Package ratelimit provides per-client-IP inbound rate limiting as HTTP
// middleware. This protects us from a noisy or misbehaving caller
// monopolizing the service; it has nothing to do with the vendor side —
// see internal/fxvendor/resilience for that.
package ratelimit

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// entry pairs a token bucket limiter with the last time it was used, so
// the background sweep knows which entries are stale.
type entry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// Hooks lets callers observe rejections for a later metrics phase
// without this package depending on Prometheus directly.
type Hooks struct {
	OnRejected func()
}

// Limiter rate-limits requests per client IP using a token bucket per IP
// (golang.org/x/time/rate). rps is the sustained rate; burst is how many
// requests can be made back-to-back before the sustained rate kicks in —
// this absorbs legitimate bursty traffic (e.g. a checkout page firing a
// handful of calls at once) without punishing it like a flat request cap
// would.
type Limiter struct {
	rps   rate.Limit
	burst int
	hooks Hooks

	mu      sync.Mutex
	entries map[string]*entry

	// idleTTL is how long an IP's limiter can sit unused before the sweep
	// evicts it. Without this, every distinct client IP we ever see stays
	// in the map forever — in a long-running process fielding traffic
	// from many different IPs (NATed office networks, mobile carriers,
	// bots) that map grows without bound and is a slow memory leak.
	idleTTL time.Duration

	stopSweep chan struct{}
}

// defaultIdleTTL is how long an IP's limiter can sit unused before the
// background sweep evicts it.
const defaultIdleTTL = 10 * time.Minute

// New creates a Limiter and starts its background cleanup sweep. Call
// Stop when shutting down to stop the sweep goroutine.
func New(rps float64, burst int, hooks Hooks) *Limiter {
	return newWithTTL(rps, burst, defaultIdleTTL, hooks)
}

// newWithTTL is used by tests to exercise the sweep on a short interval
// without waiting on the production default. idleTTL is fixed at
// construction and never mutated afterward, so it's safe to read from
// the sweep goroutine without holding mu.
func newWithTTL(rps float64, burst int, idleTTL time.Duration, hooks Hooks) *Limiter {
	l := &Limiter{
		rps:       rate.Limit(rps),
		burst:     burst,
		hooks:     hooks,
		entries:   make(map[string]*entry),
		idleTTL:   idleTTL,
		stopSweep: make(chan struct{}),
	}
	go l.sweepLoop()
	return l
}

func (l *Limiter) sweepLoop() {
	ticker := time.NewTicker(l.idleTTL / 2)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			l.sweep()
		case <-l.stopSweep:
			return
		}
	}
}

func (l *Limiter) sweep() {
	cutoff := time.Now().Add(-l.idleTTL)
	l.mu.Lock()
	defer l.mu.Unlock()
	for ip, e := range l.entries {
		if e.lastSeen.Before(cutoff) {
			delete(l.entries, ip)
		}
	}
}

// Stop halts the background cleanup sweep.
func (l *Limiter) Stop() {
	close(l.stopSweep)
}

func (l *Limiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	e, ok := l.entries[ip]
	if !ok {
		e = &entry{limiter: rate.NewLimiter(l.rps, l.burst)}
		l.entries[ip] = e
	}
	e.lastSeen = time.Now()
	return e.limiter.Allow()
}

// clientIP extracts the caller's IP from the request, preferring the
// first entry in X-Forwarded-For (set by a load balancer/ingress in
// front of us) and falling back to RemoteAddr for direct connections.
// This is a deliberate trust boundary: X-Forwarded-For is only safe to
// trust when we know we sit behind a proxy that sets it and strips any
// caller-supplied value first — true in a typical k8s ingress setup, but
// worth flagging as an assumption.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		first, _, _ := strings.Cut(xff, ",")
		return strings.TrimSpace(first)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Skip reports whether path should bypass rate limiting. Health,
// readiness, and metrics endpoints are infrastructure/operational traffic
// (k8s probes, Prometheus scrapes) rather than caller traffic, and
// typically fire frequently and predictably — rate limiting them would
// risk the kubelet itself getting 429'd on a liveness check, which would
// be us rate-limiting our own health reporting.
func Skip(path string) bool {
	switch path {
	case "/health", "/ready", "/metrics":
		return true
	default:
		return false
	}
}

// Middleware returns inbound-rate-limiting HTTP middleware. onLimited,
// if non-nil, is called whenever a request is rejected (for a metrics
// hook in a later phase); writeTooManyRequests must write the response.
func (l *Limiter) Middleware(writeTooManyRequests func(w http.ResponseWriter, retryAfter time.Duration)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if Skip(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}

			ip := clientIP(r)
			if !l.allow(ip) {
				if l.hooks.OnRejected != nil {
					l.hooks.OnRejected()
				}
				// We don't track exact reservation time per rejection, so
				// we advertise a Retry-After equal to roughly one token's
				// worth of refill time — a reasonable, simple estimate for
				// "try again shortly" rather than a precise wait time.
				retryAfter := time.Second
				if l.rps > 0 {
					retryAfter = time.Duration(float64(time.Second) / float64(l.rps))
				}
				writeTooManyRequests(w, retryAfter)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
