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

	// overLimit 在本站按 max_tokens 主动截断时收尾，留空则回落 truncate。
	overLimit func(http.ResponseWriter, string)

	// inbound 把调用方的请求体转成本站内部的 OpenAI chat 形态，留空表示已经是该形态。
	//
	// 账号分配、模型匹配、限流、余额不足换号与本地计量全部建立在内部形态上，
	// 因此不同入口协议只需在这里各写一次转换，主流程不必分叉。
	inbound func(map[string]any) map[string]any

	// outbound 把内部形态的响应转回调用方协议，留空表示直接下发。
	outbound func(map[string]any, string) map[string]any

	// newRewriter 构造流式改写器，把内部 SSE chunk 转成调用方协议的事件。
	newRewriter func(model string, promptTokens int64) streamRewriter

	// anthropic 表示该端点支持 Anthropic 协议双向转换。
	anthropic bool
}

// streamRewriter 把内部 OpenAI chunk 改写成目标协议的 SSE 事件序列。
type streamRewriter interface {
	// Rewrite 处理一个内部 chunk，返回需要下发的事件行。
	Rewrite(chunk string) []string

	// Finish 补齐收尾事件，outputTokens 是本站自算的输出 token 数。
	Finish(outputTokens int64) []string

	// SetStopReason 覆盖收尾原因，用于本站主动截断。
	SetStopReason(reason string)
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
		overLimit:   finishOverLimitResponses,
		anthropic:   false,
	}
}

// messagesSpec 是 /v1/messages 的差异定义（Anthropic Messages 兼容）。
//
// 请求进来先翻成内部 OpenAI 形态，出去再翻回 Anthropic 形态：
// 这样上游是 OpenAI 兼容站还是 Anthropic 原生站都由既有逻辑决定，
// 调用方用哪种协议接入都不会改变账号分配与计费口径。
func messagesSpec() inferenceSpec {
	return inferenceSpec{
		endpoint: "/v1/messages",
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
		truncate:    finishTruncatedMessages,
		inbound:     FromAnthropicRequest,
		outbound:    ToAnthropicMessage,
		newRewriter: func(model string, promptTokens int64) streamRewriter {
			return newAnthropicOutputRewriter(model, promptTokens)
		},
		anthropic: true,
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
		// 推理模型的思维链走 reasoning_content，上游同样按输出 token 计费。
		if reasoning := tokenizer.ReasoningContent(delta); reasoning != nil {
			out.WriteString(ContentText(reasoning))
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

// finishOverLimitResponses 在 Responses 输出触及 max_output_tokens 时收尾。
func finishOverLimitResponses(w http.ResponseWriter, model string) {
	payload := map[string]any{
		"type": "response.incomplete",
		"response": map[string]any{
			"id":                 fmt.Sprintf("resp_%d", time.Now().UnixNano()),
			"object":             "response",
			"created_at":         time.Now().Unix(),
			"model":              model,
			"status":             "incomplete",
			"incomplete_details": map[string]any{"reason": "max_output_tokens"},
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

// outputTokenLimit 读取请求里声明的输出上限，未声明时返回 0。
//
// 三个字段分别来自 chat（max_tokens）、新版 chat（max_completion_tokens）
// 与 Responses（max_output_tokens）；Anthropic 的 max_tokens 在入站转换后也走同一字段。
func outputTokenLimit(body map[string]any) int64 {
	for _, field := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		if value, ok := asInt(body[field]); ok && value > 0 {
			return value
		}
	}
	return 0
}

// outputHardCap 把声明的上限放宽成本站真正执行的硬上限。
//
// 本地 token 估算刻意偏保守（宁可高估），直接按声明值截断会把合规输出砍掉一截；
// 放宽 25% 再加 8 个 token 的余量后，只有明显无视 max_tokens 的上游才会被截断。
func outputHardCap(limit int64) int64 {
	if limit <= 0 {
		return 0
	}
	return limit + limit/4 + 8
}

// enforceOutputLimit 在非流式响应里裁掉超出上限的正文。
//
// 有些中转站完全忽略 max_tokens（实测声明 8 却返回几百 token），
// 调用方按上限预算显示与计费就会失真，因此本站在下发前自己收口。
// 返回值表示是否发生了截断。
func enforceOutputLimit(payload map[string]any, limit int64) bool {
	hardCap := outputHardCap(limit)
	if hardCap <= 0 || payload == nil {
		return false
	}
	if tokenizer.CountCompletionPayload(payload) <= hardCap {
		return false
	}

	truncated := false
	if choices, ok := payload["choices"].([]any); ok {
		for _, item := range choices {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if message, ok := entry["message"].(map[string]any); ok {
				if text, ok := message["content"].(string); ok && text != "" {
					if trimmed, cut := trimToTokens(text, hardCap); cut {
						message["content"] = trimmed
						truncated = true
					}
				}
				// 思维链也算输出，忽略它就会出现「正文很短却计了几百 token」的错觉。
				for _, field := range []string{"reasoning_content", "reasoning"} {
					text, ok := message[field].(string)
					if !ok || text == "" {
						continue
					}
					if trimmed, cut := trimToTokens(text, hardCap); cut {
						message[field] = trimmed
						truncated = true
					}
				}
			}
			if text, ok := entry["text"].(string); ok && text != "" {
				if trimmed, cut := trimToTokens(text, hardCap); cut {
					entry["text"] = trimmed
					truncated = true
				}
			}
			if truncated {
				entry["finish_reason"] = "length"
			}
		}
	}
	if text, ok := payload["output_text"].(string); ok && text != "" {
		if trimmed, cut := trimToTokens(text, hardCap); cut {
			payload["output_text"] = trimmed
			truncated = true
		}
	}
	// Responses 的正文在 output[].content[].text 里，与 output_text 是两份数据，
	// 只裁其中一份会让客户端按未裁的那份渲染。
	if output, ok := payload["output"].([]any); ok {
		for _, item := range output {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			blocks, ok := entry["content"].([]any)
			if !ok {
				continue
			}
			for _, block := range blocks {
				segment, ok := block.(map[string]any)
				if !ok {
					continue
				}
				text, ok := segment["text"].(string)
				if !ok || text == "" {
					continue
				}
				if trimmed, cut := trimToTokens(text, hardCap); cut {
					segment["text"] = trimmed
					truncated = true
				}
			}
		}
	}
	if truncated {
		if _, ok := payload["status"]; ok {
			payload["status"] = "incomplete"
			payload["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
		}
	}
	return truncated
}

// trimToTokens 把文本裁到不超过给定 token 数，按 rune 边界二分查找。
func trimToTokens(text string, limit int64) (string, bool) {
	if limit <= 0 || tokenizer.CountText(text) <= limit {
		return text, false
	}
	runes := []rune(text)
	low, high := 0, len(runes)
	for low < high {
		mid := (low + high + 1) / 2
		if tokenizer.CountText(string(runes[:mid])) <= limit {
			low = mid
		} else {
			high = mid - 1
		}
	}
	return string(runes[:low]), true
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
