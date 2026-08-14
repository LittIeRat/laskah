package store

import (
	"testing"
	"time"
)

// newAccount 构造一个配置了额度查询的账号：没有凭据的账号会被视为无限额度，
// 那样余额相关断言就失去意义，因此这里始终带上访问令牌与用户 ID。
func newAccount(t *testing.T, name string, balance float64) *Account {
	t.Helper()
	account, verr := BuildAccount(AccountInput{
		Name:        name,
		BaseURL:     "https://api.newapi.com/v1",
		AccessToken: "tok-" + name,
		UserID:      "114514",
	})
	if verr != nil {
		t.Fatalf("构建账号失败: %v", verr)
	}
	checked := time.Now().UTC()
	account.CheckedAt = &checked
	account.Balance = balance
	return account
}

func TestBuildAccountDefaults(t *testing.T) {
	account, verr := BuildAccount(AccountInput{Name: "acct", BaseURL: "api.newapi.com/v1/"})
	if verr != nil {
		t.Fatalf("不应报错: %v", verr)
	}
	if account.BaseURL != "https://api.newapi.com/v1" {
		t.Fatalf("base url 归一化失败: %s", account.BaseURL)
	}
	// 凭据请求地址留空时应回落到去掉 /v1 的供应商地址。
	if account.SiteURL != "https://api.newapi.com" {
		t.Fatalf("凭据地址应自动回落: %s", account.SiteURL)
	}
	if account.TimeoutSeconds != DefaultQueryTimeoutSeconds {
		t.Fatalf("默认超时应为 %d 秒, got %d", DefaultQueryTimeoutSeconds, account.TimeoutSeconds)
	}
	if account.QueryIntervalMin != 0 {
		t.Fatalf("默认应关闭自动查询, got %d", account.QueryIntervalMin)
	}
	// 请求时刷新默认开启，避免余额耗尽后继续把流量打到同一个账号。
	if !account.RefreshOnRequest {
		t.Fatalf("默认应开启请求时刷新")
	}
	if account.RequestRefreshSec != DefaultRequestRefreshSeconds {
		t.Fatalf("请求时刷新间隔默认应为 %d 秒, got %d", DefaultRequestRefreshSeconds, account.RequestRefreshSec)
	}
	if !account.Enabled || !account.AutoDelete {
		t.Fatalf("默认应启用且开启自动删号")
	}
	if account.Currency != "USD" {
		t.Fatalf("默认币种应为 USD")
	}
	if MaxKeysPerAccount != 5 || DefaultBatchKeys != 5 {
		t.Fatalf("常量错误: %d %d", MaxKeysPerAccount, DefaultBatchKeys)
	}
	// 没填访问令牌与用户 ID 时按无限额度处理。
	if account.HasBalanceQuery() || !account.Unlimited() {
		t.Fatalf("未配置额度查询应视为无限余额: %#v", account)
	}
}

// TestAccountUnlimitedWhenNoBalanceQuery 覆盖“无限余额”账号的行为。
//
// 这类账号既不会被判定耗尽，也不需要任何额度查询，
// 因此不会因为余额字段是 0 而被自动删除。
func TestAccountUnlimitedWhenNoBalanceQuery(t *testing.T) {
	account, verr := BuildAccount(AccountInput{Name: "free", BaseURL: "https://a.com/v1"})
	if verr != nil {
		t.Fatalf("构建失败: %v", verr)
	}
	account.QueryIntervalMin = 1
	account.Balance = 0

	if !account.Unlimited() || account.HasBalanceQuery() {
		t.Fatalf("缺少凭据应判定为无限额度")
	}
	if account.Exhausted() {
		t.Fatalf("无限额度账号不应判定耗尽")
	}
	if !account.Usable() {
		t.Fatalf("无限额度账号应可承接流量")
	}
	if account.DueForQuery(time.Now()) || account.NeedsRequestRefresh(time.Now()) {
		t.Fatalf("无限额度账号不应触发任何额度查询")
	}

	view := PublicAccount(account, 1, 0)
	if view["unlimited"] != true || view["hasBalanceQuery"] != false {
		t.Fatalf("视图应标记无限额度: %#v", view)
	}

	// 只配一半凭据同样不算配置完成。
	half, _ := BuildAccount(AccountInput{Name: "half", BaseURL: "https://a.com/v1", AccessToken: "tok"})
	if half.HasBalanceQuery() {
		t.Fatalf("缺少用户 ID 时不应视为已配置额度查询")
	}
}

func TestBuildAccountValidation(t *testing.T) {
	if _, verr := BuildAccount(AccountInput{Name: "x"}); verr == nil {
		t.Fatalf("缺少 base url 应报错")
	}
	if _, verr := BuildAccount(AccountInput{BaseURL: "https://a.com/v1"}); verr == nil {
		t.Fatalf("缺少用户名称应报错")
	}
	if _, verr := BuildAccount(AccountInput{Name: "x", BaseURL: "https://a.com/v1", MinBalance: -1}); verr == nil {
		t.Fatalf("负数最低余额应报错")
	}
	if _, verr := BuildAccount(AccountInput{Name: "x", BaseURL: "https://a.com/v1", TimeoutSeconds: 0}); verr == nil {
		t.Fatalf("超时 0 秒应报错")
	}
	if _, verr := BuildAccount(AccountInput{Name: "x", BaseURL: "https://a.com/v1", QueryIntervalMin: -5}); verr == nil {
		t.Fatalf("负数查询间隔应报错")
	}

	account, verr := BuildAccount(AccountInput{
		Name:             " tabi ",
		BaseURL:          "https://a.com/v1",
		SiteURL:          "https://api.newapi.com",
		UserID:           float64(114514),
		AccessToken:      " tok-abc ",
		TimeoutSeconds:   30,
		QueryIntervalMin: 15,
		Models:           "gpt-4o-mini,gpt-4o",
	})
	if verr != nil {
		t.Fatalf("不应报错: %v", verr)
	}
	if account.Name != "tabi" || account.UserID != "114514" || account.AccessToken != "tok-abc" {
		t.Fatalf("字段解析失败: %#v", account)
	}
	if account.TimeoutSeconds != 30 || account.QueryIntervalMin != 15 {
		t.Fatalf("查询配置解析失败: %#v", account)
	}
	if len(account.Models) != 2 {
		t.Fatalf("勾选模型解析失败: %#v", account.Models)
	}
	if account.QueryTimeout() != 30*time.Second {
		t.Fatalf("查询超时换算错误: %v", account.QueryTimeout())
	}
}

func TestBuildGroupValidation(t *testing.T) {
	if _, verr := BuildGroup(GroupInput{}); verr == nil {
		t.Fatalf("空名称应报错")
	}
	long := ""
	for i := 0; i < 70; i++ {
		long += "a"
	}
	if _, verr := BuildGroup(GroupInput{Name: long}); verr == nil {
		t.Fatalf("超长名称应报错")
	}
	group, verr := BuildGroup(GroupInput{Name: " 团队A "})
	if verr != nil || group.Name != "团队A" {
		t.Fatalf("分组构建失败: %#v %v", group, verr)
	}
	if !group.Enabled {
		t.Fatalf("分组默认应启用")
	}

	disabled := false
	off, verr := BuildGroup(GroupInput{Name: "关闭", Enabled: &disabled})
	if verr != nil || off.Enabled {
		t.Fatalf("应支持创建时禁用: %#v %v", off, verr)
	}
}

// TestDisabledGroupLeavesPool 验证禁用分组后其账号立即退出分配池。
func TestDisabledGroupLeavesPool(t *testing.T) {
	group, _ := BuildGroup(GroupInput{Name: "A"})
	account := newAccount(t, "in-a", 10)
	account.GroupID = group.ID

	data := newTestData(account)
	data.Groups = append(data.Groups, group)
	provider, _ := BuildProvider(ProviderInput{BaseURL: "https://x.example.com", AccountID: account.ID})
	data.Providers = append(data.Providers, provider)
	key, _ := BuildKey(KeyInput{Name: "k"})
	data.Keys = append(data.Keys, key)
	data.reindex()

	if len(data.UsableAccounts("")) != 1 {
		t.Fatalf("启用分组内的账号应可用")
	}
	if data.AssignAccount(key) == nil {
		t.Fatalf("启用时应能分配账号")
	}

	group.Enabled = false
	if len(data.UsableAccounts("")) != 0 {
		t.Fatalf("禁用分组的账号应退出分配池")
	}
	if !data.GroupEnabled("") {
		t.Fatalf("空分组 ID 应视为允许")
	}
	if data.GroupEnabled("grp_missing") {
		t.Fatalf("不存在的分组应视为不可用")
	}
	if data.AssignAccount(key) != nil {
		t.Fatalf("禁用分组后不应再分配该账号")
	}
}

func TestAccountUsableAndExhausted(t *testing.T) {
	fresh, _ := BuildAccount(AccountInput{Name: "fresh", BaseURL: "https://a.com/v1"})
	if !fresh.Usable() || fresh.Exhausted() {
		t.Fatalf("未查询余额的账号应可用且未耗尽")
	}

	drained := newAccount(t, "drained", 0)
	if drained.Usable() || !drained.Exhausted() {
		t.Fatalf("余额为 0 应判定耗尽")
	}

	failed := newAccount(t, "failed", 0)
	failed.CheckError = "网络错误"
	if failed.Exhausted() {
		t.Fatalf("查询失败时不应判定耗尽（避免误删）")
	}
	if !failed.Usable() {
		t.Fatalf("查询失败时不应停止接流量")
	}

	disabled := newAccount(t, "disabled", 10)
	disabled.Enabled = false
	if disabled.Usable() {
		t.Fatalf("禁用账号不应可用")
	}

	withFloor := newAccount(t, "floor", 0.5)
	withFloor.MinBalance = 1
	if withFloor.Usable() || !withFloor.Exhausted() {
		t.Fatalf("低于余额下限应判定耗尽")
	}
}

func TestAccountDueForQuery(t *testing.T) {
	now := time.Now()
	account := newAccount(t, "acct", 5)
	if account.DueForQuery(now) {
		t.Fatalf("间隔为 0 表示不自动查询")
	}

	account.QueryIntervalMin = 10
	recent := now.Add(-2 * time.Minute)
	account.CheckedAt = &recent
	if account.DueForQuery(now) {
		t.Fatalf("未到间隔不应触发")
	}
	stale := now.Add(-20 * time.Minute)
	account.CheckedAt = &stale
	if !account.DueForQuery(now) {
		t.Fatalf("超过间隔应触发")
	}
	account.CheckedAt = nil
	if !account.DueForQuery(now) {
		t.Fatalf("从未查询过应触发")
	}
}

func TestAccountNeedsRequestRefresh(t *testing.T) {
	now := time.Now()
	account := newAccount(t, "acct", 5)

	// 刚查询过：在节流窗口内不再重复查询。
	recent := now.Add(-5 * time.Second)
	account.CheckedAt = &recent
	if account.NeedsRequestRefresh(now) {
		t.Fatalf("节流窗口内不应重复查询")
	}

	stale := now.Add(-2 * time.Minute)
	account.CheckedAt = &stale
	if !account.NeedsRequestRefresh(now) {
		t.Fatalf("超过节流窗口应触发查询")
	}

	account.CheckedAt = nil
	if !account.NeedsRequestRefresh(now) {
		t.Fatalf("从未查询过必须先查一次")
	}

	account.RefreshOnRequest = false
	if account.NeedsRequestRefresh(now) {
		t.Fatalf("关闭请求时刷新后不应触发")
	}

	account.RefreshOnRequest = true
	account.Enabled = false
	if account.NeedsRequestRefresh(now) {
		t.Fatalf("已禁用账号不需要刷新")
	}

	account.Enabled = true
	account.RequestRefreshSec = 0
	if account.RequestRefreshInterval() != DefaultRequestRefreshSeconds*time.Second {
		t.Fatalf("间隔为 0 应回落到默认值: %v", account.RequestRefreshInterval())
	}
	account.RequestRefreshSec = MaxRequestRefreshSeconds * 10
	if account.RequestRefreshInterval() != MaxRequestRefreshSeconds*time.Second {
		t.Fatalf("间隔应被截断到上限: %v", account.RequestRefreshInterval())
	}
}

func newTestData(accounts ...*Account) *Data {
	data := &Data{Accounts: accounts, Groups: []*Group{}, Providers: []*Provider{}, Keys: []*APIKey{}}
	data.reindex()
	return data
}

func TestAssignAccountBalancesLoad(t *testing.T) {
	first := newAccount(t, "acct-1", 10)
	second := newAccount(t, "acct-2", 20)
	empty := newAccount(t, "acct-3", 0)

	data := newTestData(first, second, empty)
	for _, account := range data.Accounts {
		provider, _ := BuildProvider(ProviderInput{BaseURL: "https://x.example.com", AccountID: account.ID})
		data.Providers = append(data.Providers, provider)
	}
	data.reindex()

	keys := []*APIKey{}
	for i := 0; i < 4; i++ {
		key, _ := BuildKey(KeyInput{Name: "k"})
		data.Keys = append(data.Keys, key)
		keys = append(keys, key)
	}
	data.reindex()

	assignments := map[string]int{}
	for _, key := range keys {
		account := data.AssignAccount(key)
		if account == nil {
			t.Fatalf("应能分配到账号")
		}
		if account.ID == empty.ID {
			t.Fatalf("余额耗尽的账号不应被分配")
		}
		assignments[account.ID]++
	}
	if assignments[first.ID] != 2 || assignments[second.ID] != 2 {
		t.Fatalf("应在可用账号间均摊: %#v", assignments)
	}

	existing := keys[0].AccountID
	if again := data.AssignAccount(keys[0]); again.ID != existing {
		t.Fatalf("可用账号应保持粘性")
	}
}

func TestAssignAccountRespectsGroupScope(t *testing.T) {
	groupA, _ := BuildGroup(GroupInput{Name: "A"})
	groupB, _ := BuildGroup(GroupInput{Name: "B"})
	inA := newAccount(t, "in-a", 10)
	inA.GroupID = groupA.ID
	inB := newAccount(t, "in-b", 100)
	inB.GroupID = groupB.ID

	data := newTestData(inA, inB)
	data.Groups = append(data.Groups, groupA, groupB)
	for _, account := range data.Accounts {
		provider, _ := BuildProvider(ProviderInput{BaseURL: "https://x.example.com", AccountID: account.ID})
		data.Providers = append(data.Providers, provider)
	}
	data.reindex()

	scoped, _ := BuildKey(KeyInput{Name: "scoped", GroupID: groupA.ID})
	data.Keys = append(data.Keys, scoped)
	data.reindex()

	// 即使 B 组余额更高，限定分组的密钥也只能落在 A 组。
	account := data.AssignAccount(scoped)
	if account == nil || account.ID != inA.ID {
		t.Fatalf("应只在指定分组内分配: %#v", account)
	}

	free, _ := BuildKey(KeyInput{Name: "free"})
	data.Keys = append(data.Keys, free)
	data.reindex()
	if data.AssignAccount(free) == nil {
		t.Fatalf("未限定分组的密钥应能分配")
	}
}

func TestAssignAccountSkipsAccountsWithoutAPIs(t *testing.T) {
	account := newAccount(t, "no-api", 100)
	data := newTestData(account)
	key, _ := BuildKey(KeyInput{Name: "k"})
	data.Keys = append(data.Keys, key)
	data.reindex()

	if data.AssignAccount(key) != nil {
		t.Fatalf("没有上游 API 的账号不应被分配")
	}
	if key.AccountID != "" {
		t.Fatalf("分配失败时不应写入 accountId")
	}
}

func TestRemoveAccountsCascades(t *testing.T) {
	account := newAccount(t, "target", 0)
	account.UsedAmount = 7
	account.Stats.TotalTokens = 400
	other := newAccount(t, "other", 5)

	data := newTestData(account, other)
	for i := 0; i < 3; i++ {
		provider, _ := BuildProvider(ProviderInput{BaseURL: "https://x.example.com", AccountID: account.ID})
		data.Providers = append(data.Providers, provider)
	}
	keepProvider, _ := BuildProvider(ProviderInput{BaseURL: "https://y.example.com", AccountID: other.ID})
	data.Providers = append(data.Providers, keepProvider)

	bound, _ := BuildKey(KeyInput{Name: "bound", AccountID: account.ID})
	free, _ := BuildKey(KeyInput{Name: "free", AccountID: other.ID})
	data.Keys = append(data.Keys, bound, free)
	data.reindex()

	if data.CountAccountKeys(account.ID) != 3 || data.KeysUsingAccount(account.ID) != 1 {
		t.Fatalf("索引统计错误: %d %d", data.CountAccountKeys(account.ID), data.KeysUsingAccount(account.ID))
	}

	removed := data.RemoveAccounts([]string{account.ID}, "余额耗尽自动删除")
	if len(removed) != 1 || removed[0].Keys != 3 || removed[0].UsedAmount != 7 || removed[0].Tokens != 400 {
		t.Fatalf("删除记录错误: %#v", removed)
	}
	if len(data.Accounts) != 1 || data.Accounts[0].ID != other.ID {
		t.Fatalf("账号未被删除: %#v", data.Accounts)
	}
	if len(data.Providers) != 1 || data.Providers[0].AccountID != other.ID {
		t.Fatalf("名下 API 应级联删除: %#v", data.Providers)
	}
	if bound.AccountID != "" {
		t.Fatalf("绑定该账号的密钥应解绑")
	}
	if free.AccountID != other.ID {
		t.Fatalf("其他账号的密钥不应受影响")
	}
	if data.RemoveAccounts([]string{"missing"}, "x") != nil {
		t.Fatalf("删除不存在的账号应返回 nil")
	}
}

func TestRemoveGroupsCascades(t *testing.T) {
	group, _ := BuildGroup(GroupInput{Name: "团队"})
	account := newAccount(t, "in-group", 5)
	account.GroupID = group.ID

	data := newTestData(account)
	data.Groups = append(data.Groups, group)
	provider, _ := BuildProvider(ProviderInput{BaseURL: "https://x.example.com", AccountID: account.ID})
	data.Providers = append(data.Providers, provider)
	key, _ := BuildKey(KeyInput{Name: "k", GroupID: group.ID, AccountID: account.ID})
	data.Keys = append(data.Keys, key)
	data.reindex()

	if removed := data.RemoveGroups([]string{group.ID}); removed != 1 {
		t.Fatalf("应删除 1 个分组, got %d", removed)
	}
	if len(data.Groups) != 0 || len(data.Accounts) != 0 || len(data.Providers) != 0 {
		t.Fatalf("分组删除应级联清理账号与 API")
	}
	if key.GroupID != "" || key.AccountID != "" {
		t.Fatalf("密钥应解绑分组与账号: %#v", key)
	}
	if data.RemoveGroups([]string{"missing"}) != 0 {
		t.Fatalf("删除不存在分组应返回 0")
	}
}

func TestGroupSummaryAndTotals(t *testing.T) {
	group, _ := BuildGroup(GroupInput{Name: "A"})
	other, _ := BuildGroup(GroupInput{Name: "B"})

	first := newAccount(t, "a1", 12.5)
	first.GroupID = group.ID
	first.UsedAmount = 2.5
	first.TotalAmount = 15
	first.Stats = AccountStats{Requests: 10, TotalTokens: 1000}

	second := newAccount(t, "a2", 4)
	second.GroupID = other.ID
	second.UsedAmount = 5
	second.Stats = AccountStats{Requests: 4, TotalTokens: 400}

	data := newTestData(first, second)
	data.Groups = append(data.Groups, group, other)
	provider, _ := BuildProvider(ProviderInput{BaseURL: "https://x.example.com", AccountID: first.ID})
	provider.Stats.TotalTokens = 1400
	data.Providers = append(data.Providers, provider)

	key, _ := BuildKey(KeyInput{Name: "k", GroupID: group.ID, AccountID: first.ID})
	key.Stats = KeyStats{Requests: 14, TotalTokens: 1400}
	unassigned, _ := BuildKey(KeyInput{Name: "u"})
	data.Keys = append(data.Keys, key, unassigned)
	data.RemovedAccounts = append(data.RemovedAccounts, RemovedAccount{Name: "gone", GroupID: group.ID, UsedAmount: 9, Tokens: 300})
	data.reindex()

	summary := data.GroupSummary(group.ID)
	if summary["balance"] != 12.5 || summary["usedAmount"] != 2.5 {
		t.Fatalf("分组余额汇总错误: %#v", summary)
	}
	if summary["lifetimeUsed"] != 11.5 {
		t.Fatalf("分组历史消耗应含已删号: %#v", summary)
	}
	if summary["tokens"] != int64(1000) || summary["lifetimeToken"] != int64(1300) {
		t.Fatalf("分组 token 汇总错误: %#v", summary)
	}
	if summary["accounts"] != 1 || summary["apiCount"] != 1 || summary["keys"] != 1 {
		t.Fatalf("分组计数错误: %#v", summary)
	}

	totals := data.AccountTotals()
	balance := totals["balance"].(map[string]any)
	if balance["total"] != 16.5 || balance["usedAmount"] != 7.5 || balance["lifetime"] != 16.5 {
		t.Fatalf("总余额汇总错误: %#v", balance)
	}
	tokens := totals["tokens"].(map[string]any)
	if tokens["accounts"] != int64(1400) || tokens["keys"] != int64(1400) || tokens["lifetime"] != int64(1700) {
		t.Fatalf("token 汇总错误: %#v", tokens)
	}
	groups := totals["groups"].([]any)
	if len(groups) != 2 {
		t.Fatalf("应返回两个分组的汇总: %#v", groups)
	}
	keysSummary := totals["keys"].(map[string]any)
	if keysSummary["total"] != 2 || keysSummary["assigned"] != 1 {
		t.Fatalf("密钥统计错误: %#v", keysSummary)
	}
}

func TestPublicAccountHidesCredentials(t *testing.T) {
	account, _ := BuildAccount(AccountInput{
		Name:        "secret",
		BaseURL:     "https://api.newapi.com/v1",
		SiteURL:     "https://api.newapi.com",
		UserID:      "114514",
		AccessToken: "tok-123",
	})
	view := PublicAccount(account, 5, 2)

	// 保存后界面只能看余额，不能回显任何配置细节。
	for _, field := range []string{"accessToken", "userId", "baseUrl", "siteUrl", "cookie"} {
		if _, leaked := view[field]; leaked {
			t.Fatalf("视图不应包含 %s", field)
		}
	}
	if view["hasAccessToken"] != true || view["hasUserId"] != true || view["hasBaseUrl"] != true {
		t.Fatalf("应标记凭据已配置: %#v", view)
	}
	if view["apiCount"] != 5 || view["boundKeys"] != 2 || view["maxApiCount"] != MaxKeysPerAccount {
		t.Fatalf("计数字段错误: %#v", view)
	}
}

func TestAccountPersistence(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", "tok")
	t.Setenv("MASTER_KEY", "unit-test-master-key")
	file := t.TempDir() + "/db.json"

	first := New(file)
	if err := first.Load(); err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	group, _ := BuildGroup(GroupInput{Name: "团队"})
	account, _ := BuildAccount(AccountInput{Name: "persist", BaseURL: "https://api.newapi.com/v1", AccessToken: "tok-abc", GroupID: group.ID})
	account.Balance = 3.25
	provider, _ := BuildProvider(ProviderInput{BaseURL: "https://x.example.com", AccountID: account.ID})
	key, _ := BuildKey(KeyInput{Name: "k", AccountID: account.ID, GroupID: group.ID})
	if err := first.Update(func(data *Data) error {
		data.Groups = append(data.Groups, group)
		data.Accounts = append(data.Accounts, account)
		data.Providers = append(data.Providers, provider)
		data.Keys = append(data.Keys, key)
		return nil
	}); err != nil {
		t.Fatalf("Update 失败: %v", err)
	}

	second := New(file)
	if err := second.Load(); err != nil {
		t.Fatalf("重新 Load 失败: %v", err)
	}
	second.View(func(data *Data) {
		reloaded := data.FindAccount(account.ID)
		if reloaded == nil {
			t.Fatalf("账号未持久化")
		}
		if reloaded.Balance != 3.25 || reloaded.AccessToken != "tok-abc" {
			t.Fatalf("账号字段未持久化: %#v", reloaded)
		}
		if data.FindGroup(group.ID) == nil {
			t.Fatalf("分组未持久化")
		}
		if data.CountAccountKeys(account.ID) != 1 {
			t.Fatalf("上游归属未持久化")
		}
		if data.Keys[0].AccountID != account.ID || data.Keys[0].GroupID != group.ID {
			t.Fatalf("密钥归属未持久化")
		}
	})
}
