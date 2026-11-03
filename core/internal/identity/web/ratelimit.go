package web

import (
	"sync"
	"time"
)

// Limiter is a fixed-window counter per key (client IP). It lives in memory:
// limits are per instance, which is enough for a single binary; the
// per-account lockout in the database covers distributed guessing.
type Limiter struct {
	limit  int
	window time.Duration
	now    func() time.Time

	mu      sync.Mutex
	windows map[string]*limitWindow
	swept   time.Time
}

type limitWindow struct {
	start    time.Time
	count    int
	reported bool
}

// NewLimiter allows limit events per key per window.
func NewLimiter(limit int, window time.Duration) *Limiter {
	return &Limiter{limit: limit, window: window, now: time.Now, windows: map[string]*limitWindow{}}
}

// Allow counts an event for key. When the limit is exceeded it returns
// false with the time until the window ends; first is true for the first
// rejection in the window (to report it once).
func (l *Limiter) Allow(key string) (ok bool, retryAfter time.Duration, first bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if now.Sub(l.swept) > l.window {
		for k, w := range l.windows {
			if now.Sub(w.start) >= l.window {
				delete(l.windows, k)
			}
		}
		l.swept = now
	}
	w, found := l.windows[key]
	if !found || now.Sub(w.start) >= l.window {
		w = &limitWindow{start: now}
		l.windows[key] = w
	}
	if w.count < l.limit {
		w.count++
		return true, 0, false
	}
	first = !w.reported
	w.reported = true
	return false, w.start.Add(l.window).Sub(now), first
}
