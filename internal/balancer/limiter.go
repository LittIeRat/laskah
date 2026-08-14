package balancer

import (
	"sync"
	"time"
)

// RateLimiter 是按分钟滑动窗口的计数限流器。
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string][]time.Time
}

// NewRateLimiter 创建限流器。
func NewRateLimiter() *RateLimiter {
	return &RateLimiter{buckets: map[string][]time.Time{}}
}

// Decision 是一次限流判定结果。
type Decision struct {
	Allowed    bool
	Remaining  int
	RetryAfter time.Duration
}

// Allow 判定指定标识在窗口内是否还有配额，允许时会记录一次调用。
func (r *RateLimiter) Allow(id string, limitPerMin int, now time.Time) Decision {
	if limitPerMin <= 0 {
		return Decision{Allowed: true, Remaining: -1}
	}
	if now.IsZero() {
		now = time.Now()
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	windowStart := now.Add(-time.Minute)
	hits := r.buckets[id]
	kept := hits[:0]
	for _, hit := range hits {
		if hit.After(windowStart) {
			kept = append(kept, hit)
		}
	}

	if len(kept) >= limitPerMin {
		r.buckets[id] = kept
		retryAfter := kept[0].Add(time.Minute).Sub(now)
		if retryAfter < 0 {
			retryAfter = 0
		}
		return Decision{Allowed: false, Remaining: 0, RetryAfter: retryAfter}
	}

	kept = append(kept, now)
	r.buckets[id] = kept
	return Decision{Allowed: true, Remaining: limitPerMin - len(kept)}
}

// Prune 清理过期窗口，避免内存无限增长。
func (r *RateLimiter) Prune(now time.Time) {
	if now.IsZero() {
		now = time.Now()
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	windowStart := now.Add(-time.Minute)
	for id, hits := range r.buckets {
		kept := hits[:0]
		for _, hit := range hits {
			if hit.After(windowStart) {
				kept = append(kept, hit)
			}
		}
		if len(kept) == 0 {
			delete(r.buckets, id)
			continue
		}
		r.buckets[id] = kept
	}
}
