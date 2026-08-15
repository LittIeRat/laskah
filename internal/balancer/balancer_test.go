package balancer

import (
	"errors"
	"testing"
	"time"

	"laskah/internal/store"
)

func provider(id string, weight float64, priority int, enabled bool, models ...string) *store.Provider {
	return &store.Provider{
		ID:       id,
		Name:     id,
		Enabled:  enabled,
		Weight:   weight,
		Priority: priority,
		Models:   models,
		ModelMap: map[string]string{},
	}
}

func TestValidStrategy(t *testing.T) {
	for _, name := range Strategies {
		if !ValidStrategy(name) {
			t.Fatalf("%s 应为合法策略", name)
		}
	}
	if ValidStrategy("random-guess") {
		t.Fatalf("未知策略应判定为非法")
	}
	if New("random-guess", 0, 0).Strategy() != StrategyWeightedRandom {
		t.Fatalf("非法策略应回落到 weighted-random")
	}
}

func TestCandidatesFiltering(t *testing.T) {
	cooling := provider("cool", 1, 0, true, "gpt-4o")
	cooling.CooldownUntil = time.Now().Add(time.Minute)

	pool := []*store.Provider{
		provider("ok", 1, 0, true, "gpt-4o"),
		provider("off", 1, 0, false, "gpt-4o"),
		cooling,
		provider("other-model", 1, 0, true, "claude-3"),
		provider("bound-out", 1, 0, true, "gpt-4o"),
	}

	lb := New(StrategyPriority, time.Minute, 3)
	got := lb.Candidates(pool, Criteria{Model: "gpt-4o", ProviderIDs: []string{"ok"}})
	if len(got) != 1 || got[0].ID != "ok" {
		t.Fatalf("候选过滤错误: %#v", got)
	}

	all := lb.Candidates(pool, Criteria{Model: "gpt-4o"})
	if len(all) != 2 {
		t.Fatalf("应有 2 个候选（排除禁用/冷却/模型不匹配）, got %d", len(all))
	}
}

func TestOrderRespectsPriorityGroups(t *testing.T) {
	lb := New(StrategyPriority, time.Minute, 3)
	pool := []*store.Provider{
		provider("low", 5, 0, true),
		provider("high-a", 1, 10, true),
		provider("high-b", 9, 10, true),
	}
	ordered := lb.Order(pool, Criteria{})
	if len(ordered) != 3 {
		t.Fatalf("应返回 3 个, got %d", len(ordered))
	}
	if ordered[0].ID != "high-b" || ordered[1].ID != "high-a" || ordered[2].ID != "low" {
		t.Fatalf("排序错误: %s, %s, %s", ordered[0].ID, ordered[1].ID, ordered[2].ID)
	}
}

func TestRoundRobinRotates(t *testing.T) {
	lb := New(StrategyRoundRobin, time.Minute, 3)
	pool := []*store.Provider{provider("a", 1, 0, true), provider("b", 1, 0, true), provider("c", 1, 0, true)}
	seen := []string{}
	for i := 0; i < 3; i++ {
		seen = append(seen, lb.Pick(pool, Criteria{}).ID)
	}
	if seen[0] == seen[1] && seen[1] == seen[2] {
		t.Fatalf("round-robin 应轮转, got %#v", seen)
	}
	if lb.Pick(pool, Criteria{}).ID != seen[0] {
		t.Fatalf("第 4 次应回到起点, got %#v", seen)
	}
}

func TestLeastLatencyAndLeastInflight(t *testing.T) {
	fast := provider("fast", 1, 0, true)
	fast.Stats.Success = 10
	fast.Stats.AvgLatencyMS = 100
	slow := provider("slow", 1, 0, true)
	slow.Stats.Success = 10
	slow.Stats.AvgLatencyMS = 900

	lb := New(StrategyLeastLatency, time.Minute, 3)
	if lb.Pick([]*store.Provider{slow, fast}, Criteria{}).ID != "fast" {
		t.Fatalf("least-latency 应优先低延迟")
	}

	busy := provider("busy", 1, 0, true)
	busy.Inflight = 7
	idle := provider("idle", 1, 0, true)
	if lb.Pick([]*store.Provider{busy, idle}, Criteria{Strategy: StrategyLeastInflight}).ID != "idle" {
		t.Fatalf("least-inflight 应优先空闲提供商")
	}
}

func TestWeightedRandomCoversAllAndFavorsWeight(t *testing.T) {
	lb := New(StrategyWeightedRandom, time.Minute, 3)
	heavy := provider("heavy", 99, 0, true)
	light := provider("light", 1, 0, true)
	pool := []*store.Provider{heavy, light}

	heavyFirst := 0
	for i := 0; i < 200; i++ {
		ordered := lb.Order(pool, Criteria{})
		if len(ordered) != 2 {
			t.Fatalf("加权洗牌应保留全部候选, got %d", len(ordered))
		}
		if ordered[0].ID == "heavy" {
			heavyFirst++
		}
	}
	if heavyFirst < 150 {
		t.Fatalf("高权重应更常被选中, got %d/200", heavyFirst)
	}
}

func TestPickReturnsNilWhenNoCandidate(t *testing.T) {
	lb := New(StrategyWeightedRandom, time.Minute, 3)
	if lb.Pick([]*store.Provider{provider("off", 1, 0, false)}, Criteria{}) != nil {
		t.Fatalf("无候选时应返回 nil")
	}
}

func TestReportSuccessAndFailureCooldown(t *testing.T) {
	lb := New(StrategyWeightedRandom, 10*time.Second, 3)
	target := provider("p", 1, 0, true)

	lb.ReportSuccess(target, 200*time.Millisecond, Usage{PromptTokens: 5, CompletionTokens: 7, TotalTokens: 12})
	if target.Stats.Requests != 1 || target.Stats.Success != 1 || target.Stats.TotalTokens != 12 {
		t.Fatalf("成功统计错误: %#v", target.Stats)
	}
	if target.Stats.AvgLatencyMS != 200 {
		t.Fatalf("首次延迟应为 200ms, got %d", target.Stats.AvgLatencyMS)
	}
	lb.ReportSuccess(target, 400*time.Millisecond, Usage{})
	if target.Stats.AvgLatencyMS != 300 {
		t.Fatalf("延迟移动平均错误, got %d", target.Stats.AvgLatencyMS)
	}

	for i := 0; i < 2; i++ {
		lb.ReportFailure(target, errors.New("boom"))
	}
	if !target.CooldownUntil.IsZero() {
		t.Fatalf("未达到阈值不应冷却")
	}
	lb.ReportFailure(target, errors.New("boom"))
	if target.CooldownUntil.IsZero() {
		t.Fatalf("达到阈值应进入冷却")
	}
	if target.Stats.LastError == nil || target.Stats.LastError.Message != "boom" {
		t.Fatalf("最近错误未记录: %#v", target.Stats.LastError)
	}

	firstCooldown := target.CooldownUntil
	lb.ReportFailure(target, errors.New("boom"))
	if !target.CooldownUntil.After(firstCooldown) {
		t.Fatalf("连续失败冷却时间应递增")
	}

	lb.ReportSuccess(target, time.Millisecond, Usage{})
	if !target.CooldownUntil.IsZero() || target.ConsecutiveFailures != 0 {
		t.Fatalf("成功后应解除冷却")
	}
}

func TestRateLimiterSlidingWindow(t *testing.T) {
	limiter := NewRateLimiter()
	now := time.Now()

	if decision := limiter.Allow("k", 0, now); !decision.Allowed || decision.Remaining != -1 {
		t.Fatalf("limit<=0 应无限制: %#v", decision)
	}

	for i := 0; i < 3; i++ {
		if decision := limiter.Allow("k", 3, now); !decision.Allowed {
			t.Fatalf("第 %d 次应放行", i+1)
		}
	}
	blocked := limiter.Allow("k", 3, now)
	if blocked.Allowed {
		t.Fatalf("超出配额应拒绝")
	}
	if blocked.RetryAfter <= 0 || blocked.RetryAfter > time.Minute {
		t.Fatalf("RetryAfter 应在 1 分钟内: %v", blocked.RetryAfter)
	}

	if !limiter.Allow("other", 3, now).Allowed {
		t.Fatalf("不同密钥应独立计数")
	}
	if !limiter.Allow("k", 3, now.Add(61*time.Second)).Allowed {
		t.Fatalf("窗口滑出后应放行")
	}

	limiter.Prune(now.Add(10 * time.Minute))
	if !limiter.Allow("k", 1, now.Add(10*time.Minute)).Allowed {
		t.Fatalf("Prune 后应重新放行")
	}
}

// TestRateLimiterPeekDoesNotConsumeQuota 锁定 Peek 的语义：只判定、不记账。
//
// 账号选择要先试探再决定，如果试探本身就扣配额，被跳过的账号会被自己的探测打满。
func TestRateLimiterPeekDoesNotConsumeQuota(t *testing.T) {
	limiter := NewRateLimiter()
	now := time.Now()

	if decision := limiter.Peek("acct", 0, now); !decision.Allowed || decision.Remaining != -1 {
		t.Fatalf("limit<=0 应无限制: %#v", decision)
	}

	for i := 0; i < 5; i++ {
		if decision := limiter.Peek("acct", 2, now); !decision.Allowed || decision.Remaining != 2 {
			t.Fatalf("Peek 不应消耗配额: %#v", decision)
		}
	}

	limiter.Allow("acct", 2, now)
	limiter.Allow("acct", 2, now)
	if decision := limiter.Peek("acct", 2, now); decision.Allowed {
		t.Fatalf("配额用尽后 Peek 应拒绝: %#v", decision)
	}
	if decision := limiter.Peek("acct", 2, now.Add(61*time.Second)); !decision.Allowed {
		t.Fatalf("窗口滑出后 Peek 应放行: %#v", decision)
	}
}

// TestAccountBucketNamespacing 保证账号级与密钥级限流不会撞 key。
func TestAccountBucketNamespacing(t *testing.T) {
	if AccountBucket("abc") == "abc" {
		t.Fatalf("账号级限流标识必须与密钥 ID 区分开")
	}
	limiter := NewRateLimiter()
	now := time.Now()
	limiter.Allow(AccountBucket("abc"), 1, now)
	if !limiter.Allow("abc", 1, now).Allowed {
		t.Fatalf("同名密钥不应被账号级计数影响")
	}
}
