package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"laskah/internal/store"
	"laskah/internal/tokenizer"
)

// FromAnthropicRequest 把 Anthropic Messages 请求标准化成本站内部的 OpenAI chat 形态。
//
// 为什么要转成内部形态而不是直接透传：账号分配、模型匹配、频率限制、余额不足换号与
// 本地 token 计量全部建立在 OpenAI 形态之上。统一入口后，上游是 OpenAI 还是 Anthropic
// 都由既有的 ToAnthropicBody 决定，调用方用哪种协议进来都不影响调度行为。
func FromAnthropicRequest(body map[string]any) map[string]any {
	converted := map[string]any{}
	for key, value := range body {
		switch key {
		case "system", "messages", "stop_sequences", "tools", "tool_choice", "metadata", "anthropic_version":
			// 这些字段的语义与 OpenAI 不同，逐个显式翻译，不原样带过去。
			continue
		default:
			converted[key] = value
		}
	}

	messages := []any{}
	if system := anthropicText(body["system"]); system != "" {
		messages = append(messages, map[string]any{"role": "system", "content": system})
	}
	if raw, ok := body["messages"].([]any); ok {
		for _, item := range raw {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			role, _ := entry["role"].(string)
			if role != "assistant" {
				role = "user"
			}
			messages = append(messages, map[string]any{"role": role, "content": anthropicContent(entry["content"])})
		}
	}
	converted["messages"] = messages

	if stops, ok := body["stop_sequences"]; ok {
		converted["stop"] = stops
	}
	if tools := anthropicTools(body["tools"]); len(tools) > 0 {
		converted["tools"] = tools
	}
	return converted
}

// anthropicText 把 system 字段拍平成纯文本，兼容字符串与文本块数组两种写法。
func anthropicText(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(typed)
	case []any:
		parts := []string{}
		for _, item := range typed {
			if text := anthropicText(item); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n\n")
	case map[string]any:
		if text, ok := typed["text"].(string); ok {
			return strings.TrimSpace(text)
		}
		return ""
	default:
		return ""
	}
}

// anthropicContent 把一条消息的 content 转成 OpenAI 形态。
//
// 纯文本时直接给字符串，含图片时给 OpenAI 的分段数组；tool_use / tool_result
// 转成文本保留语义，因为多数 OpenAI 兼容上游并不认 Anthropic 的工具块结构，
// 静默丢掉会让对话上下文缺失。
func anthropicContent(value any) any {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case []any:
		var text strings.Builder
		parts := []any{}
		for _, item := range typed {
			block, ok := item.(map[string]any)
			if !ok {
				if raw, ok := item.(string); ok {
					text.WriteString(raw)
				}
				continue
			}
			switch block["type"] {
			case "text":
				if raw, ok := block["text"].(string); ok {
					text.WriteString(raw)
				}
			case "image":
				if url := anthropicImageURL(block["source"]); url != "" {
					parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
				}
			default:
				if encoded, err := json.Marshal(block); err == nil {
					text.WriteString(string(encoded))
				}
			}
		}
		if len(parts) == 0 {
			return text.String()
		}
		if text.Len() > 0 {
			parts = append([]any{map[string]any{"type": "text", "text": text.String()}}, parts...)
		}
		return parts
	case map[string]any:
		return anthropicContent([]any{typed})
	default:
		return fmt.Sprint(typed)
	}
}

// anthropicImageURL 把 Anthropic 的 base64 图片块转成 data URL。
func anthropicImageURL(source any) string {
	entry, ok := source.(map[string]any)
	if !ok {
		return ""
	}
	if url, ok := entry["url"].(string); ok && url != "" {
		return url
	}
	data, _ := entry["data"].(string)
	if data == "" {
		return ""
	}
	mediaType, _ := entry["media_type"].(string)
	if mediaType == "" {
		mediaType = "image/png"
	}
	return "data:" + mediaType + ";base64," + data
}

// anthropicTools 把 Anthropic 工具定义转成 OpenAI function 形态。
func anthropicTools(value any) []any {
	list, ok := value.([]any)
	if !ok {
		return nil
	}
	tools := []any{}
	for _, item := range list {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := entry["name"].(string)
		if name == "" {
			continue
		}
		function := map[string]any{"name": name}
		if description, ok := entry["description"].(string); ok && description != "" {
			function["description"] = description
		}
		if schema, ok := entry["input_schema"]; ok && schema != nil {
			function["parameters"] = schema
		}
		tools = append(tools, map[string]any{"type": "function", "function": function})
	}
	return tools
}

// anthropicStopReason 把 OpenAI 的 finish_reason 映射回 Anthropic 的 stop_reason。
func anthropicStopReason(finish string) string {
	switch finish {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "refusal"
	default:
		return "end_turn"
	}
}

// ToAnthropicMessage 把 OpenAI chat.completion 响应转成 Anthropic message 响应。
func ToAnthropicMessage(payload map[string]any, model string) map[string]any {
	text := ""
	thinking := ""
	stopReason := "end_turn"
	toolBlocks := []any{}
	if choices, ok := payload["choices"].([]any); ok && len(choices) > 0 {
		if entry, ok := choices[0].(map[string]any); ok {
			if message, ok := entry["message"].(map[string]any); ok {
				text = ContentText(message["content"])
				// 推理模型可能只填 reasoning_content 而把正文留空。原样丢弃会让
				// Anthropic 客户端收到一条空消息，因此转成 thinking 块保留内容。
				if reasoning := tokenizer.ReasoningContent(message); reasoning != nil {
					thinking = ContentText(reasoning)
				}
				if calls, ok := message["tool_calls"].([]any); ok {
					for _, item := range calls {
						if block := anthropicToolUse(item); block != nil {
							toolBlocks = append(toolBlocks, block)
						}
					}
				}
			}
			if finish, ok := entry["finish_reason"].(string); ok {
				stopReason = anthropicStopReason(finish)
			}
		}
	}
	// 块顺序对齐官方实现：thinking → text → tool_use。
	blocks := []any{}
	if thinking != "" {
		blocks = append(blocks, map[string]any{"type": "thinking", "thinking": thinking})
	}
	if text != "" || len(toolBlocks) == 0 {
		blocks = append(blocks, map[string]any{"type": "text", "text": text})
	}
	blocks = append(blocks, toolBlocks...)

	usage := usageFromMap(payload["usage"])
	id, _ := payload["id"].(string)
	if id == "" {
		id = fmt.Sprintf("msg_%d", time.Now().UnixNano())
	}
	if model == "" {
		model = store.MustString(payload["model"])
	}
	return map[string]any{
		"id":            id,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       blocks,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  usage.PromptTokens,
			"output_tokens": usage.CompletionTokens,
		},
	}
}

// anthropicToolUse 把 OpenAI 的 tool_call 转成 Anthropic 的 tool_use 块。
func anthropicToolUse(item any) map[string]any {
	entry, ok := item.(map[string]any)
	if !ok {
		return nil
	}
	function, ok := entry["function"].(map[string]any)
	if !ok {
		return nil
	}
	name, _ := function["name"].(string)
	if name == "" {
		return nil
	}
	input := map[string]any{}
	if arguments, ok := function["arguments"].(string); ok && arguments != "" {
		_ = json.Unmarshal([]byte(arguments), &input)
	}
	id, _ := entry["id"].(string)
	if id == "" {
		id = fmt.Sprintf("toolu_%d", time.Now().UnixNano())
	}
	return map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}
}

// anthropicOutputRewriter 把本站内部的 OpenAI chunk 改写成 Anthropic SSE 事件序列。
//
// 无论上游是 OpenAI 还是 Anthropic，pipeStream 交到这里的都已经是统一的 OpenAI chunk，
// 因此这里只需要负责「一种输入形态 → Anthropic 事件」，不必关心上游协议。
type anthropicOutputRewriter struct {
	id           string
	model        string
	promptTokens int64
	opened       bool
	stopReason   string

	// blockIndex 是当前正在下发的内容块下标，blockKind 是它的类型（thinking / text）。
	// 思维链与正文必须放在不同的块里，否则 Anthropic 客户端会把思维链当正文渲染。
	blockIndex int
	blockKind  string
}

func newAnthropicOutputRewriter(model string, promptTokens int64) *anthropicOutputRewriter {
	return &anthropicOutputRewriter{
		id:           fmt.Sprintf("msg_%d", time.Now().UnixNano()),
		model:        model,
		promptTokens: promptTokens,
		stopReason:   "end_turn",
	}
}

func (r *anthropicOutputRewriter) Rewrite(chunk string) []string {
	payload := sseData(chunk)
	if payload == "" {
		return nil
	}
	decoded := map[string]any{}
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		return nil
	}
	text := ""
	thinking := ""
	if choices, ok := decoded["choices"].([]any); ok {
		for _, item := range choices {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if delta, ok := entry["delta"].(map[string]any); ok {
				if value, ok := delta["content"].(string); ok {
					text += value
				}
				if reasoning := tokenizer.ReasoningContent(delta); reasoning != nil {
					thinking += ContentText(reasoning)
				}
			}
			if finish, ok := entry["finish_reason"].(string); ok && finish != "" {
				r.stopReason = anthropicStopReason(finish)
			}
		}
	}
	if text == "" && thinking == "" {
		return nil
	}

	lines := r.open()
	if thinking != "" {
		lines = append(lines, r.switchBlock("thinking")...)
		lines = append(lines, anthropicEvent("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": r.blockIndex,
			"delta": map[string]any{"type": "thinking_delta", "thinking": thinking},
		}))
	}
	if text != "" {
		lines = append(lines, r.switchBlock("text")...)
		lines = append(lines, anthropicEvent("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": r.blockIndex,
			"delta": map[string]any{"type": "text_delta", "text": text},
		}))
	}
	return lines
}

// open 在首次下发前补出 message_start，重复调用无副作用。
func (r *anthropicOutputRewriter) open() []string {
	if r.opened {
		return []string{}
	}
	r.opened = true
	return []string{anthropicEvent("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            r.id,
			"type":          "message",
			"role":          "assistant",
			"model":         r.model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": r.promptTokens, "output_tokens": 0},
		},
	})}
}

// switchBlock 切换到指定类型的内容块，必要时关闭上一个块并递增下标。
func (r *anthropicOutputRewriter) switchBlock(kind string) []string {
	if r.blockKind == kind {
		return nil
	}
	lines := []string{}
	if r.blockKind != "" {
		lines = append(lines, anthropicEvent("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": r.blockIndex,
		}))
		r.blockIndex++
	}
	r.blockKind = kind
	block := map[string]any{"type": "text", "text": ""}
	if kind == "thinking" {
		block = map[string]any{"type": "thinking", "thinking": "", "signature": ""}
	}
	return append(lines, anthropicEvent("content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         r.blockIndex,
		"content_block": block,
	}))
}

func (r *anthropicOutputRewriter) Finish(outputTokens int64) []string {
	// 上游一个字节都没给（或只给了空串）时也要补出完整骨架，
	// 否则客户端会一直等一个永远不会到来的 message_stop。
	lines := r.open()
	lines = append(lines, r.switchBlock("text")...)
	lines = append(lines,
		anthropicEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": r.blockIndex}),
		anthropicEvent("message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": r.stopReason, "stop_sequence": nil},
			"usage": map[string]any{"output_tokens": outputTokens},
		}),
		anthropicEvent("message_stop", map[string]any{"type": "message_stop"}),
	)
	return lines
}

// SetStopReason 供上层在本站主动截断时覆盖收尾原因。
func (r *anthropicOutputRewriter) SetStopReason(reason string) {
	r.stopReason = reason
}

// anthropicEvent 序列化一个带 event 名的 SSE 事件。
func anthropicEvent(name string, payload map[string]any) string {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return "event: " + name + "\ndata: " + string(encoded) + "\n\n"
}

// finishTruncatedMessages 在 Anthropic 流式输出被本站截断时补齐收尾事件。
func finishTruncatedMessages(w http.ResponseWriter, model string) {
	_ = model
	for _, line := range []string{
		anthropicEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}),
		anthropicEvent("message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": "max_tokens", "stop_sequence": nil},
		}),
		anthropicEvent("message_stop", map[string]any{"type": "message_stop"}),
	} {
		_, _ = io.WriteString(w, line)
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}
