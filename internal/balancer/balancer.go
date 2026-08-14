package balancer

import (
	"math/rand"
	"sort"
	"sync"
	"time"

	"laskah/internal/store"
)

// 支持的负载均衡策略。
const (
	StrategyWeightedRandom = "weighted-random"
	StrategyRoundRobin     = "round-robin"
	StrategyLeastLatency   = "least-latency"
	StrategyLeastInflight  = "least-inflight"
	StrategyPriority       = "priority"
)

// Strategies 列出全部合法策略名。
var Strategies = []string{
	StrategyWeightedRandom,
	StrategyRoundRobin,
	StrategyLeastLatency,
	StrategyLeastInflight,
	StrategyPriority,
}

// ValidStrategy 判断策略名是否受支持。
func ValidStrategy(name string) bool {
	for _, item := range Strategies {
		if item == name {
			return true
		}
	}
	return false
}

// Criteria 描述一次选路的过滤条件。
type Criteria struct {
	Model       string
	ProviderIDs []string
	Strategy    string
	Now         time.Time
}

// Balancer 负责候选筛选、排序与健康状态维护。
type Balancer struct {
	mu               sync.Mutex
	defaultStrategy  string
	cooldown         time.Duration
	failureThreshold int
	cursor           uint64
	rng              *rand.Rand
}

// New 创建负载均衡器。
func New(strategy string, cooldown time.Duration, failureThreshold int) *Balancer {
	if !ValidStrategy(strategy) {
		strategy = StrategyWeightedRandom
	}
	if cooldown <= 0 {
		cooldown = 30 * time.Second
	}
	if failureThreshold <= 0 {
		failureThreshold = 3
	}
	return &Balancer{
		defaultStrategy:  strategy,
		cooldown:         cooldown,
		failureThreshold: failureThreshold,
		rng:              rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// SetStrategy 更新默认策略。
func (b *Balancer) SetStrategy(strategy string) {
	if !ValidStrategy(strategy) {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.defaultStrategy = strategy
}

// Strategy 返回当前默认策略。
func (b *Balancer) Strategy() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.defaultStrategy
}

// Candidates 过滤出可用的提供商。
func (b *Balancer) Candidates(providers []*store.Provider, criteria Criteria) []*store.Provider {
	now := criteria.Now
	if now.IsZero() {
		now = time.Now()
	}
	allowed := map[string]bool{}
	for _, id := range criteria.ProviderIDs {
		allowed[id] = true
	}

	result := make([]*store.Provider, 0, len(providers))
	for _, provider := range providers {
		if !provider.Enabled {
			continue
		}
		if provider.CooldownUntil.After(now) {
			continue
		}
		if len(allowed) > 0 && !allowed[provider.ID] {
			continue
		}
		if !provider.SupportsModel(criteria.Model) {
			continue
		}
		result = append(result, provider)
	}
	return result
}

// Order 返回按优先级与策略排序后的候选列表。
func (b *Balancer) Order(providers []*store.Provider, criteria Criteria) []*store.Provider {
	pool := b.Candidates(providers, criteria)
	if len(pool) <= 1 {
		return pool
	}

	strategy := criteria.Strategy
	if !ValidStrategy(strategy) {
		strategy = b.Strategy()
	}

	groups := map[int][]*store.Provider{}
	priorities := []int{}
	for _, provider := range pool {
		if _, seen := groups[provider.Priority]; !seen {
			priorities = append(priorities, provider.Priority)
		}
		groups[provider.Priority] = append(groups[provider.Priority], provider)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(priorities)))

	ordered := make([]*store.Provider, 0, len(pool))
	for _, priority := range priorities {
		ordered = append(ordered, b.sortGroup(groups[priority], strategy)...)
	}
	return ordered
}

// Pick 返回排序后的首个候选，没有则返回 nil。
func (b *Balancer) Pick(providers []*store.Provider, criteria Criteria) *store.Provider {
	ordered := b.Order(providers, criteria)
	if len(ordered) == 0 {
		return nil
	}
	return ordered[0]
}

func (b *Balancer) sortGroup(group []*store.Provider, strategy string) []*store.Provider {
	list := make([]*store.Provider, len(group))
	copy(list, group)

	switch strategy {
	case StrategyRoundRobin:
		b.mu.Lock()
		offset := int(b.cursor % uint64(len(list)))
		b.cursor++
		b.mu.Unlock()
		return append(list[offset:], list[:offset]...)
	case StrategyLeastInflight:
		sort.SliceStable(list, func(i, j int) bool {
			if list[i].Inflight != list[j].Inflight {
				return list[i].Inflight < list[j].Inflight
			}
			return list[i].Weight > list[j].Weight
		})
		return list
	case StrategyLeastLatency:
		sort.SliceStable(list, func(i, j int) bool {
			return latencyScore(list[i]) < latencyScore(list[j])
		})
		return list
	case StrategyPriority:
		sort.SliceStable(list, func(i, j int) bool {
			return list[i].Weight > list[j].Weight
		})
		return list
	default:
		return b.weightedShuffle(list)
	}
}

func latencyScore(provider *store.Provider) float64 {
	if provider.Stats.Success == 0 {
		return 0
	}
	weight := provider.Weight
	if weight <= 0 {
		weight = 0.001
	}
	return float64(provider.Stats.AvgLatencyMS) / weight
}

func (b *Balancer) weightedShuffle(list []*store.Provider) []*store.Provider {
	pool := make([]*store.Provider, len(list))
	copy(pool, list)
	result := make([]*store.Provider, 0, len(pool))

	for len(pool) > 0 {
		total := 0.0
		for _, provider := range pool {
			total += provider.Weight
		}
		b.mu.Lock()
		target := b.rng.Float64() * total
		b.mu.Unlock()

		chosen := len(pool) - 1
		for index, provider := range pool {
			target -= provider.Weight
			if target <= 0 {
				chosen = index
				break
			}
		}
		result = append(result, pool[chosen])
		pool = append(pool[:chosen], pool[chosen+1:]...)
	}
	return result
}

// Usage 表示一次调用消耗的 token。
type Usage struct {
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
}

// ReportSuccess 记录成功调用并解除冷却。
func (b *Balancer) ReportSuccess(provider *store.Provider, latency time.Duration, usage Usage) {
	now := time.Now().UTC()
	stats := &provider.Stats
	stats.Requests++
	stats.Success++
	latencyMS := latency.Milliseconds()
	if stats.Success == 1 {
		stats.AvgLatencyMS = latencyMS
	} else {
		stats.AvgLatencyMS += (latencyMS - stats.AvgLatencyMS) / stats.Success
	}
	stats.PromptTokens += usage.PromptTokens
	stats.CompletionTokens += usage.CompletionTokens
	stats.TotalTokens += usage.TotalTokens
	stats.LastUsedAt = &now
	stats.LastError = nil

	provider.ConsecutiveFailures = 0
	provider.CooldownUntil = time.Time{}
}

// ReportFailure 记录失败调用，必要时进入递增冷却。
func (b *Balancer) ReportFailure(provider *store.Provider, err error) {
	now := time.Now().UTC()
	message := "unknown"
	if err != nil {
		message = err.Error()
	}

	stats := &provider.Stats
	stats.Requests++
	stats.Failure++
	stats.LastUsedAt = &now
	stats.LastError = &store.LastError{Message: message, At: now}

	provider.ConsecutiveFailures++
	if provider.ConsecutiveFailures >= b.failureThreshold {
		factor := provider.ConsecutiveFailures - b.failureThreshold + 1
		if factor > 6 {
			factor = 6
		}
		provider.CooldownUntil = time.Now().Add(b.cooldown * time.Duration(factor))
	}
}
