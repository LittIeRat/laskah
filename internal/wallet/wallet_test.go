package wallet

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"laskah/internal/script"
)

func TestFetchFromUserSelf(t *testing.T) {
	var gotAuth, gotUser, gotContentType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/status":
			_, _ = fmt.Fprint(w, `{"data":{"quota_per_unit":500000}}`)
		case "/api/user/self":
			gotAuth = r.Header.Get("Authorization")
			gotUser = r.Header.Get("New-Api-User")
			gotContentType = r.Header.Get("Content-Type")
			_, _ = fmt.Fprint(w, `{"success":true,"data":{"group":"vip","quota":5000000,"used_quota":2500000}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	snapshot := NewClient().Fetch(context.Background(), Credentials{
		BaseURL:     server.URL,
		UserID:      "114514",
		AccessToken: "tok-abc",
		Timeout:     5 * time.Second,
	})
	if snapshot.Err != nil {
		t.Fatalf("查询应成功: %v", snapshot.Err)
	}
	if snapshot.Balance != 10 || snapshot.UsedAmount != 5 || snapshot.TotalAmount != 15 {
		t.Fatalf("额度换算错误: %#v", snapshot)
	}
	if snapshot.PlanName != "vip" || snapshot.Currency != "USD" || snapshot.Source != "/api/user/self" {
		t.Fatalf("快照字段错误: %#v", snapshot)
	}
	if gotAuth != "Bearer tok-abc" || gotUser != "114514" || gotContentType != "application/json" {
		t.Fatalf("请求头错误: %q %q %q", gotAuth, gotUser, gotContentType)
	}
}

func TestFetchUsesCustomQuotaPerUnit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/status":
			_, _ = fmt.Fprint(w, `{"data":{"quota_per_unit":1000}}`)
		case "/api/user/self":
			_, _ = fmt.Fprint(w, `{"success":true,"data":{"quota":2000,"used_quota":1000}}`)
		}
	}))
	defer server.Close()

	snapshot := NewClient().Fetch(context.Background(), Credentials{BaseURL: server.URL, AccessToken: "tok"})
	if snapshot.Err != nil || snapshot.Balance != 2 || snapshot.UsedAmount != 1 {
		t.Fatalf("自定义换算单位未生效: %#v", snapshot)
	}
	if snapshot.QuotaPerUnit != 1000 {
		t.Fatalf("应回传站点换算单位: %#v", snapshot)
	}
	if snapshot.PlanName != "默认套餐" {
		t.Fatalf("缺少分组时应回落默认套餐: %#v", snapshot)
	}
}

func TestFetchFallsBackToUsageEndpoint(t *testing.T) {
	var method, auth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/user/self":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = fmt.Fprint(w, `{"success":false,"message":"invalid token"}`)
		case "/api/usage":
			method = r.Method
			auth = r.Header.Get("Authorization")
			_, _ = fmt.Fprint(w, `{"balance":7.5,"used":2.5}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	snapshot := NewClient().Fetch(context.Background(), Credentials{
		BaseURL:     server.URL,
		AccessToken: "bad-token",
		APIKey:      "sk-upstream",
	})
	if snapshot.Err != nil {
		t.Fatalf("应回落到 /api/usage: %v", snapshot.Err)
	}
	if snapshot.Balance != 7.5 || snapshot.UsedAmount != 2.5 || snapshot.Source != "/api/usage" {
		t.Fatalf("回落结果错误: %#v", snapshot)
	}
	if method != http.MethodPost || auth != "Bearer sk-upstream" {
		t.Fatalf("/api/usage 请求方式错误: %s %s", method, auth)
	}
}

func TestFetchWithoutCredentials(t *testing.T) {
	snapshot := NewClient().Fetch(context.Background(), Credentials{BaseURL: "https://api.newapi.com"})
	if snapshot.Err == nil {
		t.Fatalf("缺少凭据应报错")
	}
	if !strings.Contains(snapshot.Err.Error(), "凭据") {
		t.Fatalf("错误信息应提示凭据缺失: %v", snapshot.Err)
	}

	empty := NewClient().Fetch(context.Background(), Credentials{})
	if empty.Err == nil || !strings.Contains(empty.Err.Error(), "请求地址") {
		t.Fatalf("缺少请求地址应报错: %v", empty.Err)
	}
}

func TestFetchReportsUpstreamFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/user/self" {
			_, _ = fmt.Fprint(w, `{"success":false,"message":"额度不足"}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	snapshot := NewClient().Fetch(context.Background(), Credentials{BaseURL: server.URL, AccessToken: "tok"})
	if snapshot.Err == nil || !strings.Contains(snapshot.Err.Error(), "额度不足") {
		t.Fatalf("应透出上游错误信息: %v", snapshot.Err)
	}
	if snapshot.CheckedAt.IsZero() {
		t.Fatalf("失败也应记录检查时间")
	}
}

func TestFetchUsageErrorField(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/usage" {
			_, _ = fmt.Fprint(w, `{"error":{"message":"key disabled"}}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	snapshot := NewClient().Fetch(context.Background(), Credentials{BaseURL: server.URL, APIKey: "sk-x"})
	if snapshot.Err == nil || !strings.Contains(snapshot.Err.Error(), "key disabled") {
		t.Fatalf("应识别 error 字段: %v", snapshot.Err)
	}
}

// TestFetchReportsChallengePage 确认站点被 Cloudflare 挡下时给出可读原因而不是 JSON 语法错误。
func TestFetchReportsChallengePage(t *testing.T) {
	var gotAgent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAgent = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		_, _ = fmt.Fprint(w, `<!DOCTYPE html><html lang="en-US"><head><title>Just a moment...</title></head><body>challenges.cloudflare.com</body></html>`)
	}))
	defer server.Close()

	snapshot := NewClient().Fetch(context.Background(), Credentials{
		BaseURL:     server.URL,
		UserID:      "114514",
		AccessToken: "tok-abc",
		Timeout:     5 * time.Second,
	})
	if snapshot.Err == nil {
		t.Fatal("拦截页应视为查询失败")
	}
	message := snapshot.Err.Error()
	if !strings.Contains(message, "人机验证") {
		t.Fatalf("应说明是人机验证页: %s", message)
	}
	if strings.Contains(message, "<") {
		t.Fatalf("错误信息不应包含原始 HTML: %s", message)
	}
	if gotAgent == "" || strings.Contains(gotAgent, "Go-http-client") {
		t.Fatalf("额度查询未带浏览器 UA: %q", gotAgent)
	}
}

// scriptProgram 编译一段测试脚本，编译失败直接失败退出。
func scriptProgram(t *testing.T, source string) *script.Program {
	t.Helper()
	program, err := script.Parse(source)
	if err != nil {
		t.Fatalf("脚本应能编译: %v", err)
	}
	return program
}

// TestFetchByScriptUsesCustomQueryURL 确认自定义额度查询地址会替换脚本里的 {{baseUrl}}，
// 否则「额度接口与推理地址不同源」的站点永远查不到余额。
func TestFetchByScriptUsesCustomQueryURL(t *testing.T) {
	var gotPath, gotAuth, gotUser string
	console := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotUser = r.Header.Get("New-Api-User")
		_, _ = fmt.Fprint(w, `{"success":true,"data":{"group":"vip","quota":5000000,"used_quota":2500000}}`)
	}))
	defer console.Close()

	inference := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("填了额度查询地址后不应再请求推理站点: %s", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer inference.Close()

	program := scriptProgram(t, `({
  request: {
    url: "{{baseUrl}}/api/user/self",
    method: "GET",
    headers: {
      "Authorization": "Bearer {{accessToken}}",
      "New-Api-User": "{{userId}}"
    }
  },
  extractor: function (response) {
    if (response.success && response.data) {
      return {
        planName: response.data.group,
        remaining: response.data.quota / 500000,
        used: response.data.used_quota / 500000,
        total: (response.data.quota + response.data.used_quota) / 500000,
        unit: "USD"
      };
    }
    return { isValid: false, invalidMessage: response.message };
  }
})`)

	snapshot := NewClient().Fetch(context.Background(), Credentials{
		BaseURL:     inference.URL,
		QueryURL:    console.URL,
		AccessToken: "tok-abc",
		UserID:      "114514",
		Script:      program,
		Timeout:     5 * time.Second,
	})
	if snapshot.Err != nil {
		t.Fatalf("脚本查询应成功: %v", snapshot.Err)
	}
	if snapshot.Balance != 10 || snapshot.UsedAmount != 5 || snapshot.TotalAmount != 15 {
		t.Fatalf("额度换算错误: %#v", snapshot)
	}
	if !snapshot.HasBalance || !snapshot.HasUsed || !snapshot.HasTotal {
		t.Fatalf("应标记三项数值均已返回: %#v", snapshot)
	}
	if snapshot.Source != "script" || snapshot.PlanName != "vip" || snapshot.Currency != "USD" {
		t.Fatalf("快照字段错误: %#v", snapshot)
	}
	if gotPath != "/api/user/self" || gotAuth != "Bearer tok-abc" || gotUser != "114514" {
		t.Fatalf("脚本请求构造错误: %q %q %q", gotPath, gotAuth, gotUser)
	}
}

// TestFetchByScriptReadsNonSuccessJSON 确认非 2xx 的 JSON 正文仍交给 extractor：
// 很多站点用 401/403 配合 JSON 说明失效原因，直接判失败会丢掉这条信息。
func TestFetchByScriptReadsNonSuccessJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, `{"success":false,"message":"令牌已过期"}`)
	}))
	defer server.Close()

	program := scriptProgram(t, `({
  request: { url: "{{baseUrl}}/api/user/self", method: "GET" },
  extractor: function (response) {
    if (response.success) { return { remaining: 1 }; }
    return { isValid: false, invalidMessage: response.message };
  }
})`)

	snapshot := NewClient().Fetch(context.Background(), Credentials{BaseURL: server.URL, Script: program})
	if snapshot.Err == nil {
		t.Fatal("isValid=false 应视为查询失败")
	}
	if !strings.Contains(snapshot.Err.Error(), "令牌已过期") {
		t.Fatalf("应透出脚本给出的失效原因: %v", snapshot.Err)
	}
}

// TestFetchByScriptDerivesRemaining 覆盖只返回 total/used 的脚本：
// 剩余额度是耗尽判定的唯一依据，必须能推算出来。
func TestFetchByScriptDerivesRemaining(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"total":30,"used":12}`)
	}))
	defer server.Close()

	program := scriptProgram(t, `({
  request: { url: "{{baseUrl}}/quota", method: "GET" },
  extractor: function (response) {
    return { total: response.total, used: response.used, unit: "CNY" };
  }
})`)

	snapshot := NewClient().Fetch(context.Background(), Credentials{BaseURL: server.URL, Script: program})
	if snapshot.Err != nil {
		t.Fatalf("应能由 total - used 推算剩余: %v", snapshot.Err)
	}
	if snapshot.Balance != 18 || !snapshot.HasBalance {
		t.Fatalf("剩余额度推算错误: %#v", snapshot)
	}
	if snapshot.Currency != "CNY" {
		t.Fatalf("应采用脚本给出的单位: %#v", snapshot)
	}
}

// TestFetchByScriptWithoutNumbers 确认脚本查不到任何数值时报错而不是把余额写成 0，
// 否则账号会因为脚本写得不全被误判欠费并暂停。
func TestFetchByScriptWithoutNumbers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"ok":true}`)
	}))
	defer server.Close()

	program := scriptProgram(t, `({
  request: { url: "{{baseUrl}}/quota", method: "GET" },
  extractor: function (response) { return { isValid: response.ok, planName: "pro" }; }
})`)

	snapshot := NewClient().Fetch(context.Background(), Credentials{BaseURL: server.URL, Script: program})
	if snapshot.Err == nil || !strings.Contains(snapshot.Err.Error(), "remaining") {
		t.Fatalf("缺少剩余额度应报错: %v", snapshot.Err)
	}
	if snapshot.HasBalance {
		t.Fatalf("不应标记余额可用: %#v", snapshot)
	}
}

// TestFetchByScriptPostsBody 覆盖 POST + 请求体 + apiKey 占位符的脚本形态。
func TestFetchByScriptPostsBody(t *testing.T) {
	var method, auth, body, contentType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		auth = r.Header.Get("Authorization")
		contentType = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
		_, _ = fmt.Fprint(w, `{"balance":3.25}`)
	}))
	defer server.Close()

	program := scriptProgram(t, `({
  request: {
    url: "{{baseUrl}}/api/usage",
    method: "POST",
    headers: { "Authorization": "Bearer {{apiKey}}" },
    body: { scope: "balance" }
  },
  extractor: function (response) {
    return { isValid: !response.error, remaining: response.balance, unit: "USD" };
  }
})`)

	snapshot := NewClient().Fetch(context.Background(), Credentials{BaseURL: server.URL, APIKey: "sk-live", Script: program})
	if snapshot.Err != nil || snapshot.Balance != 3.25 {
		t.Fatalf("POST 脚本查询失败: %#v", snapshot)
	}
	if method != http.MethodPost || auth != "Bearer sk-live" || contentType != "application/json" {
		t.Fatalf("请求构造错误: %s %s %s", method, auth, contentType)
	}
	if !strings.Contains(body, "\"scope\"") {
		t.Fatalf("请求体未透传: %s", body)
	}
}

// TestFetchByScriptRejectsHTML 确认站点返回人机验证页时给出可读原因。
func TestFetchByScriptRejectsHTML(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		_, _ = fmt.Fprint(w, `<!DOCTYPE html><html><head><title>Just a moment...</title></head><body>challenges.cloudflare.com</body></html>`)
	}))
	defer server.Close()

	program := scriptProgram(t, `({
  request: { url: "{{baseUrl}}/quota", method: "GET" },
  extractor: function (response) { return { remaining: response.balance }; }
})`)

	snapshot := NewClient().Fetch(context.Background(), Credentials{BaseURL: server.URL, Script: program})
	if snapshot.Err == nil || !strings.Contains(snapshot.Err.Error(), "人机验证") {
		t.Fatalf("应识别人机验证页: %v", snapshot.Err)
	}
}

// TestFetchSlowStatusProbeDoesNotBlockBalance 覆盖本轮修复的核心问题：
// /api/status 只为拿换算单位，卡住时不能吃掉整份超时预算，
// 否则真正的余额查询必然报 context deadline exceeded。
func TestFetchSlowStatusProbeDoesNotBlockBalance(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/status":
			// 探测挂到超过账号超时：以前三次请求共享一个 deadline，这里会连带拖死余额查询。
			<-r.Context().Done()
		case "/api/user/self":
			_, _ = fmt.Fprint(w, `{"success":true,"data":{"quota":5000000,"used_quota":0}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	snapshot := NewClient().Fetch(context.Background(), Credentials{
		BaseURL:     server.URL,
		AccessToken: "tok",
		UserID:      "114514",
		Timeout:     3 * time.Second,
	})
	if snapshot.Err != nil {
		t.Fatalf("探测慢不应影响余额查询: %v", snapshot.Err)
	}
	if snapshot.Balance != 10 {
		t.Fatalf("应按默认换算单位得到余额: %#v", snapshot)
	}
	if snapshot.QuotaPerUnit != DefaultQuotaPerUnit {
		t.Fatalf("探测失败应回落默认换算单位: %#v", snapshot)
	}
}

// TestFetchTimeoutIsPerAttempt 确认配置的超时是「单个请求最多等多久」，
// 而不是所有尝试共享的总预算：/api/user/self 用满整份时间后，
// /api/usage 仍要拿到完整的一份，否则回落路径形同虚设。
func TestFetchTimeoutIsPerAttempt(t *testing.T) {
	var usageHit bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/status":
			_, _ = fmt.Fprint(w, `{"data":{"quota_per_unit":500000}}`)
		case "/api/user/self":
			<-r.Context().Done()
		case "/api/usage":
			usageHit = true
			_, _ = fmt.Fprint(w, `{"balance":4.5}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	snapshot := NewClient().Fetch(context.Background(), Credentials{
		BaseURL:     server.URL,
		AccessToken: "tok",
		UserID:      "114514",
		APIKey:      "sk-upstream",
		Timeout:     time.Second,
	})
	if !usageHit {
		t.Fatal("第一条路径超时后应继续尝试 /api/usage")
	}
	if snapshot.Err != nil || snapshot.Balance != 4.5 {
		t.Fatalf("回落查询应成功: %#v", snapshot)
	}
}

// TestFetchTimeoutMessageIsActionable 确认超时错误不再是一句裸的
// context deadline exceeded：管理员需要知道是哪个地址、等了多久、下一步怎么办。
func TestFetchTimeoutMessageIsActionable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	program := scriptProgram(t, `({
  request: { url: "{{baseUrl}}/quota", method: "GET" },
  extractor: function (response) { return { remaining: response.balance }; }
})`)

	snapshot := NewClient().Fetch(context.Background(), Credentials{
		BaseURL: server.URL,
		Script:  program,
		Timeout: 500 * time.Millisecond,
	})
	if snapshot.Err == nil {
		t.Fatal("应报超时")
	}
	message := snapshot.Err.Error()
	for _, want := range []string{"超时", "/quota", "超时时间"} {
		if !strings.Contains(message, want) {
			t.Fatalf("错误信息缺少 %q: %s", want, message)
		}
	}
	if strings.Contains(message, "context deadline exceeded") {
		t.Fatalf("不应把裸错误抛给管理员: %s", message)
	}
}

// TestNormalizeTimeout 覆盖超时归一化：0 与越界回落默认值，上限被夹住。
func TestNormalizeTimeout(t *testing.T) {
	if NormalizeTimeout(0) != DefaultTimeout {
		t.Fatalf("0 应回落默认值")
	}
	if NormalizeTimeout(-5*time.Second) != DefaultTimeout {
		t.Fatalf("负值应回落默认值")
	}
	if NormalizeTimeout(45*time.Second) != 45*time.Second {
		t.Fatalf("区间内的值应原样使用")
	}
	if NormalizeTimeout(time.Hour) != MaxTimeout {
		t.Fatalf("超过上限应夹到 %s", MaxTimeout)
	}
	if DefaultTimeout != 30*time.Second {
		t.Fatalf("默认超时应为 30 秒, got %s", DefaultTimeout)
	}
	// 探测预算不能超过账号自己的超时，也不能超过硬上限。
	if probeTimeout(2*time.Second) != 2*time.Second {
		t.Fatalf("探测不应比配置的超时还长")
	}
	if probeTimeout(time.Minute) != maxStatusProbeTimeout {
		t.Fatalf("探测应被夹到 %s", maxStatusProbeTimeout)
	}
}
