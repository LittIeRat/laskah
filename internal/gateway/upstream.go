package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"laskah/internal/httpx"
	"laskah/internal/store"
)

// Upstream 负责构造并发送上游请求。
type Upstream struct {
	client *http.Client
}

// NewUpstream 创建上游客户端。
func NewUpstream() *Upstream {
	return &Upstream{client: &http.Client{}}
}

// JoinURL 拼接 baseUrl 与端点路径。
func JoinURL(baseURL, endpoint string) string {
	base := strings.TrimRight(baseURL, "/")
	lower := strings.ToLower(endpoint)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return endpoint
	}
	if endpoint == "" {
		return base
	}
	if !strings.HasPrefix(endpoint, "/") {
		endpoint = "/" + endpoint
	}
	return base + endpoint
}

// ChatURL 返回聊天端点完整地址。
func ChatURL(p *store.Provider) string {
	endpoint := p.Paths.Chat
	if endpoint == "" {
		endpoint = store.DefaultPaths(p.Type).Chat
	}
	return JoinURL(p.BaseURL, endpoint)
}

// ModelsURL 返回模型列表端点完整地址。
func ModelsURL(p *store.Provider) string {
	endpoint := p.Paths.Models
	if endpoint == "" {
		endpoint = store.DefaultPaths(p.Type).Models
	}
	return JoinURL(p.BaseURL, endpoint)
}

// AuthHeaders 按协议类型构造鉴权与自定义请求头。
func AuthHeaders(p *store.Provider) http.Header {
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	header.Set("Accept", "application/json")
	// 不带浏览器 UA 时 Cloudflare 一类的 WAF 会直接回人机验证页。
	header.Set("User-Agent", httpx.UpstreamUserAgent())
	for key, value := range p.Headers {
		header.Set(key, value)
	}
	if p.APIKey == "" {
		return header
	}
	switch p.Type {
	case store.TypeAnthropic:
		header.Set("X-Api-Key", p.APIKey)
		if header.Get("Anthropic-Version") == "" {
			header.Set("Anthropic-Version", "2023-06-01")
		}
	case store.TypeGemini:
		header.Set("Authorization", "Bearer "+p.APIKey)
		header.Set("X-Goog-Api-Key", p.APIKey)
	default:
		header.Set("Authorization", "Bearer "+p.APIKey)
	}
	return header
}

// ChatRequest 是标准化后的聊天请求体。
type ChatRequest struct {
	Model    string         `json:"model"`
	Messages []Message      `json:"messages"`
	Stream   bool           `json:"stream,omitempty"`
	Raw      map[string]any `json:"-"`
}

// Message 是一条对话消息，content 允许字符串或分段数组。
type Message struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// ContentText 把 content 字段拍平成纯文本。
func ContentText(content any) string {
	switch typed := content.(type) {
	case nil:
		return ""
	case string:
		return typed
	case []any:
		var out strings.Builder
		for _, part := range typed {
			switch segment := part.(type) {
			case string:
				out.WriteString(segment)
			case map[string]any:
				if text, ok := segment["text"].(string); ok {
					out.WriteString(text)
				}
			}
		}
		return out.String()
	case map[string]any:
		if text, ok := typed["text"].(string); ok {
			return text
		}
		return ""
	default:
		return fmt.Sprint(typed)
	}
}

// ToAnthropicBody 把 OpenAI 风格请求转换成 Anthropic messages 请求。
func ToAnthropicBody(body map[string]any) map[string]any {
	payload := map[string]any{
		"model":  body["model"],
		"stream": asBool(body["stream"]),
	}

	systemParts := []string{}
	messages := []map[string]any{}
	if rawMessages, ok := body["messages"].([]any); ok {
		for _, item := range rawMessages {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			role, _ := entry["role"].(string)
			text := ContentText(entry["content"])
			if role == "system" {
				if text != "" {
					systemParts = append(systemParts, text)
				}
				continue
			}
			if role != "assistant" {
				role = "user"
			}
			messages = append(messages, map[string]any{"role": role, "content": text})
		}
	}
	payload["messages"] = messages

	maxTokens := int64(4096)
	if value, ok := asInt(body["max_tokens"]); ok {
		maxTokens = value
	} else if value, ok := asInt(body["max_completion_tokens"]); ok {
		maxTokens = value
	}
	payload["max_tokens"] = maxTokens

	if len(systemParts) > 0 {
		payload["system"] = strings.Join(systemParts, "\n\n")
	}
	if value, ok := body["temperature"]; ok {
		payload["temperature"] = value
	}
	if value, ok := body["top_p"]; ok {
		payload["top_p"] = value
	}
	if stop, ok := body["stop"]; ok {
		switch typed := stop.(type) {
		case []any:
			payload["stop_sequences"] = typed
		case string:
			payload["stop_sequences"] = []any{typed}
		}
	}
	return payload
}

// FromAnthropicResponse 把 Anthropic 响应转换成 OpenAI chat.completion 结构。
func FromAnthropicResponse(raw map[string]any, requestedModel string) map[string]any {
	var text strings.Builder
	if blocks, ok := raw["content"].([]any); ok {
		for _, block := range blocks {
			entry, ok := block.(map[string]any)
			if !ok {
				continue
			}
			if entry["type"] == "text" {
				if value, ok := entry["text"].(string); ok {
					text.WriteString(value)
				}
			}
		}
	}

	promptTokens := int64(0)
	completionTokens := int64(0)
	if usage, ok := raw["usage"].(map[string]any); ok {
		if value, ok := asInt(usage["input_tokens"]); ok {
			promptTokens = value
		}
		if value, ok := asInt(usage["output_tokens"]); ok {
			completionTokens = value
		}
	}

	finishReason := "stop"
	if raw["stop_reason"] == "max_tokens" {
		finishReason = "length"
	}

	id, _ := raw["id"].(string)
	if id == "" {
		id = fmt.Sprintf("chatcmpl_%d", time.Now().UnixNano())
	}
	model := requestedModel
	if model == "" {
		model, _ = raw["model"].(string)
	}

	return map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": text.String()},
				"finish_reason": finishReason,
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"total_tokens":      promptTokens + completionTokens,
		},
	}
}

// Response 包装上游响应及耗时信息。
type Response struct {
	HTTP      *http.Response
	StartedAt time.Time
	URL       string
	Cancel    context.CancelFunc
}

// SendChat 向指定提供商发送聊天请求。
func (u *Upstream) SendChat(parent context.Context, provider *store.Provider, body map[string]any) (*Response, error) {
	payload := map[string]any{}
	for key, value := range body {
		payload[key] = value
	}
	payload["model"] = provider.UpstreamModel(store.MustString(body["model"]))
	if provider.Type == store.TypeAnthropic {
		payload = ToAnthropicBody(payload)
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("序列化上游请求失败: %w", err)
	}

	timeout := time.Duration(provider.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	ctx, cancel := context.WithTimeout(parent, timeout)

	target := ChatURL(provider)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(encoded))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("构造上游请求失败: %w", err)
	}
	request.Header = AuthHeaders(provider)

	startedAt := time.Now()
	response, err := u.client.Do(request)
	if err != nil {
		cancel()
		return nil, err
	}
	return &Response{HTTP: response, StartedAt: startedAt, URL: target, Cancel: cancel}, nil
}

// ProbeResult 是连通性探测结果。
type ProbeResult struct {
	OK        bool
	Status    int
	LatencyMS int64
	Models    []string
	Error     string
}

// ListModels 探测上游 models 端点，用于连通性测试与模型同步。
func (u *Upstream) ListModels(parent context.Context, provider *store.Provider) ProbeResult {
	timeout := time.Duration(provider.TimeoutMS) * time.Millisecond
	if timeout <= 0 || timeout > 30*time.Second {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	startedAt := time.Now()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, ModelsURL(provider), nil)
	if err != nil {
		return ProbeResult{Error: err.Error(), LatencyMS: time.Since(startedAt).Milliseconds()}
	}
	request.Header = AuthHeaders(provider)

	response, err := u.client.Do(request)
	if err != nil {
		return ProbeResult{Error: err.Error(), LatencyMS: time.Since(startedAt).Milliseconds()}
	}
	defer response.Body.Close()

	latency := time.Since(startedAt).Milliseconds()
	contentType := response.Header.Get("Content-Type")
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return ProbeResult{Status: response.StatusCode, LatencyMS: latency, Error: err.Error()}
	}
	body := string(raw)

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return ProbeResult{Status: response.StatusCode, LatencyMS: latency, Error: httpx.DescribeUpstreamFailure(contentType, body)}
	}

	// 有些站点被 WAF 挡下时仍回 200，正文却是人机验证页，必须按失败处理。
	if httpx.LooksLikeHTML(contentType, body) {
		return ProbeResult{Status: response.StatusCode, LatencyMS: latency, Error: httpx.DescribeUpstreamFailure(contentType, body)}
	}

	decoded := map[string]any{}
	models := []string{}
	if err := json.Unmarshal(raw, &decoded); err == nil {
		models = extractModelIDs(decoded["data"])
		if len(models) == 0 {
			models = extractModelIDs(decoded["models"])
		}
	} else {
		var list []any
		if err := json.Unmarshal(raw, &list); err != nil {
			return ProbeResult{Status: response.StatusCode, LatencyMS: latency, Error: "上游响应不是合法 JSON: " + httpx.CleanUpstreamText(contentType, body)}
		}
		models = extractModelIDs(list)
	}
	return ProbeResult{OK: true, Status: response.StatusCode, LatencyMS: latency, Models: models}
}

func extractModelIDs(value any) []string {
	list, ok := value.([]any)
	if !ok {
		return nil
	}
	result := []string{}
	for _, item := range list {
		switch typed := item.(type) {
		case string:
			result = append(result, strings.TrimPrefix(typed, "models/"))
		case map[string]any:
			for _, field := range []string{"id", "name", "model"} {
				if text, ok := typed[field].(string); ok && text != "" {
					result = append(result, strings.TrimPrefix(text, "models/"))
					break
				}
			}
		}
	}
	return result
}

func readSnippet(body io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(body, 8<<10))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// ReadErrorBody 读取错误响应正文片段。
//
// 上游被 WAF 挡下时会回一整页 HTML，直接透传会把提示条撑爆，也会让「余额不足」
// 关键字匹配失效，所以这里先压成一行纯文本。限长比界面提示宽一些，留给关键字匹配。
func ReadErrorBody(response *http.Response) string {
	raw := readSnippet(response.Body)
	contentType := ""
	if response != nil {
		contentType = response.Header.Get("Content-Type")
	}
	if httpx.IsChallengePage(contentType, raw) {
		return httpx.DescribeUpstreamFailure(contentType, raw)
	}
	return httpx.CleanUpstreamTextLimit(contentType, raw, 600)
}

func asBool(value any) bool {
	flag, ok := value.(bool)
	return ok && flag
}

func asInt(value any) (int64, bool) {
	switch typed := value.(type) {
	case float64:
		return int64(typed), true
	case int64:
		return typed, true
	case int:
		return int64(typed), true
	case json.Number:
		parsed, err := typed.Int64()
		return parsed, err == nil
	default:
		return 0, false
	}
}
