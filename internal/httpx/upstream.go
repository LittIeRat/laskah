package httpx

import (
	"html"
	"os"
	"strings"
	"sync"
)

// browserUserAgent 是访问上游站点的默认 User-Agent。
//
// Go 默认会发 Go-http-client/1.1，Cloudflare 等 WAF 见到就直接返回人机验证页，
// 于是模型列表与额度查询都会拿到一整页 HTML。统一带上常见浏览器标识可以绕过
// 这类基于 UA 的粗粒度拦截；需要自定义时用 UPSTREAM_USER_AGENT 覆盖。
const browserUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

var (
	userAgentOnce  sync.Once
	userAgentValue string
)

// UpstreamUserAgent 返回访问上游站点使用的 User-Agent。
func UpstreamUserAgent() string {
	userAgentOnce.Do(func() {
		userAgentValue = strings.TrimSpace(os.Getenv("UPSTREAM_USER_AGENT"))
		if userAgentValue == "" {
			userAgentValue = browserUserAgent
		}
	})
	return userAgentValue
}

// maxUpstreamMessageRunes 限制回显给界面的上游报错长度，避免整页 HTML 灌进提示条。
const maxUpstreamMessageRunes = 240

// challengeMarkers 是人机验证与 WAF 拦截页的特征串（均为小写）。
var challengeMarkers = []string{
	"just a moment",
	"challenges.cloudflare.com",
	"cf-browser-verification",
	"cf_chl_opt",
	"__cf_chl",
	"attention required",
	"checking your browser",
	"enable javascript and cookies",
	"ddos-guard",
	"captcha",
}

// LooksLikeHTML 判断响应正文是 HTML 页面而不是 JSON。
func LooksLikeHTML(contentType, body string) bool {
	if strings.Contains(strings.ToLower(contentType), "text/html") {
		return true
	}
	trimmed := strings.TrimLeft(body, " \t\r\n\ufeff")
	if trimmed == "" {
		return false
	}
	lower := strings.ToLower(trimmed)
	return strings.HasPrefix(lower, "<!doctype") || strings.HasPrefix(lower, "<html")
}

// IsChallengePage 判断响应是否为人机验证或 WAF 拦截页。
func IsChallengePage(contentType, body string) bool {
	if !LooksLikeHTML(contentType, body) {
		return false
	}
	lower := strings.ToLower(body)
	for _, marker := range challengeMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// CleanUpstreamText 把上游正文压成一行可读文本：剥掉标签与脚本、还原实体、折叠空白并截断。
func CleanUpstreamText(contentType, body string) string {
	return CleanUpstreamTextLimit(contentType, body, maxUpstreamMessageRunes)
}

// CleanUpstreamTextLimit 与 CleanUpstreamText 相同，但可以指定截断长度。
//
// 网关判定「余额不足」要在原始文案里找关键字，所以那条链路用更宽的上限。
func CleanUpstreamTextLimit(contentType, body string, limit int) string {
	text := body
	if LooksLikeHTML(contentType, body) {
		text = stripTags(body)
	}
	text = html.UnescapeString(text)
	text = collapseSpaces(text)
	return truncateRunes(text, limit)
}

// DescribeUpstreamFailure 把上游失败响应翻译成对运维有指导意义的一句话。
func DescribeUpstreamFailure(contentType, body string) string {
	if IsChallengePage(contentType, body) {
		return "上游站点返回人机验证页（Cloudflare 一类的 WAF 拦截），服务器直连被挡下。请在上游把本机公网 IP 加进白名单，或确认 Base URL 指向 API 域名而不是网页域名"
	}
	if LooksLikeHTML(contentType, body) {
		detail := CleanUpstreamText(contentType, body)
		if detail == "" {
			detail = "空白页面"
		}
		return "上游返回 HTML 页面而不是 JSON（Base URL 可能写成了网页地址，或少了 /v1）：" + detail
	}
	if cleaned := CleanUpstreamText(contentType, body); cleaned != "" {
		return cleaned
	}
	return "上游返回空响应"
}

// stripTags 去掉 HTML 标签，并整段丢弃 script / style 内容。
func stripTags(raw string) string {
	var out strings.Builder
	out.Grow(len(raw) / 2)
	depth := 0
	for index := 0; index < len(raw); {
		if raw[index] == '<' {
			name, end := tagAt(raw, index)
			switch name {
			case "script", "style", "head", "noscript", "svg":
				depth++
			case "/script", "/style", "/head", "/noscript", "/svg":
				if depth > 0 {
					depth--
				}
			}
			out.WriteByte(' ')
			index = end
			continue
		}
		if depth == 0 {
			out.WriteByte(raw[index])
		}
		index++
	}
	return out.String()
}

// tagAt 读取 raw[start] 处的标签名（小写）与标签结束后的下标。
func tagAt(raw string, start int) (string, int) {
	index := start + 1
	nameStart := index
	for index < len(raw) && raw[index] != '>' {
		index++
	}
	end := index
	if end < len(raw) {
		end++
	}
	inner := raw[nameStart:index]
	if cut := strings.IndexAny(inner, " \t\r\n"); cut >= 0 {
		inner = inner[:cut]
	}
	return strings.ToLower(strings.TrimSpace(inner)), end
}

// collapseSpaces 把连续空白折叠成单个空格。
func collapseSpaces(raw string) string {
	var out strings.Builder
	out.Grow(len(raw))
	space := false
	for _, char := range raw {
		switch char {
		case ' ', '\t', '\r', '\n', '\f', '\v', '\u00a0':
			space = true
		default:
			if space && out.Len() > 0 {
				out.WriteByte(' ')
			}
			space = false
			out.WriteRune(char)
		}
	}
	return strings.TrimSpace(out.String())
}

// truncateRunes 按字符数截断，避免把多字节汉字切坏。
func truncateRunes(raw string, limit int) string {
	if limit <= 0 {
		return raw
	}
	runes := []rune(raw)
	if len(runes) <= limit {
		return raw
	}
	return strings.TrimSpace(string(runes[:limit])) + "…"
}
