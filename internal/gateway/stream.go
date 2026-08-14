package gateway

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"laskah/internal/balancer"
)

// anthropicStreamConverter 把 Anthropic SSE 事件转换成 OpenAI chunk 格式。
type anthropicStreamConverter struct {
	id    string
	model string
	usage balancer.Usage
}

func newAnthropicStreamConverter(model string) *anthropicStreamConverter {
	return &anthropicStreamConverter{
		id:    fmt.Sprintf("chatcmpl_%d", time.Now().UnixNano()),
		model: model,
	}
}

// Convert 处理一行 SSE 文本，返回需要下发的 OpenAI 格式数据行。
func (c *anthropicStreamConverter) Convert(line string) []string {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "data:") {
		return nil
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	if payload == "" || payload == "[DONE]" {
		return nil
	}

	event := map[string]any{}
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return nil
	}

	switch event["type"] {
	case "message_start":
		message, ok := event["message"].(map[string]any)
		if !ok {
			return nil
		}
		if c.model == "" {
			if model, ok := message["model"].(string); ok {
				c.model = model
			}
		}
		if usage, ok := message["usage"].(map[string]any); ok {
			if prompt, ok := asInt(usage["input_tokens"]); ok {
				c.usage.PromptTokens = prompt
				c.usage.TotalTokens = c.usage.PromptTokens + c.usage.CompletionTokens
			}
		}
		return nil

	case "content_block_delta":
		delta, ok := event["delta"].(map[string]any)
		if !ok {
			return nil
		}
		text, ok := delta["text"].(string)
		if !ok || text == "" {
			return nil
		}
		return []string{c.encode(map[string]any{"content": text}, nil)}

	case "message_delta":
		if usage, ok := event["usage"].(map[string]any); ok {
			if completion, ok := asInt(usage["output_tokens"]); ok {
				c.usage.CompletionTokens = completion
				c.usage.TotalTokens = c.usage.PromptTokens + c.usage.CompletionTokens
			}
		}
		finish := "stop"
		if delta, ok := event["delta"].(map[string]any); ok {
			if reason, ok := delta["stop_reason"].(string); ok && reason == "max_tokens" {
				finish = "length"
			}
		}
		return []string{c.encode(map[string]any{}, &finish)}

	default:
		return nil
	}
}

func (c *anthropicStreamConverter) encode(delta map[string]any, finishReason *string) string {
	choice := map[string]any{"index": 0, "delta": delta, "finish_reason": nil}
	if finishReason != nil {
		choice["finish_reason"] = *finishReason
	}
	chunk := map[string]any{
		"id":      c.id,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   c.model,
		"choices": []any{choice},
	}
	encoded, err := json.Marshal(chunk)
	if err != nil {
		return ""
	}
	return "data: " + string(encoded) + "\n\n"
}
