package web

import (
	"sync"
	"time"
)

// limiter counts events for each key in a fixed time window. The sign-in
// form uses it to slow down password guessing. The counts are in memory,
// thus each replica has its own limit.
type limiter struct {
	max    int
	window time.Duration

	mu      sync.Mutex
	entries map[string]*limitEntry
	swept   time.Time
}

type limitEntry struct {
	count int
	start time.Time
}

func newLimiter(max int, window time.Duration) *limiter {
	return &limiter{max: max, window: window, entries: map[string]*limitEntry{}}
}

// allow records an event for key and tells if the key is below the limit.
func (l *limiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.swept) > l.window {
		for k, e := range l.entries {
			if now.Sub(e.start) > l.window {
				delete(l.entries, k)
			}
		}
		l.swept = now
	}
	e, ok := l.entries[key]
	if !ok || now.Sub(e.start) > l.window {
		e = &limitEntry{start: now}
		l.entries[key] = e
	}
	e.count++
	return e.count <= l.max
}

// reset removes the count of key, for example after a correct password.
func (l *limiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}
