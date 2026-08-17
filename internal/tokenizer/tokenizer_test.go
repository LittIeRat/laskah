package tokenizer

import "testing"

// TestCountTextASCII 校验英文按约 4 字符 1 token 估算。
func TestCountTextASCII(t *testing.T) {
	if got := CountText("hello"); got != 2 {
		t.Fatalf("hello 期望 2，得到 %d", got)
	}
	if got := CountText("hello world"); got != 4 {
		t.Fatalf("hello world 期望 4，得到 %d", got)
	}
	if got := CountText(""); got != 0 {
		t.Fatalf("空串期望 0，得到 %d", got)
	}
}

// TestCountTextCJK 校验中文按 1 字 1 token 估算。
func TestCountTextCJK(t *testing.T) {
	if got := CountText("你好世界"); got != 4 {
		t.Fatalf("你好世界 期望 4，得到 %d", got)
	}
	if got := CountText("你好，世界"); got != 5 {
		t.Fatalf("带标点期望 5，得到 %d", got)
	}
}

// TestCounterChunked 校验分片累计与一次性计数结果一致。
//
// 流式响应是逐 chunk 到达的，若分片会改变结果，输出计费就会随网络分片抖动。
func TestCounterChunked(t *testing.T) {
	full := "The quick brown fox jumps over 13 lazy dogs. 中文混排测试。"
	want := CountText(full)

	for _, size := range []int{1, 2, 3, 5, 7, 11} {
		var counter Counter
		for index := 0; index < len(full); index += size {
			end := index + size
			if end > len(full) {
				end = len(full)
			}
			counter.Add(full[index:end])
		}
		if got := counter.Total(); got != want {
			t.Fatalf("分片大小 %d：期望 %d，得到 %d", size, want, got)
		}
	}
}

// TestCountMessages 校验消息数组含固定开销。
func TestCountMessages(t *testing.T) {
	messages := []any{
		map[string]any{"role": "system", "content": "你是助手"},
		map[string]any{"role": "user", "content": "hello"},
	}
	got := CountMessages(messages)
	if got <= PerRequestOverhead+2*PerMessageOverhead {
		t.Fatalf("消息 token 估算过低: %d", got)
	}
	if CountMessages(nil) != 0 {
		t.Fatal("空消息应为 0")
	}
}

// TestCountContentParts 校验分段 content 与图片分段。
func TestCountContentParts(t *testing.T) {
	text := CountContent([]any{
		map[string]any{"type": "text", "text": "hello world"},
	})
	if text != CountText("hello world") {
		t.Fatalf("文本分段期望 %d，得到 %d", CountText("hello world"), text)
	}

	image := CountContent([]any{
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/a.png"}},
	})
	if image != imagePartTokens {
		t.Fatalf("图片分段期望 %d，得到 %d", imagePartTokens, image)
	}
}

// TestCountPromptVariants 校验三种请求形态都能算出输入 token。
func TestCountPromptVariants(t *testing.T) {
	chat := CountPrompt(map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "hello world"},
	}})
	responses := CountPrompt(map[string]any{"input": "hello world"})
	legacy := CountPrompt(map[string]any{"prompt": "hello world"})

	if chat <= 0 || responses <= 0 || legacy <= 0 {
		t.Fatalf("三种形态都应大于 0: chat=%d responses=%d legacy=%d", chat, responses, legacy)
	}
	if CountPrompt(nil) != 0 {
		t.Fatal("空请求体应为 0")
	}
}

// TestCountCompletionPayload 校验三种响应形态的输出统计。
func TestCountCompletionPayload(t *testing.T) {
	chat := CountCompletionPayload(map[string]any{"choices": []any{
		map[string]any{"message": map[string]any{"role": "assistant", "content": "hello world"}},
	}})
	if chat != CountText("hello world") {
		t.Fatalf("chat 响应期望 %d，得到 %d", CountText("hello world"), chat)
	}

	legacy := CountCompletionPayload(map[string]any{"choices": []any{
		map[string]any{"text": "hello world"},
	}})
	if legacy != CountText("hello world") {
		t.Fatalf("legacy 响应期望 %d，得到 %d", CountText("hello world"), legacy)
	}

	responses := CountCompletionPayload(map[string]any{"output": []any{
		map[string]any{"content": []any{map[string]any{"type": "output_text", "text": "hello world"}}},
	}})
	if responses != CountText("hello world") {
		t.Fatalf("responses 期望 %d，得到 %d", CountText("hello world"), responses)
	}

	if CountCompletionPayload(nil) != 0 {
		t.Fatal("空响应应为 0")
	}
}
