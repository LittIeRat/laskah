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
	return r.decide(id, limitPerMin, now, true)
}

// Peek 只判定配额是否还有剩余，不记录本次调用。
//
// 用于「挑选账号」这类需要先试探再决定的场景：如果试探本身就记一次命中，
// 被跳过的账号会白白消耗配额，最后所有账号都被自己的探测打满。
func (r *RateLimiter) Peek(id string, limitPerMin int, now time.Time) Decision {
	return r.decide(id, limitPerMin, now, false)
}

func (r *RateLimiter) decide(id string, limitPerMin int, now time.Time, record bool) Decision {
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

	if !record {
		r.buckets[id] = kept
		return Decision{Allowed: true, Remaining: limitPerMin - len(kept)}
	}
	kept = append(kept, now)
	r.buckets[id] = kept
	return Decision{Allowed: true, Remaining: limitPerMin - len(kept)}
}

// AccountBucket 是账号级频率限制在限流器里的标识。
//
// 与密钥级限流共用一个限流器，靠前缀区分命名空间，避免 ID 撞车。
func AccountBucket(accountID string) string {
	return "account:" + accountID
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
