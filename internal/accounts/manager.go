// Package accounts 负责账号额度刷新与余额耗尽后的自动清理。
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
	deleted   bool
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

// Refresh 查询单个账号额度，写回结果，并在余额耗尽且开启自动删除时清理账号。
//
// 未配置额度查询的账号按无限余额处理，直接返回而不产生任何上游请求。
func (m *Manager) Refresh(ctx context.Context, id string) map[string]any {
	creds, name, found := m.credentials(id)
	if !found {
		return nil
	}
	if creds.AccessToken == "" || creds.UserID == "" {
		return unlimitedResult(id, name)
	}

	snapshot := m.Wallet.Fetch(ctx, creds)
	return m.apply(id, name, snapshot)
}

// unlimitedResult 是无限额度账号的固定查询结果。
func unlimitedResult(id, name string) map[string]any {
	return map[string]any{
		"id":        id,
		"name":      name,
		"ok":        true,
		"unlimited": true,
		"exhausted": false,
		"deleted":   false,
	}
}

func (m *Manager) credentials(id string) (wallet.Credentials, string, bool) {
	var (
		creds wallet.Credentials
		name  string
		found bool
	)
	m.Store.View(func(data *store.Data) {
		account := data.FindAccount(id)
		if account == nil {
			return
		}
		found = true
		name = account.Name

		base := account.SiteURL
		if base == "" {
			base = account.BaseURL
		}
		creds = wallet.Credentials{
			BaseURL:     base,
			UserID:      account.UserID,
			AccessToken: account.AccessToken,
			Timeout:     account.QueryTimeout(),
		}
		for _, provider := range data.AccountProviders(account.ID) {
			if provider.APIKey != "" {
				creds.APIKey = provider.APIKey
				break
			}
		}
	})
	return creds, name, found
}

func (m *Manager) apply(id, name string, snapshot wallet.Snapshot) map[string]any {
	var (
		deleted   bool
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
		account.Balance = snapshot.Balance
		account.UsedAmount = snapshot.UsedAmount
		account.TotalAmount = snapshot.TotalAmount
		if snapshot.PlanName != "" {
			account.PlanName = snapshot.PlanName
		}
		account.BalanceFrom = snapshot.Source
		account.UpdatedAt = time.Now().UTC()
		balance = account.Balance
		used = account.UsedAmount
		total = account.TotalAmount
		planName = account.PlanName
		exhausted = account.Exhausted()

		if exhausted && account.AutoDelete {
			data.RemoveAccounts([]string{account.ID}, exhaustedReason(account))
			deleted = true
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
		"deleted":     deleted,
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
		// 账号已不存在（可能刚被其他请求触发的刷新删掉）。
		call.deleted = true
	default:
		call.deleted, _ = result["deleted"].(bool)
		call.exhausted, _ = result["exhausted"].(bool)
		_, call.failed = result["error"]
		call.usable = call.failed || (!call.deleted && !call.exhausted)
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

			creds, name, found := m.credentials(accountID)
			if !found {
				return
			}
			// 无限额度账号不查上游，直接记为跳过。
			if creds.AccessToken == "" || creds.UserID == "" {
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
			results = append(results, unlimitedResult(item.id, item.name))
			continue
		}
		if result := m.apply(item.id, item.name, item.snapshot); result != nil {
			results = append(results, result)
		}
	}
	return results
}

// DropAccount 立即删除一个账号，用于上游明确报告余额不足的场景。
//
// 不看 AutoDelete 开关：上游已经拒绝了这次请求，继续留着只会持续失败。
// 返回是否真的删除了账号（并发下可能已被其他请求删掉）。
func (m *Manager) DropAccount(accountID, reason string) bool {
	if accountID == "" {
		return false
	}
	if reason == "" {
		reason = "上游报余额不足自动删除"
	}
	removed := false
	_ = m.Store.Update(func(data *store.Data) error {
		if data.FindAccount(accountID) == nil {
			return nil
		}
		removed = len(data.RemoveAccounts([]string{accountID}, reason)) > 0
		return nil
	})
	return removed
}

// exhaustedReason 生成删号原因，带上余额与生效下限，便于事后核对。
func exhaustedReason(account *store.Account) string {
	return fmt.Sprintf("余额触及下限自动删除（余额 %.6f / 下限 %.2f %s）",
		account.Balance, account.BalanceFloor(), account.Currency)
}

// SweepExhausted 清理余额已耗尽且开启自动删除的账号，返回被删除的账号名。
func (m *Manager) SweepExhausted() []string {
	names := []string{}
	_ = m.Store.Update(func(data *store.Data) error {
		ids := []string{}
		reasons := []string{}
		for _, account := range data.Accounts {
			if account.AutoDelete && account.Exhausted() {
				ids = append(ids, account.ID)
				reasons = append(reasons, exhaustedReason(account))
				names = append(names, account.Name)
			}
		}
		// 逐个删除而不是批量：删号原因要带上各自的余额与下限。
		for index, id := range ids {
			data.RemoveAccounts([]string{id}, reasons[index])
		}
		return nil
	})
	return names
}
