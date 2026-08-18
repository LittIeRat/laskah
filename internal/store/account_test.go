package store

import (
	"os"
	"strings"
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
	if !account.Enabled || !account.AutoSuspend {
		t.Fatalf("默认应启用且开启余额耗尽自动暂停")
	}
	if account.Suspended || account.RateLimitPerMin != nil || account.RateLimit() != 0 {
		t.Fatalf("默认不应暂停且不应有频率限制: %#v", account)
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
// 因此不会因为余额字段是 0 而被自动暂停。
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
		t.Fatalf("分组历史消耗应含已移除账号: %#v", summary)
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

func TestUsableAccountsForModelFiltersByProviderModels(t *testing.T) {
	gptAccount := newAccount(t, "gpt-only", 10)
	claudeAccount := newAccount(t, "claude-only", 10)

	data := newTestData(gptAccount, claudeAccount)
	gptProvider, _ := BuildProvider(ProviderInput{BaseURL: "https://gpt.example.com", AccountID: gptAccount.ID, Models: []string{"gpt-4o", "gpt-4o-mini"}})
	claudeProvider, _ := BuildProvider(ProviderInput{BaseURL: "https://claude.example.com", AccountID: claudeAccount.ID, Models: []string{"claude-3-opus"}})
	data.Providers = append(data.Providers, gptProvider, claudeProvider)
	data.reindex()

	if accounts := data.UsableAccountsForModel("", "gpt-4o"); len(accounts) != 1 || accounts[0].ID != gptAccount.ID {
		t.Fatalf("请求 gpt-4o 只应命中 gpt 账号: %#v", accounts)
	}
	if accounts := data.UsableAccountsForModel("", "claude-3-opus"); len(accounts) != 1 || accounts[0].ID != claudeAccount.ID {
		t.Fatalf("请求 claude 只应命中 claude 账号: %#v", accounts)
	}
	if accounts := data.UsableAccountsForModel("", "gemini-pro"); len(accounts) != 0 {
		t.Fatalf("无人提供的模型不应有候选账号: %#v", accounts)
	}
	// 不限模型时两个账号都算可用。
	if accounts := data.UsableAccounts(""); len(accounts) != 2 {
		t.Fatalf("不限模型应返回全部可用账号: %#v", accounts)
	}

	if !data.AccountServesModel(gptAccount.ID, "gpt-4o-mini") {
		t.Fatalf("gpt 账号应能承接 gpt-4o-mini")
	}
	if data.AccountServesModel(gptAccount.ID, "claude-3-opus") {
		t.Fatalf("gpt 账号不应被判定为能承接 claude")
	}
}

func TestAssignAccountForModelPicksAccountThatHasIt(t *testing.T) {
	// gpt 账号余额更低、创建更早，正常排序会优先命中它；
	// 因此只要请求 claude 时选中了 claude 账号，就证明是模型维度在起作用。
	gptAccount := newAccount(t, "gpt-only", 5)
	claudeAccount := newAccount(t, "claude-only", 500)

	data := newTestData(gptAccount, claudeAccount)
	gptProvider, _ := BuildProvider(ProviderInput{BaseURL: "https://gpt.example.com", AccountID: gptAccount.ID, Models: []string{"gpt-4o-mini"}})
	claudeProvider, _ := BuildProvider(ProviderInput{BaseURL: "https://claude.example.com", AccountID: claudeAccount.ID, Models: []string{"claude-3-opus"}})
	data.Providers = append(data.Providers, gptProvider, claudeProvider)

	key, _ := BuildKey(KeyInput{Name: "k"})
	data.Keys = append(data.Keys, key)
	data.reindex()

	chosen := data.AssignAccountForModel(key, "gpt-4o-mini")
	if chosen == nil || chosen.ID != gptAccount.ID {
		t.Fatalf("请求 gpt-4o-mini 应落到 gpt 账号: %#v", chosen)
	}
	bound := key.AccountID
	if bound != gptAccount.ID {
		t.Fatalf("首次分配应写入常驻绑定: %s", bound)
	}

	// 绑定账号接不了 claude：本次请求换到 claude 账号，但常驻绑定不动，
	// 否则一次冷门模型请求就会把密钥永久迁走、打乱既有均摊。
	switched := data.AssignAccountForModel(key, "claude-3-opus")
	if switched == nil || switched.ID != claudeAccount.ID {
		t.Fatalf("请求 claude 应换到 claude 账号: %#v", switched)
	}
	if key.AccountID != bound {
		t.Fatalf("临时换号不应改写常驻绑定: %s -> %s", bound, key.AccountID)
	}

	// 回到原模型仍然粘在原账号。
	if again := data.AssignAccountForModel(key, "gpt-4o-mini"); again == nil || again.ID != gptAccount.ID {
		t.Fatalf("原模型应保持粘性: %#v", again)
	}

	// 谁都不提供的模型必须分配失败，而不是硬塞一个账号。
	if nobody := data.AssignAccountForModel(key, "gemini-pro"); nobody != nil {
		t.Fatalf("无人提供的模型不应分配到账号: %#v", nobody)
	}
}

func TestAssignAccountForModelSkipsCooldownAndDisabledProviders(t *testing.T) {
	hot := newAccount(t, "hot", 10)
	backup := newAccount(t, "backup", 10)

	data := newTestData(hot, backup)
	hotProvider, _ := BuildProvider(ProviderInput{BaseURL: "https://hot.example.com", AccountID: hot.ID, Models: []string{"gpt-4o"}})
	backupProvider, _ := BuildProvider(ProviderInput{BaseURL: "https://backup.example.com", AccountID: backup.ID, Models: []string{"gpt-4o"}})
	data.Providers = append(data.Providers, hotProvider, backupProvider)

	key, _ := BuildKey(KeyInput{Name: "k"})
	data.Keys = append(data.Keys, key)
	data.reindex()

	// 唯一支持该模型的上游进入冷却后，账号就不该再承接这个模型。
	hotProvider.CooldownUntil = time.Now().Add(time.Minute)
	if accounts := data.UsableAccountsForModel("", "gpt-4o"); len(accounts) != 1 || accounts[0].ID != backup.ID {
		t.Fatalf("冷却中的上游不应让账号入选: %#v", accounts)
	}

	backupProvider.Enabled = false
	if accounts := data.UsableAccountsForModel("", "gpt-4o"); len(accounts) != 0 {
		t.Fatalf("禁用上游后不应还有候选: %#v", accounts)
	}
	if data.AssignAccountForModel(key, "gpt-4o") != nil {
		t.Fatalf("没有可承接该模型的账号时应分配失败")
	}
}

func TestAccountKeyPressureCountsOnlyMatchingProviders(t *testing.T) {
	// wide 账号挂 3 个 Key 但只有 1 个支持 claude，narrow 账号 1 个 Key 且支持 claude。
	// 按总数算 wide 压力更低会被优先选中，按模型算两者相同，此时余额更高的 narrow 胜出。
	wide := newAccount(t, "wide", 10)
	narrow := newAccount(t, "narrow", 999)

	data := newTestData(wide, narrow)
	for _, models := range [][]string{{"gpt-4o"}, {"gpt-4o-mini"}, {"claude-3-opus"}} {
		provider, _ := BuildProvider(ProviderInput{BaseURL: "https://wide.example.com", AccountID: wide.ID, Models: models})
		data.Providers = append(data.Providers, provider)
	}
	narrowProvider, _ := BuildProvider(ProviderInput{BaseURL: "https://narrow.example.com", AccountID: narrow.ID, Models: []string{"claude-3-opus"}})
	data.Providers = append(data.Providers, narrowProvider)
	data.reindex()

	if got := data.accountKeyPressureForModel(wide.ID, "claude-3-opus"); got != 0 {
		t.Fatalf("未绑定密钥时压力应为 0: %v", got)
	}

	key, _ := BuildKey(KeyInput{Name: "k"})
	key.AccountID = wide.ID
	data.Keys = append(data.Keys, key)
	data.reindex()

	// 一个密钥压在 wide 上：按全部 3 个 Key 算是 1/3，按 claude 维度只有 1 个 Key，是 1。
	if got := data.accountKeyPressure(wide.ID); got != 1.0/3.0 {
		t.Fatalf("不限模型时应按全部可用上游计算: %v", got)
	}
	if got := data.accountKeyPressureForModel(wide.ID, "claude-3-opus"); got != 1 {
		t.Fatalf("按模型应只计入支持该模型的上游: %v", got)
	}

	fresh, _ := BuildKey(KeyInput{Name: "fresh"})
	data.Keys = append(data.Keys, fresh)
	data.reindex()
	if chosen := data.AssignAccountForModel(fresh, "claude-3-opus"); chosen == nil || chosen.ID != narrow.ID {
		t.Fatalf("claude 请求应分给压力相同但余额更高的 narrow: %#v", chosen)
	}
}

// TestBalanceFloorDefaultsToHalfDollar 锁定内置 0.5 USD 安全线。
//
// 余额只剩几毛钱时上游大概率连一次预扣费都过不了，提前暂停比让调用方吃一次
// 失败再换号体验好得多；账号自填的下限更高时仍然尊重账号设置。
func TestBalanceFloorDefaultsToHalfDollar(t *testing.T) {
	if MinBalanceFloorUSD != 0.5 {
		t.Fatalf("内置安全线应为 0.5 USD, got %v", MinBalanceFloorUSD)
	}

	account := newAccount(t, "floor", 10)
	if account.MinBalance != 0 {
		t.Fatalf("默认最低余额应为 0: %v", account.MinBalance)
	}
	if account.BalanceFloor() != MinBalanceFloorUSD {
		t.Fatalf("未自填下限时应抬到安全线: %v", account.BalanceFloor())
	}

	account.MinBalance = 2
	if account.BalanceFloor() != 2 {
		t.Fatalf("自填下限更高时应生效: %v", account.BalanceFloor())
	}

	view := PublicAccount(newAccount(t, "view", 10), 1, 0)
	if view["balanceFloor"] != MinBalanceFloorUSD {
		t.Fatalf("视图应暴露生效下限: %#v", view["balanceFloor"])
	}
}

// TestExhaustedAtBalanceFloor 校验 0.5 USD 上下的判定边界。
func TestExhaustedAtBalanceFloor(t *testing.T) {
	cases := []struct {
		balance   float64
		exhausted bool
	}{
		{0, true},
		{0.182898, true},
		{0.49, true},
		{0.5, true},
		{0.500001, false},
		{1.25, false},
	}
	for _, item := range cases {
		account := newAccount(t, "edge", item.balance)
		if account.Exhausted() != item.exhausted {
			t.Fatalf("余额 %v 的耗尽判定应为 %v", item.balance, item.exhausted)
		}
		if account.Usable() == item.exhausted {
			t.Fatalf("余额 %v 的可用判定与耗尽判定矛盾", item.balance)
		}
	}

	// 无限额度账号不受安全线约束。
	unlimited, _ := BuildAccount(AccountInput{Name: "free", BaseURL: "https://a.com/v1"})
	unlimited.Balance = 0
	if unlimited.Exhausted() || !unlimited.Usable() {
		t.Fatalf("无限额度账号不应被安全线删掉")
	}
}

// TestAssignAccountSkipsAccountsBelowFloor 保证低于安全线的账号不再被分配。
func TestAssignAccountSkipsAccountsBelowFloor(t *testing.T) {
	healthy := newAccount(t, "healthy", 20)
	thin := newAccount(t, "thin", 0.3)

	data := newTestData(healthy, thin)
	for _, account := range data.Accounts {
		provider, _ := BuildProvider(ProviderInput{BaseURL: "https://x.example.com", AccountID: account.ID})
		data.Providers = append(data.Providers, provider)
	}
	data.reindex()

	if usable := data.UsableAccounts(""); len(usable) != 1 || usable[0].ID != healthy.ID {
		t.Fatalf("低于安全线的账号不应进入可用池: %#v", usable)
	}

	for index := 0; index < 3; index++ {
		key, _ := BuildKey(KeyInput{Name: "k"})
		data.Keys = append(data.Keys, key)
		data.reindex()
		account := data.AssignAccount(key)
		if account == nil || account.ID != healthy.ID {
			t.Fatalf("第 %d 次分配应命中余额充足的账号: %#v", index+1, account)
		}
	}

	// 已绑定的密钥在账号掉到安全线以下后会被解绑并改派。
	bound, _ := BuildKey(KeyInput{Name: "bound"})
	bound.AccountID = thin.ID
	data.Keys = append(data.Keys, bound)
	data.reindex()
	if account := data.AssignAccount(bound); account == nil || account.ID != healthy.ID {
		t.Fatalf("绑定到低余额账号的密钥应被改派: %#v", account)
	}
	if bound.AccountID != healthy.ID {
		t.Fatalf("改派后应更新常驻绑定: %s", bound.AccountID)
	}
}

// TestSuspendKeepsProvidersAndBindings 验证「暂停」与「删除」的关键差别：
// 暂停只把账号移出分配池，上游 API、余额与密钥绑定全部保留。
func TestSuspendKeepsProvidersAndBindings(t *testing.T) {
	account := newAccount(t, "acct", 10)
	data := newTestData(account)
	provider, _ := BuildProvider(ProviderInput{BaseURL: "https://x.example.com", AccountID: account.ID})
	data.Providers = append(data.Providers, provider)
	key, _ := BuildKey(KeyInput{Name: "k"})
	key.AccountID = account.ID
	data.Keys = append(data.Keys, key)
	data.reindex()

	if !account.Suspend("余额不足") {
		t.Fatalf("首次暂停应返回 true")
	}
	if account.Suspend("再来一次") {
		t.Fatalf("重复暂停应返回 false，避免刷新暂停时间与原因")
	}
	if account.SuspendReason != "余额不足" || account.SuspendedAt == nil {
		t.Fatalf("应记录暂停原因与时间: %#v", account)
	}
	if account.Usable() {
		t.Fatalf("暂停账号不应可用")
	}
	if len(data.UsableAccounts("")) != 0 {
		t.Fatalf("暂停账号应退出分配池")
	}
	if data.CountAccountKeys(account.ID) != 1 {
		t.Fatalf("暂停不应删除上游 API")
	}
	if key.AccountID != account.ID {
		t.Fatalf("暂停不应解除密钥绑定: %s", key.AccountID)
	}

	view := PublicAccount(account, 1, 1)
	if view["suspended"] != true || view["suspendReason"] != "余额不足" {
		t.Fatalf("视图应暴露暂停状态: %#v", view)
	}

	account.Resume()
	if account.Suspended || account.SuspendReason != "" || account.SuspendedAt != nil || !account.Enabled {
		t.Fatalf("启用后应完全清除暂停状态: %#v", account)
	}
	if len(data.UsableAccounts("")) != 1 {
		t.Fatalf("启用后账号应回到分配池")
	}
}

// TestSuspendAccountsBatch 覆盖批量暂停：不存在的 ID 与已暂停账号都不计入结果。
func TestSuspendAccountsBatch(t *testing.T) {
	first := newAccount(t, "first", 10)
	second := newAccount(t, "second", 10)
	data := newTestData(first, second)

	if names := data.SuspendAccounts([]string{first.ID, second.ID, "missing"}, "批量"); len(names) != 2 {
		t.Fatalf("应暂停两个账号: %#v", names)
	}
	if names := data.SuspendAccounts([]string{first.ID}, "再来"); len(names) != 0 {
		t.Fatalf("已暂停账号不应重复计数: %#v", names)
	}
}

// TestBuildAccountRateLimit 覆盖账号级频率限制的解析与校验：留空即不限制。
func TestBuildAccountRateLimit(t *testing.T) {
	base := AccountInput{Name: "acct", BaseURL: "https://api.newapi.com/v1"}

	blank := base
	account, verr := BuildAccount(blank)
	if verr != nil {
		t.Fatalf("留空不应报错: %v", verr)
	}
	if account.RateLimitPerMin != nil || account.RateLimit() != 0 {
		t.Fatalf("留空应表示不限制: %#v", account.RateLimitPerMin)
	}

	empty := base
	empty.RateLimitPerMin = ""
	if account, verr := BuildAccount(empty); verr != nil || account.RateLimit() != 0 {
		t.Fatalf("空字符串应视为不限制: %v %#v", verr, account)
	}

	valid := base
	valid.RateLimitPerMin = "60"
	account, verr = BuildAccount(valid)
	if verr != nil {
		t.Fatalf("合法频率限制不应报错: %v", verr)
	}
	if account.RateLimit() != 60 {
		t.Fatalf("应解析出每分钟 60 次: %#v", account.RateLimitPerMin)
	}

	for _, bad := range []any{0, -1, "abc", MaxAccountRateLimitPerMin + 1} {
		input := base
		input.RateLimitPerMin = bad
		if _, verr := BuildAccount(input); verr == nil {
			t.Fatalf("非法频率限制应报错: %#v", bad)
		}
	}
}

// TestAssignAccountGatedSkipsWithoutRebinding 验证准入判定（账号级频率限制）
// 只影响本次落点，不改写密钥的常驻绑定。
func TestAssignAccountGatedSkipsWithoutRebinding(t *testing.T) {
	limited := newAccount(t, "limited", 10)
	spare := newAccount(t, "spare", 10)
	data := newTestData(limited, spare)
	for _, account := range data.Accounts {
		provider, _ := BuildProvider(ProviderInput{BaseURL: "https://x.example.com", AccountID: account.ID})
		data.Providers = append(data.Providers, provider)
	}
	key, _ := BuildKey(KeyInput{Name: "k"})
	key.AccountID = limited.ID
	data.Keys = append(data.Keys, key)
	data.reindex()

	gate := func(account *Account) bool { return account.ID != limited.ID }
	if chosen := data.AssignAccountGated(key, "", gate); chosen == nil || chosen.ID != spare.ID {
		t.Fatalf("被拦下的账号应换成备用账号: %#v", chosen)
	}
	if key.AccountID != limited.ID {
		t.Fatalf("限流换号不应改写常驻绑定: %s", key.AccountID)
	}

	// 窗口过去后（gate 放行）仍回到原账号。
	if chosen := data.AssignAccountGated(key, "", nil); chosen == nil || chosen.ID != limited.ID {
		t.Fatalf("放行后应回到原绑定账号: %#v", chosen)
	}

	// 全部账号都被拦下时没有可用账号。
	if chosen := data.AssignAccountGated(key, "", func(*Account) bool { return false }); chosen != nil {
		t.Fatalf("全部超限时应返回 nil: %#v", chosen)
	}
}

// TestExplicitlySupportsModelVsSupportsModel 划清“明确声明”与“不限模型”的边界。
func TestExplicitlySupportsModelVsSupportsModel(t *testing.T) {
	catchAll, _ := BuildProvider(ProviderInput{BaseURL: "https://a.example.com"})
	if !catchAll.SupportsModel("claude-3-opus") {
		t.Fatalf("模型列表留空应接受任何模型")
	}
	if catchAll.ExplicitlySupportsModel("claude-3-opus") {
		t.Fatalf("留空不等于声明支持该模型")
	}

	declared, _ := BuildProvider(ProviderInput{BaseURL: "https://b.example.com", Models: "gpt-4*,claude-3-opus"})
	if !declared.ExplicitlySupportsModel("claude-3-opus") || !declared.ExplicitlySupportsModel("gpt-4o") {
		t.Fatalf("精确名与通配符都应算明确声明")
	}
	if declared.ExplicitlySupportsModel("gemini-1.5-pro") || declared.SupportsModel("gemini-1.5-pro") {
		t.Fatalf("未声明的模型不应匹配")
	}
	if declared.ExplicitlySupportsModel("") {
		t.Fatalf("空模型名不算明确声明")
	}

	aliased, _ := BuildProvider(ProviderInput{BaseURL: "https://c.example.com", Models: "gpt-4o", ModelMap: map[string]string{"opus": "claude-3-opus"}})
	if !aliased.ExplicitlySupportsModel("opus") {
		t.Fatalf("别名应算明确声明")
	}
}

// TestAssignAccountPrefersAccountDeclaringModel 是本轮需求的核心：
// 请求某个模型时，优先用「明确挂了这个模型」的账号，而不是模型列表留空的兜底账号。
func TestAssignAccountPrefersAccountDeclaringModel(t *testing.T) {
	declared := newAccount(t, "declared", 10)
	catchAll := newAccount(t, "catch-all", 999)

	data := newTestData(declared, catchAll)
	// declared 只挂 claude；catchAll 不设模型限制，什么都收。
	claude, _ := BuildProvider(ProviderInput{BaseURL: "https://claude.example.com", AccountID: declared.ID, Models: "claude-3-opus"})
	loose, _ := BuildProvider(ProviderInput{BaseURL: "https://any.example.com", AccountID: catchAll.ID})
	data.Providers = append(data.Providers, claude, loose)
	data.reindex()

	if !data.AccountDeclaresModel(declared.ID, "claude-3-opus") {
		t.Fatalf("declared 账号应算明确声明")
	}
	if data.AccountDeclaresModel(catchAll.ID, "claude-3-opus") {
		t.Fatalf("留空的账号不应算明确声明")
	}

	// 即便 catchAll 余额高得多（排序上更占优），claude 请求也要落到明确声明的账号。
	for index := 0; index < 3; index++ {
		key, _ := BuildKey(KeyInput{Name: "k"})
		data.Keys = append(data.Keys, key)
		data.reindex()
		account := data.AssignAccountForModel(key, "claude-3-opus")
		if account == nil || account.ID != declared.ID {
			t.Fatalf("第 %d 次应命中明确声明该模型的账号: %#v", index+1, account)
		}
	}

	// 没有账号明确声明的模型才回退到兜底账号。
	key, _ := BuildKey(KeyInput{Name: "fallback"})
	data.Keys = append(data.Keys, key)
	data.reindex()
	if account := data.AssignAccountForModel(key, "gemini-1.5-pro"); account == nil || account.ID != catchAll.ID {
		t.Fatalf("无人声明时应回退到不限模型的账号: %#v", account)
	}
}

// TestAssignAccountForModelBorrowsWithoutRebinding 保证「临时借号」不改写常驻绑定。
//
// 绑定在兜底账号上的密钥请求 claude 时应借用声明了 claude 的账号跑完这次请求，
// key.AccountID 不能被改掉，否则一次冷门模型请求就把密钥永久迁走了。
func TestAssignAccountForModelBorrowsWithoutRebinding(t *testing.T) {
	catchAll := newAccount(t, "catch-all", 50)
	declared := newAccount(t, "declared", 10)

	data := newTestData(catchAll, declared)
	loose, _ := BuildProvider(ProviderInput{BaseURL: "https://any.example.com", AccountID: catchAll.ID})
	claude, _ := BuildProvider(ProviderInput{BaseURL: "https://claude.example.com", AccountID: declared.ID, Models: "claude-3-opus"})
	data.Providers = append(data.Providers, loose, claude)

	key, _ := BuildKey(KeyInput{Name: "bound"})
	key.AccountID = catchAll.ID
	data.Keys = append(data.Keys, key)
	data.reindex()

	if account := data.AssignAccountForModel(key, "claude-3-opus"); account == nil || account.ID != declared.ID {
		t.Fatalf("应借用声明了该模型的账号: %#v", account)
	}
	if key.AccountID != catchAll.ID {
		t.Fatalf("常驻绑定不应被改写: %s", key.AccountID)
	}

	// 不限模型的请求仍然走常驻绑定。
	if account := data.AssignAccountForModel(key, ""); account == nil || account.ID != catchAll.ID {
		t.Fatalf("未指定模型时应保持粘性: %#v", account)
	}
}

// TestBuildAccountQueryScript 覆盖脚本查询的三条边界：脚本非法要拦、
// 额度查询地址必须是完整地址、只配脚本也算「能查额度」（不再按无限余额处理）。
func TestBuildAccountQueryScript(t *testing.T) {
	const balanceScript = `({
	  request: {
	    url: "{{baseUrl}}/api/user/self",
	    method: "GET",
	    headers: { "Authorization": "Bearer {{accessToken}}", "New-Api-User": "{{userId}}" }
	  },
	  extractor: function (response) {
	    return { remaining: response.data.quota / 500000, unit: "USD" };
	  }
	})`

	if _, verr := BuildAccount(AccountInput{Name: "x", BaseURL: "https://a.com/v1", QueryScript: "({ request: { url: 1 } })"}); verr == nil {
		t.Fatalf("非法脚本应报错")
	}
	if _, verr := BuildAccount(AccountInput{Name: "x", BaseURL: "https://a.com/v1", QueryScript: "fetch('http://evil')"}); verr == nil {
		t.Fatalf("脚本里的危险调用应被拒绝")
	}
	if _, verr := BuildAccount(AccountInput{Name: "x", BaseURL: "https://a.com/v1", QueryURL: "console.example.com/api"}); verr == nil {
		t.Fatalf("额度查询地址不是完整地址应报错")
	}

	// 额度查询地址留空必须放行：不少自建上游根本没有额度接口。
	plain, verr := BuildAccount(AccountInput{Name: "x", BaseURL: "https://a.com/v1"})
	if verr != nil {
		t.Fatalf("留空额度查询地址不应报错: %v", verr)
	}
	if plain.QueryURL != "" || plain.HasQueryScript() || plain.HasBalanceQuery() || !plain.Unlimited() {
		t.Fatalf("未配置任何查询手段应视为无限余额: %#v", plain)
	}

	scripted, verr := BuildAccount(AccountInput{
		Name:        "scripted",
		BaseURL:     "https://a.com/v1",
		QueryURL:    "https://console.example.com/api",
		QueryScript: balanceScript,
	})
	if verr != nil {
		t.Fatalf("合法脚本不应报错: %v", verr)
	}
	if scripted.QueryURL != "https://console.example.com/api" {
		t.Fatalf("额度查询地址未保存: %q", scripted.QueryURL)
	}
	if !scripted.HasQueryScript() || scripted.QueryProgram() == nil {
		t.Fatalf("脚本应已编译: %#v", scripted)
	}
	// 只有脚本、没有访问令牌时依然要能参与额度查询与耗尽判定。
	if scripted.HasBuiltinQuery() || !scripted.HasBalanceQuery() || scripted.Unlimited() {
		t.Fatalf("仅脚本也应算配置了额度查询: %#v", scripted)
	}
	if scripted.QueryProgram().Method() != "GET" {
		t.Fatalf("脚本方法解析错误: %q", scripted.QueryProgram().Method())
	}

	// 清除脚本后回到无限额度，避免删掉脚本却仍按 0 余额把账号踢出池子。
	if err := scripted.SetQueryScript(""); err != nil {
		t.Fatalf("清除脚本不应报错: %v", err)
	}
	if scripted.HasQueryScript() || scripted.HasBalanceQuery() {
		t.Fatalf("清除脚本后应恢复无限额度: %#v", scripted)
	}
	if err := scripted.SetQueryScript("({ request: {} })"); err == nil {
		t.Fatalf("非法脚本写入应报错")
	}
}

// TestPublicAccountHidesQueryScript 确认脚本源码不会经视图回显：
// 脚本里常被写进硬编码令牌，回显等于把凭据交出去。
func TestPublicAccountHidesQueryScript(t *testing.T) {
	const balanceScript = `({
	  request: {
	    url: "{{baseUrl}}/api/user/self",
	    method: "GET",
	    headers: { "Authorization": "Bearer {{accessToken}}", "New-Api-User": "{{userId}}" }
	  },
	  extractor: function (response) {
	    return { remaining: response.data.quota / 500000, unit: "USD" };
	  }
	})`

	account, verr := BuildAccount(AccountInput{
		Name:        "scripted",
		BaseURL:     "https://api.newapi.com/v1",
		QueryURL:    "https://console.example.com/api",
		QueryScript: balanceScript,
	})
	if verr != nil {
		t.Fatalf("构建失败: %v", verr)
	}
	view := PublicAccount(account, 1, 0)

	for _, field := range []string{"queryScript", "queryUrl", "balanceScript"} {
		if _, leaked := view[field]; leaked {
			t.Fatalf("视图不应包含 %s", field)
		}
	}
	if view["hasQueryScript"] != true || view["hasQueryUrl"] != true {
		t.Fatalf("应标记脚本与查询地址已配置: %#v", view)
	}
	for key, value := range view {
		if text, ok := value.(string); ok && strings.Contains(text, "extractor") {
			t.Fatalf("字段 %s 泄露了脚本源码", key)
		}
	}
}

// TestQueryScriptPersistence 确认脚本以密文落盘、重新加载后能解密并自动编译。
func TestQueryScriptPersistence(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", "tok")
	t.Setenv("MASTER_KEY", "unit-test-master-key")
	file := t.TempDir() + "/db.json"

	const balanceScript = `({
	  request: {
	    url: "{{baseUrl}}/api/user/self",
	    method: "GET",
	    headers: { "Authorization": "Bearer {{accessToken}}", "New-Api-User": "{{userId}}" }
	  },
	  extractor: function (response) {
	    return { remaining: response.data.quota / 500000, unit: "USD" };
	  }
	})`

	first := New(file)
	if err := first.Load(); err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	account, verr := BuildAccount(AccountInput{
		Name:        "scripted",
		BaseURL:     "https://api.newapi.com/v1",
		QueryURL:    "https://console.example.com/api",
		QueryScript: balanceScript,
	})
	if verr != nil {
		t.Fatalf("构建失败: %v", verr)
	}
	if err := first.Update(func(data *Data) error {
		data.Accounts = append(data.Accounts, account)
		return nil
	}); err != nil {
		t.Fatalf("Update 失败: %v", err)
	}

	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("读取落盘文件失败: %v", err)
	}
	if strings.Contains(string(raw), "extractor") {
		t.Fatalf("脚本明文落盘了")
	}
	if !strings.Contains(string(raw), "\"queryScript\":\"enc.") {
		t.Fatalf("脚本应以密文形式落盘: %s", string(raw))
	}

	second := New(file)
	if err := second.Load(); err != nil {
		t.Fatalf("重新 Load 失败: %v", err)
	}
	second.View(func(data *Data) {
		if data.Version != 7 {
			t.Fatalf("数据版本应升到 7, got %d", data.Version)
		}
		reloaded := data.FindAccount(account.ID)
		if reloaded == nil {
			t.Fatalf("账号未持久化")
		}
		if reloaded.QueryURL != "https://console.example.com/api" {
			t.Fatalf("额度查询地址未持久化: %q", reloaded.QueryURL)
		}
		if reloaded.ScriptError != "" {
			t.Fatalf("脚本重新编译失败: %s", reloaded.ScriptError)
		}
		if !reloaded.HasQueryScript() || reloaded.QueryProgram() == nil {
			t.Fatalf("脚本应在加载后自动编译: %#v", reloaded)
		}
		if reloaded.QueryScript != strings.TrimSpace(balanceScript) {
			t.Fatalf("脚本源码解密后不一致: %q", reloaded.QueryScript)
		}
	})
}

// TestQueryTimeoutFloor 覆盖额度查询超时的归一化。
//
// 旧默认值 10 秒在真实站点上经常不够（满屏 context deadline exceeded），
// 因此历史数据里的小值一律抬到默认值，只有管理员填了更大的值才照用。
func TestQueryTimeoutFloor(t *testing.T) {
	if DefaultQueryTimeoutSeconds != 30 || MaxQueryTimeoutSeconds != 300 {
		t.Fatalf("超时常量错误: %d %d", DefaultQueryTimeoutSeconds, MaxQueryTimeoutSeconds)
	}

	defaults, verr := BuildAccount(AccountInput{Name: "x", BaseURL: "https://a.com/v1"})
	if verr != nil {
		t.Fatalf("不应报错: %v", verr)
	}
	if defaults.TimeoutSeconds != DefaultQueryTimeoutSeconds {
		t.Fatalf("默认超时应为 %d 秒, got %d", DefaultQueryTimeoutSeconds, defaults.TimeoutSeconds)
	}
	if defaults.QueryTimeout() != 30*time.Second {
		t.Fatalf("默认超时换算错误: %v", defaults.QueryTimeout())
	}

	if _, verr := BuildAccount(AccountInput{Name: "x", BaseURL: "https://a.com/v1", TimeoutSeconds: 301}); verr == nil {
		t.Fatalf("超过 %d 秒应报错", MaxQueryTimeoutSeconds)
	}
	long, verr := BuildAccount(AccountInput{Name: "x", BaseURL: "https://a.com/v1", TimeoutSeconds: 180})
	if verr != nil {
		t.Fatalf("180 秒应被接受: %v", verr)
	}
	if long.QueryTimeout() != 180*time.Second {
		t.Fatalf("自填的长超时应原样生效: %v", long.QueryTimeout())
	}

	// 旧数据（v6 及之前）里普遍是 10 秒，加载后要被抬到默认值。
	legacy := &Account{TimeoutSeconds: 10}
	if legacy.QueryTimeout() != 30*time.Second {
		t.Fatalf("历史小超时应抬到默认值: %v", legacy.QueryTimeout())
	}
	zero := &Account{}
	if zero.QueryTimeout() != 30*time.Second {
		t.Fatalf("未设置时应用默认值: %v", zero.QueryTimeout())
	}
	huge := &Account{TimeoutSeconds: 100000}
	if huge.QueryTimeout() != 300*time.Second {
		t.Fatalf("超大值应被夹到上限: %v", huge.QueryTimeout())
	}
}
