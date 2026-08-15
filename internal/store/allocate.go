package store

import (
	"sort"
	"time"
)

// reindex 重建内存索引，使鉴权与归属查询保持 O(1)。
//
// 每次结构性变更（增删账号/分组/API/密钥）后调用一次，
// 换取热路径上不再线性扫描全部密钥与上游 API。
func (d *Data) reindex() {
	d.reindexAdmins()
	d.keyByHash = make(map[string]*APIKey, len(d.Keys))
	d.accountByID = make(map[string]*Account, len(d.Accounts))
	d.groupByID = make(map[string]*Group, len(d.Groups))
	d.providerByID = make(map[string]*Provider, len(d.Providers))
	d.accountProvs = make(map[string][]*Provider, len(d.Accounts))
	d.accountKeyLoad = make(map[string]int, len(d.Accounts))

	for _, group := range d.Groups {
		d.groupByID[group.ID] = group
	}
	for _, account := range d.Accounts {
		d.accountByID[account.ID] = account
	}
	for _, provider := range d.Providers {
		d.providerByID[provider.ID] = provider
		if provider.AccountID != "" {
			d.accountProvs[provider.AccountID] = append(d.accountProvs[provider.AccountID], provider)
		}
	}
	for _, key := range d.Keys {
		if key.KeyHash != "" {
			d.keyByHash[key.KeyHash] = key
		}
		if key.AccountID != "" {
			d.accountKeyLoad[key.AccountID]++
		}
	}
}

// Reindex 对外暴露索引重建，供仓库加载后调用。
func (d *Data) Reindex() {
	d.reindex()
}

// UsableAccounts 返回当前可以承接流量的账号，可按分组过滤。
//
// 除账号自身状态外还要求所属分组处于启用状态：禁用分组等于整池下线。
func (d *Data) UsableAccounts(groupID string) []*Account {
	return d.UsableAccountsForModel(groupID, "")
}

// UsableAccountsForModel 在可用账号中进一步筛出能承接指定模型的账号。
//
// model 为空表示不限模型。指定模型时，账号名下必须至少有一个启用、未冷却
// 且声明支持该模型的上游 API，否则把请求交给它必然以“没有可用上游”失败。
func (d *Data) UsableAccountsForModel(groupID, model string) []*Account {
	result := []*Account{}
	for _, account := range d.Accounts {
		if groupID != "" && account.GroupID != groupID {
			continue
		}
		if !d.GroupEnabled(account.GroupID) {
			continue
		}
		if account.Usable() && d.healthyProvidersForModel(account.ID, model) > 0 {
			result = append(result, account)
		}
	}
	return result
}

// HealthyAccountProviders 统计账号名下当前真正可用的上游 API 数量。
//
// 只计入启用中且不在冷却期的条目：一个账号即使挂了 5 个 Key，
// 若全部在冷却中也不应该继续被分配流量，否则请求必然失败。
func (d *Data) HealthyAccountProviders(accountID string) int {
	return d.healthyProvidersForModel(accountID, "")
}

// healthyProvidersForModel 统计账号名下当前能立刻承接该模型的上游 API 数量。
//
// model 为空时退化为“不限模型”，因此同一个实现可服务通用分配与按模型分配两条路径。
func (d *Data) healthyProvidersForModel(accountID, model string) int {
	now := time.Now()
	count := 0
	for _, provider := range d.AccountProviders(accountID) {
		if !provider.Enabled || provider.CooldownUntil.After(now) {
			continue
		}
		if model != "" && !provider.SupportsModel(model) {
			continue
		}
		count++
	}
	return count
}

// AccountServesModel 判断账号当前是否能承接该模型的请求。
func (d *Data) AccountServesModel(accountID, model string) bool {
	return d.healthyProvidersForModel(accountID, model) > 0
}

// AssignAccount 为网关密钥挑选账号：已有且仍可用则保持，否则按负载与余额重新分配。
//
// 密钥限定分组时只在该分组内挑选；未限定时在全部账号中挑选。
func (d *Data) AssignAccount(key *APIKey) *Account {
	return d.AssignAccountForModel(key, "")
}

// AssignAccountForModel 按请求的模型为网关密钥挑选账号。
//
// model 非空时，粘性绑定与重新分配都要求账号名下有支持该模型的可用上游：
// 请求 claude-3-opus 就不能落到只挂了 gpt 系列的账号上，否则必然失败。
// 换号只影响本次请求的落点，key.AccountID 仍指向常驻绑定，
// 避免一次冷门模型请求把密钥永久迁走、打乱既有的均摊结果。
func (d *Data) AssignAccountForModel(key *APIKey, model string) *Account {
	if key == nil {
		return nil
	}
	if key.AccountID != "" {
		current := d.FindAccount(key.AccountID)
		sameGroup := current != nil && (key.GroupID == "" || current.GroupID == key.GroupID)
		sticky := sameGroup &&
			d.GroupEnabled(current.GroupID) &&
			current.Usable() &&
			d.healthyProvidersForModel(current.ID, model) > 0
		if sticky {
			return current
		}
		// 绑定账号只是不支持这个模型时，不解绑：换一个能接的账号跑完这次请求，
		// 常驻绑定留给它本来擅长的模型，避免密钥在模型间来回漂移。
		if model != "" && sameGroup &&
			d.GroupEnabled(current.GroupID) &&
			current.Usable() &&
			d.HealthyAccountProviders(current.ID) > 0 {
			return d.pickAccountForModel(key.GroupID, model)
		}
		d.detachKeyAccount(key)
	}

	pool := d.UsableAccountsForModel(key.GroupID, model)
	if len(pool) == 0 {
		return nil
	}

	// 排序目标是把密钥摊平到各账号，同时优先用余额更充足、可用 Key 更多的账号：
	// 1) 每个可用上游承载的密钥数最少 —— 避免把 20 个密钥压在只剩 1 个 Key 的账号上；
	// 2) 绑定密钥数最少；
	// 3) 无限额度账号优先于有限额度账号（不会中途耗尽）；
	// 4) 余额更高；
	// 5) 创建更早，保证结果稳定可预期。
	sortAccountPool(d, pool, model)

	chosen := pool[0]
	key.AccountID = chosen.ID
	key.UpdatedAt = time.Now().UTC()
	if d.accountKeyLoad != nil {
		d.accountKeyLoad[chosen.ID]++
	}
	return chosen
}

// pickAccountForModel 只为本次请求挑一个能接该模型的账号，不改动密钥的常驻绑定。
func (d *Data) pickAccountForModel(groupID, model string) *Account {
	pool := d.UsableAccountsForModel(groupID, model)
	if len(pool) == 0 {
		return nil
	}
	sortAccountPool(d, pool, model)
	return pool[0]
}

// sortAccountPool 把候选账号按分配优先级排序。
//
// 排序目标是把密钥摊平到各账号，同时优先用余额更充足、可用 Key 更多的账号：
// 1) 每个可用上游承载的密钥数最少 —— 避免把 20 个密钥压在只剩 1 个 Key 的账号上；
// 2) 绑定密钥数最少；
// 3) 无限额度账号优先于有限额度账号（不会中途耗尽）；
// 4) 余额更高；
// 5) 创建更早，保证结果稳定可预期。
func sortAccountPool(d *Data, pool []*Account, model string) {
	sort.SliceStable(pool, func(i, j int) bool {
		left, right := pool[i], pool[j]
		leftShare, rightShare := d.accountKeyPressureForModel(left.ID, model), d.accountKeyPressureForModel(right.ID, model)
		if leftShare != rightShare {
			return leftShare < rightShare
		}
		leftLoad, rightLoad := d.KeysUsingAccount(left.ID), d.KeysUsingAccount(right.ID)
		if leftLoad != rightLoad {
			return leftLoad < rightLoad
		}
		if left.Unlimited() != right.Unlimited() {
			return left.Unlimited()
		}
		if left.Balance != right.Balance {
			return left.Balance > right.Balance
		}
		return left.CreatedAt.Before(right.CreatedAt)
	})
}

// accountKeyPressure 是“每个可用上游 API 承载的网关密钥数”。
//
// 用比值而不是绝对密钥数排序，才能在不同账号可用 Key 数量不等时公平分配。
func (d *Data) accountKeyPressure(accountID string) float64 {
	return d.accountKeyPressureForModel(accountID, "")
}

// accountKeyPressureForModel 只按能承接该模型的上游数量计算压力。
//
// 账号挂了 5 个 Key 但只有 1 个支持目标模型时，它对这次请求的实际承载力就是 1，
// 按总数算会高估容量、把请求挤到一个窄口上。
func (d *Data) accountKeyPressureForModel(accountID, model string) float64 {
	healthy := d.healthyProvidersForModel(accountID, model)
	if healthy <= 0 {
		return float64(1 << 20)
	}
	return float64(d.KeysUsingAccount(accountID)) / float64(healthy)
}

func (d *Data) detachKeyAccount(key *APIKey) {
	if key.AccountID == "" {
		return
	}
	if d.accountKeyLoad != nil && d.accountKeyLoad[key.AccountID] > 0 {
		d.accountKeyLoad[key.AccountID]--
	}
	key.AccountID = ""
}

// GroupSummary 汇总单个分组的余额、消耗与容量信息。
func (d *Data) GroupSummary(groupID string) map[string]any {
	var (
		balance     float64
		usedAmount  float64
		totalAmount float64
		tokens      int64
		requests    int64
		accountNum  int
		usableNum   int
		apiCount    int
	)
	unlimitedNum := 0
	for _, account := range d.Accounts {
		if account.GroupID != groupID {
			continue
		}
		accountNum++
		if account.Unlimited() {
			unlimitedNum++
		} else {
			balance += account.Balance
			usedAmount += account.UsedAmount
			totalAmount += account.TotalAmount
		}
		tokens += account.Stats.TotalTokens
		requests += account.Stats.Requests
		apiCount += d.CountAccountKeys(account.ID)
		if account.Usable() {
			usableNum++
		}
	}

	removedUsed := 0.0
	var removedTokens int64
	for _, item := range d.RemovedAccounts {
		if item.GroupID != groupID {
			continue
		}
		removedUsed += item.UsedAmount
		removedTokens += item.Tokens
	}

	keyCount := 0
	for _, key := range d.Keys {
		if key.GroupID == groupID {
			keyCount++
		}
	}

	return map[string]any{
		"balance":       round4(balance),
		"usedAmount":    round4(usedAmount),
		"totalAmount":   round4(totalAmount),
		"lifetimeUsed":  round4(usedAmount + removedUsed),
		"tokens":        tokens,
		"lifetimeToken": tokens + removedTokens,
		"requests":      requests,
		"accounts":      accountNum,
		"usable":        usableNum,
		"unlimited":     unlimitedNum,
		"apiCount":      apiCount,
		"keys":          keyCount,
		"currency":      "USD",
		"enabled":       d.GroupEnabled(groupID),
	}
}

// AccountTotals 汇总全部账号的余额、消耗与用量，用于 /dashboard 展示。
func (d *Data) AccountTotals() map[string]any {
	var (
		balance     float64
		usedAmount  float64
		totalAmount float64
		enabled     int
		exhausted   int
		apiCount    int
		totalTokens int64
		requests    int64
	)
	unlimited := 0
	for _, account := range d.Accounts {
		// 无限额度账号不参与金额汇总，否则 0 余额会把总额拉低造成误解。
		if account.Unlimited() {
			unlimited++
		} else {
			balance += account.Balance
			usedAmount += account.UsedAmount
			totalAmount += account.TotalAmount
		}
		if account.Enabled {
			enabled++
		}
		if account.Exhausted() {
			exhausted++
		}
		apiCount += d.CountAccountKeys(account.ID)
		totalTokens += account.Stats.TotalTokens
		requests += account.Stats.Requests
	}

	var (
		keyTokens   int64
		keyRequests int64
		assigned    int
	)
	for _, key := range d.Keys {
		keyTokens += key.Stats.TotalTokens
		keyRequests += key.Stats.Requests
		if key.AccountID != "" {
			assigned++
		}
	}

	var providerTokens int64
	for _, provider := range d.Providers {
		providerTokens += provider.Stats.TotalTokens
	}

	removedUsed := 0.0
	var removedTokens int64
	for _, item := range d.RemovedAccounts {
		removedUsed += item.UsedAmount
		removedTokens += item.Tokens
	}

	groups := make([]any, 0, len(d.Groups))
	for _, group := range d.Groups {
		groups = append(groups, PublicGroup(group, d.GroupSummary(group.ID)))
	}

	return map[string]any{
		"groups": groups,
		"accounts": map[string]any{
			"total":     len(d.Accounts),
			"enabled":   enabled,
			"exhausted": exhausted,
			"unlimited": unlimited,
			"removed":   len(d.RemovedAccounts),
			"apiCount":  apiCount,
		},
		"balance": map[string]any{
			"total":       round4(balance),
			"currency":    "USD",
			"usedAmount":  round4(usedAmount),
			"totalAmount": round4(totalAmount),
			"removedUsed": round4(removedUsed),
			"lifetime":    round4(usedAmount + removedUsed),
		},
		"tokens": map[string]any{
			"accounts":  totalTokens,
			"keys":      keyTokens,
			"providers": providerTokens,
			"lifetime":  totalTokens + removedTokens,
		},
		"requests": map[string]any{
			"accounts": requests,
			"keys":     keyRequests,
		},
		"keys": map[string]any{
			"total":    len(d.Keys),
			"assigned": assigned,
		},
	}
}

func round4(value float64) float64 {
	scaled := value * 10000
	if scaled >= 0 {
		scaled += 0.5
	} else {
		scaled -= 0.5
	}
	return float64(int64(scaled)) / 10000
}
