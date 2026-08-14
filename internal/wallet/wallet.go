// Package wallet 查询 New API 站点账号的额度与已用量。
package wallet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
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

// Fetch 依次尝试 /api/user/self 与 /api/usage，任一成功即返回。
func (c *Client) Fetch(parent context.Context, creds Credentials) Snapshot {
	timeout := creds.Timeout
	if timeout <= 0 || timeout > 120*time.Second {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	base := strings.TrimRight(strings.TrimSpace(creds.BaseURL), "/")
	if base == "" {
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
		lastErr = fmt.Errorf("未配置可用的额度查询凭据（需要 访问令牌 + 用户 ID，或上游 API Key）")
	}
	return Snapshot{Err: lastErr, CheckedAt: time.Now().UTC(), QuotaPerUnit: quotaPerUnit, Currency: "USD"}
}

// quotaPerUnit 读取站点 /api/status 的 quota_per_unit，失败时回落默认值。
func (c *Client) quotaPerUnit(ctx context.Context, base string) float64 {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/status", nil)
	if err != nil {
		return DefaultQuotaPerUnit
	}
	request.Header.Set("Accept", "application/json")
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
	if err := decode(response.Body, &payload); err != nil || payload.Data.QuotaPerUnit <= 0 {
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

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/user/self", nil)
	if err != nil {
		return Snapshot{Err: err}, false
	}
	request.Header.Set("Content-Type", "application/json")
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
		return Snapshot{Err: fmt.Errorf("/api/user/self 返回 HTTP %d: %s", response.StatusCode, snippet(response.Body))}, false
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
	if err := decode(response.Body, &payload); err != nil {
		return Snapshot{Err: err}, false
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

// fetchUsage 用上游 API Key 查询 /api/usage，作为访问令牌不可用时的回落。
func (c *Client) fetchUsage(ctx context.Context, base string, creds Credentials) (Snapshot, bool) {
	apiKey := strings.TrimSpace(creds.APIKey)
	if apiKey == "" {
		return Snapshot{}, false
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/usage", bytes.NewReader([]byte("{}")))
	if err != nil {
		return Snapshot{Err: err}, false
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+apiKey)

	response, err := c.http.Do(request)
	if err != nil {
		return Snapshot{Err: err}, false
	}
	defer drain(response)

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Snapshot{Err: fmt.Errorf("/api/usage 返回 HTTP %d: %s", response.StatusCode, snippet(response.Body))}, false
	}

	payload := struct {
		Error   any     `json:"error"`
		Balance float64 `json:"balance"`
		Used    float64 `json:"used"`
	}{}
	if err := decode(response.Body, &payload); err != nil {
		return Snapshot{Err: err}, false
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

func decode(body io.Reader, target any) error {
	raw, err := io.ReadAll(io.LimitReader(body, maxBodyBytes))
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, target)
}

func snippet(body io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(body, 400))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// drain 读尽并关闭响应体，让连接可以回到连接池复用。
func drain(response *http.Response) {
	if response == nil || response.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	_ = response.Body.Close()
}
