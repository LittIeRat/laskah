package store

import (
	"strings"
	"time"
)

// MaxKeysPerAccount 限制单个账号最多绑定的上游 API 数量。
const MaxKeysPerAccount = 5

// DefaultBatchKeys 是界面批量粘贴框的推荐条数。
const DefaultBatchKeys = 5

// DefaultSiteURL 是默认的额度查询站点地址。
const DefaultSiteURL = "https://api.newapi.com"

// DefaultQueryTimeoutSeconds 是额度查询的默认超时秒数。
const DefaultQueryTimeoutSeconds = 10

// DefaultRequestRefreshSeconds 是“请求时刷新余额”的默认最小间隔。
//
// 请求路径上不能每次都打一次上游额度接口，否则高并发会把额度接口打挂；
// 用一个短窗口做节流，既能及时发现余额耗尽，也不会放大上游压力。
const DefaultRequestRefreshSeconds = 60

// MaxRequestRefreshSeconds 限制请求时刷新的间隔上限。
const MaxRequestRefreshSeconds = 3600

// MinBalanceFloorUSD 是内置的余额安全线（USD）。
//
// 余额掉到这个水位以下时，账号很可能连一次正常请求的预扣费都付不起：
// 继续把流量打过去只会换来上游报错、重试与截断，体验比直接换号差得多。
// 因此不论账号自己填的最低余额是多少，这条线都强制生效。
const MinBalanceFloorUSD = 0.5

// AccountStats 记录账号维度的累计用量。
type AccountStats struct {
	Requests    int64      `json:"requests"`
	Success     int64      `json:"success"`
	Failure     int64      `json:"failure"`
	TotalTokens int64      `json:"totalTokens"`
	LastUsedAt  *time.Time `json:"lastUsedAt"`
}

// Account 是一个上游站点账号，名下包含多个 API 用于账号内负载均衡。
//
// AccessToken 只存在于内存，落盘时写入 SealedAccessToken 密文。
// 配置保存后界面不再回显任何凭据，只允许查询余额或删除账号。
type Account struct {
	ID      string `json:"id"`
	GroupID string `json:"groupId"`
	Name    string `json:"name"`
	SiteURL string `json:"siteUrl"`
	BaseURL string `json:"baseUrl"`

	// 额度查询凭据。
	UserID            string `json:"userId"`
	AccessToken       string `json:"-"`
	SealedAccessToken string `json:"accessToken"`
	TimeoutSeconds    int    `json:"timeoutSeconds"`
	QueryIntervalMin  int    `json:"queryIntervalMin"`

	// 请求时刷新：调用到达时若余额数据超过 RequestRefreshSec 秒未更新，先查一次再分配。
	RefreshOnRequest  bool `json:"refreshOnRequest"`
	RequestRefreshSec int  `json:"requestRefreshSec"`

	Enabled    bool    `json:"enabled"`
	AutoDelete bool    `json:"autoDelete"`
	MinBalance float64 `json:"minBalance"`

	Balance      float64    `json:"balance"`
	UsedAmount   float64    `json:"usedAmount"`
	TotalAmount  float64    `json:"totalAmount"`
	Currency     string     `json:"currency"`
	PlanName     string     `json:"planName"`
	QuotaPerUnit float64    `json:"quotaPerUnit"`
	BalanceFrom  string     `json:"balanceFrom"`
	CheckedAt    *time.Time `json:"checkedAt"`
	CheckError   string     `json:"checkError"`

	Models    []string     `json:"models"`
	Note      string       `json:"note"`
	CreatedAt time.Time    `json:"createdAt"`
	UpdatedAt time.Time    `json:"updatedAt"`
	Stats     AccountStats `json:"stats"`

	sealedFrom string
}

// RemovedAccount 记录被自动清理的账号，便于界面回溯。
type RemovedAccount struct {
	ID         string    `json:"id"`
	GroupID    string    `json:"groupId"`
	Name       string    `json:"name"`
	SiteURL    string    `json:"siteUrl"`
	Reason     string    `json:"reason"`
	Balance    float64   `json:"balance"`
	UsedAmount float64   `json:"usedAmount"`
	Tokens     int64     `json:"tokens"`
	Keys       int       `json:"keys"`
	RemovedAt  time.Time `json:"removedAt"`
}

// AccountInput 是创建账号时的原始输入。
type AccountInput struct {
	ID                string `json:"id"`
	GroupID           string `json:"groupId"`
	Name              string `json:"name"`
	SiteURL           string `json:"siteUrl"`
	BaseURL           string `json:"baseUrl"`
	UserID            any    `json:"userId"`
	AccessToken       string `json:"accessToken"`
	TimeoutSeconds    any    `json:"timeoutSeconds"`
	QueryIntervalMin  any    `json:"queryIntervalMin"`
	RefreshOnRequest  *bool  `json:"refreshOnRequest"`
	RequestRefreshSec any    `json:"requestRefreshSec"`
	Enabled           *bool  `json:"enabled"`
	AutoDelete        *bool  `json:"autoDelete"`
	MinBalance        any    `json:"minBalance"`
	Models            any    `json:"models"`
	Note              string `json:"note"`
}

// BuildAccount 校验输入并生成规范化账号对象。
func BuildAccount(input AccountInput) (*Account, *ValidationError) {
	verr := &ValidationError{}

	baseURL := NormalizeBaseURL(input.BaseURL)
	if baseURL == "" {
		verr.Errorf("base url 不能为空")
	}

	// 凭据请求地址留空则复用供应商 base url。
	siteURL := NormalizeBaseURL(input.SiteURL)
	if siteURL == "" {
		siteURL = strings.TrimSuffix(baseURL, "/v1")
	}

	minBalance, ok := toFloat(input.MinBalance, 0)
	if !ok || minBalance < 0 {
		verr.Errorf("最低余额必须是非负数字")
		minBalance = 0
	}

	timeoutSeconds, ok := toFloat(input.TimeoutSeconds, DefaultQueryTimeoutSeconds)
	if !ok || timeoutSeconds < 1 || timeoutSeconds > 120 {
		verr.Errorf("超时时间需要是 1-120 秒")
		timeoutSeconds = DefaultQueryTimeoutSeconds
	}

	intervalMin, ok := toFloat(input.QueryIntervalMin, 0)
	if !ok || intervalMin < 0 || intervalMin > 1440 {
		verr.Errorf("自动查询间隔需要是 0-1440 分钟")
		intervalMin = 0
	}

	requestRefreshSec, ok := toFloat(input.RequestRefreshSec, DefaultRequestRefreshSeconds)
	if !ok || requestRefreshSec < 0 || requestRefreshSec > MaxRequestRefreshSeconds {
		verr.Errorf("请求时刷新间隔需要是 0-%d 秒", MaxRequestRefreshSeconds)
		requestRefreshSec = DefaultRequestRefreshSeconds
	}

	name := strings.TrimSpace(input.Name)
	if name == "" {
		verr.Errorf("用户名称不能为空")
	}

	if verr.HasErrors() {
		return nil, verr
	}

	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	autoDelete := true
	if input.AutoDelete != nil {
		autoDelete = *input.AutoDelete
	}
	// 默认开启请求时刷新：这是“余额用完却继续打同一个账号”的直接防线。
	refreshOnRequest := true
	if input.RefreshOnRequest != nil {
		refreshOnRequest = *input.RefreshOnRequest
	}

	now := time.Now().UTC()
	return &Account{
		ID:                firstNonEmpty(input.ID, NewID("acct")),
		GroupID:           strings.TrimSpace(input.GroupID),
		Name:              name,
		SiteURL:           siteURL,
		BaseURL:           baseURL,
		UserID:            strings.TrimSpace(MustString(input.UserID)),
		AccessToken:       strings.TrimSpace(input.AccessToken),
		TimeoutSeconds:    int(timeoutSeconds),
		QueryIntervalMin:  int(intervalMin),
		RefreshOnRequest:  refreshOnRequest,
		RequestRefreshSec: int(requestRefreshSec),
		Enabled:           enabled,
		AutoDelete:        autoDelete,
		MinBalance:        minBalance,
		Currency:          "USD",
		Models:            SplitList(input.Models),
		Note:              strings.TrimSpace(input.Note),
		CreatedAt:         now,
		UpdatedAt:         now,
	}, nil
}

// BuildGroup 校验并生成用户分组。
func BuildGroup(input GroupInput) (*Group, *ValidationError) {
	verr := &ValidationError{}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		verr.Errorf("分组名称不能为空")
	}
	if len([]rune(name)) > 64 {
		verr.Errorf("分组名称不能超过 64 个字符")
	}
	if verr.HasErrors() {
		return nil, verr
	}
	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	now := time.Now().UTC()
	return &Group{
		ID:        firstNonEmpty(input.ID, NewID("grp")),
		Name:      name,
		Note:      strings.TrimSpace(input.Note),
		Enabled:   enabled,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

// GroupEnabled 判断分组是否允许承接流量。
//
// 分组不存在（例如密钥未限定分组）时视为允许，由账号自身状态决定。
func (d *Data) GroupEnabled(groupID string) bool {
	if groupID == "" {
		return true
	}
	group := d.FindGroup(groupID)
	if group == nil {
		return false
	}
	return group.Enabled
}

// FindGroup 按 ID 查找分组。
func (d *Data) FindGroup(id string) *Group {
	if id == "" {
		return nil
	}
	if d.groupByID != nil {
		return d.groupByID[id]
	}
	for _, group := range d.Groups {
		if group.ID == id {
			return group
		}
	}
	return nil
}

// FindGroupByName 按名称查找分组，用于避免重名。
func (d *Data) FindGroupByName(name string) *Group {
	trimmed := strings.TrimSpace(name)
	for _, group := range d.Groups {
		if strings.EqualFold(group.Name, trimmed) {
			return group
		}
	}
	return nil
}

// GroupAccounts 返回分组下的账号。
func (d *Data) GroupAccounts(groupID string) []*Account {
	result := []*Account{}
	for _, account := range d.Accounts {
		if account.GroupID == groupID {
			result = append(result, account)
		}
	}
	return result
}

// RemoveGroups 删除分组及其名下账号、上游 API，并解绑相关密钥。
func (d *Data) RemoveGroups(ids []string) int {
	wanted := map[string]bool{}
	for _, id := range ids {
		if strings.TrimSpace(id) != "" {
			wanted[id] = true
		}
	}
	if len(wanted) == 0 {
		return 0
	}

	accountIDs := []string{}
	for _, account := range d.Accounts {
		if wanted[account.GroupID] {
			accountIDs = append(accountIDs, account.ID)
		}
	}
	d.RemoveAccounts(accountIDs, "所属分组已删除")

	kept := make([]*Group, 0, len(d.Groups))
	removed := 0
	for _, group := range d.Groups {
		if wanted[group.ID] {
			removed++
			continue
		}
		kept = append(kept, group)
	}
	d.Groups = kept

	now := time.Now().UTC()
	for _, key := range d.Keys {
		if wanted[key.GroupID] {
			key.GroupID = ""
			key.UpdatedAt = now
		}
	}
	d.reindex()
	return removed
}

// FindAccount 按 ID 查找账号。
func (d *Data) FindAccount(id string) *Account {
	if id == "" {
		return nil
	}
	if d.accountByID != nil {
		return d.accountByID[id]
	}
	for _, account := range d.Accounts {
		if account.ID == id {
			return account
		}
	}
	return nil
}

// AccountProviders 返回归属于指定账号的上游 API。
func (d *Data) AccountProviders(accountID string) []*Provider {
	if d.accountProvs != nil {
		if cached, ok := d.accountProvs[accountID]; ok {
			return cached
		}
		return []*Provider{}
	}
	result := []*Provider{}
	for _, provider := range d.Providers {
		if provider.AccountID == accountID {
			result = append(result, provider)
		}
	}
	return result
}

// CountAccountKeys 统计账号下的上游 API 数量。
func (d *Data) CountAccountKeys(accountID string) int {
	return len(d.AccountProviders(accountID))
}

// KeysUsingAccount 统计已分配到该账号的网关密钥数量。
func (d *Data) KeysUsingAccount(accountID string) int {
	if d.accountKeyLoad != nil {
		return d.accountKeyLoad[accountID]
	}
	count := 0
	for _, key := range d.Keys {
		if key.AccountID == accountID {
			count++
		}
	}
	return count
}

// RemoveAccounts 删除账号及其名下上游 API，并解绑相关网关密钥。
func (d *Data) RemoveAccounts(ids []string, reason string) []RemovedAccount {
	wanted := map[string]bool{}
	for _, id := range ids {
		if strings.TrimSpace(id) != "" {
			wanted[id] = true
		}
	}
	if len(wanted) == 0 {
		return nil
	}

	removed := []RemovedAccount{}
	keptAccounts := make([]*Account, 0, len(d.Accounts))
	for _, account := range d.Accounts {
		if !wanted[account.ID] {
			keptAccounts = append(keptAccounts, account)
			continue
		}
		removed = append(removed, RemovedAccount{
			ID:         account.ID,
			GroupID:    account.GroupID,
			Name:       account.Name,
			SiteURL:    account.SiteURL,
			Reason:     reason,
			Balance:    account.Balance,
			UsedAmount: account.UsedAmount,
			Tokens:     account.Stats.TotalTokens,
			Keys:       d.CountAccountKeys(account.ID),
			RemovedAt:  time.Now().UTC(),
		})
	}
	if len(removed) == 0 {
		return nil
	}
	d.Accounts = keptAccounts

	keptProviders := make([]*Provider, 0, len(d.Providers))
	for _, provider := range d.Providers {
		if wanted[provider.AccountID] {
			continue
		}
		keptProviders = append(keptProviders, provider)
	}
	d.Providers = keptProviders

	now := time.Now().UTC()
	for _, key := range d.Keys {
		if wanted[key.AccountID] {
			key.AccountID = ""
			key.UpdatedAt = now
		}
	}

	d.RemovedAccounts = append(d.RemovedAccounts, removed...)
	if len(d.RemovedAccounts) > 200 {
		d.RemovedAccounts = d.RemovedAccounts[len(d.RemovedAccounts)-200:]
	}
	d.reindex()
	return removed
}

// HasBalanceQuery 判断账号是否配置了余额查询凭据。
//
// 未配置时余额无从得知，按“无限额度”处理：既不判定耗尽，也不做请求时刷新，
// 避免把没有额度概念的自建上游误删。
func (a *Account) HasBalanceQuery() bool {
	return strings.TrimSpace(a.AccessToken) != "" && strings.TrimSpace(a.UserID) != ""
}

// Unlimited 判断账号是否按无限额度对待。
func (a *Account) Unlimited() bool {
	return !a.HasBalanceQuery()
}

// BalanceFloor 返回该账号真正生效的余额下限。
//
// 取「账号自填的最低余额」与内置安全线 MinBalanceFloorUSD 的较大值：
// 填 0（默认）也会被抬到 0.5 USD，填得更高则尊重账号自己的设置。
func (a *Account) BalanceFloor() float64 {
	if a.MinBalance > MinBalanceFloorUSD {
		return a.MinBalance
	}
	return MinBalanceFloorUSD
}

// Usable 判断账号当前是否可以承接流量。
//
// 查询失败（CheckError 非空）时保持可用，避免网络抖动导致全站不可用。
func (a *Account) Usable() bool {
	if !a.Enabled {
		return false
	}
	if a.Unlimited() {
		return true
	}
	if a.CheckedAt == nil || a.CheckError != "" {
		return true
	}
	return a.Balance > a.BalanceFloor()
}

// Exhausted 判断账号余额是否已触及下限（需已成功查询过余额）。
//
// 余额 <= BalanceFloor() 即视为耗尽：账号只剩不到一次请求的钱时提前退场，
// 比等上游报「预扣费失败」再删号更早，调用方也不会先吃一次失败。
func (a *Account) Exhausted() bool {
	if a.Unlimited() {
		return false
	}
	return a.CheckedAt != nil && a.CheckError == "" && a.Balance <= a.BalanceFloor()
}

// QueryTimeout 返回额度查询超时时长。
func (a *Account) QueryTimeout() time.Duration {
	seconds := a.TimeoutSeconds
	if seconds <= 0 {
		seconds = DefaultQueryTimeoutSeconds
	}
	return time.Duration(seconds) * time.Second
}

// DueForQuery 判断按账号自身间隔是否到了自动查询时间。
func (a *Account) DueForQuery(now time.Time) bool {
	if a.QueryIntervalMin <= 0 || a.Unlimited() {
		return false
	}
	if a.CheckedAt == nil {
		return true
	}
	return now.Sub(*a.CheckedAt) >= time.Duration(a.QueryIntervalMin)*time.Minute
}

// RequestRefreshInterval 返回请求时刷新的节流窗口。
func (a *Account) RequestRefreshInterval() time.Duration {
	seconds := a.RequestRefreshSec
	if seconds <= 0 {
		seconds = DefaultRequestRefreshSeconds
	}
	if seconds > MaxRequestRefreshSeconds {
		seconds = MaxRequestRefreshSeconds
	}
	return time.Duration(seconds) * time.Second
}

// NeedsRequestRefresh 判断请求到达时是否应先刷新余额再分配流量。
//
// 从未成功查询过的账号必须先查一次，否则余额未知就直接放流量；
// 已查询过的账号只在超过节流窗口后才重查。
func (a *Account) NeedsRequestRefresh(now time.Time) bool {
	if !a.RefreshOnRequest || !a.Enabled || a.Unlimited() {
		return false
	}
	if a.CheckedAt == nil {
		return true
	}
	return now.Sub(*a.CheckedAt) >= a.RequestRefreshInterval()
}

// PublicAccount 返回账号的脱敏视图。
//
// 保存后不回显任何凭据：只暴露是否已配置的布尔标记与余额、用量。
func PublicAccount(a *Account, apiCount, boundKeys int) map[string]any {
	return map[string]any{
		"id":                a.ID,
		"groupId":           a.GroupID,
		"name":              a.Name,
		"hasSiteUrl":        a.SiteURL != "",
		"hasBaseUrl":        a.BaseURL != "",
		"hasAccessToken":    a.AccessToken != "",
		"hasUserId":         a.UserID != "",
		"timeoutSeconds":    a.TimeoutSeconds,
		"queryIntervalMin":  a.QueryIntervalMin,
		"refreshOnRequest":  a.RefreshOnRequest,
		"requestRefreshSec": a.RequestRefreshSec,
		"enabled":           a.Enabled,
		"autoDelete":        a.AutoDelete,
		"minBalance":        a.MinBalance,
		"balanceFloor":      a.BalanceFloor(),
		"balance":           a.Balance,
		"usedAmount":        a.UsedAmount,
		"totalAmount":       a.TotalAmount,
		"currency":          a.Currency,
		"planName":          a.PlanName,
		"balanceFrom":       a.BalanceFrom,
		"checkedAt":         a.CheckedAt,
		"checkError":        a.CheckError,
		"models":            a.Models,
		"createdAt":         a.CreatedAt,
		"updatedAt":         a.UpdatedAt,
		"stats":             a.Stats,
		"apiCount":          apiCount,
		"maxApiCount":       MaxKeysPerAccount,
		"boundKeys":         boundKeys,
		"usable":            a.Usable(),
		"exhausted":         a.Exhausted(),
		"unlimited":         a.Unlimited(),
		"hasBalanceQuery":   a.HasBalanceQuery(),
	}
}

// PublicGroup 返回分组视图及其汇总数据。
func PublicGroup(g *Group, summary map[string]any) map[string]any {
	view := map[string]any{
		"id":        g.ID,
		"name":      g.Name,
		"note":      g.Note,
		"enabled":   g.Enabled,
		"createdAt": g.CreatedAt,
		"updatedAt": g.UpdatedAt,
	}
	for key, value := range summary {
		view[key] = value
	}
	return view
}
