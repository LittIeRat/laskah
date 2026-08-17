// Package tokenizer 提供纯 Go 的本地 token 估算，用于不依赖上游自报用量的计费与统计。
//
// 为什么要自己数：部分上游站点会谎报 usage（少报输入、多报输出，或干脆不返回），
// 拿它做计费与配额判断会失真。这里按字符类别加权估算，不引入任何 CGO 词表依赖，
// 单次调用只做一遍 rune 扫描，热路径开销可忽略。
//
// 精度取向：宁可略微高估也不低估。低估会让账号在真实余额耗尽后仍被分配流量，
// 换来上游报错与请求截断；高估只会让本地余额提前一点退场，代价小得多。
package tokenizer

import (
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"
)

// 每条消息的固定开销：role 名、分隔符与结构标记。
//
// 与 OpenAI 官方 cookbook 对 chat 消息的估算口径一致（每条约 4 个 token，
// 整个请求再加 3 个用于回复引导）。
const (
	PerMessageOverhead = 4
	PerRequestOverhead = 3
)

// asciiCharsPerToken 是英文/数字的平均压缩比（约 4 个字符 1 个 token）。
const asciiCharsPerToken = 4

// CountText 估算一段纯文本的 token 数。
func CountText(text string) int64 {
	var counter Counter
	counter.Add(text)
	return counter.Total()
}

// Counter 支持分片累计，用于流式响应边收边算。
//
// 尾部未结束的 ASCII 单词会被暂存，等下一片到达后再合并计数，
// 因此把同一段文本切成任意片段送入，结果与一次性计数一致。
type Counter struct {
	tokens  int64
	pending strings.Builder
}

// Add 累计一个文本片段。
func (c *Counter) Add(text string) {
	if text == "" {
		return
	}
	if c.pending.Len() > 0 {
		buffered := c.pending.String()
		c.pending.Reset()
		text = buffered + text
	}

	// 尾部若停在半个 UTF-8 字符或未结束的 ASCII 单词里，先留到下一片再算。
	split := trimIncompleteRune(text)
	for split > 0 {
		ch := text[split-1]
		if !isASCIIWordByte(ch) {
			break
		}
		split--
	}
	if split < len(text) {
		c.pending.WriteString(text[split:])
		text = text[:split]
	}
	c.tokens += scan(text)
}

// AddTokens 直接累加已知 token 数，用于结构性开销。
func (c *Counter) AddTokens(count int64) {
	if count > 0 {
		c.tokens += count
	}
}

// Total 返回当前累计值，并把暂存的尾部单词一并计入。
func (c *Counter) Total() int64 {
	total := c.tokens
	if c.pending.Len() > 0 {
		total += scan(c.pending.String())
	}
	return total
}

// trimIncompleteRune 返回文本中最后一个完整 UTF-8 字符的结束位置。
//
// 流式分片会在任意字节边界切断，把半个多字节字符送进 scan 会被当成
// 若干个替换字符各算一个 token，导致同一段文本按不同分片得到不同结果。
func trimIncompleteRune(text string) int {
	for offset := 1; offset <= utf8.UTFMax && offset <= len(text); offset++ {
		ch := text[len(text)-offset]
		if ch < 0x80 {
			break
		}
		if ch >= 0xC0 {
			if offset < runeByteLen(ch) {
				return len(text) - offset
			}
			break
		}
	}
	return len(text)
}

// runeByteLen 由首字节推断该 UTF-8 字符的总字节数。
func runeByteLen(lead byte) int {
	switch {
	case lead >= 0xF0:
		return 4
	case lead >= 0xE0:
		return 3
	case lead >= 0xC0:
		return 2
	default:
		return 1
	}
}

func isASCIIWordByte(ch byte) bool {
	switch {
	case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9':
		return true
	default:
		return false
	}
}

// scan 是核心估算：一遍扫描，按字符类别分段计数。
func scan(text string) int64 {
	var (
		tokens int64
		runLen int
		spaces int
	)
	flushWord := func() {
		if runLen == 0 {
			return
		}
		tokens += int64((runLen + asciiCharsPerToken - 1) / asciiCharsPerToken)
		runLen = 0
	}
	flushSpaces := func() {
		if spaces <= 1 {
			spaces = 0
			return
		}
		// 单个空格通常与后一个单词合成同一个 token；连续空白才单独成词。
		tokens += int64((spaces - 1 + asciiCharsPerToken - 1) / asciiCharsPerToken)
		spaces = 0
	}

	for _, ch := range text {
		switch {
		case ch < 128 && isASCIIWordByte(byte(ch)):
			flushSpaces()
			runLen++
		case ch == '\n':
			flushWord()
			flushSpaces()
			tokens++
		case ch == ' ' || ch == '\t' || ch == '\r' || unicode.IsSpace(ch):
			flushWord()
			spaces++
		case isWide(ch):
			// CJK 表意文字与假名/谚文：主流分词器下约 1 个字符 1 个 token。
			flushWord()
			flushSpaces()
			tokens++
		default:
			// 标点、符号、emoji 与其它文字：各自单独成 token。
			flushWord()
			flushSpaces()
			tokens++
		}
	}
	flushWord()
	flushSpaces()
	return tokens
}

// isWide 判断是否属于 CJK 表意文字、假名或谚文。
func isWide(ch rune) bool {
	switch {
	case ch >= 0x3040 && ch <= 0x30FF: // 平假名 / 片假名
		return true
	case ch >= 0x3400 && ch <= 0x4DBF: // 扩展 A 区
		return true
	case ch >= 0x4E00 && ch <= 0x9FFF: // 基本汉字
		return true
	case ch >= 0xF900 && ch <= 0xFAFF: // 兼容汉字
		return true
	case ch >= 0xAC00 && ch <= 0xD7AF: // 谚文音节
		return true
	case ch >= 0x20000 && ch <= 0x2FA1F: // 扩展 B 区及以上
		return true
	default:
		return false
	}
}

// CountContent 估算 OpenAI 消息 content 字段的 token 数。
//
// content 允许是字符串、分段数组或对象；图片等非文本分段按固定开销计入，
// 因为它们确实会消耗上游 token，只是无法从本地文本推断真实数量。
func CountContent(content any) int64 {
	var counter Counter
	countContentInto(&counter, content)
	return counter.Total()
}

// imagePartTokens 是单个图片分段的保守估算值。
//
// 低分辨率图片在 OpenAI 计价里约 85 token，高分辨率会显著更多；
// 这里取一个折中的下限，避免把带图请求算成几乎不花钱。
const imagePartTokens = 300

func countContentInto(counter *Counter, content any) {
	switch typed := content.(type) {
	case nil:
		return
	case string:
		counter.Add(typed)
	case []any:
		for _, part := range typed {
			countContentInto(counter, part)
		}
	case map[string]any:
		if text, ok := typed["text"].(string); ok {
			counter.Add(text)
			return
		}
		if kind, ok := typed["type"].(string); ok {
			switch kind {
			case "image_url", "image", "input_image":
				counter.AddTokens(imagePartTokens)
				return
			}
		}
		counter.Add(jsonText(typed))
	default:
		counter.Add(jsonText(typed))
	}
}

// CountMessages 估算 chat.completions 请求里 messages 的 prompt token 数。
func CountMessages(messages []any) int64 {
	var total int64
	for _, item := range messages {
		total += PerMessageOverhead
		entry, ok := item.(map[string]any)
		if !ok {
			total += CountContent(item)
			continue
		}
		var counter Counter
		if role, ok := entry["role"].(string); ok {
			counter.Add(role)
		}
		if name, ok := entry["name"].(string); ok {
			counter.Add(name)
		}
		countContentInto(&counter, entry["content"])
		// 工具调用同样占用输入 token，按其 JSON 文本计入。
		if calls, ok := entry["tool_calls"]; ok && calls != nil {
			counter.Add(jsonText(calls))
		}
		total += counter.Total()
	}
	if total > 0 {
		total += PerRequestOverhead
	}
	return total
}

// CountPrompt 估算请求体的输入 token 数。
//
// 依次识别 chat 的 messages、Responses API 的 input/instructions，
// 以及旧式 completions 的 prompt，并把 tools 定义计入输入。
func CountPrompt(body map[string]any) int64 {
	if body == nil {
		return 0
	}
	var total int64
	switch {
	case body["messages"] != nil:
		if messages, ok := body["messages"].([]any); ok {
			total += CountMessages(messages)
		}
	case body["input"] != nil:
		total += CountResponsesInput(body["input"])
	case body["prompt"] != nil:
		total += CountContent(body["prompt"])
	}
	if instructions, ok := body["instructions"].(string); ok && instructions != "" {
		total += CountText(instructions) + PerMessageOverhead
	}
	if tools, ok := body["tools"]; ok && tools != nil {
		total += CountText(jsonText(tools))
	}
	return total
}

// CountResponsesInput 估算 Responses API 的 input 字段。
//
// input 既可能是一段纯文本，也可能是 message 数组（content 里用 input_text 分段）。
func CountResponsesInput(input any) int64 {
	switch typed := input.(type) {
	case nil:
		return 0
	case string:
		if typed == "" {
			return 0
		}
		return CountText(typed) + PerMessageOverhead + PerRequestOverhead
	case []any:
		return CountMessages(typed)
	default:
		return CountContent(typed)
	}
}

// CountCompletionPayload 估算一个非流式响应的输出 token 数。
//
// 兼容 chat.completions 的 choices[].message、旧式 completions 的 choices[].text、
// Anthropic 的 content 块，以及 Responses API 的 output/output_text。
func CountCompletionPayload(payload map[string]any) int64 {
	if payload == nil {
		return 0
	}
	var counter Counter
	if text, ok := payload["output_text"].(string); ok && text != "" {
		counter.Add(text)
	}
	if output, ok := payload["output"].([]any); ok {
		for _, item := range output {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			countContentInto(&counter, entry["content"])
		}
	}
	if choices, ok := payload["choices"].([]any); ok {
		for _, item := range choices {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if message, ok := entry["message"].(map[string]any); ok {
				countContentInto(&counter, message["content"])
				if calls, ok := message["tool_calls"]; ok && calls != nil {
					counter.Add(jsonText(calls))
				}
				continue
			}
			if text, ok := entry["text"].(string); ok {
				counter.Add(text)
			}
		}
	}
	if content, ok := payload["content"].([]any); ok {
		countContentInto(&counter, content)
	}
	return counter.Total()
}

func jsonText(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
}
