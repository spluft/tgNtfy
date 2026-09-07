// Package limit implements the per-service ingest rate limiter: a sliding 1-second
// window (burst, cap 30) plus a sliding 60-second window (sustained, cap 100).
package limit

import (
	"sync"
	"time"
)

// window is a fixed-duration sliding window tracking recent timestamps in a ring buffer.
type window struct {
	size  time.Duration
	cap   int
	times []time.Time
	cur   int // write cursor (ring buffer)
}

func newWindow(size time.Duration, cap int) *window {
	return &window{size: size, cap: cap, times: make([]time.Time, cap)}
}

// allow records `now` and reports whether the request fits within the window by counting
// buffered timestamps still inside the window over the last `size` duration.
func (w *window) allow(now time.Time) bool {
	if len(w.times) < w.cap {
		w.times = append(w.times, now)
		return true
	}
	// Count how many buffered times fall inside the window (from newest to oldest).
	count := 0
	bounds := now.Add(-w.size)
	for i := w.cap - 1; i >= 0; i-- {
		t := w.times[(w.cur+i)%w.cap]
		if t.IsZero() || t.Before(bounds) {
			continue
		}
		count++
	}
	if count >= w.cap {
		return false
	}
	w.times[w.cur] = now
	w.cur = (w.cur + 1) % w.cap
	return true
}

// Limiter holds the two sliding windows for a single service.
type Limiter struct {
	burst *window // 1s window, cap 30
	rate  *window // 60s window, cap 100
}

// New returns a Limiter with the A-7 default caps.
func New() *Limiter {
	return &Limiter{
		burst: newWindow(time.Second, 30),
		rate:  newWindow(time.Minute, 100),
	}
}

// Allow reports whether a new ingest for this service fits the 30/s AND 100/min caps.
func (l *Limiter) Allow(now time.Time) bool {
	if !l.burst.allow(now) {
		return false
	}
	return l.rate.allow(now)
}

// Registry keeps one Limiter per service id, created lazily. Limits are keyed by
// service id (not token), so token rotation does not reset counters.
type Registry struct {
	mu   sync.Mutex
	seen map[string]*Limiter
}

// NewRegistry returns an empty service registry.
func NewRegistry() *Registry { return &Registry{seen: map[string]*Limiter{}} }

// AllowForService returns true if service fits both rate windows at now.
func (r *Registry) AllowForService(service string, now time.Time) bool {
	r.mu.Lock()
	l, ok := r.seen[service]
	if !ok {
		l = New()
		r.seen[service] = l
	}
	r.mu.Unlock()
	return l.Allow(now)
}
