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
	return d.UsableAccountsGated(groupID, model, nil)
}

// AccountGate 是分配前的额外准入判定，返回 false 表示这次请求不要用该账号。
//
// 账号级频率限制走这条路：限流器由网关持有，store 不该依赖它，
// 因此把判定以函数形式注入，账号选择逻辑仍然只有一份。
type AccountGate func(*Account) bool

// UsableAccountsGated 在可用账号基础上再应用一层准入判定。
//
// gate 为 nil 时等价于不做额外过滤。
func (d *Data) UsableAccountsGated(groupID, model string, gate AccountGate) []*Account {
	result := []*Account{}
	for _, account := range d.Accounts {
		if groupID != "" && account.GroupID != groupID {
			continue
		}
		if !d.GroupEnabled(account.GroupID) {
			continue
		}
		if !account.Usable() || d.healthyProvidersForModel(account.ID, model) <= 0 {
			continue
		}
		if gate != nil && !gate(account) {
			continue
		}
		result = append(result, account)
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
	explicit, loose := d.providerCountsForModel(accountID, model)
	return explicit + loose
}

// explicitProvidersForModel 统计账号名下“明确声明过该模型”的可用上游数量。
//
// 与 healthyProvidersForModel 的差别是不含模型列表留空的“什么都收”上游：
// 那类上游只是没设限制，并不代表真的有这个模型。
func (d *Data) explicitProvidersForModel(accountID, model string) int {
	explicit, _ := d.providerCountsForModel(accountID, model)
	return explicit
}

// providerCountsForModel 一次遍历同时给出两种口径的可用上游数量。
//
// explicit 是明确声明该模型的上游数；loose 是模型列表留空、按“不限”接收的上游数。
// model 为空时全部计入 loose——此时没有模型维度的偏好可言。
func (d *Data) providerCountsForModel(accountID, model string) (explicit int, loose int) {
	now := time.Now()
	for _, provider := range d.AccountProviders(accountID) {
		if !provider.Enabled || provider.CooldownUntil.After(now) {
			continue
		}
		if model == "" {
			loose++
			continue
		}
		switch {
		case provider.ExplicitlySupportsModel(model):
			explicit++
		case provider.SupportsModel(model):
			loose++
		}
	}
	return explicit, loose
}

// AccountServesModel 判断账号当前是否能承接该模型的请求。
func (d *Data) AccountServesModel(accountID, model string) bool {
	return d.healthyProvidersForModel(accountID, model) > 0
}

// AccountDeclaresModel 判断账号是否明确声明支持该模型。
func (d *Data) AccountDeclaresModel(accountID, model string) bool {
	return d.explicitProvidersForModel(accountID, model) > 0
}

// AssignAccount 为网关密钥挑选账号：已有且仍可用则保持，否则按负载与余额重新分配。
//
// 密钥限定分组时只在该分组内挑选；未限定时在全部账号中挑选。
func (d *Data) AssignAccount(key *APIKey) *Account {
	return d.AssignAccountForModel(key, "")
}

// AssignAccountForModel 按请求的模型为网关密钥挑选账号。
//
// model 非空时，粘性绑定与重新分配都限定在“能承接该模型的账号”里：
// 请求 claude-3-opus 就不会落到只挂了 gpt 系列的账号上。
// 换号只影响本次请求的落点，key.AccountID 仍指向常驻绑定，
// 避免一次冷门模型请求把密钥永久迁走、打乱既有的均摊结果。
func (d *Data) AssignAccountForModel(key *APIKey, model string) *Account {
	return d.AssignAccountGated(key, model, nil)
}

// AssignAccountGated 在按模型分配的基础上再应用一层准入判定。
//
// 被 gate 拦下的账号只是「这一次不要用」（例如已达每分钟频率上限），
// 因此绝不解除粘性绑定：临时借一个别的账号顶上，窗口过去后仍回到原账号，
// 否则一次限流就会把密钥永久迁走，把负载均衡的结果打乱。
func (d *Data) AssignAccountGated(key *APIKey, model string, gate AccountGate) *Account {
	if key == nil {
		return nil
	}

	pool := d.accountPoolGated(key.GroupID, model, gate)

	if key.AccountID != "" {
		current := d.FindAccount(key.AccountID)
		sameGroup := current != nil && (key.GroupID == "" || current.GroupID == key.GroupID)
		// 候选池已经过滤过分组启停、账号可用性与模型匹配，命中即可保持粘性。
		if sameGroup && containsAccount(pool, current.ID) {
			return current
		}
		// 绑定账号本身仍然健康，只是这次不适合承接（模型不匹配或已达频率上限）：
		// 临时借一个能接的账号，常驻绑定保持不动。
		if sameGroup &&
			(model != "" || (gate != nil && !gate(current))) &&
			d.GroupEnabled(current.GroupID) &&
			current.Usable() &&
			d.HealthyAccountProviders(current.ID) > 0 {
			return pickFromPool(d, pool, model)
		}
		d.detachKeyAccount(key)
	}

	chosen := pickFromPool(d, pool, model)
	if chosen == nil {
		return nil
	}
	key.AccountID = chosen.ID
	key.UpdatedAt = time.Now().UTC()
	if d.accountKeyLoad != nil {
		d.accountKeyLoad[chosen.ID]++
	}
	return chosen
}

// accountPoolForModel 返回本次请求真正应该考虑的账号池。
//
// 指定模型时优先只用“明确声明过该模型”的账号；一个都没有才回退到
// 模型列表留空的“什么都收”账号。没有这层优先级，请求冷门模型时很容易被
// 分到一个没设模型限制、其实并不提供它的账号上，白跑一次上游。
func (d *Data) accountPoolForModel(groupID, model string) []*Account {
	return d.accountPoolGated(groupID, model, nil)
}

// accountPoolGated 是带准入判定的候选池构造。
func (d *Data) accountPoolGated(groupID, model string, gate AccountGate) []*Account {
	pool := d.UsableAccountsGated(groupID, model, gate)
	if model == "" || len(pool) == 0 {
		return pool
	}
	explicit := make([]*Account, 0, len(pool))
	for _, account := range pool {
		if d.AccountDeclaresModel(account.ID, model) {
			explicit = append(explicit, account)
		}
	}
	if len(explicit) > 0 {
		return explicit
	}
	return pool
}

// pickFromPool 排序候选池并取第一个，池为空时返回 nil。
func pickFromPool(d *Data, pool []*Account, model string) *Account {
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
	return pool[0]
}

func containsAccount(pool []*Account, accountID string) bool {
	for _, account := range pool {
		if account.ID == accountID {
			return true
		}
	}
	return false
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
		localCost   float64
		tokens      int64
		promptTok   int64
		outputTok   int64
		upstreamTok int64
		requests    int64
		accountNum  int
		usableNum   int
		apiCount    int
	)
	unlimitedNum := 0
	suspendedNum := 0
	manualNum := 0
	queriedBalance := 0.0
	manualAmount := 0.0
	for _, account := range d.Accounts {
		if account.GroupID != groupID {
			continue
		}
		accountNum++
		if account.Suspended {
			suspendedNum++
		}
		if account.HasManualBalance() {
			manualNum++
			manualAmount += account.Balance
		} else if account.HasBalanceQuery() {
			queriedBalance += account.Balance
		}
		if account.Unlimited() {
			unlimitedNum++
		} else {
			balance += account.Balance
			usedAmount += account.UsedAmount
			totalAmount += account.TotalAmount
		}
		// 本地计费金额与余额来源无关：即使余额走上游查询，也要有一份自算口径。
		localCost += account.Stats.Cost
		tokens += account.Stats.TotalTokens
		promptTok += account.Stats.PromptTokens
		outputTok += account.Stats.CompletionTokens
		upstreamTok += account.Stats.UpstreamTokens
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
		"balance":          round4(balance),
		"queriedBalance":   round4(queriedBalance),
		"manualAmount":     round4(manualAmount),
		"usedAmount":       round4(usedAmount),
		"totalAmount":      round4(totalAmount),
		"localCost":        round4(localCost),
		"lifetimeUsed":     round4(usedAmount + removedUsed),
		"tokens":           tokens,
		"promptTokens":     promptTok,
		"completionTokens": outputTok,
		"upstreamTokens":   upstreamTok,
		"lifetimeToken":    tokens + removedTokens,
		"requests":         requests,
		"accounts":         accountNum,
		"usable":           usableNum,
		"suspended":        suspendedNum,
		"unlimited":        unlimitedNum,
		"manualBalance":    manualNum,
		"apiCount":         apiCount,
		"keys":             keyCount,
		"currency":         "USD",
		"enabled":          d.GroupEnabled(groupID),
	}
}

// AccountTotals 汇总全部账号的余额、消耗与用量，用于 /dashboard 展示。
func (d *Data) AccountTotals() map[string]any {
	var (
		balance     float64
		usedAmount  float64
		totalAmount float64
		localCost   float64
		enabled     int
		suspended   int
		exhausted   int
		apiCount    int
		totalTokens int64
		promptTok   int64
		outputTok   int64
		upstreamTok int64
		requests    int64
	)
	unlimited := 0
	manualBalance := 0
	queriedBalance := 0.0
	manualAmount := 0.0
	queriedNum := 0
	var checkedAt *time.Time
	staleNum := 0
	for _, account := range d.Accounts {
		// 无限额度账号不参与金额汇总，否则 0 余额会把总额拉低造成误解。
		if account.Unlimited() {
			unlimited++
		} else {
			balance += account.Balance
			usedAmount += account.UsedAmount
			totalAmount += account.TotalAmount
		}
		if account.HasManualBalance() {
			manualBalance++
			manualAmount += account.Balance
		} else if account.HasBalanceQuery() {
			queriedNum++
			queriedBalance += account.Balance
			if account.CheckedAt == nil {
				staleNum++
			} else if checkedAt == nil || account.CheckedAt.After(*checkedAt) {
				checkedAt = account.CheckedAt
			}
		}
		if account.Usable() {
			enabled++
		}
		if account.Suspended {
			suspended++
		}
		if account.Exhausted() {
			exhausted++
		}
		apiCount += d.CountAccountKeys(account.ID)
		localCost += account.Stats.Cost
		totalTokens += account.Stats.TotalTokens
		promptTok += account.Stats.PromptTokens
		outputTok += account.Stats.CompletionTokens
		upstreamTok += account.Stats.UpstreamTokens
		requests += account.Stats.Requests
	}

	var (
		keyTokens   int64
		keyRequests int64
		keyCost     float64
		assigned    int
	)
	for _, key := range d.Keys {
		keyTokens += key.Stats.TotalTokens
		keyRequests += key.Stats.Requests
		keyCost += key.Stats.Cost
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
			"total":         len(d.Accounts),
			"enabled":       enabled,
			"suspended":     suspended,
			"exhausted":     exhausted,
			"unlimited":     unlimited,
			"manualBalance": manualBalance,
			"queried":       queriedNum,
			"neverChecked":  staleNum,
			"removed":       len(d.RemovedAccounts),
			"apiCount":      apiCount,
		},
		"balance": map[string]any{
			"total": round4(balance),
			// queriedBalance / manualAmount 把总余额拆成「上游查询得到的」与「本地手动扣减的」两部分，
			// 便于判断某个数字到底该信谁。
			"queriedBalance": round4(queriedBalance),
			"manualAmount":   round4(manualAmount),
			"checkedAt":      checkedAt,
			"currency":       "USD",
			"usedAmount":     round4(usedAmount),
			"totalAmount":    round4(totalAmount),
			"removedUsed":    round4(removedUsed),
			"lifetime":       round4(usedAmount + removedUsed),
			// localCost 是完全由本站 tokenizer 与单价算出的消耗，用于对照上游账单。
			"localCost": round4(localCost),
			"keyCost":   round4(keyCost),
		},
		"tokens": map[string]any{
			"accounts":    totalTokens,
			"keys":        keyTokens,
			"providers":   providerTokens,
			"prompt":      promptTok,
			"completion":  outputTok,
			"upstream":    upstreamTok,
			"lifetime":    totalTokens + removedTokens,
			"selfMetered": true,
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
