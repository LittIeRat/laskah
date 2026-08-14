package gateway

import (
	"encoding/json"
	"strings"
	"testing"

	"laskah/internal/store"
)

func TestJoinURLAndEndpointResolution(t *testing.T) {
	cases := [][3]string{
		{"https://api.example.com/v1", "/chat/completions", "https://api.example.com/v1/chat/completions"},
		{"https://api.example.com/v1/", "chat/completions", "https://api.example.com/v1/chat/completions"},
		{"https://api.example.com", "", "https://api.example.com"},
		{"https://api.example.com", "https://other.example.com/x", "https://other.example.com/x"},
	}
	for _, item := range cases {
		if got := JoinURL(item[0], item[1]); got != item[2] {
			t.Fatalf("JoinURL(%q,%q)=%q, want %q", item[0], item[1], got, item[2])
		}
	}

	openai := &store.Provider{BaseURL: "https://api.openai.com/v1", Type: store.TypeOpenAI}
	if ChatURL(openai) != "https://api.openai.com/v1/chat/completions" {
		t.Fatalf("openai chat 端点错误: %s", ChatURL(openai))
	}
	if ModelsURL(openai) != "https://api.openai.com/v1/models" {
		t.Fatalf("openai models 端点错误: %s", ModelsURL(openai))
	}

	claude := &store.Provider{BaseURL: "https://api.anthropic.com/v1", Type: store.TypeAnthropic}
	if ChatURL(claude) != "https://api.anthropic.com/v1/messages" {
		t.Fatalf("anthropic chat 端点错误: %s", ChatURL(claude))
	}

	custom := &store.Provider{BaseURL: "https://x.example.com", Type: store.TypeOpenAI, Paths: store.Paths{Chat: "/custom/chat"}}
	if ChatURL(custom) != "https://x.example.com/custom/chat" {
		t.Fatalf("自定义路径未生效: %s", ChatURL(custom))
	}
}

func TestAuthHeadersPerProviderType(t *testing.T) {
	openai := AuthHeaders(&store.Provider{Type: store.TypeOpenAI, APIKey: "sk-a", Headers: map[string]string{"X-Trace": "1"}})
	if openai.Get("Authorization") != "Bearer sk-a" {
		t.Fatalf("openai 鉴权头错误: %s", openai.Get("Authorization"))
	}
	if openai.Get("X-Trace") != "1" {
		t.Fatalf("自定义头丢失")
	}

	claude := AuthHeaders(&store.Provider{Type: store.TypeAnthropic, APIKey: "sk-b"})
	if claude.Get("X-Api-Key") != "sk-b" || claude.Get("Anthropic-Version") == "" {
		t.Fatalf("anthropic 头错误: %#v", claude)
	}
	if claude.Get("Authorization") != "" {
		t.Fatalf("anthropic 不应设置 Authorization")
	}

	gemini := AuthHeaders(&store.Provider{Type: store.TypeGemini, APIKey: "sk-c"})
	if gemini.Get("X-Goog-Api-Key") != "sk-c" {
		t.Fatalf("gemini 头错误")
	}

	none := AuthHeaders(&store.Provider{Type: store.TypeOpenAI})
	if none.Get("Authorization") != "" {
		t.Fatalf("无密钥时不应设置鉴权头")
	}
}

func TestContentTextFlattening(t *testing.T) {
	if ContentText("hello") != "hello" {
		t.Fatalf("字符串 content 处理错误")
	}
	segments := []any{
		map[string]any{"type": "text", "text": "第一段"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "http://x"}},
		"直接字符串",
	}
	if got := ContentText(segments); got != "第一段直接字符串" {
		t.Fatalf("分段 content 拍平错误: %q", got)
	}
	if ContentText(nil) != "" {
		t.Fatalf("nil 应返回空串")
	}
	if ContentText(map[string]any{"text": "x"}) != "x" {
		t.Fatalf("对象 content 处理错误")
	}
}

func TestToAnthropicBodyConversion(t *testing.T) {
	body := map[string]any{
		"model":  "claude-3-5-sonnet",
		"stream": true,
		"messages": []any{
			map[string]any{"role": "system", "content": "你是助手"},
			map[string]any{"role": "system", "content": "保持简短"},
			map[string]any{"role": "user", "content": "你好"},
			map[string]any{"role": "assistant", "content": "在的"},
			map[string]any{"role": "tool", "content": "工具结果"},
		},
		"temperature": 0.5,
		"top_p":       0.9,
		"stop":        "END",
	}
	payload := ToAnthropicBody(body)

	if payload["system"] != "你是助手\n\n保持简短" {
		t.Fatalf("system 合并错误: %v", payload["system"])
	}
	messages, ok := payload["messages"].([]map[string]any)
	if !ok || len(messages) != 3 {
		t.Fatalf("消息转换错误: %#v", payload["messages"])
	}
	if messages[0]["role"] != "user" || messages[1]["role"] != "assistant" || messages[2]["role"] != "user" {
		t.Fatalf("角色映射错误: %#v", messages)
	}
	if payload["max_tokens"] != int64(4096) {
		t.Fatalf("max_tokens 默认值错误: %v", payload["max_tokens"])
	}
	if stops, ok := payload["stop_sequences"].([]any); !ok || len(stops) != 1 {
		t.Fatalf("stop 转换错误: %#v", payload["stop_sequences"])
	}
	if payload["stream"] != true || payload["temperature"] != 0.5 || payload["top_p"] != 0.9 {
		t.Fatalf("透传字段错误: %#v", payload)
	}

	withMax := ToAnthropicBody(map[string]any{"max_completion_tokens": float64(321)})
	if withMax["max_tokens"] != int64(321) {
		t.Fatalf("max_completion_tokens 未映射: %v", withMax["max_tokens"])
	}
}

func TestFromAnthropicResponseConversion(t *testing.T) {
	raw := map[string]any{
		"id":          "msg_123",
		"model":       "claude-3-5-sonnet",
		"stop_reason": "max_tokens",
		"content": []any{
			map[string]any{"type": "text", "text": "你好"},
			map[string]any{"type": "thinking", "text": "忽略"},
			map[string]any{"type": "text", "text": "，我是 Claude"},
		},
		"usage": map[string]any{"input_tokens": float64(11), "output_tokens": float64(4)},
	}
	converted := FromAnthropicResponse(raw, "claude-alias")

	if converted["object"] != "chat.completion" || converted["model"] != "claude-alias" {
		t.Fatalf("基础字段错误: %#v", converted)
	}
	choices, ok := converted["choices"].([]any)
	if !ok || len(choices) != 1 {
		t.Fatalf("choices 结构错误: %#v", converted["choices"])
	}
	choice := choices[0].(map[string]any)
	message := choice["message"].(map[string]any)
	if message["content"] != "你好，我是 Claude" {
		t.Fatalf("文本拼接错误: %v", message["content"])
	}
	if choice["finish_reason"] != "length" {
		t.Fatalf("stop_reason 映射错误: %v", choice["finish_reason"])
	}
	usage := converted["usage"].(map[string]any)
	if usage["prompt_tokens"] != int64(11) || usage["completion_tokens"] != int64(4) || usage["total_tokens"] != int64(15) {
		t.Fatalf("用量换算错误: %#v", usage)
	}
}

func TestAnthropicStreamConverter(t *testing.T) {
	converter := newAnthropicStreamConverter("claude-alias")

	if got := converter.Convert("event: message_start"); got != nil {
		t.Fatalf("非 data 行应忽略")
	}
	converter.Convert(`data: {"type":"message_start","message":{"model":"claude-3","usage":{"input_tokens":9}}}`)
	if converter.usage.PromptTokens != 9 {
		t.Fatalf("input_tokens 未记录: %#v", converter.usage)
	}

	chunks := converter.Convert(`data: {"type":"content_block_delta","delta":{"text":"你好"}}`)
	if len(chunks) != 1 || !strings.HasPrefix(chunks[0], "data: ") || !strings.HasSuffix(chunks[0], "\n\n") {
		t.Fatalf("增量 chunk 格式错误: %#v", chunks)
	}
	decoded := map[string]any{}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(strings.TrimSpace(chunks[0]), "data: ")), &decoded); err != nil {
		t.Fatalf("chunk 不是合法 JSON: %v", err)
	}
	if decoded["object"] != "chat.completion.chunk" || decoded["model"] != "claude-alias" {
		t.Fatalf("chunk 字段错误: %#v", decoded)
	}
	delta := decoded["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if delta["content"] != "你好" {
		t.Fatalf("增量内容错误: %#v", delta)
	}

	final := converter.Convert(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":6}}`)
	if len(final) != 1 {
		t.Fatalf("结束 chunk 缺失")
	}
	if converter.usage.CompletionTokens != 6 || converter.usage.TotalTokens != 15 {
		t.Fatalf("用量累计错误: %#v", converter.usage)
	}
	if got := converter.Convert("data: [DONE]"); got != nil {
		t.Fatalf("[DONE] 应忽略")
	}
	if got := converter.Convert("data: {bad json"); got != nil {
		t.Fatalf("非法 JSON 应忽略")
	}
}

func TestUsageParsing(t *testing.T) {
	openai := usageFromMap(map[string]any{"prompt_tokens": float64(3), "completion_tokens": float64(4), "total_tokens": float64(7)})
	if openai.TotalTokens != 7 {
		t.Fatalf("OpenAI 用量解析错误: %#v", openai)
	}
	claude := usageFromMap(map[string]any{"input_tokens": float64(5), "output_tokens": float64(6)})
	if claude.PromptTokens != 5 || claude.CompletionTokens != 6 || claude.TotalTokens != 11 {
		t.Fatalf("Anthropic 用量解析错误: %#v", claude)
	}
	if usageFromMap("not-a-map").TotalTokens != 0 {
		t.Fatalf("非法输入应返回空用量")
	}

	if _, ok := usageFromSSELine("data: [DONE]"); ok {
		t.Fatalf("[DONE] 不应产生用量")
	}
	if _, ok := usageFromSSELine(`data: {"choices":[]}`); ok {
		t.Fatalf("无 usage 字段不应产生用量")
	}
	parsed, ok := usageFromSSELine(`data: {"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`)
	if !ok || parsed.TotalTokens != 5 {
		t.Fatalf("SSE 用量解析错误: %#v", parsed)
	}
}

func TestRequestedModelID(t *testing.T) {
	cases := map[string]string{
		"/v1/models":                  "",
		"/v1/models/":                 "",
		"/models":                     "",
		"/v1/models/gpt-4o":           "gpt-4o",
		"/v1/models/gpt-4o/":          "gpt-4o",
		"/models/gpt-4o-mini":         "gpt-4o-mini",
		"/v1/models/meta/llama-3-70b": "meta/llama-3-70b",
		"/other":                      "",
	}
	for path, want := range cases {
		if got := requestedModelID(path); got != want {
			t.Fatalf("requestedModelID(%q)=%q, want %q", path, got, want)
		}
	}
}

func TestProviderModelNamesDropsWildcards(t *testing.T) {
	provider := &store.Provider{
		Models:   []string{"gpt-4o", " gpt-4o-mini ", "gpt-4*", "*", "", "gpt-4o"},
		ModelMap: map[string]string{"fast": "gpt-4o-mini", "any*": "x"},
	}
	got := providerModelNames(provider)

	seen := map[string]bool{}
	for _, name := range got {
		if strings.Contains(name, "*") {
			t.Fatalf("通配符不应出现在模型列表: %q", name)
		}
		if seen[name] {
			t.Fatalf("模型名重复: %q", name)
		}
		seen[name] = true
	}
	for _, want := range []string{"gpt-4o", "gpt-4o-mini", "fast"} {
		if !seen[want] {
			t.Fatalf("缺少模型 %q: %#v", want, got)
		}
	}
	if len(got) != 3 {
		t.Fatalf("应只保留 3 个可枚举模型: %#v", got)
	}
}

// TestIsBalanceExhausted 明确划定“余额不足自动删号”的判定边界。
//
// 关键是不能把限流或上游故障误判成余额耗尽，否则会误删正常账号。
func TestIsBalanceExhausted(t *testing.T) {
	hits := []struct {
		status int
		body   string
	}{
		{400, `{"error":{"message":"该令牌额度不足，余额不足，请充值"}}`},
		{402, "insufficient balance"},
		{403, `{"error":{"code":"insufficient_quota","message":"You exceeded your current quota"}}`},
		{429, "Your credit balance is too low to access the Anthropic API"},
		{401, "当前账号已欠费，请充值"},
		{400, "预扣费失败，余额已耗尽"},
		// New API 的预扣费失败只报两个金额，不含“不足”字样，且使用全角货币符号。
		{400, "预扣费额度失败, 用户剩余额度: ＄0.182898, 需要预扣费额度: ＄0.290486"},
		{403, `{"error":{"message":"预扣费额度失败, 用户剩余额度: $0.02, 需要预扣费额度: $1.10"}}`},
		{400, "quota check failed: remaining quota 0.18, required 0.29"},
	}
	for _, item := range hits {
		if !IsBalanceExhausted(item.status, item.body) {
			t.Fatalf("应判定为余额不足: %d %s", item.status, item.body)
		}
	}

	misses := []struct {
		status int
		body   string
	}{
		{429, `{"error":{"message":"Rate limit reached, please slow down"}}`},
		{500, "余额不足"},
		{502, "bad gateway"},
		{503, "insufficient balance"},
		{400, `{"error":{"message":"model not found"}}`},
		{200, "insufficient quota"},
		// 剩余额度足够覆盖本次预扣费，属于正常提示，不能删号。
		{400, "预扣费额度校验: 用户剩余额度: ＄12.50, 需要预扣费额度: ＄0.29"},
		// 只有一个金额时无法比较，不能凭空判定。
		{400, "用户剩余额度: ＄0.18"},
		// 两个数字与额度无关，不能凑成余额不足。
		{400, `{"error":{"message":"当前剩余重试 1 次，需要 3 次"}}`},
	}
	for _, item := range misses {
		if IsBalanceExhausted(item.status, item.body) {
			t.Fatalf("不应判定为余额不足: %d %s", item.status, item.body)
		}
	}
}

// TestNormalizeErrorText 保证全角金额与冒号能折叠成半角后再匹配。
func TestNormalizeErrorText(t *testing.T) {
	got := normalizeErrorText("预扣费额度失败，用户剩余额度：＄0.18")
	if !strings.Contains(got, "$0.18") {
		t.Fatalf("全角货币符号未折叠: %q", got)
	}
	if !strings.Contains(got, ",") {
		t.Fatalf("全角逗号未折叠: %q", got)
	}
	if normalizeErrorText("Insufficient Balance") != "insufficient balance" {
		t.Fatalf("英文未转小写")
	}
}

// TestTruncateReason 保证删号原因不会写入超长上游响应。
func TestTruncateReason(t *testing.T) {
	if got := truncateReason("  余额不足  "); got != "余额不足" {
		t.Fatalf("应去掉首尾空白: %q", got)
	}
	long := strings.Repeat("余", 200)
	got := truncateReason(long)
	if len([]rune(got)) != 121 {
		t.Fatalf("应截断到 120 字符加省略号: %d", len([]rune(got)))
	}
}
