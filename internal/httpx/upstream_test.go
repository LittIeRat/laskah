package httpx

import "testing"

// TestLooksLikeHTMLDetectsPages 覆盖 JSON 与 HTML 的区分。
func TestLooksLikeHTMLDetectsPages(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		body        string
		want        bool
	}{
		{"json", "application/json", `{"data":[]}`, false},
		{"empty", "", "", false},
		{"doctype", "", "<!DOCTYPE html><html><body>x</body></html>", true},
		{"leading-space", "", "\n  <html><body>x</body></html>", true},
		{"content-type-only", "text/html; charset=UTF-8", "whatever", true},
	}
	for _, item := range cases {
		if got := LooksLikeHTML(item.contentType, item.body); got != item.want {
			t.Fatalf("%s: LooksLikeHTML = %v, want %v", item.name, got, item.want)
		}
	}
}

// TestIsChallengePageDetectsCloudflare 覆盖人机验证页识别。
func TestIsChallengePageDetectsCloudflare(t *testing.T) {
	page := `<!DOCTYPE html><html lang="en-US"><head><title>Just a moment...</title>` +
		`<meta http-equiv="content-security-policy" content="script-src 'nonce-x' https://challenges.cloudflare.com"></head><body></body></html>`
	if !IsChallengePage("text/html", page) {
		t.Fatal("Cloudflare 拦截页应被识别")
	}
	if IsChallengePage("application/json", `{"error":"just a moment"}`) {
		t.Fatal("JSON 响应不应被判定为拦截页")
	}
}

// TestDescribeUpstreamFailureExplainsChallenge 确认拦截页给出可操作提示而不是原始 HTML。
func TestDescribeUpstreamFailureExplainsChallenge(t *testing.T) {
	page := "<!DOCTYPE html><html><head><title>Just a moment...</title>" +
		"<script>var a = 1 < 2;</script></head><body>challenges.cloudflare.com</body></html>"
	message := DescribeUpstreamFailure("text/html", page)
	if message == "" {
		t.Fatal("应给出说明")
	}
	for _, banned := range []string{"<", ">", "DOCTYPE"} {
		if contains(message, banned) {
			t.Fatalf("说明不应包含原始标记 %q: %s", banned, message)
		}
	}
	if !contains(message, "人机验证") {
		t.Fatalf("应说明是人机验证页: %s", message)
	}
}

// TestCleanUpstreamTextStripsScriptAndTruncates 覆盖标签剥离、实体还原与截断。
func TestCleanUpstreamTextStripsScriptAndTruncates(t *testing.T) {
	raw := "<html><head><style>.a{color:red}</style><script>var x = 1;</script></head><body>  &#39;none&#39;  \n  hello   world </body></html>"
	cleaned := CleanUpstreamText("text/html", raw)
	if contains(cleaned, "var x") || contains(cleaned, "color:red") {
		t.Fatalf("script/style 内容应被丢弃: %s", cleaned)
	}
	if !contains(cleaned, "'none'") {
		t.Fatalf("HTML 实体应被还原: %s", cleaned)
	}
	if !contains(cleaned, "hello world") {
		t.Fatalf("连续空白应折叠: %s", cleaned)
	}

	long := ""
	for i := 0; i < 400; i++ {
		long += "中"
	}
	truncated := CleanUpstreamTextLimit("application/json", long, 10)
	if len([]rune(truncated)) > 11 {
		t.Fatalf("应按字符截断, got %d 个字符", len([]rune(truncated)))
	}
}

// TestUpstreamUserAgentIsBrowserLike 确认默认 UA 不是 Go 默认值。
func TestUpstreamUserAgentIsBrowserLike(t *testing.T) {
	agent := UpstreamUserAgent()
	if agent == "" || contains(agent, "Go-http-client") {
		t.Fatalf("默认 UA 不应暴露 Go 客户端: %q", agent)
	}
	if !contains(agent, "Mozilla/5.0") {
		t.Fatalf("默认 UA 应伪装成浏览器: %q", agent)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
