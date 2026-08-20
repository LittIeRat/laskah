package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

	// Cloudflare 一类的 WAF 会拦掉 Go 默认 UA，所有上游请求都要带浏览器标识。
	for name, header := range map[string]http.Header{"openai": openai, "anthropic": claude, "gemini": gemini, "none": none} {
		agent := header.Get("User-Agent")
		if agent == "" || strings.Contains(agent, "Go-http-client") {
			t.Fatalf("%s 缺少浏览器 UA: %q", name, agent)
		}
		if header.Get("Accept") != "application/json" {
			t.Fatalf("%s 缺少 Accept 头", name)
		}
	}
}

// TestListModelsRejectsChallengePage 确认上游回人机验证页时按失败处理并给出可读原因。
func TestListModelsRejectsChallengePage(t *testing.T) {
	var gotAgent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAgent = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		// 200 但正文是拦截页，这是 Cloudflare 最常见的表现。
		_, _ = w.Write([]byte(`<!DOCTYPE html><html lang="en-US"><head><title>Just a moment...</title></head><body>challenges.cloudflare.com</body></html>`))
	}))
	defer server.Close()

	result := NewUpstream().ListModels(context.Background(), &store.Provider{
		Type:      store.TypeOpenAI,
		BaseURL:   server.URL + "/v1",
		APIKey:    "sk-a",
		Paths:     store.DefaultPaths(store.TypeOpenAI),
		TimeoutMS: 5000,
	})
	if result.OK {
		t.Fatal("拦截页不应被当成成功")
	}
	if !strings.Contains(result.Error, "人机验证") {
		t.Fatalf("应说明是人机验证页: %s", result.Error)
	}
	if strings.Contains(result.Error, "<") {
		t.Fatalf("错误信息不应包含原始 HTML: %s", result.Error)
	}
	if gotAgent == "" || strings.Contains(gotAgent, "Go-http-client") {
		t.Fatalf("请求未带浏览器 UA: %q", gotAgent)
	}
}

// TestListModelsParsesJSON 确认正常 JSON 仍能解析。
func TestListModelsParsesJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-4o-mini"},{"id":"claude-3-5-sonnet"}]}`))
	}))
	defer server.Close()

	result := NewUpstream().ListModels(context.Background(), &store.Provider{
		Type:      store.TypeOpenAI,
		BaseURL:   server.URL + "/v1",
		APIKey:    "sk-a",
		Paths:     store.DefaultPaths(store.TypeOpenAI),
		TimeoutMS: 5000,
	})
	if !result.OK {
		t.Fatalf("正常响应应成功: %s", result.Error)
	}
	if len(result.Models) != 2 {
		t.Fatalf("应解析出 2 个模型, got %v", result.Models)
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

// TestIsBalanceExhausted 明确划定“余额不足自动暂停”的判定边界。
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
		// 剩余额度足够覆盖本次预扣费，属于正常提示，不能暂停账号。
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

// TestTruncateReason 保证暂停原因不会写入超长上游响应。
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

// TestSSEBalanceError 校验流式响应里的余额不足识别。
//
// 关键是不能把模型正文里出现的“余额不足”当成账号欠费：只看 error 字段。
func TestSSEBalanceError(t *testing.T) {
	hits := []string{
		`data: {"error":{"message":"预扣费额度失败, 用户剩余额度: ＄0.182898, 需要预扣费额度: ＄0.290486"}}`,
		`data: {"error":{"code":"insufficient_quota","message":"You exceeded your current quota"}}`,
		`data: {"error":{"message":"当前账号已欠费，请充值"}}`,
	}
	for _, line := range hits {
		if _, ok := sseBalanceError(line); !ok {
			t.Fatalf("应识别为余额不足: %s", line)
		}
	}

	misses := []string{
		"data: [DONE]",
		"",
		": keep-alive",
		"event: message_start",
		`data: {"choices":[{"delta":{"content":"你的余额不足时应该充值"}}]}`,
		`data: {"error":{"message":"model not found"}}`,
		`data: not-json`,
	}
	for _, line := range misses {
		if _, ok := sseBalanceError(line); ok {
			t.Fatalf("不应识别为余额不足: %s", line)
		}
	}
}

// TestBalanceExhaustedInPayload 覆盖“HTTP 200 但 error 字段报余额不足”。
func TestBalanceExhaustedInPayload(t *testing.T) {
	payload := map[string]any{"error": map[string]any{"message": "预扣费额度失败, 用户剩余额度: ＄0.18, 需要预扣费额度: ＄0.29"}}
	detail, ok := balanceExhaustedInPayload(payload)
	if !ok || !strings.Contains(detail, "0.18") {
		t.Fatalf("应从 error 字段识别余额不足: %q %v", detail, ok)
	}

	// 正常回答里出现相关字样不能触发暂停。
	normal := map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "余额不足请充值"}}}}
	if _, ok := balanceExhaustedInPayload(normal); ok {
		t.Fatalf("正文内容不应触发余额不足判定")
	}
	if _, ok := balanceExhaustedInPayload(map[string]any{"error": nil}); ok {
		t.Fatalf("error 为 null 不应触发")
	}
	if _, ok := balanceExhaustedInPayload(nil); ok {
		t.Fatalf("nil 不应触发")
	}
}

// TestPreferDeclaredModel 验证账号内部的上游排序：明确声明该模型的 Key 排在前面。
//
// 模型列表留空的上游属于“什么都收”，真实提供情况未知，只能当兜底。
func TestPreferDeclaredModel(t *testing.T) {
	loose := &store.Provider{ID: "loose", BaseURL: "https://a", Models: []string{}}
	declared := &store.Provider{ID: "declared", BaseURL: "https://b", Models: []string{"claude-3-opus"}}
	wildcard := &store.Provider{ID: "wildcard", BaseURL: "https://c", Models: []string{"claude-3*"}}

	ordered := preferDeclaredModel([]*store.Provider{loose, declared, wildcard}, "claude-3-opus")
	if len(ordered) != 3 {
		t.Fatalf("不应丢弃候选: %d", len(ordered))
	}
	if ordered[0].ID != "declared" || ordered[1].ID != "wildcard" || ordered[2].ID != "loose" {
		t.Fatalf("声明该模型的上游应排在兜底之前: %s %s %s", ordered[0].ID, ordered[1].ID, ordered[2].ID)
	}

	// 全是兜底或全是声明时保持原顺序，不做额外分配。
	onlyLoose := []*store.Provider{loose, {ID: "loose2", Models: []string{}}}
	if got := preferDeclaredModel(onlyLoose, "claude-3-opus"); got[0].ID != "loose" || got[1].ID != "loose2" {
		t.Fatalf("同类候选应保持原顺序: %#v", got)
	}
	onlyDeclared := []*store.Provider{declared, wildcard}
	if got := preferDeclaredModel(onlyDeclared, "claude-3-opus"); got[0].ID != "declared" || got[1].ID != "wildcard" {
		t.Fatalf("同类候选应保持原顺序: %#v", got)
	}

	// 未指定模型或单个候选时原样返回。
	if got := preferDeclaredModel([]*store.Provider{loose, declared}, ""); got[0].ID != "loose" {
		t.Fatalf("未指定模型不应重排")
	}
	if got := preferDeclaredModel([]*store.Provider{loose}, "claude-3-opus"); len(got) != 1 {
		t.Fatalf("单候选应原样返回")
	}
}

// TestToAnthropicMessageBlockOrder 校验出站块顺序与思维链保留。
func TestToAnthropicMessageBlockOrder(t *testing.T) {
	converted := ToAnthropicMessage(map[string]any{
		"id": "resp_1",
		"choices": []any{map[string]any{
			"finish_reason": "stop",
			"message": map[string]any{
				"role":              "assistant",
				"content":           "answer",
				"reasoning_content": "think",
			},
		}},
		"usage": map[string]any{"prompt_tokens": float64(7), "completion_tokens": float64(3)},
	}, "kimi")

	blocks, ok := converted["content"].([]any)
	if !ok || len(blocks) != 2 {
		t.Fatalf("content 块数量错误: %#v", converted["content"])
	}
	first := blocks[0].(map[string]any)
	if first["type"] != "thinking" || first["thinking"] != "think" {
		t.Fatalf("首块应为 thinking: %#v", first)
	}
	second := blocks[1].(map[string]any)
	if second["type"] != "text" || second["text"] != "answer" {
		t.Fatalf("次块应为 text: %#v", second)
	}
	if converted["stop_reason"] != "end_turn" {
		t.Fatalf("stop_reason 错误: %v", converted["stop_reason"])
	}
}

// TestAnthropicRewriterSeparatesThinkingBlock 校验流式思维链与正文分块下发。
//
// 两者混在同一个 index 里会让 Anthropic 客户端把思维链渲染成正式回答。
func TestAnthropicRewriterSeparatesThinkingBlock(t *testing.T) {
	rewriter := newAnthropicOutputRewriter("kimi", 12)
	out := []string{}
	out = append(out, rewriter.Rewrite(`data: {"choices":[{"delta":{"reasoning_content":"think"}}]}`)...)
	out = append(out, rewriter.Rewrite(`data: {"choices":[{"delta":{"content":"answer"}}]}`)...)
	out = append(out, rewriter.Finish(5)...)
	joined := strings.Join(out, "")

	for _, want := range []string{
		`"type":"thinking_delta"`,
		`"thinking":"think"`,
		`"type":"text_delta"`,
		`"text":"answer"`,
		`"index":1`,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("缺少 %s:\n%s", want, joined)
		}
	}
	if strings.Count(joined, "event: content_block_start") != 2 {
		t.Fatalf("应有两个 content_block_start:\n%s", joined)
	}
	if strings.Count(joined, "event: content_block_stop") != 2 {
		t.Fatalf("应有两个 content_block_stop:\n%s", joined)
	}
	if strings.Count(joined, "event: message_start") != 1 {
		t.Fatalf("message_start 应只出现一次:\n%s", joined)
	}
}

// TestChatStreamDeltaCountsReasoning 校验流式思维链计入本地输出用量。
func TestChatStreamDeltaCountsReasoning(t *testing.T) {
	got := chatStreamDelta(`data: {"choices":[{"delta":{"content":"a","reasoning_content":"b"}}]}`)
	if !strings.Contains(got, "a") || !strings.Contains(got, "b") {
		t.Fatalf("正文与思维链都应计入: %q", got)
	}
}
