package security

import (
	"sync"
	"time"
)

// Throttle 限制同一来源的失败尝试次数，用于抵御口令爆破。
type Throttle struct {
	mu       sync.Mutex
	entries  map[string]*throttleEntry
	limit    int
	window   time.Duration
	lockout  time.Duration
	maxEntry int
}

type throttleEntry struct {
	failures   int
	firstAt    time.Time
	lockedTill time.Time
}

// NewThrottle 创建失败计数器。
func NewThrottle(limit int, window, lockout time.Duration) *Throttle {
	if limit < 1 {
		limit = 5
	}
	if window <= 0 {
		window = 10 * time.Minute
	}
	if lockout <= 0 {
		lockout = 15 * time.Minute
	}
	return &Throttle{
		entries:  map[string]*throttleEntry{},
		limit:    limit,
		window:   window,
		lockout:  lockout,
		maxEntry: 4096,
	}
}

// Check 返回该来源是否仍被允许尝试，以及剩余锁定时长。
func (t *Throttle) Check(key string) (bool, time.Duration) {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	entry, ok := t.entries[key]
	if !ok {
		return true, 0
	}
	if now.Before(entry.lockedTill) {
		return false, entry.lockedTill.Sub(now)
	}
	if now.Sub(entry.firstAt) > t.window {
		delete(t.entries, key)
	}
	return true, 0
}

// Fail 记录一次失败，达到阈值后锁定该来源。
func (t *Throttle) Fail(key string) {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked(now)

	entry, ok := t.entries[key]
	if !ok || now.Sub(entry.firstAt) > t.window {
		entry = &throttleEntry{firstAt: now}
		t.entries[key] = entry
	}
	entry.failures++
	if entry.failures >= t.limit {
		entry.lockedTill = now.Add(t.lockout)
		entry.failures = 0
		entry.firstAt = now
	}
}

// Reset 登录成功后清除失败计数。
func (t *Throttle) Reset(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, key)
}

// Prune 清理过期条目，避免内存无界增长。
func (t *Throttle) Prune() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked(time.Now())
}

func (t *Throttle) pruneLocked(now time.Time) {
	for key, entry := range t.entries {
		if now.After(entry.lockedTill) && now.Sub(entry.firstAt) > t.window {
			delete(t.entries, key)
		}
	}
	if len(t.entries) <= t.maxEntry {
		return
	}
	for key := range t.entries {
		delete(t.entries, key)
		if len(t.entries) <= t.maxEntry {
			break
		}
	}
}
