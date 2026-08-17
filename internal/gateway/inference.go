package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"laskah/internal/balancer"
	"laskah/internal/store"
	"laskah/internal/tokenizer"
)

// Usage 是本次调用的 token 用量，与 balancer.Usage 同一类型。
//
// 起别名只为让网关内部的签名短一些，不引入新的语义。
type Usage = balancer.Usage

// inferenceSpec 描述一种推理端点的差异点。
//
// 账号分配、频率限制、上游重试、余额不足换号与本地 token 计量全部是共享逻辑，
// 只有「请求体长什么样、打哪个上游端点、流式增量怎么读」因端点而异，
// 因此把差异收敛成这一个结构，避免 chat 与 responses 各写一份主流程后行为漂移。
type inferenceSpec struct {
	// endpoint 是本站对外路径，用于响应头标识。
	endpoint string

	// validate 校验请求体，返回空串表示合法。
	validate func(map[string]any) string

	// send 发送上游请求。
	send func(*Upstream, context.Context, *store.Provider, map[string]any) (*Response, error)

	// streamDelta 从一行 SSE 中取出正文增量，用于本地累计输出 token。
	streamDelta func(string) string

	// truncate 在流式输出中途换号时给调用方一个明确收尾。
	truncate func(http.ResponseWriter, string)

	// anthropic 表示该端点支持 Anthropic 协议双向转换。
	anthropic bool
}

// chatSpec 是 /v1/chat/completions 的差异定义。
func chatSpec() inferenceSpec {
	return inferenceSpec{
		endpoint: "/v1/chat/completions",
		validate: func(body map[string]any) string {
			messages, ok := body["messages"].([]any)
			if !ok || len(messages) == 0 {
				return "messages 字段必须是非空数组"
			}
			return ""
		},
		send: func(u *Upstream, ctx context.Context, provider *store.Provider, body map[string]any) (*Response, error) {
			return u.SendChat(ctx, provider, body)
		},
		streamDelta: chatStreamDelta,
		truncate:    finishTruncatedStream,
		anthropic:   true,
	}
}

// responsesSpec 是 /v1/responses 的差异定义。
//
// Responses 是 OpenAI 的新形态接口：输入字段是 input/instructions，
// 流式事件是 response.output_text.delta，因此增量与收尾都要单独处理。
// 不做 Anthropic 转换：把 Responses 语义硬翻成 messages 会静默丢掉
// 工具调用与多段输出，宁可让不支持的上游明确报错。
func responsesSpec() inferenceSpec {
	return inferenceSpec{
		endpoint: "/v1/responses",
		validate: func(body map[string]any) string {
			if body["input"] == nil && body["messages"] == nil && strings.TrimSpace(store.MustString(body["instructions"])) == "" {
				return "input 字段不能为空"
			}
			return ""
		},
		send: func(u *Upstream, ctx context.Context, provider *store.Provider, body map[string]any) (*Response, error) {
			return u.SendResponses(ctx, provider, body)
		},
		streamDelta: responsesStreamDelta,
		truncate:    finishTruncatedResponses,
		anthropic:   false,
	}
}

// sseData 取出一行 SSE 的 data 负载，非数据行返回空串。
func sseData(line string) string {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "data:") {
		return ""
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	if payload == "" || payload == "[DONE]" || !strings.HasPrefix(payload, "{") {
		return ""
	}
	return payload
}

// chatStreamDelta 读取 chat.completion.chunk 的正文增量。
func chatStreamDelta(line string) string {
	payload := sseData(line)
	if payload == "" {
		return ""
	}
	decoded := map[string]any{}
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		return ""
	}
	choices, ok := decoded["choices"].([]any)
	if !ok {
		return ""
	}
	var out strings.Builder
	for _, item := range choices {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		delta, ok := entry["delta"].(map[string]any)
		if !ok {
			continue
		}
		if text, ok := delta["content"].(string); ok {
			out.WriteString(text)
		}
		// 工具调用的参数也是上游产出的输出 token，必须计入。
		if calls, ok := delta["tool_calls"]; ok && calls != nil {
			if encoded, err := json.Marshal(calls); err == nil {
				out.Write(encoded)
			}
		}
	}
	return out.String()
}

// responsesStreamDelta 读取 Responses 流事件的正文增量。
func responsesStreamDelta(line string) string {
	payload := sseData(line)
	if payload == "" {
		return ""
	}
	decoded := map[string]any{}
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		return ""
	}
	kind, _ := decoded["type"].(string)
	if !strings.HasSuffix(kind, ".delta") {
		return ""
	}
	switch delta := decoded["delta"].(type) {
	case string:
		return delta
	case nil:
		return ""
	default:
		encoded, err := json.Marshal(delta)
		if err != nil {
			return ""
		}
		return string(encoded)
	}
}

// finishTruncatedResponses 在 Responses 流式输出中途换号时补一个收尾事件。
//
// 已下发的内容撤不回来，但必须让客户端明确知道这次输出被截断，
// 否则它会一直等一个永远不会到来的 response.completed。
func finishTruncatedResponses(w http.ResponseWriter, model string) {
	payload := map[string]any{
		"type": "response.incomplete",
		"response": map[string]any{
			"id":                 fmt.Sprintf("resp_%d", time.Now().UnixNano()),
			"object":             "response",
			"created_at":         time.Now().Unix(),
			"model":              model,
			"status":             "incomplete",
			"incomplete_details": map[string]any{"reason": "upstream_balance_exhausted"},
		},
	}
	if encoded, err := json.Marshal(payload); err == nil {
		_, _ = io.WriteString(w, "event: response.incomplete\ndata: "+string(encoded)+"\n\n")
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// localUsage 用本站自己的估算组装用量，upstream 仅作对照保留。
//
// 计费、密钥配额与看板金额全部只看本地口径：上游站点自报的 usage 可能被夸大，
// 直接采信会让用户按别人写的数字付钱。
func localUsage(prompt, completion, upstream int64) Usage {
	if prompt < 0 {
		prompt = 0
	}
	if completion < 0 {
		completion = 0
	}
	return Usage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      prompt + completion,
		UpstreamTokens:   upstream,
	}
}

// usageBody 把本地用量写成 OpenAI 规范的 usage 结构。
func usageBody(usage Usage) map[string]any {
	return map[string]any{
		"prompt_tokens":     usage.PromptTokens,
		"completion_tokens": usage.CompletionTokens,
		"total_tokens":      usage.TotalTokens,
		// Responses 用 input/output 命名，一并给出以兼容两类客户端。
		"input_tokens":  usage.PromptTokens,
		"output_tokens": usage.CompletionTokens,
	}
}

// countPromptTokens 估算一次请求的输入 token。
func countPromptTokens(body map[string]any) int64 {
	return tokenizer.CountPrompt(body)
}
