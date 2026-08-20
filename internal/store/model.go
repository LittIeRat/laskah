package store

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// ProviderType 表示上游 API 的协议规格。
type ProviderType string

// 支持的上游协议类型。
const (
	TypeOpenAI    ProviderType = "openai"
	TypeAnthropic ProviderType = "anthropic"
	TypeGemini    ProviderType = "gemini"
)

// ProviderTypes 列出全部合法协议类型。
var ProviderTypes = []ProviderType{TypeOpenAI, TypeAnthropic, TypeGemini}

// Paths 描述上游各端点的路径。
//
// 三个字段都允许填写完整的 http(s) 地址：JoinURL 遇到完整 URL 会直接使用它，
// 因此账号可以为不遵循 base + 固定后缀约定的上游指定精确端点。
type Paths struct {
	Chat      string `json:"chat"`
	Models    string `json:"models"`
	Responses string `json:"responses"`
}

// ProviderStats 记录上游 API 的累计调用指标。
type ProviderStats struct {
	Requests         int64 `json:"requests"`
	Success          int64 `json:"success"`
	Failure          int64 `json:"failure"`
	PromptTokens     int64 `json:"promptTokens"`
	CompletionTokens int64 `json:"completionTokens"`
	TotalTokens      int64 `json:"totalTokens"`
	// UpstreamTokens 是上游自报的累计 token，仅作对照。
	//
	// 计费与配额一律用本站自己算出的 TotalTokens：部分站点会谎报用量，
	// 但保留它们的数字才能在出现偏差时定位是哪一侧的问题。
	UpstreamTokens int64      `json:"upstreamTokens"`
	AvgLatencyMS   int64      `json:"avgLatencyMs"`
	LastError      *LastError `json:"lastError"`
	LastUsedAt     *time.Time `json:"lastUsedAt"`
}

// LastError 保存最近一次失败信息。
type LastError struct {
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

// Provider 是账号名下的一个上游 API 条目。
//
// APIKey 仅存在于内存，落盘时写入 SealedAPIKey（AES-256-GCM 密文）。
type Provider struct {
	ID           string            `json:"id"`
	AccountID    string            `json:"accountId"`
	Name         string            `json:"name"`
	Type         ProviderType      `json:"type"`
	BaseURL      string            `json:"baseUrl"`
	APIKey       string            `json:"-"`
	SealedAPIKey string            `json:"apiKey"`
	Models       []string          `json:"models"`
	ModelMap     map[string]string `json:"modelMap"`
	Headers      map[string]string `json:"headers"`
	Paths        Paths             `json:"paths"`
	Weight       float64           `json:"weight"`
	Priority     int               `json:"priority"`
	TimeoutMS    int64             `json:"timeoutMs"`
	Enabled      bool              `json:"enabled"`
	Tags         []string          `json:"tags"`
	Note         string            `json:"note"`
	CreatedAt    time.Time         `json:"createdAt"`
	UpdatedAt    time.Time         `json:"updatedAt"`
	Stats        ProviderStats     `json:"stats"`

	// 运行时状态与加密缓存不落盘。
	sealedFrom          string
	CooldownUntil       time.Time `json:"-"`
	Inflight            int64     `json:"-"`
	ConsecutiveFailures int       `json:"-"`
}

// KeyStats 记录网关密钥的用量。
//
// token 字段全部来自本站自己的估算，Cost 是按账号计价方式折算的金额；
// UpstreamTokens 保留上游自报值，仅用于与本地口径对照。
type KeyStats struct {
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

// APIKey 是下游调用本网关使用的密钥。
//
// Key 仅存在于内存；落盘时写入 SealedKey 与 KeyHash，鉴权按摘要索引命中，
// 既避免明文入库，也让查找保持 O(1)。
type APIKey struct {
	Name            string     `json:"name"`
	ID              string     `json:"id"`
	AccountID       string     `json:"accountId"`
	GroupID         string     `json:"groupId"`
	Key             string     `json:"-"`
	SealedKey       string     `json:"key"`
	KeyHash         string     `json:"keyHash"`
	KeyMasked       string     `json:"keyMasked"`
	Enabled         bool       `json:"enabled"`
	AllowedModels   []string   `json:"allowedModels"`
	ProviderIDs     []string   `json:"providerIds"`
	Tags            []string   `json:"tags"`
	QuotaTokens     *int64     `json:"quotaTokens"`
	RateLimitPerMin *int       `json:"rateLimitPerMin"`
	ExpiresAt       *time.Time `json:"expiresAt"`
	Note            string     `json:"note"`
	CreatedAt       time.Time  `json:"createdAt"`
	UpdatedAt       time.Time  `json:"updatedAt"`
	Stats           KeyStats   `json:"stats"`

	sealedFrom string
}

// KeyState 表示密钥当前可用状态。
type KeyState string

// 密钥状态取值。
const (
	KeyActive        KeyState = "active"
	KeyDisabled      KeyState = "disabled"
	KeyExpired       KeyState = "expired"
	KeyQuotaExceeded KeyState = "quota_exceeded"
)

// Config 保存全局设置与管理员凭据。
//
// Users 是管理面账户列表（口令为 PBKDF2-SHA256 散列、账户名为密文+摘要）；
// EncryptionSalt 用于派生数据加密密钥。Setup 为 false 时服务处于待初始化状态，
// 只有创建超级管理员的接口可用。
type Config struct {
	AdminToken     string       `json:"adminToken"`
	Users          []*AdminUser `json:"users"`
	Setup          bool         `json:"setup"`
	EncryptionSalt string       `json:"encryptionSalt"`
	Strategy       string       `json:"strategy"`
	MaxRetries     int          `json:"maxRetries"`
	CreatedAt      time.Time    `json:"createdAt"`
}

// Group 是一个用户分组，账号按分组归类。
//
// Enabled 为 false 时分组内全部账号都不再承接流量，但配置与统计保留，
// 便于临时下线一个账号池而不必删号。
type Group struct {
	Name      string    `json:"name"`
	ID        string    `json:"id"`
	Note      string    `json:"note"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Data 是持久化到磁盘的完整数据。
type Data struct {
	Version         int              `json:"version"`
	Config          Config           `json:"config"`
	Groups          []*Group         `json:"groups"`
	Accounts        []*Account       `json:"accounts"`
	Providers       []*Provider      `json:"providers"`
	Keys            []*APIKey        `json:"keys"`
	RemovedAccounts []RemovedAccount `json:"removedAccounts"`

	// 索引不落盘，用于把鉴权与归属查询降到 O(1)。
	adminByHash    map[string]*AdminUser
	keyByHash      map[string]*APIKey
	accountByID    map[string]*Account
	groupByID      map[string]*Group
	providerByID   map[string]*Provider
	accountProvs   map[string][]*Provider
	accountKeyLoad map[string]int
}

// ProviderInput 是创建上游 API 时的原始输入。
type ProviderInput struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Type      string            `json:"type"`
	BaseURL   string            `json:"baseUrl"`
	APIKey    string            `json:"apiKey"`
	Models    any               `json:"models"`
	ModelMap  map[string]string `json:"modelMap"`
	Headers   map[string]string `json:"headers"`
	Paths     *Paths            `json:"paths"`
	Weight    any               `json:"weight"`
	Priority  any               `json:"priority"`
	TimeoutMS any               `json:"timeoutMs"`
	Enabled   *bool             `json:"enabled"`
	Tags      any               `json:"tags"`
	Note      string            `json:"note"`
	AccountID string            `json:"accountId"`

	// 批量导入时兼容的蛇形字段。
	BaseURLSnake string `json:"base_url"`
	URLField     string `json:"url"`
	Endpoint     string `json:"endpoint"`
	APIKeySnake  string `json:"api_key"`
	KeyField     string `json:"key"`
	TokenField   string `json:"token"`
	ModelField   any    `json:"model"`
}

// KeyInput 是创建或更新网关密钥时的原始输入。
type KeyInput struct {
	ID              string `json:"id"`
	AccountID       string `json:"accountId"`
	GroupID         string `json:"groupId"`
	Name            string `json:"name"`
	Prefix          string `json:"prefix"`
	Enabled         *bool  `json:"enabled"`
	AllowedModels   any    `json:"allowedModels"`
	ProviderIDs     any    `json:"providerIds"`
	Tags            any    `json:"tags"`
	QuotaTokens     any    `json:"quotaTokens"`
	RateLimitPerMin any    `json:"rateLimitPerMin"`
	ExpiresAt       string `json:"expiresAt"`
	Note            string `json:"note"`
}

// GroupInput 是创建分组时的输入。
type GroupInput struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Note    string `json:"note"`
	Enabled *bool  `json:"enabled"`
}

// DefaultPaths 返回指定协议的默认端点路径。
func DefaultPaths(t ProviderType) Paths {
	switch t {
	case TypeAnthropic:
		return Paths{Chat: "/messages", Models: "/models", Responses: "/responses"}
	default:
		return Paths{Chat: "/chat/completions", Models: "/models", Responses: "/responses"}
	}
}

// FullEndpoint 校验用户自填的完整端点地址。
//
// 只接受 http(s) 且带主机名的绝对地址：相对路径会被 JoinURL 拼到 base url 后面，
// 与「自定义完整地址」的语义相悖，容易产出 https://a.com/v1/https://b.com 这类结果。
func FullEndpoint(raw string) (string, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", true
	}
	lower := strings.ToLower(trimmed)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		return "", false
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Host == "" {
		return "", false
	}
	return strings.TrimRight(trimmed, "/"), true
}

// NormalizeBaseURL 补全协议头并去掉尾部斜杠。
func NormalizeBaseURL(raw string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
	if trimmed == "" {
		return ""
	}
	lower := strings.ToLower(trimmed)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		trimmed = "https://" + trimmed
	}
	return strings.TrimRight(trimmed, "/")
}

// ValidProviderType 判断协议类型是否受支持。
func ValidProviderType(t string) bool {
	for _, item := range ProviderTypes {
		if string(item) == t {
			return true
		}
	}
	return false
}

// HostLabel 从 baseUrl 提取主机名，作为默认名称。
func HostLabel(baseURL string) string {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" {
		return "provider"
	}
	return parsed.Host
}

// NormalizeModelID 把供应商前缀模型名归一成“同一模型的可比较形态”。
//
// 例如 modeloc / 某些 SDK 会发 anthropic/claude-fable-5，而后台常配置 claude-fable-5；
// 若只做精确字符串匹配，就会出现“明明有模型却 503 无账号可承接”。
func NormalizeModelID(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	if slash := strings.Index(model, "/"); slash > 0 && slash < len(model)-1 {
		return strings.TrimSpace(model[slash+1:])
	}
	return model
}

// modelIDsMatch 判断两个模型标识是否等价，兼容 vendor/model 与裸模型名互认。
func modelIDsMatch(left, right string) bool {
	left = NormalizeModelID(left)
	right = NormalizeModelID(right)
	return left != "" && left == right
}

// SupportsModel 判断上游是否可以处理该模型，支持通配符与别名。
//
// 模型列表为空表示“不限”，即接受任何模型。这是宽松匹配，
// 用于决定一个上游能否被尝试；要判断它是否真的声明了该模型，用 ExplicitlySupportsModel。
func (p *Provider) SupportsModel(model string) bool {
	if model == "" {
		return true
	}
	if p.ExplicitlySupportsModel(model) {
		return true
	}
	// 模型列表留空表示“不限”，接受任何模型。
	return len(p.Models) == 0
}

// ExplicitlySupportsModel 判断上游是否“明确声明”支持该模型。
//
// 与 SupportsModel 的区别只在模型列表为空时：那种账号是“什么都收”，
// 并不代表它真的有这个模型。分配账号时要优先用明确声明过的账号，
// 否则「请求 claude-3-opus」很容易落到一个模型列表留空、其实只挂了 gpt Key 的账号上。
func (p *Provider) ExplicitlySupportsModel(model string) bool {
	if model == "" {
		return false
	}
	if _, ok := p.ModelMap[model]; ok {
		return true
	}
	normalized := NormalizeModelID(model)
	if normalized != model {
		if _, ok := p.ModelMap[normalized]; ok {
			return true
		}
	}
	for _, pattern := range p.Models {
		if pattern == "*" || modelIDsMatch(pattern, model) {
			return true
		}
		if WildcardMatch(pattern, model) || (normalized != model && WildcardMatch(pattern, normalized)) {
			return true
		}
	}
	return false
}

// UpstreamModel 返回实际发送给上游的模型名。
func (p *Provider) UpstreamModel(model string) string {
	if mapped, ok := p.ModelMap[model]; ok && mapped != "" {
		return mapped
	}
	normalized := NormalizeModelID(model)
	if normalized != model {
		if mapped, ok := p.ModelMap[normalized]; ok && mapped != "" {
			return mapped
		}
	}
	return normalized
}

// WildcardMatch 支持 gpt-4* 这类通配符匹配。
func WildcardMatch(pattern, value string) bool {
	if !strings.Contains(pattern, "*") {
		return false
	}
	segments := strings.Split(pattern, "*")
	for i, segment := range segments {
		segments[i] = regexp.QuoteMeta(segment)
	}
	expr := "^" + strings.Join(segments, ".*") + "$"
	matched, err := regexp.MatchString(expr, value)
	return err == nil && matched
}

// AllowsModel 判断密钥是否允许调用该模型。
func (k *APIKey) AllowsModel(model string) bool {
	if model == "" || len(k.AllowedModels) == 0 {
		return true
	}
	for _, allowed := range k.AllowedModels {
		if allowed == "*" || modelIDsMatch(allowed, model) {
			return true
		}
	}
	return false
}

// State 计算密钥在指定时刻的可用状态。
func (k *APIKey) State(now time.Time) KeyState {
	if !k.Enabled {
		return KeyDisabled
	}
	if k.ExpiresAt != nil && k.ExpiresAt.Before(now) {
		return KeyExpired
	}
	if k.QuotaTokens != nil && k.Stats.TotalTokens >= *k.QuotaTokens {
		return KeyQuotaExceeded
	}
	return KeyActive
}

// MaskKey 遮蔽密钥中部，只保留头尾用于展示。
func MaskKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 10 {
		return key[:min(2, len(key))] + "******"
	}
	return key[:6] + "******" + key[len(key)-4:]
}

// ValidationError 聚合校验阶段的全部错误信息。
type ValidationError struct {
	Errors []string
}

func (e *ValidationError) Error() string {
	return strings.Join(e.Errors, "; ")
}

// Errorf 追加一条格式化的校验错误。
func (e *ValidationError) Errorf(format string, args ...any) {
	e.Errors = append(e.Errors, fmt.Sprintf(format, args...))
}

// HasErrors 判断是否存在校验错误。
func (e *ValidationError) HasErrors() bool {
	return len(e.Errors) > 0
}
