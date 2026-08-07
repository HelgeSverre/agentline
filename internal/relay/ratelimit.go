package relay

import (
	"sync"
	"time"
)

type limitWindow struct {
	start time.Time
	count int
}

type limiter struct {
	mu     sync.Mutex
	window time.Duration
	limit  int
	now    func() time.Time
	keys   map[string]limitWindow
}

func newLimiter(window time.Duration, limit int, now func() time.Time) *limiter {
	return &limiter{window: window, limit: limit, now: now, keys: make(map[string]limitWindow)}
}

func (l *limiter) allow(key string) bool {
	if l.limit <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for key, candidate := range l.keys {
		if now.Sub(candidate.start) >= l.window {
			delete(l.keys, key)
		}
	}
	w := l.keys[key]
	if w.start.IsZero() || now.Sub(w.start) >= l.window {
		w = limitWindow{start: now}
	}
	if w.count >= l.limit {
		return false
	}
	w.count++
	l.keys[key] = w
	return true
}
