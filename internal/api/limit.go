package api

import (
	"sync"
	"time"
)

const rateCleanupInterval = time.Second

type rateWindow struct {
	used    int
	expires time.Time
}

// rateLimiter uses per-client fixed windows with a bounded key table. When full,
// new clients wait for cleanup rather than evicting an active client's quota.
type rateLimiter struct {
	mu                   sync.Mutex
	clients              map[string]rateWindow
	requests, maxClients int
	window               time.Duration
	nextSweep            time.Time
}

func newRateLimiter(requests int, window time.Duration, maxClients int) *rateLimiter {
	return &rateLimiter{clients: make(map[string]rateWindow), requests: requests, window: window, maxClients: maxClients}
}

// allow returns zero on admission, or a positive retry delay.
func (limiter *rateLimiter) allow(key string, now time.Time) time.Duration {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	if !now.Before(limiter.nextSweep) {
		for client, window := range limiter.clients {
			if !now.Before(window.expires) {
				delete(limiter.clients, client)
			}
		}
		limiter.nextSweep = now.Add(rateCleanupInterval)
	}
	window, exists := limiter.clients[key]
	if !exists && len(limiter.clients) >= limiter.maxClients {
		return limiter.nextSweep.Sub(now)
	}
	if !exists || !now.Before(window.expires) {
		window = rateWindow{expires: now.Add(limiter.window)}
	}
	if window.used >= limiter.requests {
		return window.expires.Sub(now)
	}
	window.used++
	limiter.clients[key] = window
	return 0
}
