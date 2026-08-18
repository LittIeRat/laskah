package store

import (
	"strings"
	"time"

	"laskah/internal/script"
)

// MaxKeysPerAccount 限制单个账号最多绑定的上游 API 数量。
const MaxKeysPerAccount = 5

// DefaultBatchKeys 是界面批量粘贴框的推荐条数。
const DefaultBatchKeys = 5

// DefaultSiteURL 是默认的额度查询站点地址。
const DefaultSiteURL = "https://api.newapi.com"

// DefaultQueryTimeoutSeconds 是额度查询的默认超时秒数。
//
// 取 30 秒：额度接口普遍挂在 Cloudflare 后面，冷连接握手加上站点查库经常超过 10 秒，
// 超时太短的直接后果是满屏 context deadline exceeded，账号余额停在旧值。
const DefaultQueryTimeoutSeconds = 30

// MaxQueryTimeoutSeconds 是允许配置的额度查询超时上限。
//
// 放宽到 300 秒是为了兜住个别极慢的自建站点；这个超时只作用于额度查询，
// 不影响网关转发，因此放宽不会拖住调用方。
const MaxQueryTimeoutSeconds = 300

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

// BillingMode 是账号的本地计价方式。
type BillingMode string

// 支持的计价方式。
//
// 计价只影响本站自己的金额统计与手动余额扣减，不改变发往上游的任何内容。
const (
	// BillingNone 不计价：只统计 token，不产生金额。
	BillingNone BillingMode = "none"
	// BillingPerMToken 按量计费：每 100 万 token 多少钱。
	BillingPerMToken BillingMode = "per_mtoken"
	// BillingPerCall 按次计费：每次成功请求多少钱。
	BillingPerCall BillingMode = "per_call"
)

// BillingModes 列出全部合法计价方式。
var BillingModes = []BillingMode{BillingNone, BillingPerMToken, BillingPerCall}

// ValidBillingMode 判断计价方式是否受支持。
func ValidBillingMode(mode string) bool {
	for _, item := range BillingModes {
		if string(item) == mode {
			return true
		}
	}
	return false
}

// MaxUnitPrice 限制单价上限，用于拦住明显的误输入（如把分当成元填）。
const MaxUnitPrice = 100000

// ReserveTokensPerCall 是按量计费时「预留一次请求」的假定 token 数。
//
// 手动余额是本地精确扣减的，不存在上游预扣费那套安全线；但余额掉到连一次
// 普通请求都付不起时仍应提前退场，否则最后一次调用会在中途把余额打成负数。
const ReserveTokensPerCall = 4000

// MaxAccountRateLimitPerMin 限制账号级频率限制的上限。
//
// 频率限制是「一分钟允许多少次请求」，留空（nil）表示不限制。
// 设上限只是为了拦住明显的误输入，正常账号不会需要这么高的配额。
const MaxAccountRateLimitPerMin = 100000

// AccountStats 记录账号维度的累计用量。
//
// TotalTokens / PromptTokens / CompletionTokens 全部来自本站自己的 token 估算：
// 部分上游站点会谎报 usage，用它们的数字做计费和配额判断会失真。
// UpstreamTokens 保留上游自报值，仅用于对照排查。
type AccountStats struct {
	Requests         int64      `json:"requests"`
	Success          int64      `json:"success"`
	Failure          int64      `json:"failure"`
	PromptTokens     int64      `json:"promptTokens"`
	CompletionTokens int64      `json:"completionTokens"`
	TotalTokens      int64      `json:"totalTokens"`
	UpstreamTokens   int64      `json:"upstreamTokens"`
	Cost             float64    `json:"cost"`
	LastUsedAt       *time.Time `json:"lastUsedAt"`
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

	// QueryURL 是额度查询的完整请求地址，留空表示按站点地址推导默认端点。
	//
	// 有些上游把额度接口挂在与推理地址完全无关的路径甚至域名上，
	// 「base url + 固定后缀」的约定在那里不成立，因此允许直接填完整地址。
	QueryURL string `json:"queryUrl"`

	// QueryScript 是管理员自填的额度查询脚本（cc-switch 形态），空串表示不使用脚本。
	//
	// 只存在于内存，落盘时写入 SealedQueryScript 密文：脚本里可能被写进硬编码令牌，
	// 与访问令牌同等对待才不会出现「凭据加密了但脚本明文躺在数据文件里」。
	QueryScript       string `json:"-"`
	SealedQueryScript string `json:"queryScript"`

	// ScriptError 记录脚本编译失败的原因，仅供界面提示。
	ScriptError string `json:"scriptError"`

	// 额度查询凭据。
	UserID            string `json:"userId"`
	AccessToken       string `json:"-"`
	SealedAccessToken string `json:"accessToken"`
	TimeoutSeconds    int    `json:"timeoutSeconds"`
	QueryIntervalMin  int    `json:"queryIntervalMin"`

	// 请求时刷新：调用到达时若余额数据超过 RequestRefreshSec 秒未更新，先查一次再分配。
	RefreshOnRequest  bool `json:"refreshOnRequest"`
	RequestRefreshSec int  `json:"requestRefreshSec"`

	Endpoints Paths `json:"endpoints"`

	// 计价配置：本站按自己统计的 token 折算金额，不采信上游自报用量。
	BillingMode    BillingMode `json:"billingMode"`
	PricePerMToken float64     `json:"pricePerMToken"`
	PricePerCall   float64     `json:"pricePerCall"`

	// ManualBalance 表示余额由管理员手填并按本地计费扣减，而不是查询上游。
	//
	// 与额度查询互斥地覆盖「无限余额」：两者都没配置才视为无限。
	ManualBalance  bool    `json:"manualBalance"`
	InitialBalance float64 `json:"initialBalance"`

	Enabled    bool    `json:"enabled"`
	MinBalance float64 `json:"minBalance"`

	// AutoSuspend 表示余额触及下限时自动暂停该账号（不再删除）。
	//
	// 暂停保留账号、上游 API 与统计数据，只是退出分配池，
	// 管理员充值后重新启用即可立刻恢复承接流量。
	AutoSuspend bool `json:"autoSuspend"`

	// AutoDeleteLegacy 只用于读取 v4 及更早版本的 autoDelete 字段。
	//
	// 迁移完成后置为 nil，因此不会再写回磁盘。
	AutoDeleteLegacy *bool `json:"autoDelete,omitempty"`

	// 暂停状态：Suspended 为 true 时账号退出分配池，直到管理员重新启用。
	Suspended     bool       `json:"suspended"`
	SuspendReason string     `json:"suspendReason"`
	SuspendedAt   *time.Time `json:"suspendedAt"`

	// RateLimitPerMin 是账号级每分钟请求上限，nil 或 <=0 表示不限制。
	//
	// 触及上限时网关会换用其它账号，而不是给调用方返回 429。
	RateLimitPerMin *int `json:"rateLimitPerMin"`

	Balance      float64    `json:"balance"`
	UsedAmount   float64    `json:"usedAmount"`
	TotalAmount  float64    `json:"totalAmount"`
	Currency     string     `json:"currency"`
	PlanName     string     `json:"planName"`
	QuotaPerUnit float64    `json:"quotaPerUnit"`
	BalanceFrom  string     `json:"balanceFrom"`
	CheckedAt    *time.Time `json:"checkedAt"`
	CheckError   string     `json:"checkError"`

	// BalanceExtra 是脚本 extractor 返回的 extra 文本，用于展示自定义信息。
	BalanceExtra string `json:"balanceExtra"`

	Models    []string     `json:"models"`
	Note      string       `json:"note"`
	CreatedAt time.Time    `json:"createdAt"`
	UpdatedAt time.Time    `json:"updatedAt"`
	Stats     AccountStats `json:"stats"`

	sealedFrom string
	// sealedScriptFrom 与 sealedFrom 同理，用于跳过未变更脚本的重复加密。
	sealedScriptFrom string
	// program 是编译后的查询脚本，随 QueryScript 一起更新。
	program *script.Program
}

// SetQueryScript 编译并写入查询脚本，空串表示清除脚本。
//
// 编译在这里完成而不是查询时：语法错误必须在保存阶段就被拒绝，
// 否则要等到下一次额度查询才发现脚本根本跑不起来。
func (a *Account) SetQueryScript(source string) error {
	trimmed := strings.TrimSpace(source)
	if trimmed == "" {
		a.QueryScript = ""
		a.ScriptError = ""
		a.program = nil
		return nil
	}
	program, err := script.Parse(trimmed)
	if err != nil {
		return err
	}
	a.QueryScript = trimmed
	a.ScriptError = ""
	a.program = program
	return nil
}

// CompileQueryScript 重新编译已持久化的脚本，供加载数据后调用。
//
// 编译失败时保留源码但记录原因：直接丢弃脚本会让管理员在界面上
// 看到「没配置查询」而不是「脚本坏了」，反而更难排查。
func (a *Account) CompileQueryScript() error {
	trimmed := strings.TrimSpace(a.QueryScript)
	if trimmed == "" {
		a.program = nil
		a.ScriptError = ""
		return nil
	}
	program, err := script.Parse(trimmed)
	if err != nil {
		a.program = nil
		a.ScriptError = err.Error()
		return err
	}
	a.program = program
	a.ScriptError = ""
	return nil
}

// QueryProgram 返回已编译的查询脚本，未配置或编译失败时为 nil。
func (a *Account) QueryProgram() *script.Program {
	return a.program
}

// HasQueryScript 判断账号是否配置了可用的查询脚本。
func (a *Account) HasQueryScript() bool {
	return a.program != nil
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
	QueryURL          string `json:"queryUrl"`
	QueryScript       string `json:"queryScript"`
	TimeoutSeconds    any    `json:"timeoutSeconds"`
	QueryIntervalMin  any    `json:"queryIntervalMin"`
	RefreshOnRequest  *bool  `json:"refreshOnRequest"`
	RequestRefreshSec any    `json:"requestRefreshSec"`
	Enabled           *bool  `json:"enabled"`
	AutoSuspend       *bool  `json:"autoSuspend"`
	AutoDelete        *bool  `json:"autoDelete"`
	MinBalance        any    `json:"minBalance"`
	RateLimitPerMin   any    `json:"rateLimitPerMin"`
	Models            any    `json:"models"`
	Note              string `json:"note"`

	// 自定义完整端点地址：留空则按 baseUrl 拼接默认后缀。
	ChatURL      string `json:"chatUrl"`
	ModelsURL    string `json:"modelsUrl"`
	ResponsesURL string `json:"responsesUrl"`

	// 手动余额与计价。
	BillingMode    string `json:"billingMode"`
	PricePerMToken any    `json:"pricePerMToken"`
	PricePerCall   any    `json:"pricePerCall"`
	ManualBalance  *bool  `json:"manualBalance"`
	InitialBalance any    `json:"initialBalance"`
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
	if !ok || timeoutSeconds < 1 || timeoutSeconds > MaxQueryTimeoutSeconds {
		verr.Errorf("超时时间需要是 1-%d 秒", MaxQueryTimeoutSeconds)
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

	// 频率限制留空表示无限制；填了就必须是 1-MaxAccountRateLimitPerMin 的整数。
	var rateLimit *int
	if !isBlank(input.RateLimitPerMin) {
		value, okRate := toFloat(input.RateLimitPerMin, 0)
		switch {
		case !okRate || value < 1:
			verr.Errorf("频率限制需要是每分钟 1-%d 次的整数，留空表示不限制", MaxAccountRateLimitPerMin)
		case value > MaxAccountRateLimitPerMin:
			verr.Errorf("频率限制不能超过每分钟 %d 次", MaxAccountRateLimitPerMin)
		default:
			converted := int(value)
			rateLimit = &converted
		}
	}

	name := strings.TrimSpace(input.Name)
	if name == "" {
		verr.Errorf("用户名称不能为空")
	}

	// 额度查询地址同样要求完整地址，但允许留空：
	// 不少自建上游根本没有额度接口，强制填写只会逼人瞎填。
	queryURL, ok := FullEndpoint(input.QueryURL)
	if !ok {
		verr.Errorf("额度查询地址需要填写完整地址（以 http:// 或 https:// 开头），不查额度可留空")
	}

	// 自定义端点必须是完整地址：填了却不是绝对 URL 就直接报错，
	// 否则会被拼到 base url 后面，产出一个谁都没预期的地址。
	endpoints := Paths{}
	for _, item := range []struct {
		label string
		raw   string
		field *string
	}{
		{"对话端点地址", input.ChatURL, &endpoints.Chat},
		{"模型列表端点地址", input.ModelsURL, &endpoints.Models},
		{"Responses 端点地址", input.ResponsesURL, &endpoints.Responses},
	} {
		value, ok := FullEndpoint(item.raw)
		if !ok {
			verr.Errorf("%s需要填写完整地址（以 http:// 或 https:// 开头）", item.label)
			continue
		}
		*item.field = value
	}

	billingMode := BillingMode(strings.TrimSpace(input.BillingMode))
	if billingMode == "" {
		billingMode = BillingNone
	}
	if !ValidBillingMode(string(billingMode)) {
		verr.Errorf("计价方式必须是 none / per_mtoken / per_call")
		billingMode = BillingNone
	}

	pricePerMToken, ok := toFloat(input.PricePerMToken, 0)
	if !ok || pricePerMToken < 0 || pricePerMToken > MaxUnitPrice {
		verr.Errorf("每 100 万 token 单价需要是 0-%d 的数字", MaxUnitPrice)
		pricePerMToken = 0
	}
	pricePerCall, ok := toFloat(input.PricePerCall, 0)
	if !ok || pricePerCall < 0 || pricePerCall > MaxUnitPrice {
		verr.Errorf("每次请求单价需要是 0-%d 的数字", MaxUnitPrice)
		pricePerCall = 0
	}
	switch billingMode {
	case BillingPerMToken:
		if pricePerMToken <= 0 {
			verr.Errorf("按量计费需要填写每 100 万 token 的价格")
		}
	case BillingPerCall:
		if pricePerCall <= 0 {
			verr.Errorf("按次计费需要填写每次请求的价格")
		}
	}

	initialBalance, ok := toFloat(input.InitialBalance, 0)
	if !ok || initialBalance < 0 {
		verr.Errorf("手动余额必须是非负数字")
		initialBalance = 0
	}
	manualBalance := false
	if input.ManualBalance != nil {
		manualBalance = *input.ManualBalance
	}
	if manualBalance && billingMode == BillingNone {
		verr.Errorf("启用手动余额时必须选择按量或按次计价，否则余额永远不会扣减")
	}

	// 查询脚本先编译一遍：语法或结构错误必须在创建阶段就退回，
	// 否则账号会带着一段永远跑不起来的脚本进入分配池。
	var queryProgram *script.Program
	if trimmedScript := strings.TrimSpace(input.QueryScript); trimmedScript != "" {
		program, scriptErr := script.Parse(trimmedScript)
		if scriptErr != nil {
			verr.Errorf("额度查询脚本无效: %s", scriptErr.Error())
		} else {
			queryProgram = program
		}
	}

	if verr.HasErrors() {
		return nil, verr
	}

	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	// 余额触及下限时自动暂停（历史字段 autoDelete 仍然兼容读取）。
	autoSuspend := true
	switch {
	case input.AutoSuspend != nil:
		autoSuspend = *input.AutoSuspend
	case input.AutoDelete != nil:
		autoSuspend = *input.AutoDelete
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
		QueryURL:          queryURL,
		QueryScript:       scriptSource(queryProgram),
		program:           queryProgram,
		Endpoints:         endpoints,
		BillingMode:       billingMode,
		PricePerMToken:    pricePerMToken,
		PricePerCall:      pricePerCall,
		ManualBalance:     manualBalance,
		InitialBalance:    initialBalance,
		Balance:           manualBalanceStart(manualBalance, initialBalance),
		TotalAmount:       manualBalanceStart(manualBalance, initialBalance),
		Enabled:           enabled,
		AutoSuspend:       autoSuspend,
		RateLimitPerMin:   rateLimit,
		MinBalance:        minBalance,
		Currency:          "USD",
		Models:            SplitList(input.Models),
		Note:              strings.TrimSpace(input.Note),
		CreatedAt:         now,
		UpdatedAt:         now,
	}, nil
}

// scriptSource 返回脚本源码，未配置脚本时为空串。
func scriptSource(program *script.Program) string {
	if program == nil {
		return ""
	}
	return program.Source()
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

// HasBalanceQuery 判断账号是否配置了余额查询方式。
//
// 两条路径任一成立即算已配置：内置的 New API 凭据（访问令牌 + 用户 ID），
// 或者管理员自填的查询脚本。都没有且未启用手动余额时按“无限额度”处理，
// 既不判定耗尽，也不做请求时刷新，避免把没有额度概念的自建上游误停。
func (a *Account) HasBalanceQuery() bool {
	return a.HasQueryScript() || a.HasBuiltinQuery()
}

// HasBuiltinQuery 判断是否配置了内置 New API 查询凭据。
func (a *Account) HasBuiltinQuery() bool {
	return strings.TrimSpace(a.AccessToken) != "" && strings.TrimSpace(a.UserID) != ""
}

// QueryBase 返回额度查询使用的站点地址。
//
// 优先用账号自填的站点地址，留空时回落到推理 base url：
// 多数 New API 站点两者同源，只有少数把控制台与推理接口分开部署。
func (a *Account) QueryBase() string {
	if base := strings.TrimSpace(a.SiteURL); base != "" {
		return base
	}
	return strings.TrimSpace(a.BaseURL)
}

// HasManualBalance 判断账号是否使用管理员手填余额。
//
// 手动余额由本站按自己统计的 token 与单价扣减，完全不依赖上游自报数据，
// 因此上游谎报用量时余额依然准确。
func (a *Account) HasManualBalance() bool {
	return a.ManualBalance && a.BillingMode != BillingNone && a.BillingMode != ""
}

// Unlimited 判断账号是否按无限额度对待。
func (a *Account) Unlimited() bool {
	return !a.HasBalanceQuery() && !a.HasManualBalance()
}

// EstimatedCallCost 估算本账号一次普通请求的金额，未计价时为 0。
//
// 按量计费按 ReserveTokensPerCall 个 token 折算：这只是「够不够再来一次」的判断基准，
// 真实扣费仍按实际统计到的 token 结算。
func (a *Account) EstimatedCallCost() float64 {
	switch a.BillingMode {
	case BillingPerMToken:
		return a.PricePerMToken * ReserveTokensPerCall / 1e6
	case BillingPerCall:
		return a.PricePerCall
	default:
		return 0
	}
}

// CostFor 按账号计价方式折算一次请求的金额。
//
// tokens 是本站自己统计出的总 token 数，calls 是本次要计费的请求次数。
func (a *Account) CostFor(tokens int64, calls int64) float64 {
	switch a.BillingMode {
	case BillingPerMToken:
		if tokens <= 0 {
			return 0
		}
		return a.PricePerMToken * float64(tokens) / 1e6
	case BillingPerCall:
		if calls <= 0 {
			return 0
		}
		return a.PricePerCall * float64(calls)
	default:
		return 0
	}
}

// Charge 记账一次调用的金额，并在手动余额模式下扣减余额。
//
// 无论是否手动余额都累计 Stats.Cost：管理员即使用上游查询余额，也需要一份
// 完全由本站计量的消耗口径来对照上游账单。余额不会被扣成负数。
func (a *Account) Charge(tokens int64, calls int64) float64 {
	cost := a.CostFor(tokens, calls)
	if cost <= 0 {
		return 0
	}
	a.Stats.Cost = round6(a.Stats.Cost + cost)
	if !a.HasManualBalance() {
		return cost
	}
	a.Balance = round6(a.Balance - cost)
	if a.Balance < 0 {
		a.Balance = 0
	}
	a.UsedAmount = round6(a.UsedAmount + cost)
	now := time.Now().UTC()
	a.CheckedAt = &now
	a.CheckError = ""
	a.BalanceFrom = "local"
	return cost
}

// BalanceFloor 返回该账号真正生效的余额下限。
//
// 上游查询余额时取「账号自填的最低余额」与内置安全线 MinBalanceFloorUSD 的较大值：
// 上游预扣费按预估价格执行，余额只剩几分钱时必然失败，提前退场比吃一次报错好。
// 手动余额是本地精确扣减的，不存在预扣费误差，因此只需守住「够再来一次」，
// 否则 0.5 USD 的固定安全线会把按次计价 $0.001 的账号浪费掉绝大部分额度。
func (a *Account) BalanceFloor() float64 {
	if a.HasManualBalance() && !a.HasBalanceQuery() {
		floor := a.EstimatedCallCost()
		if a.MinBalance > floor {
			return a.MinBalance
		}
		return floor
	}
	if a.MinBalance > MinBalanceFloorUSD {
		return a.MinBalance
	}
	return MinBalanceFloorUSD
}

// Usable 判断账号当前是否可以承接流量。
//
// 上游查询失败（CheckError 非空）时保持可用，避免网络抖动导致全站不可用；
// 手动余额不受这条豁免影响，因为它的数字本来就是本地算出来的，不会“查失败”。
// 被暂停的账号一律不可用：余额不足只是暂停原因之一，恢复由管理员决定。
func (a *Account) Usable() bool {
	if !a.Enabled || a.Suspended {
		return false
	}
	if a.Unlimited() {
		return true
	}
	if a.HasManualBalance() && !a.HasBalanceQuery() {
		return a.Balance > a.BalanceFloor()
	}
	if a.CheckedAt == nil || a.CheckError != "" {
		return true
	}
	return a.Balance > a.BalanceFloor()
}

// Exhausted 判断账号余额是否已触及下限。
//
// 余额 <= BalanceFloor() 即视为耗尽：账号只剩不到一次请求的钱时提前退场，
// 比等上游报「预扣费失败」再换号更早，调用方也不会先吃一次失败。
// 上游查询模式要求确实成功查过一次，手动余额则以本地数字为准。
func (a *Account) Exhausted() bool {
	if a.Unlimited() {
		return false
	}
	if a.HasManualBalance() && !a.HasBalanceQuery() {
		return a.Balance <= a.BalanceFloor()
	}
	return a.CheckedAt != nil && a.CheckError == "" && a.Balance <= a.BalanceFloor()
}

// manualBalanceStart 返回创建账号时的初始余额，仅手动余额模式生效。
func manualBalanceStart(manual bool, initial float64) float64 {
	if !manual {
		return 0
	}
	return initial
}

// round6 把金额收敛到 6 位小数，避免浮点累加产生长尾误差。
func round6(value float64) float64 {
	scaled := value * 1e6
	if scaled >= 0 {
		scaled += 0.5
	} else {
		scaled -= 0.5
	}
	return float64(int64(scaled)) / 1e6
}

// RateLimit 返回账号级每分钟请求上限，0 表示不限制。
func (a *Account) RateLimit() int {
	if a.RateLimitPerMin == nil || *a.RateLimitPerMin <= 0 {
		return 0
	}
	return *a.RateLimitPerMin
}

// Suspend 把账号标记为暂停，返回是否发生了状态变化。
//
// 暂停不删除任何数据：上游 API、余额与统计全部保留，
// 密钥的粘性绑定也不解除，管理员重新启用后立即可用。
func (a *Account) Suspend(reason string) bool {
	if a.Suspended {
		return false
	}
	now := time.Now().UTC()
	a.Suspended = true
	a.SuspendReason = strings.TrimSpace(reason)
	a.SuspendedAt = &now
	a.UpdatedAt = now
	return true
}

// Resume 解除暂停并同时确保账号处于启用状态。
//
// 管理员点「启用」的意图就是让账号重新接流量，因此一次性清掉两种停用状态，
// 避免出现「解除暂停了但仍然被禁用」这种需要点两次的体验。
func (a *Account) Resume() {
	now := time.Now().UTC()
	a.Suspended = false
	a.SuspendReason = ""
	a.SuspendedAt = nil
	a.Enabled = true
	a.UpdatedAt = now
}

// SuspendAccounts 批量暂停账号，返回真正被暂停的账号名。
func (d *Data) SuspendAccounts(ids []string, reason string) []string {
	names := []string{}
	for _, id := range ids {
		account := d.FindAccount(id)
		if account == nil {
			continue
		}
		if account.Suspend(reason) {
			names = append(names, account.Name)
		}
	}
	return names
}

// QueryTimeout 返回额度查询超时时长。
//
// 历史数据里存的是旧默认值 10 秒，那个值在真实站点上经常不够；
// 因此低于默认值的配置一律抬到默认值，只有管理员自己填了更大的值才照用。
// 想要更短的超时没有实际收益：额度查询不在调用方的等待路径上。
func (a *Account) QueryTimeout() time.Duration {
	seconds := a.TimeoutSeconds
	if seconds < DefaultQueryTimeoutSeconds {
		seconds = DefaultQueryTimeoutSeconds
	}
	if seconds > MaxQueryTimeoutSeconds {
		seconds = MaxQueryTimeoutSeconds
	}
	return time.Duration(seconds) * time.Second
}

// DueForQuery 判断按账号自身间隔是否到了自动查询时间。
func (a *Account) DueForQuery(now time.Time) bool {
	if a.QueryIntervalMin <= 0 || !a.HasBalanceQuery() {
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
	if !a.RefreshOnRequest || !a.Enabled || !a.HasBalanceQuery() {
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
		"id":                 a.ID,
		"groupId":            a.GroupID,
		"name":               a.Name,
		"hasSiteUrl":         a.SiteURL != "",
		"hasBaseUrl":         a.BaseURL != "",
		"hasAccessToken":     a.AccessToken != "",
		"hasUserId":          a.UserID != "",
		"hasQueryUrl":        a.QueryURL != "",
		"hasQueryScript":     strings.TrimSpace(a.QueryScript) != "",
		"scriptError":        a.ScriptError,
		"balanceExtra":       a.BalanceExtra,
		"timeoutSeconds":     a.TimeoutSeconds,
		"queryIntervalMin":   a.QueryIntervalMin,
		"refreshOnRequest":   a.RefreshOnRequest,
		"requestRefreshSec":  a.RequestRefreshSec,
		"enabled":            a.Enabled,
		"autoSuspend":        a.AutoSuspend,
		"suspended":          a.Suspended,
		"suspendReason":      a.SuspendReason,
		"suspendedAt":        a.SuspendedAt,
		"rateLimitPerMin":    a.RateLimitPerMin,
		"minBalance":         a.MinBalance,
		"balanceFloor":       a.BalanceFloor(),
		"billingMode":        string(a.BillingMode),
		"pricePerMToken":     a.PricePerMToken,
		"pricePerCall":       a.PricePerCall,
		"manualBalance":      a.HasManualBalance(),
		"initialBalance":     a.InitialBalance,
		"hasCustomChatUrl":   a.Endpoints.Chat != "",
		"hasCustomModelsUrl": a.Endpoints.Models != "",
		"hasCustomRespUrl":   a.Endpoints.Responses != "",
		"balance":            a.Balance,
		"usedAmount":         a.UsedAmount,
		"totalAmount":        a.TotalAmount,
		"currency":           a.Currency,
		"planName":           a.PlanName,
		"balanceFrom":        a.BalanceFrom,
		"checkedAt":          a.CheckedAt,
		"checkError":         a.CheckError,
		"models":             a.Models,
		"createdAt":          a.CreatedAt,
		"updatedAt":          a.UpdatedAt,
		"stats":              a.Stats,
		"apiCount":           apiCount,
		"maxApiCount":        MaxKeysPerAccount,
		"boundKeys":          boundKeys,
		"usable":             a.Usable(),
		"exhausted":          a.Exhausted(),
		"unlimited":          a.Unlimited(),
		"hasBalanceQuery":    a.HasBalanceQuery(),
		"cost":               round6(a.Stats.Cost),
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
