// Package accounts 负责账号额度刷新与余额耗尽后的自动暂停。
package accounts

import (
	"context"
	"fmt"
	"sync"
	"time"

	"laskah/internal/store"
	"laskah/internal/wallet"
)

// maxParallelRefresh 限制并发刷新数量，兼顾速度与资源占用。
const maxParallelRefresh = 6

// Manager 组合数据仓库与额度查询客户端。
type Manager struct {
	Store  *store.Store
	Wallet *wallet.Client

	// pending 让同一账号的并发刷新合并成一次上游查询。
	pendingMu sync.Mutex
	pending   map[string]*refreshCall
}

// refreshCall 是一次进行中的刷新，后到的等待者共享其结果。
type refreshCall struct {
	done      chan struct{}
	usable    bool
	missing   bool
	suspended bool
	exhausted bool
	failed    bool
}

// New 创建账号管理器。
func New(dataStore *store.Store, client *wallet.Client) *Manager {
	if client == nil {
		client = wallet.NewClient()
	}
	return &Manager{Store: dataStore, Wallet: client, pending: map[string]*refreshCall{}}
}

// Refresh 查询单个账号额度，写回结果，并在余额耗尽且开启自动暂停时暂停账号。
//
// 未配置额度查询的账号按无限余额处理，直接返回而不产生任何上游请求。
func (m *Manager) Refresh(ctx context.Context, id string) map[string]any {
	creds, name, found, queryable := m.credentials(id)
	if !found {
		return nil
	}
	if !queryable {
		// 手动余额账号的数字完全由本地计费维护，不存在可查询的上游额度接口，
		// 因此直接回报当前本地状态，而不是谎称“无限余额”。
		if local := m.localResult(id, name); local != nil {
			return local
		}
		return unlimitedResult(id, name)
	}

	snapshot := m.Wallet.Fetch(ctx, creds)
	return m.apply(id, name, snapshot)
}

// localResult 返回手动余额账号的本地余额视图，非手动余额账号返回 nil。
//
// 顺带执行一次耗尽判定：管理员点「刷新」时若余额已被扣到下限，
// 应当立刻看到账号被暂停，而不是等下一次调用才发现。
func (m *Manager) localResult(id, name string) map[string]any {
	var result map[string]any
	_ = m.Store.Update(func(data *store.Data) error {
		account := data.FindAccount(id)
		if account == nil || !account.HasManualBalance() {
			return nil
		}
		exhausted := account.Exhausted()
		suspended := account.Suspended
		if exhausted && account.AutoSuspend && account.Suspend(exhaustedReason(account)) {
			suspended = true
		}
		result = map[string]any{
			"id":          id,
			"name":        name,
			"ok":          true,
			"balance":     account.Balance,
			"usedAmount":  account.UsedAmount,
			"totalAmount": account.TotalAmount,
			"planName":    account.PlanName,
			"source":      "local",
			"exhausted":   exhausted,
			"suspended":   suspended,
			"unlimited":   false,
			"deleted":     false,
		}
		return nil
	})
	return result
}

// unlimitedResult 是无限额度账号的固定查询结果。
func unlimitedResult(id, name string) map[string]any {
	return map[string]any{
		"id":        id,
		"name":      name,
		"ok":        true,
		"unlimited": true,
		"exhausted": false,
		"suspended": false,
		"deleted":   false,
	}
}

// credentials 取出账号的查询配置。
//
// queryable 表示这个账号到底能不能查额度：脚本或内置凭据任一具备即为真。
// 判定放在这里而不是让调用方看字段，避免「有脚本但没令牌」被误当成无限额度。
func (m *Manager) credentials(id string) (wallet.Credentials, string, bool, bool) {
	var (
		creds     wallet.Credentials
		name      string
		found     bool
		queryable bool
	)
	m.Store.View(func(data *store.Data) {
		account := data.FindAccount(id)
		if account == nil {
			return
		}
		found = true
		name = account.Name
		queryable = account.HasBalanceQuery()

		creds = wallet.Credentials{
			BaseURL:     account.QueryBase(),
			UserID:      account.UserID,
			AccessToken: account.AccessToken,
			QueryURL:    account.QueryURL,
			Script:      account.QueryProgram(),
			Timeout:     account.QueryTimeout(),
		}
		for _, provider := range data.AccountProviders(account.ID) {
			if provider.APIKey != "" {
				creds.APIKey = provider.APIKey
				break
			}
		}
	})
	return creds, name, found, queryable
}

func (m *Manager) apply(id, name string, snapshot wallet.Snapshot) map[string]any {
	var (
		suspended bool
		balance   float64
		used      float64
		total     float64
		exhausted bool
		checkErr  string
		planName  string
	)
	_ = m.Store.Update(func(data *store.Data) error {
		account := data.FindAccount(id)
		if account == nil {
			return nil
		}
		checkedAt := snapshot.CheckedAt
		if checkedAt.IsZero() {
			checkedAt = time.Now().UTC()
		}
		account.CheckedAt = &checkedAt
		if snapshot.QuotaPerUnit > 0 {
			account.QuotaPerUnit = snapshot.QuotaPerUnit
		}
		if snapshot.Currency != "" {
			account.Currency = snapshot.Currency
		}

		if snapshot.Err != nil {
			account.CheckError = snapshot.Err.Error()
			checkErr = account.CheckError
			balance = account.Balance
			used = account.UsedAmount
			total = account.TotalAmount
			planName = account.PlanName
			return nil
		}

		account.CheckError = ""
		// 脚本可能只返回一部分字段：没返回的一律保留旧值，
		// 否则一个只回报 remaining 的脚本会把已用金额抹成 0。
		if snapshot.HasBalance || snapshot.Source != "script" {
			account.Balance = snapshot.Balance
		}
		if snapshot.HasUsed || snapshot.Source != "script" {
			account.UsedAmount = snapshot.UsedAmount
		}
		if snapshot.HasTotal || snapshot.Source != "script" {
			account.TotalAmount = snapshot.TotalAmount
		}
		if snapshot.PlanName != "" {
			account.PlanName = snapshot.PlanName
		}
		account.BalanceExtra = snapshot.Extra
		account.BalanceFrom = snapshot.Source
		account.UpdatedAt = time.Now().UTC()
		balance = account.Balance
		used = account.UsedAmount
		total = account.TotalAmount
		planName = account.PlanName
		exhausted = account.Exhausted()

		// 余额触及下限：暂停而不是删除，账号与上游 API 全部保留，
		// 管理员充值后在 /manage 点「启用」即恢复。
		if exhausted && account.AutoSuspend {
			suspended = account.Suspend(exhaustedReason(account))
		}
		if account.Suspended {
			suspended = true
		}
		return nil
	})

	result := map[string]any{
		"id":          id,
		"name":        name,
		"ok":          snapshot.Err == nil,
		"balance":     balance,
		"usedAmount":  used,
		"totalAmount": total,
		"planName":    planName,
		"source":      snapshot.Source,
		"exhausted":   exhausted,
		"suspended":   suspended,
		// deleted 保留为兼容字段：余额耗尽已改为暂停，恒为 false。
		"deleted": false,
	}
	if checkErr != "" {
		result["error"] = checkErr
	}
	return result
}

// RefreshForRequest 在请求路径上刷新指定账号的余额，返回刷新后该账号是否仍可用。
//
// 同一账号的并发请求会合并成一次上游查询（后到者等待前者结果），
// 因此突发流量不会把额度接口放大成 N 倍请求。
// 查询失败时返回 true：网络抖动不应让全站不可用，故障转移交给上游重试逻辑。
func (m *Manager) RefreshForRequest(ctx context.Context, accountID string) bool {
	if accountID == "" {
		return false
	}

	m.pendingMu.Lock()
	if call, running := m.pending[accountID]; running {
		m.pendingMu.Unlock()
		select {
		case <-call.done:
			return call.usable
		case <-ctx.Done():
			return false
		}
	}
	call := &refreshCall{done: make(chan struct{})}
	m.pending[accountID] = call
	m.pendingMu.Unlock()

	result := m.Refresh(ctx, accountID)
	switch {
	case result == nil:
		// 账号已不存在（例如管理员刚删掉）。
		call.missing = true
	default:
		call.suspended, _ = result["suspended"].(bool)
		call.exhausted, _ = result["exhausted"].(bool)
		_, call.failed = result["error"]
		call.usable = call.failed || (!call.suspended && !call.exhausted)
	}

	m.pendingMu.Lock()
	delete(m.pending, accountID)
	m.pendingMu.Unlock()
	close(call.done)
	return call.usable
}

// RefreshAll 并发刷新全部账号额度。
func (m *Manager) RefreshAll(ctx context.Context) []any {
	return m.refreshMany(ctx, m.collectIDs(func(*store.Account) bool { return true }))
}

// RefreshIDs 并发刷新指定账号，用于分组级手动刷新。
func (m *Manager) RefreshIDs(ctx context.Context, ids []string) []any {
	return m.refreshMany(ctx, ids)
}

// RefreshDue 只刷新到达各自自动查询间隔的账号，降低无谓的上游请求。
func (m *Manager) RefreshDue(ctx context.Context) []any {
	now := time.Now()
	return m.refreshMany(ctx, m.collectIDs(func(account *store.Account) bool {
		return account.DueForQuery(now)
	}))
}

func (m *Manager) collectIDs(match func(*store.Account) bool) []string {
	ids := []string{}
	m.Store.View(func(data *store.Data) {
		for _, account := range data.Accounts {
			if match(account) {
				ids = append(ids, account.ID)
			}
		}
	})
	return ids
}

func (m *Manager) refreshMany(ctx context.Context, ids []string) []any {
	if len(ids) == 0 {
		return []any{}
	}

	// 先并发发起网络查询，再串行写回，避免大量 goroutine 争抢仓库写锁。
	type outcome struct {
		id        string
		name      string
		snapshot  wallet.Snapshot
		found     bool
		unlimited bool
	}
	outcomes := make([]outcome, len(ids))

	var wg sync.WaitGroup
	gate := make(chan struct{}, maxParallelRefresh)
	for index, id := range ids {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(slot int, accountID string) {
			defer wg.Done()
			gate <- struct{}{}
			defer func() { <-gate }()

			creds, name, found, queryable := m.credentials(accountID)
			if !found {
				return
			}
			// 无上游额度接口的账号不发网络请求：手动余额走本地视图，其余按无限额度跳过。
			if !queryable {
				outcomes[slot] = outcome{id: accountID, name: name, found: true, unlimited: true}
				return
			}
			outcomes[slot] = outcome{id: accountID, name: name, snapshot: m.Wallet.Fetch(ctx, creds), found: true}
		}(index, id)
	}
	wg.Wait()

	results := make([]any, 0, len(ids))
	for _, item := range outcomes {
		if !item.found {
			continue
		}
		if item.unlimited {
			if local := m.localResult(item.id, item.name); local != nil {
				results = append(results, local)
				continue
			}
			results = append(results, unlimitedResult(item.id, item.name))
			continue
		}
		if result := m.apply(item.id, item.name, item.snapshot); result != nil {
			results = append(results, result)
		}
	}
	return results
}

// SuspendAccount 立即暂停一个账号，用于上游明确报告余额不足的场景。
//
// 不看 AutoSuspend 开关：上游已经拒绝了这次请求，继续放流量只会持续失败。
// 返回该账号此刻是否处于暂停状态（并发下可能已被其它请求暂停）。
func (m *Manager) SuspendAccount(accountID, reason string) bool {
	if accountID == "" {
		return false
	}
	if reason == "" {
		reason = "上游报余额不足自动暂停"
	}
	suspended := false
	_ = m.Store.Update(func(data *store.Data) error {
		account := data.FindAccount(accountID)
		if account == nil {
			return nil
		}
		account.Suspend(reason)
		suspended = account.Suspended
		return nil
	})
	return suspended
}

// exhaustedReason 生成暂停原因，带上余额与生效下限，便于事后核对。
func exhaustedReason(account *store.Account) string {
	return fmt.Sprintf("余额触及下限自动暂停（余额 %.6f / 下限 %.2f %s）",
		account.Balance, account.BalanceFloor(), account.Currency)
}

// SweepExhausted 暂停余额已耗尽且开启自动暂停的账号，返回被暂停的账号名。
func (m *Manager) SweepExhausted() []string {
	names := []string{}
	_ = m.Store.Update(func(data *store.Data) error {
		for _, account := range data.Accounts {
			if !account.AutoSuspend || account.Suspended || !account.Exhausted() {
				continue
			}
			// 逐个暂停：暂停原因要带上各自的余额与下限。
			if account.Suspend(exhaustedReason(account)) {
				names = append(names, account.Name)
			}
		}
		return nil
	})
	return names
}
