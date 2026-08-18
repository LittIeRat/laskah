// Package wallet 查询 New API 站点账号的额度与已用量。
package wallet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"laskah/internal/httpx"
	"laskah/internal/script"
)

// DefaultQuotaPerUnit 是 New API 默认的额度换算单位（quota / 500000 = 1 USD）。
const DefaultQuotaPerUnit = 500000.0

// maxBodyBytes 限制读取上游响应的体积。
const maxBodyBytes = 1 << 20

// Snapshot 是一次额度查询的结果。
type Snapshot struct {
	Balance      float64
	UsedAmount   float64
	TotalAmount  float64
	PlanName     string
	Currency     string
	QuotaPerUnit float64
	Source       string
	CheckedAt    time.Time
	Err          error

	// Extra 是脚本 extractor 返回的自定义展示文本。
	Extra string

	// HasBalance 区分「查到余额为 0」与「脚本没返回余额」。
	//
	// 没有这个标记，一个只回报 isValid 的脚本会把账号余额直接写成 0，
	// 进而被判定为耗尽并暂停——那是查询能力不足，不是真的没钱。
	HasBalance bool
	HasUsed    bool
	HasTotal   bool
}

// Credentials 描述查询额度所需的凭据。
//
// BaseURL 为凭据请求地址（如 https://api.newapi.com）；留空时调用方应回落到供应商 base url。
type Credentials struct {
	BaseURL     string
	UserID      string
	AccessToken string
	APIKey      string
	Timeout     time.Duration

	// QueryURL 是管理员自填的完整额度查询地址。
	//
	// 填了它就完全替代「BaseURL + 固定后缀」的推导：有些站点把额度接口
	// 挂在别的域名或不规则路径上，约定式拼接在那里根本拼不出来。
	QueryURL string

	// Script 是管理员自填的查询脚本，非 nil 时优先于内置查询路径。
	Script *script.Program
}

// Client 负责发起额度查询请求。
//
// 复用连接池并限制单主机并发，避免批量刷新时打爆上游或耗尽本地端口。
type Client struct {
	http *http.Client
}

// NewClient 创建额度查询客户端。
func NewClient() *Client {
	transport := &http.Transport{
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 8,
		MaxConnsPerHost:     16,
		IdleConnTimeout:     90 * time.Second,
	}
	return &Client{http: &http.Client{Transport: transport}}
}

// Fetch 查询账号额度。
//
// 优先使用管理员自填的脚本：它能覆盖任意站点形态，是唯一的通用手段。
// 没有脚本时退回内置的 New API 路径（/api/user/self，再退 /api/usage）。
func (c *Client) Fetch(parent context.Context, creds Credentials) Snapshot {
	timeout := creds.Timeout
	if timeout <= 0 || timeout > 120*time.Second {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	base := strings.TrimRight(strings.TrimSpace(creds.BaseURL), "/")
	if creds.Script != nil {
		return c.fetchByScript(ctx, base, creds)
	}
	if base == "" && strings.TrimSpace(creds.QueryURL) == "" {
		return Snapshot{
			Err:          fmt.Errorf("缺少凭据请求地址"),
			CheckedAt:    time.Now().UTC(),
			Currency:     "USD",
			QuotaPerUnit: DefaultQuotaPerUnit,
		}
	}
	quotaPerUnit := c.quotaPerUnit(ctx, base)

	attempts := []func() (Snapshot, bool){
		func() (Snapshot, bool) { return c.fetchUserSelf(ctx, base, creds, quotaPerUnit) },
		func() (Snapshot, bool) { return c.fetchUsage(ctx, base, creds) },
	}

	var lastErr error
	for _, attempt := range attempts {
		snapshot, ok := attempt()
		if ok {
			snapshot.CheckedAt = time.Now().UTC()
			if snapshot.Currency == "" {
				snapshot.Currency = "USD"
			}
			if snapshot.QuotaPerUnit == 0 {
				snapshot.QuotaPerUnit = quotaPerUnit
			}
			return snapshot
		}
		if snapshot.Err != nil {
			lastErr = snapshot.Err
		}
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("未配置可用的额度查询凭据（需要 访问令牌 + 用户 ID，或上游 API Key，或一段查询脚本）")
	}
	return Snapshot{Err: lastErr, CheckedAt: time.Now().UTC(), QuotaPerUnit: quotaPerUnit, Currency: "USD"}
}

// fetchByScript 用管理员自填的脚本查询额度。
//
// 脚本只负责「请求怎么发」与「响应怎么读」，实际发请求仍由宿主完成：
// 沙箱里没有任何网络能力，因此脚本无法访问 request 之外的地址。
func (c *Client) fetchByScript(ctx context.Context, base string, creds Credentials) Snapshot {
	now := func() time.Time { return time.Now().UTC() }
	vars := map[string]string{
		"baseUrl":     base,
		"apiKey":      strings.TrimSpace(creds.APIKey),
		"accessToken": strings.TrimSpace(creds.AccessToken),
		"userId":      strings.TrimSpace(creds.UserID),
	}
	// 自定义查询地址存在时覆盖 {{baseUrl}}：管理员填的就是额度接口所在的站点。
	if custom := strings.TrimRight(strings.TrimSpace(creds.QueryURL), "/"); custom != "" {
		vars["baseUrl"] = custom
	}

	requestSpec, err := creds.Script.BuildRequest(vars)
	if err != nil {
		return Snapshot{Err: fmt.Errorf("脚本请求配置无效: %w", err), CheckedAt: now(), Currency: "USD"}
	}

	var body io.Reader
	if requestSpec.Body != "" {
		body = strings.NewReader(requestSpec.Body)
	}
	request, err := http.NewRequestWithContext(ctx, requestSpec.Method, requestSpec.URL, body)
	if err != nil {
		return Snapshot{Err: err, CheckedAt: now(), Currency: "USD"}
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", httpx.UpstreamUserAgent())
	if requestSpec.Body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	for name, value := range requestSpec.Headers {
		if strings.TrimSpace(value) == "" {
			continue
		}
		request.Header.Set(name, value)
	}

	response, err := c.http.Do(request)
	if err != nil {
		return Snapshot{Err: err, CheckedAt: now(), Currency: "USD"}
	}
	defer drain(response)

	// 非 2xx 也把正文交给脚本：不少站点用 200 之外的状态码配合 JSON 说明失效原因，
	// 直接判失败会让 extractor 的 invalidMessage 分支永远走不到。
	var payload any
	if err := readJSONBody(response, &payload); err != nil {
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return Snapshot{Err: describeFailure2(response, requestSpec.URL, err), CheckedAt: now(), Currency: "USD"}
		}
		return Snapshot{Err: fmt.Errorf("脚本请求 %s %w", requestSpec.URL, err), CheckedAt: now(), Currency: "USD"}
	}

	result, err := creds.Script.Extract(payload)
	if err != nil {
		return Snapshot{Err: fmt.Errorf("脚本 extractor 执行失败: %w", err), CheckedAt: now(), Currency: "USD"}
	}
	if result.HasValid && !result.IsValid {
		message := result.InvalidMessage
		if message == "" {
			message = "脚本判定该账号额度无效"
		}
		return Snapshot{Err: errors.New(message), CheckedAt: now(), Currency: "USD"}
	}

	snapshot := Snapshot{
		Balance:     result.Remaining,
		UsedAmount:  result.Used,
		TotalAmount: result.Total,
		HasBalance:  result.HasRemaining,
		HasUsed:     result.HasUsed,
		HasTotal:    result.HasTotal,
		PlanName:    result.PlanName,
		Extra:       result.Extra,
		Currency:    firstNonEmpty(result.Unit, "USD"),
		Source:      "script",
		CheckedAt:   now(),
	}
	// 只报了总额与已用时把剩余算出来：余额是耗尽判定的唯一依据，缺了它账号就没法参与调度。
	if !snapshot.HasBalance && snapshot.HasTotal && snapshot.HasUsed {
		snapshot.Balance = result.Total - result.Used
		snapshot.HasBalance = true
	}
	// 只给了余额时补出总额，界面上的「总额/已用」才不会出现空洞。
	if !snapshot.HasTotal && snapshot.HasBalance {
		snapshot.TotalAmount = snapshot.Balance + result.Used
		snapshot.HasTotal = true
	}
	if !snapshot.HasBalance {
		// 拿不到剩余额度就不能拿它做耗尽判定：报成错误而不是默认 0，
		// 否则账号会因为脚本写得不全被误判欠费并暂停。
		snapshot.Err = errors.New("脚本未返回 remaining，也无法由 total - used 推算剩余额度")
	}
	return snapshot
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// describeFailure2 给脚本请求生成一句带地址与状态码的可读错误。
func describeFailure2(response *http.Response, target string, cause error) error {
	return fmt.Errorf("脚本请求 %s 返回 HTTP %d: %v", target, response.StatusCode, cause)
}

// quotaPerUnit 读取站点 /api/status 的 quota_per_unit，失败时回落默认值。
func (c *Client) quotaPerUnit(ctx context.Context, base string) float64 {
	if strings.TrimSpace(base) == "" {
		return DefaultQuotaPerUnit
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/status", nil)
	if err != nil {
		return DefaultQuotaPerUnit
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", httpx.UpstreamUserAgent())
	response, err := c.http.Do(request)
	if err != nil {
		return DefaultQuotaPerUnit
	}
	defer drain(response)

	payload := struct {
		Data struct {
			QuotaPerUnit float64 `json:"quota_per_unit"`
		} `json:"data"`
	}{}
	if err := decodeJSON(response.Body, &payload); err != nil || payload.Data.QuotaPerUnit <= 0 {
		return DefaultQuotaPerUnit
	}
	return payload.Data.QuotaPerUnit
}

// fetchUserSelf 用访问令牌 + 用户 ID 查询 New API 的 /api/user/self。
func (c *Client) fetchUserSelf(ctx context.Context, base string, creds Credentials, quotaPerUnit float64) (Snapshot, bool) {
	token := strings.TrimSpace(creds.AccessToken)
	if token == "" {
		return Snapshot{}, false
	}

	target := endpointFor(creds, base, "/api/user/self")
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return Snapshot{Err: err}, false
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", httpx.UpstreamUserAgent())
	request.Header.Set("Authorization", "Bearer "+token)
	if userID := strings.TrimSpace(creds.UserID); userID != "" {
		request.Header.Set("New-Api-User", userID)
	}

	response, err := c.http.Do(request)
	if err != nil {
		return Snapshot{Err: err}, false
	}
	defer drain(response)

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Snapshot{Err: describeFailure(response, target)}, false
	}

	payload := struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
		Data    struct {
			Group     string  `json:"group"`
			Quota     float64 `json:"quota"`
			UsedQuota float64 `json:"used_quota"`
		} `json:"data"`
	}{}
	if err := readJSONBody(response, &payload); err != nil {
		return Snapshot{Err: fmt.Errorf("/api/user/self %w", err)}, false
	}
	if !payload.Success {
		message := payload.Message
		if message == "" {
			message = "查询失败"
		}
		return Snapshot{Err: fmt.Errorf("/api/user/self 查询失败: %s", message)}, false
	}
	if quotaPerUnit <= 0 {
		quotaPerUnit = DefaultQuotaPerUnit
	}

	planName := payload.Data.Group
	if planName == "" {
		planName = "默认套餐"
	}
	return Snapshot{
		Balance:      payload.Data.Quota / quotaPerUnit,
		UsedAmount:   payload.Data.UsedQuota / quotaPerUnit,
		TotalAmount:  (payload.Data.Quota + payload.Data.UsedQuota) / quotaPerUnit,
		PlanName:     planName,
		QuotaPerUnit: quotaPerUnit,
		Source:       "/api/user/self",
	}, true
}

// endpointFor 决定内置查询实际请求的地址。
//
// 管理员填了完整额度查询地址时一律用它：约定式拼接对不规则站点无效，
// 而「填了却不生效」是最难排查的一类问题。
func endpointFor(creds Credentials, base, defaultPath string) string {
	if custom := strings.TrimRight(strings.TrimSpace(creds.QueryURL), "/"); custom != "" {
		return custom
	}
	return base + defaultPath
}

// fetchUsage 用上游 API Key 查询 /api/usage，作为访问令牌不可用时的回落。
func (c *Client) fetchUsage(ctx context.Context, base string, creds Credentials) (Snapshot, bool) {
	apiKey := strings.TrimSpace(creds.APIKey)
	if apiKey == "" {
		return Snapshot{}, false
	}

	target := endpointFor(creds, base, "/api/usage")
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader([]byte("{}")))
	if err != nil {
		return Snapshot{Err: err}, false
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", httpx.UpstreamUserAgent())
	request.Header.Set("Authorization", "Bearer "+apiKey)

	response, err := c.http.Do(request)
	if err != nil {
		return Snapshot{Err: err}, false
	}
	defer drain(response)

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Snapshot{Err: describeFailure(response, target)}, false
	}

	payload := struct {
		Error   any     `json:"error"`
		Balance float64 `json:"balance"`
		Used    float64 `json:"used"`
	}{}
	if err := readJSONBody(response, &payload); err != nil {
		return Snapshot{Err: fmt.Errorf("/api/usage %w", err)}, false
	}
	if payload.Error != nil {
		return Snapshot{Err: fmt.Errorf("/api/usage 返回错误: %v", payload.Error)}, false
	}

	return Snapshot{
		Balance:     payload.Balance,
		UsedAmount:  payload.Used,
		TotalAmount: payload.Balance + payload.Used,
		Source:      "/api/usage",
	}, true
}

func decodeJSON(body io.Reader, target any) error {
	raw, err := io.ReadAll(io.LimitReader(body, maxBodyBytes))
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, target)
}

func snippet(body io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(body, 8<<10))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// readJSONBody 解析 JSON 正文，正文是 HTML 时直接给出「被 WAF 拦下」这类可读原因。
//
// New API 站点挂在 Cloudflare 后面时，服务端直连会拿到一整页人机验证 HTML，
// 若照原样丢给 json.Unmarshal，界面只会看到一句难以定位的语法错误。
func readJSONBody(response *http.Response, target any) error {
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxBodyBytes))
	if err != nil {
		return err
	}
	contentType := response.Header.Get("Content-Type")
	body := string(raw)
	if httpx.LooksLikeHTML(contentType, body) {
		return errors.New(httpx.DescribeUpstreamFailure(contentType, body))
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("响应不是合法 JSON: %s", httpx.CleanUpstreamText(contentType, body))
	}
	return nil
}

// describeFailure 把非 2xx 响应翻译成一句可读错误。
func describeFailure(response *http.Response, label string) error {
	raw := snippet(response.Body)
	contentType := response.Header.Get("Content-Type")
	return fmt.Errorf("%s 返回 HTTP %d: %s", label, response.StatusCode, httpx.DescribeUpstreamFailure(contentType, raw))
}

// drain 读尽并关闭响应体，让连接可以回到连接池复用。
func drain(response *http.Response) {
	if response == nil || response.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	_ = response.Body.Close()
}
