package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"laskah/internal/store"
	"laskah/internal/tokenizer"
)

// 测试用超级管理员凭据：真实部署由 /setup 页面创建，测试里直接注入。
const (
	testSuperUser     = "Digital Gleam"
	testSuperPassword = "sup3r-secret"
)

type harness struct {
	t      *testing.T
	app    *App
	server *httptest.Server
	client *http.Client
	token  string
	csrf   string
}

func newHarness(t *testing.T) *harness {
	return newHarnessWith(t, nil)
}

// newHarnessWith 允许调整服务选项，用于验证 PUBLIC_MODELS 一类的开关。
func newHarnessWith(t *testing.T, tweak func(*Options)) *harness {
	t.Helper()
	t.Setenv("ADMIN_TOKEN", "test-admin-token")
	t.Setenv("MASTER_KEY", "unit-test-master-key")

	options := Options{
		DataFile:        filepath.Join(t.TempDir(), "db.json"),
		Strategy:        "round-robin",
		MaxRetries:      3,
		BalanceInterval: time.Hour,
	}
	if tweak != nil {
		tweak(&options)
	}

	app, err := New(options)
	if err != nil {
		t.Fatalf("初始化服务失败: %v", err)
	}
	t.Cleanup(app.Close)

	server := httptest.NewServer(app.Handler)
	t.Cleanup(server.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("创建 Cookie 容器失败: %v", err)
	}
	client := &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	h := &harness{t: t, app: app, server: server, client: client, token: app.Store.AdminToken()}
	if _, err := app.Store.CreateSuperAdmin(testSuperUser, testSuperPassword); err != nil {
		t.Fatalf("创建超级管理员失败: %v", err)
	}
	return h
}

// newBareHarness 保留“未初始化”状态，用于验证 /setup 流程。
func newBareHarness(t *testing.T) *harness {
	t.Helper()
	t.Setenv("ADMIN_TOKEN", "test-admin-token")
	t.Setenv("MASTER_KEY", "unit-test-master-key")

	app, err := New(Options{
		DataFile:        filepath.Join(t.TempDir(), "db.json"),
		Strategy:        "round-robin",
		BalanceInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("初始化服务失败: %v", err)
	}
	t.Cleanup(app.Close)

	server := httptest.NewServer(app.Handler)
	t.Cleanup(server.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("创建 Cookie 容器失败: %v", err)
	}
	client := &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &harness{t: t, app: app, server: server, client: client, token: app.Store.AdminToken()}
}

func (h *harness) do(method, path string, body any, auth string) (*http.Response, map[string]any) {
	h.t.Helper()

	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		encoded, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("序列化请求失败: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}

	request, err := http.NewRequest(method, h.server.URL+path, reader)
	if err != nil {
		h.t.Fatalf("构造请求失败: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	if auth != "" {
		request.Header.Set("Authorization", "Bearer "+auth)
	}
	if h.csrf != "" {
		request.Header.Set("X-CSRF-Token", h.csrf)
	}

	response, err := h.client.Do(request)
	if err != nil {
		h.t.Fatalf("请求失败: %v", err)
	}
	decoded := map[string]any{}
	_ = json.NewDecoder(response.Body).Decode(&decoded)
	response.Body.Close()
	return response, decoded
}

func (h *harness) admin(method, path string, body any) (*http.Response, map[string]any) {
	return h.do(method, path, body, h.token)
}

// doRaw 与 do 相同，但返回原始响应体，用于校验 SSE 流。
func (h *harness) doRaw(method, path string, body any, auth string) (*http.Response, string) {
	h.t.Helper()

	encoded, err := json.Marshal(body)
	if err != nil {
		h.t.Fatalf("序列化请求失败: %v", err)
	}
	request, err := http.NewRequest(method, h.server.URL+path, bytes.NewReader(encoded))
	if err != nil {
		h.t.Fatalf("构造请求失败: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	if auth != "" {
		request.Header.Set("Authorization", "Bearer "+auth)
	}
	if h.csrf != "" {
		request.Header.Set("X-CSRF-Token", h.csrf)
	}
	response, err := h.client.Do(request)
	if err != nil {
		h.t.Fatalf("请求失败: %v", err)
	}
	raw, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		h.t.Fatalf("读取响应体失败: %v", err)
	}
	return response, string(raw)
}

func (h *harness) get(path string) *http.Response {
	h.t.Helper()
	request, err := http.NewRequest(http.MethodGet, h.server.URL+path, nil)
	if err != nil {
		h.t.Fatalf("构造请求失败: %v", err)
	}
	response, err := h.client.Do(request)
	if err != nil {
		h.t.Fatalf("请求 %s 失败: %v", path, err)
	}
	return response
}

// login 用默认管理员凭据登录，之后的请求走 Cookie 会话 + CSRF 头。
func (h *harness) login(user, password string) (*http.Response, map[string]any) {
	h.t.Helper()
	response, body := h.do(http.MethodPost, "/admin/login", map[string]any{"user": user, "password": password}, "")
	if response.StatusCode == http.StatusOK {
		h.csrf, _ = body["csrfToken"].(string)
	}
	return response, body
}

func (h *harness) createGroup(name string) string {
	h.t.Helper()
	response, body := h.admin(http.MethodPost, "/admin/groups", map[string]any{"name": name})
	if response.StatusCode != http.StatusCreated {
		h.t.Fatalf("创建分组失败: %d %#v", response.StatusCode, body)
	}
	return body["data"].(map[string]any)["id"].(string)
}

// fakeSite 模拟 New API 站点：/api/status 提供换算单位，/api/user/self 提供余额。
// findAccount 从 /admin/accounts 列表里取出指定账号，找不到返回 nil。
func findAccount(list []any, id string) map[string]any {
	for _, item := range list {
		account, ok := item.(map[string]any)
		if ok && account["id"] == id {
			return account
		}
	}
	return nil
}

func fakeSite(t *testing.T, quota, used *float64) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/status":
			_, _ = fmt.Fprint(w, `{"data":{"quota_per_unit":500000}}`)
		case "/api/user/self":
			if r.Header.Get("New-Api-User") == "" || r.Header.Get("Authorization") == "" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = fmt.Fprintf(w, `{"success":true,"data":{"group":"vip","quota":%f,"used_quota":%f}}`, *quota, *used)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// fakeUpstream 模拟 OpenAI 兼容上游，记录命中的上游 API Key。
func fakeUpstream(t *testing.T, hits *[]string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		switch r.URL.Path {
		case "/v1/models", "/models":
			_, _ = fmt.Fprint(w, `{"data":[{"id":"gpt-4o-mini"},{"id":"gpt-4o"},{"id":"gpt-4o"}]}`)
		case "/v1/chat/completions", "/chat/completions":
			*hits = append(*hits, key)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"id":"cmpl_1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`)
		case "/v1/responses", "/responses":
			*hits = append(*hits, key)
			w.Header().Set("Content-Type", "application/json")
			// 上游故意自报一个夸张的 usage，用来验证网关只采信本地口径。
			_, _ = fmt.Fprint(w, `{"id":"resp_1","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":9999,"output_tokens":8888,"total_tokens":18887}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func chatBody() map[string]any {
	return map[string]any{
		"model":    "gpt-4o-mini",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
}

func TestLoginFlowAndThrottle(t *testing.T) {
	h := newHarness(t)

	if response, _ := h.login(testSuperUser, "wrong"); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("错误口令应返回 401, got %d", response.StatusCode)
	}

	response, body := h.login(testSuperUser, testSuperPassword)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("超级管理员凭据应可登录: %d %#v", response.StatusCode, body)
	}
	if body["user"] != testSuperUser {
		t.Fatalf("返回的账户名错误: %#v", body)
	}
	if body["isSuper"] != true || body["home"] != "/dashboard" {
		t.Fatalf("登录响应应包含角色与落地页: %#v", body)
	}
	if h.csrf == "" {
		t.Fatalf("登录应返回 csrfToken: %#v", body)
	}

	// 会话可用后无需 Bearer 令牌也能访问管理接口。
	if response, _ := h.do(http.MethodGet, "/admin/groups", nil, ""); response.StatusCode != http.StatusOK {
		t.Fatalf("Cookie 会话应可访问管理接口, got %d", response.StatusCode)
	}

	// 写请求缺少 CSRF 头应被拒绝。
	saved := h.csrf
	h.csrf = ""
	if response, _ := h.do(http.MethodPost, "/admin/groups", map[string]any{"name": "no-csrf"}, ""); response.StatusCode != http.StatusForbidden {
		t.Fatalf("缺少 CSRF 头应返回 403, got %d", response.StatusCode)
	}
	h.csrf = saved

	if response, _ := h.do(http.MethodPost, "/admin/logout", nil, ""); response.StatusCode != http.StatusOK {
		t.Fatalf("注销应返回 200, got %d", response.StatusCode)
	}
	if response, _ := h.do(http.MethodGet, "/admin/groups", nil, ""); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("注销后应返回 401, got %d", response.StatusCode)
	}

	// 连续失败触发登录限流。
	limited := false
	for index := 0; index < 8; index++ {
		if response, _ := h.login(testSuperUser, "bad-password"); response.StatusCode == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatalf("连续失败登录应触发限流")
	}
}

func TestPageGatingAndLegacyKeysRedirect(t *testing.T) {
	h := newHarness(t)

	for _, path := range []string{"/manage", "/dashboard"} {
		response := h.get(path)
		response.Body.Close()
		if response.StatusCode != http.StatusFound || response.Header.Get("Location") != "/login" {
			t.Fatalf("%s 未登录应 302 到 /login, got %d %s", path, response.StatusCode, response.Header.Get("Location"))
		}
	}

	for _, path := range []string{"/keys", "/keys/"} {
		response := h.get(path)
		response.Body.Close()
		if response.StatusCode != http.StatusMovedPermanently || response.Header.Get("Location") != "/dashboard" {
			t.Fatalf("%s 应 301 到 /dashboard, got %d %s", path, response.StatusCode, response.Header.Get("Location"))
		}
	}

	response := h.get("/")
	response.Body.Close()
	if response.StatusCode != http.StatusFound || response.Header.Get("Location") != "/dashboard" {
		t.Fatalf("/ 应跳转到 /dashboard, got %d %s", response.StatusCode, response.Header.Get("Location"))
	}

	login := h.get("/login")
	login.Body.Close()
	if login.StatusCode != http.StatusOK {
		t.Fatalf("/login 应公开可访问, got %d", login.StatusCode)
	}

	if response, _ := h.login(testSuperUser, testSuperPassword); response.StatusCode != http.StatusOK {
		t.Fatalf("登录失败: %d", response.StatusCode)
	}
	for _, path := range []string{"/manage", "/dashboard"} {
		page := h.get(path)
		contentType := page.Header.Get("Content-Type")
		page.Body.Close()
		if page.StatusCode != http.StatusOK || !strings.Contains(contentType, "text/html") {
			t.Fatalf("%s 登录后应返回 HTML, got %d %s", path, page.StatusCode, contentType)
		}
	}
}

func TestStaticAssetsAndSecurityHeaders(t *testing.T) {
	h := newHarness(t)

	for _, asset := range []string{"/style.css", "/app.js", "/login.js", "/manage.js", "/dashboard.js"} {
		response := h.get(asset)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s 应返回 200, got %d", asset, response.StatusCode)
		}
	}

	// 非白名单后缀不应被 FileServer 暴露。
	leaked := h.get("/manage.html")
	leaked.Body.Close()
	if leaked.StatusCode != http.StatusNotFound {
		t.Fatalf("直接访问 .html 资源应 404, got %d", leaked.StatusCode)
	}

	response := h.get("/login")
	response.Body.Close()
	csp := response.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "unsafe-inline") {
		t.Fatalf("CSP 配置异常: %s", csp)
	}
	for header, expected := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := response.Header.Get(header); got != expected {
			t.Fatalf("%s 应为 %s, got %s", header, expected, got)
		}
	}
}

func TestHealthHidesSensitiveData(t *testing.T) {
	h := newHarness(t)

	response, body := h.do(http.MethodGet, "/health", nil, "")
	if response.StatusCode != http.StatusOK || body["ok"] != true {
		t.Fatalf("/health 异常: %d %#v", response.StatusCode, body)
	}
	for _, field := range []string{"balance", "strategy", "adminToken", "accountList"} {
		if _, exists := body[field]; exists {
			t.Fatalf("/health 不应暴露 %s: %#v", field, body)
		}
	}
}

func TestGroupAndAccountLifecycle(t *testing.T) {
	h := newHarness(t)
	quota, used := 5000000.0, 500000.0
	site := fakeSite(t, &quota, &used)
	hits := []string{}
	upstream := fakeUpstream(t, &hits)

	groupID := h.createGroup("团队 A")
	if response, _ := h.admin(http.MethodPost, "/admin/groups", map[string]any{"name": "团队 A"}); response.StatusCode != http.StatusConflict {
		t.Fatalf("重复分组名应返回 409, got %d", response.StatusCode)
	}

	// 模型探测：结果需去重排序，供界面勾选。
	response, probe := h.admin(http.MethodPost, "/admin/models/probe", map[string]any{
		"baseUrl": upstream.URL + "/v1",
		"apiKey":  "sk-probe",
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("模型探测失败: %d %#v", response.StatusCode, probe)
	}
	models := probe["data"].([]any)
	if len(models) != 2 || models[0] != "gpt-4o" || models[1] != "gpt-4o-mini" {
		t.Fatalf("模型列表应去重排序: %#v", models)
	}

	// 批量粘贴 60 个 key，只应导入前 50 个。
	lines := make([]string, 0, 60)
	for index := 0; index < 60; index++ {
		lines = append(lines, fmt.Sprintf("sk-key-%02d", index))
	}
	response, body := h.admin(http.MethodPost, "/admin/accounts", map[string]any{
		"name":             "newapi-01",
		"groupId":          groupID,
		"baseUrl":          upstream.URL + "/v1",
		"siteUrl":          site.URL,
		"userId":           "114514",
		"accessToken":      "tok",
		"timeoutSeconds":   10,
		"queryIntervalMin": 0,
		"keys":             strings.Join(lines, "\n"),
		"selectedModels":   []string{"gpt-4o-mini"},
	})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("创建账号失败: %d %#v", response.StatusCode, body)
	}
	if created := int(body["created"].(float64)); created != store.MaxKeysPerAccount {
		t.Fatalf("单账号应只导入 %d 个 API, got %d", store.MaxKeysPerAccount, created)
	}
	if skipped := body["skipped"].([]any); len(skipped) != 60-store.MaxKeysPerAccount {
		t.Fatalf("应记录 %d 条被忽略的 key: %d", 60-store.MaxKeysPerAccount, len(skipped))
	}

	account := body["data"].(map[string]any)
	accountID := account["id"].(string)
	if account["balance"].(float64) != 10 {
		t.Fatalf("余额应换算为 quota/500000 = 10, got %#v", account["balance"])
	}
	if account["usedAmount"].(float64) != 1 {
		t.Fatalf("已用金额应为 1, got %#v", account["usedAmount"])
	}
	if account["planName"] != "vip" {
		t.Fatalf("套餐名应取自站点响应: %#v", account["planName"])
	}
	if account["hasAccessToken"] != true || account["hasUserId"] != true {
		t.Fatalf("应只回显凭据是否配置: %#v", account)
	}
	for _, field := range []string{"accessToken", "userId", "baseUrl", "siteUrl"} {
		if _, exists := account[field]; exists {
			t.Fatalf("账号视图不应回显 %s: %#v", field, account)
		}
	}

	// 缺少分组时拒绝创建。
	if response, _ := h.admin(http.MethodPost, "/admin/accounts", map[string]any{
		"name":    "no-group",
		"baseUrl": upstream.URL + "/v1",
		"keys":    "sk-x",
	}); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("未选择分组应返回 400, got %d", response.StatusCode)
	}

	// 保存后不可修改配置。
	for _, method := range []string{http.MethodPatch, http.MethodPut} {
		response, patched := h.admin(method, "/admin/accounts/"+accountID, map[string]any{"name": "renamed"})
		if response.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("%s 账号应返回 405, got %d %#v", method, response.StatusCode, patched)
		}
	}

	// 仅允许查询余额。
	if response, balance := h.admin(http.MethodGet, "/admin/accounts/"+accountID+"/balance", nil); response.StatusCode != http.StatusOK {
		t.Fatalf("查询余额失败: %d %#v", response.StatusCode, balance)
	}

	_, dashboard := h.admin(http.MethodGet, "/admin/dashboard", nil)
	totals := dashboard["data"].(map[string]any)
	groups := totals["groups"].([]any)
	if len(groups) != 1 {
		t.Fatalf("看板应包含 1 个分组: %#v", groups)
	}
	summary := groups[0].(map[string]any)
	if summary["balance"].(float64) != 10 || int(summary["apiCount"].(float64)) != store.MaxKeysPerAccount {
		t.Fatalf("分组汇总错误: %#v", summary)
	}
	if summary["enabled"] != true {
		t.Fatalf("分组应默认启用: %#v", summary)
	}

	// 删除分组应级联删除账号与其名下 API。
	if response, _ := h.admin(http.MethodDelete, "/admin/groups/"+groupID, nil); response.StatusCode != http.StatusOK {
		t.Fatalf("删除分组失败: %d", response.StatusCode)
	}
	_, after := h.admin(http.MethodGet, "/admin/accounts", nil)
	if remaining := after["data"].([]any); len(remaining) != 0 {
		t.Fatalf("删除分组后账号应被清理: %#v", remaining)
	}
	afterTotals := after["totals"].(map[string]any)["accounts"].(map[string]any)
	if afterTotals["apiCount"].(float64) != 0 {
		t.Fatalf("上游 API 应级联清理: %#v", afterTotals)
	}
}

func TestKeyBulkCreationAndAccountLocalBalancing(t *testing.T) {
	h := newHarness(t)
	quota, used := 5000000.0, 0.0
	site := fakeSite(t, &quota, &used)
	hitsA := []string{}
	hitsB := []string{}
	upstreamA := fakeUpstream(t, &hitsA)
	upstreamB := fakeUpstream(t, &hitsB)

	groupID := h.createGroup("团队 A")
	otherGroup := h.createGroup("团队 B")

	response, body := h.admin(http.MethodPost, "/admin/accounts", map[string]any{
		"name":        "acct-a",
		"groupId":     groupID,
		"baseUrl":     upstreamA.URL + "/v1",
		"siteUrl":     site.URL,
		"userId":      "1",
		"accessToken": "tok",
		"keys":        "sk-a1\nsk-a2\nsk-a3",
	})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("创建账号 A 失败: %d %#v", response.StatusCode, body)
	}

	if response, second := h.admin(http.MethodPost, "/admin/accounts", map[string]any{
		"name":        "acct-b",
		"groupId":     otherGroup,
		"baseUrl":     upstreamB.URL + "/v1",
		"siteUrl":     site.URL,
		"userId":      "2",
		"accessToken": "tok",
		"keys":        "sk-b1",
	}); response.StatusCode != http.StatusCreated {
		t.Fatalf("创建账号 B 失败: %d %#v", response.StatusCode, second)
	}

	// 批量创建限定分组的网关密钥。
	response, bulk := h.admin(http.MethodPost, "/admin/keys/bulk", map[string]any{
		"count":    5,
		"template": map[string]any{"name": "client", "groupId": groupID, "prefix": "sk-lb"},
	})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("批量创建密钥失败: %d %#v", response.StatusCode, bulk)
	}
	created := bulk["data"].([]any)
	if len(created) != 5 {
		t.Fatalf("应创建 5 个密钥: %d", len(created))
	}
	secret := created[0].(map[string]any)["key"].(string)
	if !strings.HasPrefix(secret, "sk-lb-") {
		t.Fatalf("密钥前缀错误: %s", secret)
	}

	// 分组限定的密钥只能命中本分组账号，且在账号内的 3 个 API 之间轮转。
	for index := 0; index < 3; index++ {
		if response, chat := h.do(http.MethodPost, "/v1/chat/completions", chatBody(), secret); response.StatusCode != http.StatusOK {
			t.Fatalf("第 %d 次调用失败: %d %#v", index+1, response.StatusCode, chat)
		}
	}
	if len(hitsB) != 0 {
		t.Fatalf("不应跨分组调用: %#v", hitsB)
	}
	distinct := map[string]bool{}
	for _, key := range hitsA {
		distinct[key] = true
	}
	if len(hitsA) != 3 || len(distinct) != 3 {
		t.Fatalf("账号内应轮转 3 个 API: %#v", hitsA)
	}

	_, dashboard := h.admin(http.MethodGet, "/admin/dashboard", nil)
	keys := dashboard["keys"].([]any)
	if len(keys) != 5 {
		t.Fatalf("看板应列出 5 个密钥: %d", len(keys))
	}
	first := keys[0].(map[string]any)
	if _, exists := first["key"]; exists {
		t.Fatalf("列表不应回显密钥明文: %#v", first)
	}
	if !strings.Contains(first["keyMasked"].(string), "******") {
		t.Fatalf("密钥应脱敏展示: %#v", first["keyMasked"])
	}
	// token 计量以本站自己的估算为准（上游自报的 7/次仅作对照）：
	// 上游会谎报用量，因此看板与计费都不能采信它的数字。
	tokens := dashboard["data"].(map[string]any)["tokens"].(map[string]any)
	wantPerCall := tokenizer.CountPrompt(chatBody()) + tokenizer.CountText("hi")
	if tokens["keys"].(float64) != float64(3*wantPerCall) {
		t.Fatalf("密钥累计 tokens 应为本地估算 3*%d, got %#v", wantPerCall, tokens["keys"])
	}
	if tokens["upstream"].(float64) != 21 {
		t.Fatalf("上游自报 tokens 应单独留存 3*7=21, got %#v", tokens["upstream"])
	}
	if tokens["selfMetered"] != true {
		t.Fatalf("看板应标明使用本站自算口径: %#v", tokens)
	}

	keyID := first["id"].(string)
	_, revealed := h.admin(http.MethodGet, "/admin/keys/"+keyID+"/reveal", nil)
	if revealed["data"].(map[string]any)["key"] == nil {
		t.Fatalf("显式 reveal 应返回明文: %#v", revealed)
	}

	if response, _ := h.admin(http.MethodPost, "/admin/keys/"+keyID+"/reset-usage", nil); response.StatusCode != http.StatusOK {
		t.Fatalf("重置用量失败: %d", response.StatusCode)
	}

	ids := []string{}
	for _, item := range keys {
		ids = append(ids, item.(map[string]any)["id"].(string))
	}
	_, deleted := h.admin(http.MethodDelete, "/admin/keys/batch", map[string]any{"ids": ids})
	if int(deleted["removed"].(float64)) != 5 {
		t.Fatalf("应批量删除 5 个密钥: %#v", deleted)
	}
}

// TestExhaustedAccountIsSuspendedAndTrafficFailsOver 验证余额耗尽走「暂停」而不是删除：
// 账号与上游 API 全部保留，只是退出分配池，流量无缝切到健康账号。
func TestExhaustedAccountIsSuspendedAndTrafficFailsOver(t *testing.T) {
	h := newHarness(t)
	healthyQuota, healthyUsed := 5000000.0, 0.0
	drainQuota, drainUsed := 5000000.0, 0.0
	healthySite := fakeSite(t, &healthyQuota, &healthyUsed)
	drainSite := fakeSite(t, &drainQuota, &drainUsed)
	hitsHealthy := []string{}
	hitsDrain := []string{}
	healthyUpstream := fakeUpstream(t, &hitsHealthy)
	drainUpstream := fakeUpstream(t, &hitsDrain)

	groupID := h.createGroup("团队 A")

	_, healthyBody := h.admin(http.MethodPost, "/admin/accounts", map[string]any{
		"name":        "healthy",
		"groupId":     groupID,
		"baseUrl":     healthyUpstream.URL + "/v1",
		"siteUrl":     healthySite.URL,
		"userId":      "1",
		"accessToken": "tok",
		"keys":        "sk-h1",
	})
	healthyID := healthyBody["data"].(map[string]any)["id"].(string)

	_, drainBody := h.admin(http.MethodPost, "/admin/accounts", map[string]any{
		"name":        "drained",
		"groupId":     groupID,
		"baseUrl":     drainUpstream.URL + "/v1",
		"siteUrl":     drainSite.URL,
		"userId":      "2",
		"accessToken": "tok",
		"keys":        "sk-d1",
	})
	drainedID := drainBody["data"].(map[string]any)["id"].(string)

	_, keyBody := h.admin(http.MethodPost, "/admin/keys", map[string]any{"name": "client", "accountId": drainedID})
	secret := keyBody["data"].(map[string]any)["key"].(string)

	// 余额清零后刷新，应自动暂停。
	drainQuota = 0
	drainUsed = 500000
	response, refreshed := h.admin(http.MethodPost, "/admin/accounts/"+drainedID+"/refresh", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("刷新余额失败: %d %#v", response.StatusCode, refreshed)
	}
	result := refreshed["data"].(map[string]any)
	if result["exhausted"] != true || result["suspended"] != true {
		t.Fatalf("余额耗尽应自动暂停: %#v", result)
	}
	if result["deleted"] == true {
		t.Fatalf("余额耗尽不应再删除账号: %#v", result)
	}

	_, list := h.admin(http.MethodGet, "/admin/accounts", nil)
	accounts := list["data"].([]any)
	if len(accounts) != 2 {
		t.Fatalf("暂停不应删除账号: %#v", accounts)
	}
	if len(list["removed"].([]any)) != 0 {
		t.Fatalf("暂停不应写入移除记录: %#v", list["removed"])
	}
	drained := findAccount(accounts, drainedID)
	if drained == nil || drained["suspended"] != true || drained["usable"] != false {
		t.Fatalf("耗尽账号应处于暂停且不可用状态: %#v", drained)
	}
	if drained["apiCount"].(float64) != 1 {
		t.Fatalf("暂停应保留账号名下的上游 API: %#v", drained)
	}
	reason := drained["suspendReason"].(string)
	if !strings.Contains(reason, "余额触及下限") || !strings.Contains(reason, "下限 0.50") {
		t.Fatalf("应记录暂停原因与生效下限: %q", reason)
	}
	if healthy := findAccount(accounts, healthyID); healthy == nil || healthy["suspended"] != false {
		t.Fatalf("健康账号不应被暂停: %#v", healthy)
	}

	// 调用方无需改动，请求自动切到健康账号。
	if response, chat := h.do(http.MethodPost, "/v1/chat/completions", chatBody(), secret); response.StatusCode != http.StatusOK {
		t.Fatalf("应自动切换到可用账号: %d %#v", response.StatusCode, chat)
	}
	if len(hitsHealthy) != 1 || hitsHealthy[0] != "sk-h1" {
		t.Fatalf("应命中健康账号的 API: %#v", hitsHealthy)
	}
	if len(hitsDrain) != 0 {
		t.Fatalf("已暂停账号不应再被调用: %#v", hitsDrain)
	}

	_, totalsBody := h.admin(http.MethodGet, "/admin/accounts/totals", nil)
	balance := totalsBody["data"].(map[string]any)["balance"].(map[string]any)
	if balance["total"].(float64) != 10 {
		t.Fatalf("总余额应等于健康账号余额（暂停账号余额为 0）: %#v", balance)
	}
	if balance["removedUsed"].(float64) != 0 || balance["lifetime"].(float64) != 1 {
		t.Fatalf("暂停账号的消耗应留在在册口径而非移除口径: %#v", balance)
	}
	if suspended := totalsBody["data"].(map[string]any)["accounts"].(map[string]any)["suspended"].(float64); suspended != 1 {
		t.Fatalf("汇总应统计已暂停账号数: %#v", suspended)
	}

	// 「查询总余额」先刷新再汇总，并按来源拆分余额，同时报告失败账号数。
	response, queryBody := h.admin(http.MethodPost, "/admin/accounts/balance-query", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("总余额查询应成功: %d %#v", response.StatusCode, queryBody)
	}
	report := queryBody["data"].(map[string]any)
	if report["queried"].(float64) != 2 {
		t.Fatalf("应刷新两个账号: %#v", report)
	}
	if report["failed"].(float64) != 0 {
		t.Fatalf("上游可用时不应有失败账号: %#v", report)
	}
	queried := report["totals"].(map[string]any)["balance"].(map[string]any)
	if queried["queriedBalance"].(float64) != queried["total"].(float64) {
		t.Fatalf("两个账号都配了额度查询，总额应全部来自上游查询: %#v", queried)
	}
	if queried["manualAmount"].(float64) != 0 {
		t.Fatalf("没有手动余额账号时手动额度应为 0: %#v", queried)
	}
	if groupsView, ok := report["groups"].([]any); !ok || len(groupsView) != 1 {
		t.Fatalf("总余额查询应附带分组汇总: %#v", report["groups"])
	}

	// 管理员重新启用后账号立刻回到分配池。
	drainQuota = 5000000
	drainUsed = 0
	if response, enabled := h.admin(http.MethodPost, "/admin/accounts/"+drainedID+"/enable", map[string]any{"enabled": true}); response.StatusCode != http.StatusOK {
		t.Fatalf("启用账号失败: %d %#v", response.StatusCode, enabled)
	} else if account := enabled["data"].(map[string]any); account["suspended"] != false || account["usable"] != true {
		t.Fatalf("启用后账号应恢复可用: %#v", account)
	}
}

func TestNoUsableAccountReturnsServiceUnavailable(t *testing.T) {
	h := newHarness(t)
	quota, used := 500000.0, 0.0
	site := fakeSite(t, &quota, &used)
	hits := []string{}
	upstream := fakeUpstream(t, &hits)

	groupID := h.createGroup("团队 A")
	response, body := h.admin(http.MethodPost, "/admin/accounts", map[string]any{
		"name":        "only",
		"groupId":     groupID,
		"baseUrl":     upstream.URL + "/v1",
		"siteUrl":     site.URL,
		"userId":      "1",
		"accessToken": "tok",
		"keys":        "sk-only",
		"autoSuspend": false,
	})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("创建账号失败: %d %#v", response.StatusCode, body)
	}
	accountID := body["data"].(map[string]any)["id"].(string)

	_, keyBody := h.admin(http.MethodPost, "/admin/keys", map[string]any{"name": "client"})
	secret := keyBody["data"].(map[string]any)["key"].(string)

	quota = 0
	used = 500000
	_, refreshed := h.admin(http.MethodPost, "/admin/accounts/"+accountID+"/refresh", nil)
	if refreshed["data"].(map[string]any)["suspended"] == true {
		t.Fatalf("关闭自动暂停后不应暂停账号")
	}

	response, chat := h.do(http.MethodPost, "/v1/chat/completions", chatBody(), secret)
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("没有可用账号应返回 503, got %d %#v", response.StatusCode, chat)
	}
	if len(hits) != 0 {
		t.Fatalf("余额耗尽的账号不应被调用: %#v", hits)
	}
}

func TestModelsEndpointFollowsOpenAISchema(t *testing.T) {
	h := newHarness(t)
	quota, used := 5000000.0, 0.0
	site := fakeSite(t, &quota, &used)
	hits := []string{}
	upstream := fakeUpstream(t, &hits)

	groupID := h.createGroup("团队 A")
	if response, body := h.admin(http.MethodPost, "/admin/accounts", map[string]any{
		"name":           "acct",
		"groupId":        groupID,
		"baseUrl":        upstream.URL + "/v1",
		"siteUrl":        site.URL,
		"userId":         "1",
		"accessToken":    "tok",
		"keys":           "sk-a1",
		"selectedModels": []string{"gpt-4o-mini", "gpt-4o", "gpt-4*"},
	}); response.StatusCode != http.StatusCreated {
		t.Fatalf("创建账号失败: %d %#v", response.StatusCode, body)
	}

	_, keyBody := h.admin(http.MethodPost, "/admin/keys", map[string]any{"name": "client"})
	secret := keyBody["data"].(map[string]any)["key"].(string)

	response, body := h.do(http.MethodGet, "/v1/models", nil, secret)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("模型列表请求失败: %d %#v", response.StatusCode, body)
	}
	if contentType := response.Header.Get("Content-Type"); !strings.Contains(contentType, "application/json") {
		t.Fatalf("Content-Type 应为 JSON, got %s", contentType)
	}
	if body["object"] != "list" {
		t.Fatalf("顶层 object 必须是 list: %#v", body["object"])
	}
	if len(body) != 2 {
		t.Fatalf("响应只应包含 object 与 data: %#v", body)
	}

	entries := body["data"].([]any)
	if len(entries) != 2 {
		t.Fatalf("通配符不应出现在模型列表: %#v", entries)
	}

	// 严格校验字段集合、类型与排序。
	wantIDs := []string{"gpt-4o", "gpt-4o-mini"}
	for index, item := range entries {
		entry := item.(map[string]any)
		if len(entry) != 4 {
			t.Fatalf("模型对象只应含 4 个规范字段: %#v", entry)
		}
		if entry["id"] != wantIDs[index] {
			t.Fatalf("第 %d 项 id 应为 %s, got %#v", index, wantIDs[index], entry["id"])
		}
		if entry["object"] != "model" {
			t.Fatalf("object 必须是 model: %#v", entry["object"])
		}
		if entry["owned_by"] != "laskah" {
			t.Fatalf("owned_by 错误: %#v", entry["owned_by"])
		}
		created, ok := entry["created"].(float64)
		if !ok || created <= 0 {
			t.Fatalf("created 必须是正整数时间戳: %#v", entry["created"])
		}
	}

	// 单模型查询返回裸模型对象。
	response, single := h.do(http.MethodGet, "/v1/models/gpt-4o-mini", nil, secret)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("单模型查询失败: %d %#v", response.StatusCode, single)
	}
	if single["id"] != "gpt-4o-mini" || single["object"] != "model" || len(single) != 4 {
		t.Fatalf("单模型响应格式错误: %#v", single)
	}

	if response, _ := h.do(http.MethodGet, "/v1/models/not-exists", nil, secret); response.StatusCode != http.StatusNotFound {
		t.Fatalf("未知模型应返回 404, got %d", response.StatusCode)
	}
	if response, _ := h.do(http.MethodGet, "/v1/models", nil, "sk-nope"); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("非法密钥应返回 401, got %d", response.StatusCode)
	}

	// 完全不带密钥（浏览器直接打开）返回公开模型目录：模型名不是机密，
	// 而「必须先建密钥才能看有什么模型」会明显拖慢接入。仍不泄露上游站点与账号数量。
	response, anonymous := h.do(http.MethodGet, "/v1/models", nil, "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("匿名访问应返回 200, got %d", response.StatusCode)
	}
	anonymousEntries := anonymous["data"].([]any)
	if len(anonymousEntries) != 2 {
		t.Fatalf("匿名访问应列出全站模型目录: %#v", anonymousEntries)
	}
	for _, item := range anonymousEntries {
		entry := item.(map[string]any)
		if len(entry) != 4 || entry["object"] != "model" || entry["owned_by"] != "laskah" {
			t.Fatalf("匿名条目应保持 OpenAI 规范且不暴露上游: %#v", entry)
		}
	}
	if !strings.Contains(anonymous["hint"].(string), "Authorization") {
		t.Fatalf("匿名响应应提示如何携带密钥: %#v", anonymous["hint"])
	}

	// 匿名单模型查询同样可用，未知模型仍是 404。
	if response, singleAnon := h.do(http.MethodGet, "/v1/models/gpt-4o-mini", nil, ""); response.StatusCode != http.StatusOK {
		t.Fatalf("匿名单模型查询应成功: %d %#v", response.StatusCode, singleAnon)
	} else if singleAnon["id"] != "gpt-4o-mini" || len(singleAnon) != 4 {
		t.Fatalf("匿名单模型响应格式错误: %#v", singleAnon)
	}
	if response, _ := h.do(http.MethodGet, "/v1/models/not-exists", nil, ""); response.StatusCode != http.StatusNotFound {
		t.Fatalf("匿名查询未知模型应 404, got %d", response.StatusCode)
	}

	// 密钥白名单应收窄模型列表。
	_, scopedKey := h.admin(http.MethodPost, "/admin/keys", map[string]any{"name": "scoped", "allowedModels": "gpt-4o"})
	scopedSecret := scopedKey["data"].(map[string]any)["key"].(string)
	_, scoped := h.do(http.MethodGet, "/v1/models", nil, scopedSecret)
	scopedEntries := scoped["data"].([]any)
	if len(scopedEntries) != 1 || scopedEntries[0].(map[string]any)["id"] != "gpt-4o" {
		t.Fatalf("密钥白名单未生效: %#v", scopedEntries)
	}
}

// TestPublicModelsCanBeDisabled 验证 PUBLIC_MODELS=false 时匿名目录退回空列表。
//
// 默认开放模型目录是为了方便接入，但需要严格保密供货范围的部署必须能关掉，
// 且关掉后连单模型查询也不能变成「存在性探测」通道。

// TestResponsesEndpointCompatibility 验证 /v1/responses 与 chat 走同一套账号分配与本地计量。
//
// 重点是三件事：路由带不带 /v1 前缀都通、上游自报的夸张 usage 被本地口径覆盖、
// 用量确实记到了账号与密钥的统计里。
func TestResponsesEndpointCompatibility(t *testing.T) {
	h := newHarness(t)
	quota, used := 5000000.0, 0.0
	site := fakeSite(t, &quota, &used)
	hits := []string{}
	upstream := fakeUpstream(t, &hits)

	groupID := h.createGroup("团队 A")
	if response, body := h.admin(http.MethodPost, "/admin/accounts", map[string]any{
		"name":           "acct",
		"groupId":        groupID,
		"baseUrl":        upstream.URL + "/v1",
		"siteUrl":        site.URL,
		"userId":         "1",
		"accessToken":    "tok",
		"keys":           "sk-r1",
		"selectedModels": []string{"gpt-4o-mini"},
	}); response.StatusCode != http.StatusCreated {
		t.Fatalf("创建账号失败: %d %#v", response.StatusCode, body)
	}

	_, keyBody := h.admin(http.MethodPost, "/admin/keys", map[string]any{"name": "client"})
	secret := keyBody["data"].(map[string]any)["key"].(string)
	keyID := keyBody["data"].(map[string]any)["id"].(string)

	requestBody := map[string]any{"model": "gpt-4o-mini", "input": "hi"}
	response, body := h.do(http.MethodPost, "/v1/responses", requestBody, secret)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("responses 请求失败: %d %#v", response.StatusCode, body)
	}
	if body["object"] != "response" {
		t.Fatalf("响应应保持 Responses 结构: %#v", body)
	}
	if len(hits) != 1 || hits[0] != "sk-r1" {
		t.Fatalf("应命中账号名下的上游 Key: %#v", hits)
	}

	// 上游自报 9999/8888，本站必须改写成自算口径。
	usage := body["usage"].(map[string]any)
	expectedPrompt := float64(tokenizer.CountPrompt(requestBody))
	expectedOutput := float64(tokenizer.CountText("hi"))
	if usage["prompt_tokens"].(float64) != expectedPrompt || usage["input_tokens"].(float64) != expectedPrompt {
		t.Fatalf("输入 token 应为本地估算 %v: %#v", expectedPrompt, usage)
	}
	if usage["completion_tokens"].(float64) != expectedOutput || usage["output_tokens"].(float64) != expectedOutput {
		t.Fatalf("输出 token 应为本地估算 %v: %#v", expectedOutput, usage)
	}
	if usage["total_tokens"].(float64) != expectedPrompt+expectedOutput {
		t.Fatalf("总 token 应为本地口径之和: %#v", usage)
	}

	// 不带 /v1 前缀的同名路径同样可用，方便只认 /responses 的客户端。
	if response, bare := h.do(http.MethodPost, "/responses", requestBody, secret); response.StatusCode != http.StatusOK {
		t.Fatalf("/responses 应与 /v1/responses 等价: %d %#v", response.StatusCode, bare)
	}

	// 缺少 input 时应是明确的 400，而不是把空请求转给上游。
	if response, bad := h.do(http.MethodPost, "/v1/responses", map[string]any{"model": "gpt-4o-mini"}, secret); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("缺少 input 应返回 400: %d %#v", response.StatusCode, bad)
	}

	// 用量要落到账号与密钥统计上，并且账号侧记的是本地口径。
	_, keys := h.admin(http.MethodGet, "/admin/keys", nil)
	for _, item := range keys["data"].([]any) {
		entry := item.(map[string]any)
		if entry["id"] != keyID {
			continue
		}
		stats := entry["stats"].(map[string]any)
		if stats["requests"].(float64) != 2 {
			t.Fatalf("两次成功请求应都计入密钥统计: %#v", stats)
		}
		if stats["totalTokens"].(float64) != 2*(expectedPrompt+expectedOutput) {
			t.Fatalf("密钥 token 统计应为本地口径: %#v", stats)
		}
	}

	_, totals := h.admin(http.MethodGet, "/admin/accounts/totals", nil)
	tokens := totals["data"].(map[string]any)["tokens"].(map[string]any)
	if tokens["selfMetered"] != true {
		t.Fatalf("汇总应标记为本站自算: %#v", tokens)
	}
	if tokens["upstream"].(float64) != 2*18887 {
		t.Fatalf("上游自报值应原样留存供对照: %#v", tokens)
	}
	if tokens["accounts"].(float64) != 2*(expectedPrompt+expectedOutput) {
		t.Fatalf("账号侧 token 应为本地口径: %#v", tokens)
	}
}

// TestResponsesStreamingCountsLocalTokens 验证 Responses 流式转发与本地输出 token 累计。
//
// 流式场景没有可信的 usage 可读，输出 token 只能靠逐片累计 response.output_text.delta，
// 这条路走不通的话手动计费会长期少算钱。
func TestResponsesStreamingCountsLocalTokens(t *testing.T) {
	h := newHarness(t)
	quota, used := 5000000.0, 0.0
	site := fakeSite(t, &quota, &used)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models", "/models":
			_, _ = fmt.Fprint(w, `{"data":[{"id":"gpt-4o-mini"}]}`)
			return
		case "/v1/responses", "/responses":
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"你好\"}\n\n")
		_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"世界\"}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":7777,\"output_tokens\":6666}}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(upstream.Close)

	groupID := h.createGroup("团队 A")
	if response, body := h.admin(http.MethodPost, "/admin/accounts", map[string]any{
		"name":           "acct",
		"groupId":        groupID,
		"baseUrl":        upstream.URL + "/v1",
		"siteUrl":        site.URL,
		"userId":         "1",
		"accessToken":    "tok",
		"keys":           "sk-s1",
		"selectedModels": []string{"gpt-4o-mini"},
	}); response.StatusCode != http.StatusCreated {
		t.Fatalf("创建账号失败: %d %#v", response.StatusCode, body)
	}

	_, keyBody := h.admin(http.MethodPost, "/admin/keys", map[string]any{"name": "client"})
	secret := keyBody["data"].(map[string]any)["key"].(string)

	payload := map[string]any{"model": "gpt-4o-mini", "input": "hi", "stream": true}
	response, raw := h.doRaw(http.MethodPost, "/v1/responses", payload, secret)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("流式 responses 请求失败: %d %s", response.StatusCode, raw)
	}
	if contentType := response.Header.Get("Content-Type"); !strings.Contains(contentType, "text/event-stream") {
		t.Fatalf("流式响应应为 SSE: %s", contentType)
	}
	if !strings.Contains(raw, "你好") || !strings.Contains(raw, "世界") || !strings.Contains(raw, "[DONE]") {
		t.Fatalf("流式内容未完整转发: %s", raw)
	}

	// 输出 token 来自逐片累计的 delta，而不是上游自报的 6666。
	_, totals := h.admin(http.MethodGet, "/admin/accounts/totals", nil)
	tokens := totals["data"].(map[string]any)["tokens"].(map[string]any)
	expectedOutput := float64(tokenizer.CountText("你好世界"))
	if tokens["completion"].(float64) != expectedOutput {
		t.Fatalf("流式输出 token 应为本地累计 %v: %#v", expectedOutput, tokens)
	}
	if tokens["prompt"].(float64) != float64(tokenizer.CountPrompt(map[string]any{"model": "gpt-4o-mini", "input": "hi", "stream": true})) {
		t.Fatalf("流式输入 token 应为本地估算: %#v", tokens)
	}
}

func TestPublicModelsCanBeDisabled(t *testing.T) {
	disabled := false
	h := newHarnessWith(t, func(options *Options) {
		options.PublicModels = &disabled
	})
	quota, used := 5000000.0, 0.0
	site := fakeSite(t, &quota, &used)
	hits := []string{}
	upstream := fakeUpstream(t, &hits)

	groupID := h.createGroup("团队 A")
	if response, body := h.admin(http.MethodPost, "/admin/accounts", map[string]any{
		"name":           "acct",
		"groupId":        groupID,
		"baseUrl":        upstream.URL + "/v1",
		"siteUrl":        site.URL,
		"userId":         "1",
		"accessToken":    "tok",
		"keys":           "sk-a1",
		"selectedModels": []string{"gpt-4o-mini"},
	}); response.StatusCode != http.StatusCreated {
		t.Fatalf("创建账号失败: %d %#v", response.StatusCode, body)
	}

	response, anonymous := h.do(http.MethodGet, "/v1/models", nil, "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("关闭公开目录后仍应返回 200: %d", response.StatusCode)
	}
	if len(anonymous["data"].([]any)) != 0 {
		t.Fatalf("关闭公开目录后不应列出模型: %#v", anonymous["data"])
	}
	if response, _ := h.do(http.MethodGet, "/v1/models/gpt-4o-mini", nil, ""); response.StatusCode != http.StatusNotFound {
		t.Fatalf("关闭公开目录后单模型查询应 404, got %d", response.StatusCode)
	}

	// 带上有效密钥仍然照常列出。
	_, keyBody := h.admin(http.MethodPost, "/admin/keys", map[string]any{"name": "client"})
	secret := keyBody["data"].(map[string]any)["key"].(string)
	_, scoped := h.do(http.MethodGet, "/v1/models", nil, secret)
	if len(scoped["data"].([]any)) != 1 {
		t.Fatalf("持有密钥时应正常列出模型: %#v", scoped["data"])
	}
}

func TestRequestTimeRefreshSwitchesAccountWhenBalanceRunsOut(t *testing.T) {
	h := newHarness(t)
	drainQuota, drainUsed := 5000000.0, 0.0
	healthyQuota, healthyUsed := 5000000.0, 0.0
	drainSite := fakeSite(t, &drainQuota, &drainUsed)
	healthySite := fakeSite(t, &healthyQuota, &healthyUsed)
	hitsDrain := []string{}
	hitsHealthy := []string{}
	drainUpstream := fakeUpstream(t, &hitsDrain)
	healthyUpstream := fakeUpstream(t, &hitsHealthy)

	groupID := h.createGroup("团队 A")
	_, drainBody := h.admin(http.MethodPost, "/admin/accounts", map[string]any{
		"name":              "drained",
		"groupId":           groupID,
		"baseUrl":           drainUpstream.URL + "/v1",
		"siteUrl":           drainSite.URL,
		"userId":            "1",
		"accessToken":       "tok",
		"keys":              "sk-d1",
		"requestRefreshSec": 1,
	})
	drainedID := drainBody["data"].(map[string]any)["id"].(string)
	if drainBody["data"].(map[string]any)["refreshOnRequest"] != true {
		t.Fatalf("请求时刷新应默认开启: %#v", drainBody["data"])
	}

	_, healthyBody := h.admin(http.MethodPost, "/admin/accounts", map[string]any{
		"name":              "healthy",
		"groupId":           groupID,
		"baseUrl":           healthyUpstream.URL + "/v1",
		"siteUrl":           healthySite.URL,
		"userId":            "2",
		"accessToken":       "tok",
		"keys":              "sk-h1",
		"requestRefreshSec": 1,
	})
	healthyID := healthyBody["data"].(map[string]any)["id"].(string)

	// 把密钥钉在即将耗尽的账号上。
	_, keyBody := h.admin(http.MethodPost, "/admin/keys", map[string]any{"name": "client", "accountId": drainedID})
	secret := keyBody["data"].(map[string]any)["key"].(string)

	if response, chat := h.do(http.MethodPost, "/v1/chat/completions", chatBody(), secret); response.StatusCode != http.StatusOK {
		t.Fatalf("首次调用应成功: %d %#v", response.StatusCode, chat)
	}
	if len(hitsDrain) != 1 {
		t.Fatalf("首次调用应命中原账号: %#v", hitsDrain)
	}

	// 站点侧余额清零，但没有任何人手动刷新。
	drainQuota = 0
	drainUsed = 500000
	time.Sleep(1100 * time.Millisecond)

	response, chat := h.do(http.MethodPost, "/v1/chat/completions", chatBody(), secret)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("请求时刷新后应自动切换账号: %d %#v", response.StatusCode, chat)
	}
	if len(hitsDrain) != 1 {
		t.Fatalf("余额耗尽的账号不应再被调用: %#v", hitsDrain)
	}
	if len(hitsHealthy) != 1 || hitsHealthy[0] != "sk-h1" {
		t.Fatalf("应切换到健康账号: %#v", hitsHealthy)
	}

	// 耗尽账号已被自动暂停，密钥重新绑定到健康账号。
	_, list := h.admin(http.MethodGet, "/admin/accounts", nil)
	accounts := list["data"].([]any)
	if len(accounts) != 2 {
		t.Fatalf("暂停不应删除账号: %#v", accounts)
	}
	if drained := findAccount(accounts, drainedID); drained == nil || drained["suspended"] != true {
		t.Fatalf("耗尽账号应被自动暂停: %#v", drained)
	}
	_, dashboard := h.admin(http.MethodGet, "/admin/dashboard", nil)
	keys := dashboard["keys"].([]any)
	if keys[0].(map[string]any)["accountId"] != healthyID {
		t.Fatalf("密钥应重新绑定到健康账号: %#v", keys[0])
	}
}

func TestGroupManualRefresh(t *testing.T) {
	h := newHarness(t)
	quota, used := 2500000.0, 500000.0
	site := fakeSite(t, &quota, &used)
	hits := []string{}
	upstream := fakeUpstream(t, &hits)

	groupID := h.createGroup("团队 A")
	otherGroup := h.createGroup("团队 B")
	for index, group := range []string{groupID, otherGroup} {
		if response, body := h.admin(http.MethodPost, "/admin/accounts", map[string]any{
			"name":        "acct-" + itoa(index),
			"groupId":     group,
			"baseUrl":     upstream.URL + "/v1",
			"siteUrl":     site.URL,
			"userId":      "1",
			"accessToken": "tok",
			"keys":        "sk-" + itoa(index),
		}); response.StatusCode != http.StatusCreated {
			t.Fatalf("创建账号失败: %d %#v", response.StatusCode, body)
		}
	}

	// 站点余额翻倍，手动刷新后分组汇总应立即更新。
	quota = 5000000
	response, refreshed := h.admin(http.MethodPost, "/admin/groups/"+groupID+"/refresh", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("分组手动刷新失败: %d %#v", response.StatusCode, refreshed)
	}
	results := refreshed["data"].([]any)
	if len(results) != 1 {
		t.Fatalf("只应刷新本分组的账号: %#v", results)
	}
	if refreshed["group"].(map[string]any)["balance"].(float64) != 10 {
		t.Fatalf("分组余额应刷新为 10: %#v", refreshed["group"])
	}

	// 另一个分组未被刷新，余额仍是创建时的旧值。
	_, dashboard := h.admin(http.MethodGet, "/admin/dashboard", nil)
	for _, item := range dashboard["data"].(map[string]any)["groups"].([]any) {
		group := item.(map[string]any)
		if group["id"] == otherGroup && group["balance"].(float64) != 5 {
			t.Fatalf("未刷新分组余额不应变化: %#v", group)
		}
	}

	if response, _ := h.admin(http.MethodPost, "/admin/groups/not-exists/refresh", nil); response.StatusCode != http.StatusNotFound {
		t.Fatalf("未知分组应返回 404, got %d", response.StatusCode)
	}
	if response, _ := h.admin(http.MethodGet, "/admin/groups/"+groupID+"/refresh", nil); response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET 刷新应返回 405, got %d", response.StatusCode)
	}
}

func itoa(value int) string {
	return strconv.Itoa(value)
}

func TestGatewayRejectsInvalidKey(t *testing.T) {
	h := newHarness(t)

	if response, _ := h.do(http.MethodPost, "/v1/chat/completions", chatBody(), "sk-nope"); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("非法密钥应返回 401, got %d", response.StatusCode)
	}
	if response, _ := h.do(http.MethodPost, "/v1/chat/completions", chatBody(), ""); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("缺少密钥应返回 401, got %d", response.StatusCode)
	}
}

// TestSetupFlowCreatesSuperAdmin 覆盖“部署后先创建超级管理员”的完整流程。
//
// 未初始化时所有页面都应引导到 /setup；创建完成后 /setup 不再可用，
// 并且不能重复创建第二个超级管理员。
func TestSetupFlowCreatesSuperAdmin(t *testing.T) {
	h := newBareHarness(t)

	for _, path := range []string{"/", "/login", "/dashboard", "/manage"} {
		response := h.get(path)
		response.Body.Close()
		if response.StatusCode != http.StatusFound || response.Header.Get("Location") != "/setup" {
			t.Fatalf("%s 未初始化应 302 到 /setup, got %d %s", path, response.StatusCode, response.Header.Get("Location"))
		}
	}

	page := h.get("/setup")
	page.Body.Close()
	if page.StatusCode != http.StatusOK {
		t.Fatalf("/setup 应可访问, got %d", page.StatusCode)
	}

	_, probe := h.do(http.MethodGet, "/admin/setup", nil, "")
	if probe["needsSetup"] != true {
		t.Fatalf("应报告需要初始化: %#v", probe)
	}

	// 未初始化时不允许登录。
	if response, _ := h.login(testSuperUser, testSuperPassword); response.StatusCode != http.StatusConflict {
		t.Fatalf("未初始化时登录应 409, got %d", response.StatusCode)
	}

	// 校验：口令不一致、口令过短都要拒绝。
	if response, _ := h.do(http.MethodPost, "/admin/setup", map[string]any{
		"user": "root-admin", "password": "password-1", "confirm": "password-2",
	}, ""); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("口令不一致应 400, got %d", response.StatusCode)
	}
	if response, _ := h.do(http.MethodPost, "/admin/setup", map[string]any{
		"user": "root-admin", "password": "short",
	}, ""); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("过短口令应 400, got %d", response.StatusCode)
	}

	response, created := h.do(http.MethodPost, "/admin/setup", map[string]any{
		"user": "root-admin", "password": "root-password", "confirm": "root-password",
	}, "")
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("创建超级管理员失败: %d %#v", response.StatusCode, created)
	}
	view := created["data"].(map[string]any)
	if view["isSuper"] != true || view["username"] != "root-admin" {
		t.Fatalf("返回视图错误: %#v", view)
	}

	// 不能重复初始化。
	if response, _ := h.do(http.MethodPost, "/admin/setup", map[string]any{
		"user": "second-root", "password": "root-password",
	}, ""); response.StatusCode != http.StatusConflict {
		t.Fatalf("重复初始化应 409, got %d", response.StatusCode)
	}

	// 初始化后 /setup 页面关闭，登录可用。
	closed := h.get("/setup")
	closed.Body.Close()
	if closed.StatusCode != http.StatusFound || closed.Header.Get("Location") != "/login" {
		t.Fatalf("初始化后 /setup 应跳登录, got %d %s", closed.StatusCode, closed.Header.Get("Location"))
	}
	if response, body := h.login("root-admin", "root-password"); response.StatusCode != http.StatusOK || body["isSuper"] != true {
		t.Fatalf("初始化后应可登录: %d %#v", response.StatusCode, body)
	}

	// 账户名不能以明文落盘。
	raw, err := os.ReadFile(h.app.Store.File())
	if err != nil {
		t.Fatalf("读取数据文件失败: %v", err)
	}
	if strings.Contains(string(raw), "root-admin") {
		t.Fatalf("超级管理员账户名不应明文落盘")
	}
}

// TestAdminRoleCannotReachManage 验证普通管理员只能看数据看板。
//
// 页面通过 302 拦截（改地址栏无效），接口一律 403，
// 且看板响应里不包含网关密钥列表。
func TestAdminRoleCannotReachManage(t *testing.T) {
	h := newHarness(t)
	if _, err := h.app.Store.CreateAdminUser("viewer", "viewer-password", store.RoleAdmin, "只看看板"); err != nil {
		t.Fatalf("创建管理员失败: %v", err)
	}

	response, body := h.login("viewer", "viewer-password")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("管理员应可登录: %d %#v", response.StatusCode, body)
	}
	if body["isSuper"] != false || body["home"] != "/dashboard" {
		t.Fatalf("管理员角色信息错误: %#v", body)
	}

	// 直接输网址访问 /manage 也会被弹回看板。
	for _, path := range []string{"/manage", "/manage/"} {
		page := h.get(path)
		page.Body.Close()
		if page.StatusCode != http.StatusFound || page.Header.Get("Location") != "/dashboard" {
			t.Fatalf("%s 对管理员应 302 到 /dashboard, got %d %s", path, page.StatusCode, page.Header.Get("Location"))
		}
	}
	dashboardPage := h.get("/dashboard")
	dashboardPage.Body.Close()
	if dashboardPage.StatusCode != http.StatusOK {
		t.Fatalf("管理员应能打开看板, got %d", dashboardPage.StatusCode)
	}

	// 超管接口一律 403。
	for _, item := range []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodGet, "/admin/groups", nil},
		{http.MethodPost, "/admin/groups", map[string]any{"name": "x"}},
		{http.MethodGet, "/admin/accounts", nil},
		{http.MethodGet, "/admin/keys", nil},
		{http.MethodGet, "/admin/users", nil},
		{http.MethodGet, "/admin/settings", nil},
	} {
		if response, payload := h.do(item.method, item.path, item.body, ""); response.StatusCode != http.StatusForbidden {
			t.Fatalf("%s %s 对管理员应 403, got %d %#v", item.method, item.path, response.StatusCode, payload)
		}
	}

	// 看板可读，但不下发密钥列表。
	response, dashboard := h.do(http.MethodGet, "/admin/dashboard", nil, "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("管理员应可读看板: %d", response.StatusCode)
	}
	if dashboard["isSuper"] != false {
		t.Fatalf("看板应标记非超管: %#v", dashboard["isSuper"])
	}
	if keys, ok := dashboard["keys"].([]any); ok && len(keys) != 0 {
		t.Fatalf("管理员不应看到网关密钥: %#v", keys)
	}
}

// TestAdminUserManagement 覆盖超管对管理员账户的增删启停与改密。
func TestAdminUserManagement(t *testing.T) {
	h := newHarness(t)
	if response, _ := h.login(testSuperUser, testSuperPassword); response.StatusCode != http.StatusOK {
		t.Fatalf("超管登录失败")
	}

	response, created := h.do(http.MethodPost, "/admin/users", map[string]any{
		"user": "viewer", "password": "viewer-password", "role": "admin", "note": "看板",
	}, "")
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("创建管理员失败: %d %#v", response.StatusCode, created)
	}
	viewer := created["data"].(map[string]any)
	viewerID := viewer["id"].(string)
	if viewer["isSuper"] != false || viewer["enabled"] != true {
		t.Fatalf("新建账户视图错误: %#v", viewer)
	}

	// 重复账户名与过短口令都要拒绝。
	if response, _ := h.do(http.MethodPost, "/admin/users", map[string]any{"user": "viewer", "password": "viewer-password"}, ""); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("重复账户名应 400, got %d", response.StatusCode)
	}
	if response, _ := h.do(http.MethodPost, "/admin/users", map[string]any{"user": "tiny", "password": "short"}, ""); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("过短口令应 400, got %d", response.StatusCode)
	}

	_, list := h.do(http.MethodGet, "/admin/users", nil, "")
	if users := list["data"].([]any); len(users) != 2 {
		t.Fatalf("应有两个账户: %#v", users)
	}

	// 禁用后无法登录，重新启用后恢复。
	if response, _ := h.do(http.MethodPost, "/admin/users/"+viewerID+"/enable", map[string]any{"enabled": false}, ""); response.StatusCode != http.StatusOK {
		t.Fatalf("禁用账户失败: %d", response.StatusCode)
	}
	superCSRF := h.csrf
	if response, _ := h.login("viewer", "viewer-password"); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("禁用账户不应能登录, got %d", response.StatusCode)
	}
	h.csrf = superCSRF

	if response, _ := h.do(http.MethodPost, "/admin/users/"+viewerID+"/enable", map[string]any{"enabled": true}, ""); response.StatusCode != http.StatusOK {
		t.Fatalf("启用账户失败: %d", response.StatusCode)
	}
	if response, _ := h.do(http.MethodPost, "/admin/users/"+viewerID+"/password", map[string]any{
		"password": "another-password", "confirm": "another-password",
	}, ""); response.StatusCode != http.StatusOK {
		t.Fatalf("重置口令失败: %d", response.StatusCode)
	}
	if response, _ := h.login("viewer", "another-password"); response.StatusCode != http.StatusOK {
		t.Fatalf("重置后的口令应生效, got %d", response.StatusCode)
	}

	// 重新以超管登录后删除账户；最后一个超管不可删。
	if response, _ := h.login(testSuperUser, testSuperPassword); response.StatusCode != http.StatusOK {
		t.Fatalf("超管重新登录失败")
	}
	_, refreshed := h.do(http.MethodGet, "/admin/users", nil, "")
	superID := ""
	for _, item := range refreshed["data"].([]any) {
		user := item.(map[string]any)
		if user["isSuper"] == true {
			superID = user["id"].(string)
		}
	}
	if response, _ := h.do(http.MethodDelete, "/admin/users/"+superID, nil, ""); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("不应允许删除最后一个超管, got %d", response.StatusCode)
	}
	if response, _ := h.do(http.MethodDelete, "/admin/users/"+viewerID, nil, ""); response.StatusCode != http.StatusOK {
		t.Fatalf("删除账户失败: %d", response.StatusCode)
	}
	if response, _ := h.login("viewer", "another-password"); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("已删除账户不应能登录, got %d", response.StatusCode)
	}
}

// TestGroupEnableDisableStopsTraffic 验证禁用分组后网关不再使用其账号。
func TestGroupEnableDisableStopsTraffic(t *testing.T) {
	h := newHarness(t)
	hits := []string{}
	upstream := fakeUpstream(t, &hits)

	groupID := h.createGroup("团队 A")
	if _, body := h.admin(http.MethodPost, "/admin/accounts", map[string]any{
		"name":    "acct",
		"groupId": groupID,
		"baseUrl": upstream.URL + "/v1",
		"keys":    "sk-a1",
	}); body["data"] == nil {
		t.Fatalf("创建账号失败: %#v", body)
	}

	_, keyBody := h.admin(http.MethodPost, "/admin/keys", map[string]any{"name": "client"})
	secret := keyBody["data"].(map[string]any)["key"].(string)

	if response, chat := h.do(http.MethodPost, "/v1/chat/completions", chatBody(), secret); response.StatusCode != http.StatusOK {
		t.Fatalf("启用状态下调用应成功: %d %#v", response.StatusCode, chat)
	}

	response, disabled := h.admin(http.MethodPost, "/admin/groups/"+groupID+"/enable", map[string]any{"enabled": false})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("禁用分组失败: %d %#v", response.StatusCode, disabled)
	}
	if disabled["data"].(map[string]any)["enabled"] != false {
		t.Fatalf("响应应反映禁用状态: %#v", disabled["data"])
	}

	if response, _ := h.do(http.MethodPost, "/v1/chat/completions", chatBody(), secret); response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("禁用分组后应无可用账号, got %d", response.StatusCode)
	}
	if len(hits) != 1 {
		t.Fatalf("禁用后不应再打到上游: %#v", hits)
	}

	if response, _ := h.admin(http.MethodPost, "/admin/groups/"+groupID+"/enable", map[string]any{"enabled": true}); response.StatusCode != http.StatusOK {
		t.Fatalf("重新启用失败: %d", response.StatusCode)
	}
	if response, _ := h.do(http.MethodPost, "/v1/chat/completions", chatBody(), secret); response.StatusCode != http.StatusOK {
		t.Fatalf("重新启用后应恢复, got %d", response.StatusCode)
	}
	if len(hits) != 2 {
		t.Fatalf("恢复后应重新打到上游: %#v", hits)
	}
}

// TestUpstreamInsufficientBalanceSuspendsAccount 验证上游报余额不足时立即暂停账号并换账号重试。
func TestUpstreamInsufficientBalanceSuspendsAccount(t *testing.T) {
	h := newHarness(t)

	brokeHits := 0
	broke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		brokeHits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error":{"message":"当前分组上游负载已饱和，该令牌额度不足，请充值后重试（余额不足）","type":"new_api_error"}}`)
	}))
	t.Cleanup(broke.Close)

	healthyHits := []string{}
	healthy := fakeUpstream(t, &healthyHits)

	groupID := h.createGroup("团队 A")
	_, brokeBody := h.admin(http.MethodPost, "/admin/accounts", map[string]any{
		"name":    "broke",
		"groupId": groupID,
		"baseUrl": broke.URL + "/v1",
		"keys":    "sk-broke",
	})
	brokeID := brokeBody["data"].(map[string]any)["id"].(string)

	// 先只挂一个欠费账号，把密钥钉在它上面。
	_, keyBody := h.admin(http.MethodPost, "/admin/keys", map[string]any{"name": "client", "accountId": brokeID})
	secret := keyBody["data"].(map[string]any)["key"].(string)

	_, _ = h.admin(http.MethodPost, "/admin/accounts", map[string]any{
		"name":    "healthy",
		"groupId": groupID,
		"baseUrl": healthy.URL + "/v1",
		"keys":    "sk-h1",
	})

	response, chat := h.do(http.MethodPost, "/v1/chat/completions", chatBody(), secret)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("应自动换号后成功: %d %#v", response.StatusCode, chat)
	}
	if brokeHits == 0 {
		t.Fatalf("欠费账号应被尝试过一次")
	}
	if len(healthyHits) != 1 || healthyHits[0] != "sk-h1" {
		t.Fatalf("应切换到健康账号: %#v", healthyHits)
	}

	// 欠费账号已被暂停，原因保留上游文案。
	_, list := h.admin(http.MethodGet, "/admin/accounts", nil)
	accounts := list["data"].([]any)
	if len(accounts) != 2 {
		t.Fatalf("暂停不应删除账号: %#v", accounts)
	}
	brokeView := findAccount(accounts, brokeID)
	if brokeView == nil || brokeView["suspended"] != true {
		t.Fatalf("欠费账号应被自动暂停: %#v", accounts)
	}
	if !strings.Contains(brokeView["suspendReason"].(string), "余额不足") {
		t.Fatalf("应记录暂停原因: %#v", brokeView["suspendReason"])
	}
}

// TestPrechargeShortfallSuspendsAccount 验证 New API 的“预扣费额度失败”也会立即暂停换号。
//
// 这类文案只给出两个金额（剩余 < 需要），不含“不足”字样，
// 是实际部署里最常见的余额耗尽表现，必须与显式“余额不足”同等处理。
func TestPrechargeShortfallSuspendsAccount(t *testing.T) {
	h := newHarness(t)

	brokeHits := 0
	broke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		brokeHits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error":{"message":"预扣费额度失败, 用户剩余额度: ＄0.182898, 需要预扣费额度: ＄0.290486","type":"new_api_error"}}`)
	}))
	t.Cleanup(broke.Close)

	healthyHits := []string{}
	healthy := fakeUpstream(t, &healthyHits)

	groupID := h.createGroup("预扣费分组")
	_, brokeBody := h.admin(http.MethodPost, "/admin/accounts", map[string]any{
		"name":    "precharge-broke",
		"groupId": groupID,
		"baseUrl": broke.URL + "/v1",
		"keys":    "sk-precharge",
	})
	brokeID := brokeBody["data"].(map[string]any)["id"].(string)

	_, keyBody := h.admin(http.MethodPost, "/admin/keys", map[string]any{"name": "client", "accountId": brokeID})
	secret := keyBody["data"].(map[string]any)["key"].(string)

	_, _ = h.admin(http.MethodPost, "/admin/accounts", map[string]any{
		"name":    "precharge-healthy",
		"groupId": groupID,
		"baseUrl": healthy.URL + "/v1",
		"keys":    "sk-ph1",
	})

	response, chat := h.do(http.MethodPost, "/v1/chat/completions", chatBody(), secret)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("预扣费失败后应自动换号成功: %d %#v", response.StatusCode, chat)
	}
	if brokeHits == 0 {
		t.Fatalf("欠费账号应被尝试过一次")
	}
	if len(healthyHits) != 1 || healthyHits[0] != "sk-ph1" {
		t.Fatalf("应切换到健康账号: %#v", healthyHits)
	}

	_, list := h.admin(http.MethodGet, "/admin/accounts", nil)
	accounts := list["data"].([]any)
	brokeView := findAccount(accounts, brokeID)
	if brokeView == nil || brokeView["suspended"] != true {
		t.Fatalf("预扣费失败的账号应被自动暂停: %#v", accounts)
	}
	if reason := brokeView["suspendReason"].(string); !strings.Contains(reason, "预扣费额度失败") {
		t.Fatalf("暂停原因应保留上游文案: %q", reason)
	}
}

// TestUnlimitedAccountWithoutBalanceQuery 验证未配置额度查询的账号按无限余额处理。
func TestUnlimitedAccountWithoutBalanceQuery(t *testing.T) {
	h := newHarness(t)
	hits := []string{}
	upstream := fakeUpstream(t, &hits)

	groupID := h.createGroup("团队 A")
	_, body := h.admin(http.MethodPost, "/admin/accounts", map[string]any{
		"name":    "no-query",
		"groupId": groupID,
		"baseUrl": upstream.URL + "/v1",
		"keys":    "sk-a1",
	})
	account := body["data"].(map[string]any)
	if account["unlimited"] != true || account["hasBalanceQuery"] != false {
		t.Fatalf("应标记为无限余额: %#v", account)
	}
	if account["checkError"] != "" {
		t.Fatalf("未配置额度查询不应产生查询错误: %#v", account["checkError"])
	}

	_, dashboard := h.admin(http.MethodGet, "/admin/dashboard", nil)
	totals := dashboard["data"].(map[string]any)
	if int(totals["accounts"].(map[string]any)["unlimited"].(float64)) != 1 {
		t.Fatalf("看板应统计无限额度账号: %#v", totals["accounts"])
	}

	// 无限额度账号不会因为余额是 0 而被清理。
	if response, _ := h.admin(http.MethodPost, "/admin/accounts/refresh-all", nil); response.StatusCode != http.StatusOK {
		t.Fatalf("刷新失败: %d", response.StatusCode)
	}
	_, list := h.admin(http.MethodGet, "/admin/accounts", nil)
	if len(list["data"].([]any)) != 1 {
		t.Fatalf("无限额度账号不应被自动暂停或删除: %#v", list["data"])
	}

	_, keyBody := h.admin(http.MethodPost, "/admin/keys", map[string]any{"name": "client"})
	secret := keyBody["data"].(map[string]any)["key"].(string)
	if response, _ := h.do(http.MethodPost, "/v1/chat/completions", chatBody(), secret); response.StatusCode != http.StatusOK {
		t.Fatalf("无限额度账号应能承接流量, got %d", response.StatusCode)
	}
}

// TestModelAwareAccountSelection 验证“请求特定模型时只会落到提供该模型的账号”。
func TestModelAwareAccountSelection(t *testing.T) {
	h := newHarness(t)
	quota, used := 5000000.0, 0.0
	site := fakeSite(t, &quota, &used)
	gptHits := []string{}
	claudeHits := []string{}
	gptUpstream := fakeUpstream(t, &gptHits)
	claudeUpstream := fakeUpstream(t, &claudeHits)

	groupID := h.createGroup("团队 A")

	_, gptBody := h.admin(http.MethodPost, "/admin/accounts", map[string]any{
		"name":           "gpt-only",
		"groupId":        groupID,
		"baseUrl":        gptUpstream.URL + "/v1",
		"siteUrl":        site.URL,
		"userId":         "1",
		"accessToken":    "tok",
		"keys":           "sk-gpt1",
		"selectedModels": []string{"gpt-4o-mini"},
	})
	gptAccountID := gptBody["data"].(map[string]any)["id"].(string)

	if response, claudeBody := h.admin(http.MethodPost, "/admin/accounts", map[string]any{
		"name":           "claude-only",
		"groupId":        groupID,
		"baseUrl":        claudeUpstream.URL + "/v1",
		"siteUrl":        site.URL,
		"userId":         "2",
		"accessToken":    "tok",
		"keys":           "sk-claude1",
		"selectedModels": []string{"claude-3-opus"},
	}); response.StatusCode != http.StatusCreated {
		t.Fatalf("创建 claude 账号失败: %d %#v", response.StatusCode, claudeBody)
	}

	_, keyBody := h.admin(http.MethodPost, "/admin/keys", map[string]any{"name": "client", "groupId": groupID})
	secret := keyBody["data"].(map[string]any)["key"].(string)

	// 模型列表是分组内所有账号的并集，与“按模型自动换号”的行为保持一致。
	_, listed := h.do(http.MethodGet, "/v1/models", nil, secret)
	entries := listed["data"].([]any)
	if len(entries) != 2 || entries[0].(map[string]any)["id"] != "claude-3-opus" || entries[1].(map[string]any)["id"] != "gpt-4o-mini" {
		t.Fatalf("模型列表应为两个账号的并集: %#v", entries)
	}

	// 连续多次请求 claude，全部应命中 claude 账号，不会因为轮转打到 gpt 账号。
	for index := 0; index < 4; index++ {
		payload := map[string]any{
			"model":    "claude-3-opus",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}
		if response, chat := h.do(http.MethodPost, "/v1/chat/completions", payload, secret); response.StatusCode != http.StatusOK {
			t.Fatalf("第 %d 次 claude 调用失败: %d %#v", index+1, response.StatusCode, chat)
		}
	}
	if len(claudeHits) != 4 {
		t.Fatalf("claude 请求应全部命中 claude 账号: %#v", claudeHits)
	}
	if len(gptHits) != 0 {
		t.Fatalf("claude 请求不应命中 gpt 账号: %#v", gptHits)
	}

	// 反向验证：gpt 请求只命中 gpt 账号。
	if response, chat := h.do(http.MethodPost, "/v1/chat/completions", chatBody(), secret); response.StatusCode != http.StatusOK {
		t.Fatalf("gpt 调用失败: %d %#v", response.StatusCode, chat)
	}
	if len(gptHits) != 1 || gptHits[0] != "sk-gpt1" {
		t.Fatalf("gpt 请求应命中 gpt 账号: %#v", gptHits)
	}
	if len(claudeHits) != 4 {
		t.Fatalf("gpt 请求不应命中 claude 账号: %#v", claudeHits)
	}

	// 无人提供的模型应直接返回 503，并说明是模型维度没有账号。
	response, missing := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "gemini-1.5-pro",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, secret)
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("无账号提供该模型应返回 503, got %d %#v", response.StatusCode, missing)
	}
	detail := fmt.Sprintf("%v", missing["error"])
	if !strings.Contains(detail, "gemini-1.5-pro") {
		t.Fatalf("503 文案应点明模型名: %#v", missing)
	}

	// 绑定到 gpt 账号的密钥请求 claude 时临时换号，但常驻绑定不被改写。
	_, boundKey := h.admin(http.MethodPost, "/admin/keys", map[string]any{"name": "bound", "accountId": gptAccountID})
	boundSecret := boundKey["data"].(map[string]any)["key"].(string)
	boundID := boundKey["data"].(map[string]any)["id"].(string)

	claudePayload := map[string]any{
		"model":    "claude-3-opus",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	if response, chat := h.do(http.MethodPost, "/v1/chat/completions", claudePayload, boundSecret); response.StatusCode != http.StatusOK {
		t.Fatalf("绑定密钥请求 claude 应临时换号: %d %#v", response.StatusCode, chat)
	}
	if len(claudeHits) != 5 {
		t.Fatalf("绑定密钥的 claude 请求应命中 claude 账号: %#v", claudeHits)
	}

	_, dashboard := h.admin(http.MethodGet, "/admin/dashboard", nil)
	for _, item := range dashboard["keys"].([]any) {
		entry := item.(map[string]any)
		if entry["id"] != boundID {
			continue
		}
		if entry["accountId"] != gptAccountID {
			t.Fatalf("临时换号不应改写密钥的常驻绑定: %#v", entry["accountId"])
		}
	}
}

// streamUpstream 模拟流式上游：mode 决定这次连接怎么表现。
//
// "error-first"  连一个字节正文都不给就报余额不足（可透明换号）
// "error-middle" 先吐两段正文再报余额不足（只能截断收尾）
// "ok"           正常流完
func streamUpstream(t *testing.T, mode *string, hits *[]string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models", "/models":
			_, _ = fmt.Fprint(w, `{"data":[{"id":"gpt-4o-mini"}]}`)
			return
		case "/v1/chat/completions", "/chat/completions":
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}

		*hits = append(*hits, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		shortfall := `data: {"error":{"message":"预扣费额度失败, 用户剩余额度: ＄0.182898, 需要预扣费额度: ＄0.290486"}}` + "\n\n"
		chunk := func(text string) string {
			return `data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"` + text + `"},"finish_reason":null}]}` + "\n\n"
		}

		switch *mode {
		case "error-first":
			_, _ = io.WriteString(w, shortfall)
		case "error-middle":
			_, _ = io.WriteString(w, chunk("前半段"))
			if flusher != nil {
				flusher.Flush()
			}
			_, _ = io.WriteString(w, shortfall)
		default:
			_, _ = io.WriteString(w, chunk("完整"))
			_, _ = io.WriteString(w, `data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`+"\n\n")
			_, _ = io.WriteString(w, "data: [DONE]"+"\n\n")
		}
		if flusher != nil {
			flusher.Flush()
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// TestBalanceFloorDropsAccountBeforeUpstreamFails 验证 0.5 USD 安全线：
// 余额掉到安全线以下时刷新即暂停，流量立刻转到余额充足的账号，
// 调用方不会先吃一次上游的「预扣费失败」。
func TestBalanceFloorSuspendsAccountBeforeUpstreamFails(t *testing.T) {
	h := newHarness(t)
	thinQuota, thinUsed := 5000000.0, 0.0
	richQuota, richUsed := 5000000.0, 0.0
	thinSite := fakeSite(t, &thinQuota, &thinUsed)
	richSite := fakeSite(t, &richQuota, &richUsed)
	thinHits := []string{}
	richHits := []string{}
	thinUpstream := fakeUpstream(t, &thinHits)
	richUpstream := fakeUpstream(t, &richHits)

	groupID := h.createGroup("团队 A")

	_, thinBody := h.admin(http.MethodPost, "/admin/accounts", map[string]any{
		"name":        "thin",
		"groupId":     groupID,
		"baseUrl":     thinUpstream.URL + "/v1",
		"siteUrl":     thinSite.URL,
		"userId":      "1",
		"accessToken": "tok",
		"keys":        "sk-thin",
	})
	thinAccount := thinBody["data"].(map[string]any)
	thinID := thinAccount["id"].(string)
	if thinAccount["balanceFloor"].(float64) != 0.5 {
		t.Fatalf("默认余额下限应为 0.5 USD: %#v", thinAccount["balanceFloor"])
	}

	_, richBody := h.admin(http.MethodPost, "/admin/accounts", map[string]any{
		"name":        "rich",
		"groupId":     groupID,
		"baseUrl":     richUpstream.URL + "/v1",
		"siteUrl":     richSite.URL,
		"userId":      "2",
		"accessToken": "tok",
		"keys":        "sk-rich",
	})
	richID := richBody["data"].(map[string]any)["id"].(string)

	_, keyBody := h.admin(http.MethodPost, "/admin/keys", map[string]any{"name": "client", "accountId": thinID})
	secret := keyBody["data"].(map[string]any)["key"].(string)

	// 余额还剩 $0.182898：没到 0，但连一次请求的预扣费都未必够。
	thinQuota = 91449
	thinUsed = 500000
	_, refreshed := h.admin(http.MethodPost, "/admin/accounts/"+thinID+"/refresh", nil)
	result := refreshed["data"].(map[string]any)
	if result["balance"].(float64) >= 0.5 {
		t.Fatalf("测试前置条件错误，余额应低于安全线: %#v", result["balance"])
	}
	if result["exhausted"] != true || result["suspended"] != true {
		t.Fatalf("余额低于 0.5 USD 应判定耗尽并暂停: %#v", result)
	}

	_, list := h.admin(http.MethodGet, "/admin/accounts", nil)
	accounts := list["data"].([]any)
	if thin := findAccount(accounts, thinID); thin == nil || thin["suspended"] != true {
		t.Fatalf("余额不足的账号应被暂停: %#v", accounts)
	}
	if rich := findAccount(accounts, richID); rich == nil || rich["suspended"] != false {
		t.Fatalf("余额充足的账号应保持可用: %#v", accounts)
	}

	if response, chat := h.do(http.MethodPost, "/v1/chat/completions", chatBody(), secret); response.StatusCode != http.StatusOK {
		t.Fatalf("应无缝切到余额充足的账号: %d %#v", response.StatusCode, chat)
	}
	if len(richHits) != 1 || len(thinHits) != 0 {
		t.Fatalf("流量应只落在余额充足的账号: rich=%#v thin=%#v", richHits, thinHits)
	}
}

// TestStreamBalanceShortfallSwitchesAccountTransparently 覆盖流式响应里
// 「一个字节都还没下发就发现余额不足」的场景：暂停后换账号重来，调用方看到的是完整流。
func TestStreamBalanceShortfallSwitchesAccountTransparently(t *testing.T) {
	h := newHarness(t)
	quotaA, usedA := 5000000.0, 0.0
	quotaB, usedB := 5000000.0, 0.0
	siteA := fakeSite(t, &quotaA, &usedA)
	siteB := fakeSite(t, &quotaB, &usedB)

	badMode := "error-first"
	goodMode := "ok"
	badHits := []string{}
	goodHits := []string{}
	badUpstream := streamUpstream(t, &badMode, &badHits)
	goodUpstream := streamUpstream(t, &goodMode, &goodHits)

	groupID := h.createGroup("团队 A")

	_, badBody := h.admin(http.MethodPost, "/admin/accounts", map[string]any{
		"name":        "broke",
		"groupId":     groupID,
		"baseUrl":     badUpstream.URL + "/v1",
		"siteUrl":     siteA.URL,
		"userId":      "1",
		"accessToken": "tok",
		"keys":        "sk-broke",
	})
	brokeID := badBody["data"].(map[string]any)["id"].(string)

	if response, body := h.admin(http.MethodPost, "/admin/accounts", map[string]any{
		"name":        "good",
		"groupId":     groupID,
		"baseUrl":     goodUpstream.URL + "/v1",
		"siteUrl":     siteB.URL,
		"userId":      "2",
		"accessToken": "tok",
		"keys":        "sk-good",
	}); response.StatusCode != http.StatusCreated {
		t.Fatalf("创建健康账号失败: %d %#v", response.StatusCode, body)
	}

	_, keyBody := h.admin(http.MethodPost, "/admin/keys", map[string]any{"name": "client", "accountId": brokeID})
	secret := keyBody["data"].(map[string]any)["key"].(string)

	payload := chatBody()
	payload["stream"] = true
	response, raw := h.doRaw(http.MethodPost, "/v1/chat/completions", payload, secret)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("换号后应返回 200: %d %s", response.StatusCode, raw)
	}
	if !strings.Contains(raw, "完整") || !strings.Contains(raw, "data: [DONE]") {
		t.Fatalf("调用方应收到健康账号的完整流: %s", raw)
	}
	if strings.Contains(raw, "预扣费") {
		t.Fatalf("余额不足事件不应透传给调用方: %s", raw)
	}
	if len(goodHits) != 1 {
		t.Fatalf("应重试到健康账号: %#v", goodHits)
	}

	// 欠费账号已被暂停。
	_, list := h.admin(http.MethodGet, "/admin/accounts", nil)
	broke := findAccount(list["data"].([]any), brokeID)
	if broke == nil || broke["suspended"] != true {
		t.Fatalf("欠费账号应被自动暂停: %#v", list["data"])
	}
	if !strings.Contains(broke["suspendReason"].(string), "余额不足") {
		t.Fatalf("应记录余额不足暂停原因: %#v", broke["suspendReason"])
	}
}

// TestStreamBalanceShortfallMidStreamTruncates 覆盖「已经下发部分内容后才发现余额不足」：
// 已发出的内容撤不回来，此时必须立刻截断并正常收尾（finish_reason=length + [DONE]），
// 同时暂停账号并换号，下一次请求就落到健康账号上。
func TestStreamBalanceShortfallMidStreamTruncates(t *testing.T) {
	h := newHarness(t)
	quotaA, usedA := 5000000.0, 0.0
	quotaB, usedB := 5000000.0, 0.0
	siteA := fakeSite(t, &quotaA, &usedA)
	siteB := fakeSite(t, &quotaB, &usedB)

	badMode := "error-middle"
	goodMode := "ok"
	badHits := []string{}
	goodHits := []string{}
	badUpstream := streamUpstream(t, &badMode, &badHits)
	goodUpstream := streamUpstream(t, &goodMode, &goodHits)

	groupID := h.createGroup("团队 A")

	_, badBody := h.admin(http.MethodPost, "/admin/accounts", map[string]any{
		"name":        "broke",
		"groupId":     groupID,
		"baseUrl":     badUpstream.URL + "/v1",
		"siteUrl":     siteA.URL,
		"userId":      "1",
		"accessToken": "tok",
		"keys":        "sk-broke",
	})
	brokeID := badBody["data"].(map[string]any)["id"].(string)

	h.admin(http.MethodPost, "/admin/accounts", map[string]any{
		"name":        "good",
		"groupId":     groupID,
		"baseUrl":     goodUpstream.URL + "/v1",
		"siteUrl":     siteB.URL,
		"userId":      "2",
		"accessToken": "tok",
		"keys":        "sk-good",
	})

	_, keyBody := h.admin(http.MethodPost, "/admin/keys", map[string]any{"name": "client", "accountId": brokeID})
	secret := keyBody["data"].(map[string]any)["key"].(string)

	payload := chatBody()
	payload["stream"] = true
	response, raw := h.doRaw(http.MethodPost, "/v1/chat/completions", payload, secret)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("已开始下发的流应保持 200: %d %s", response.StatusCode, raw)
	}
	if !strings.Contains(raw, "前半段") {
		t.Fatalf("已下发的内容应保留: %s", raw)
	}
	if strings.Contains(raw, "预扣费") {
		t.Fatalf("余额不足事件不应透传给调用方: %s", raw)
	}
	if !strings.Contains(raw, `"finish_reason":"length"`) {
		t.Fatalf("截断应带 finish_reason=length: %s", raw)
	}
	if !strings.HasSuffix(strings.TrimSpace(raw), "data: [DONE]") {
		t.Fatalf("截断后应以 [DONE] 收尾，避免客户端干等: %q", raw)
	}

	// 账号已被暂停，下一次请求直接落到健康账号。
	_, list := h.admin(http.MethodGet, "/admin/accounts", nil)
	if broke := findAccount(list["data"].([]any), brokeID); broke == nil || broke["suspended"] != true {
		t.Fatalf("欠费账号应被自动暂停: %#v", list["data"])
	}

	if response, retry := h.doRaw(http.MethodPost, "/v1/chat/completions", payload, secret); response.StatusCode != http.StatusOK || !strings.Contains(retry, "完整") {
		t.Fatalf("下一次请求应命中健康账号: %d %s", response.StatusCode, retry)
	}
	if len(goodHits) != 1 {
		t.Fatalf("健康账号应只被第二次请求命中: %#v", goodHits)
	}
}

// TestNonStreamBalanceShortfallInBodySwitchesAccount 覆盖「HTTP 200 但响应体里带 error 报余额不足」：
// 状态码粗筛拦不住这种上游，必须从 error 字段识别并换号。
func TestNonStreamBalanceShortfallInBodySwitchesAccount(t *testing.T) {
	h := newHarness(t)
	quotaA, usedA := 5000000.0, 0.0
	quotaB, usedB := 5000000.0, 0.0
	siteA := fakeSite(t, &quotaA, &usedA)
	siteB := fakeSite(t, &quotaB, &usedB)

	brokeHits := []string{}
	brokeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models", "/models":
			_, _ = fmt.Fprint(w, `{"data":[{"id":"gpt-4o-mini"}]}`)
		case "/v1/chat/completions", "/chat/completions":
			brokeHits = append(brokeHits, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
			w.Header().Set("Content-Type", "application/json")
			// 状态码 200，余额不足只写在 error 字段里。
			_, _ = fmt.Fprint(w, `{"error":{"message":"预扣费额度失败, 用户剩余额度: ＄0.182898, 需要预扣费额度: ＄0.290486"}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(brokeUpstream.Close)

	goodHits := []string{}
	goodUpstream := fakeUpstream(t, &goodHits)

	groupID := h.createGroup("团队 A")

	_, badBody := h.admin(http.MethodPost, "/admin/accounts", map[string]any{
		"name":        "broke",
		"groupId":     groupID,
		"baseUrl":     brokeUpstream.URL + "/v1",
		"siteUrl":     siteA.URL,
		"userId":      "1",
		"accessToken": "tok",
		"keys":        "sk-broke",
	})
	brokeID := badBody["data"].(map[string]any)["id"].(string)

	h.admin(http.MethodPost, "/admin/accounts", map[string]any{
		"name":        "good",
		"groupId":     groupID,
		"baseUrl":     goodUpstream.URL + "/v1",
		"siteUrl":     siteB.URL,
		"userId":      "2",
		"accessToken": "tok",
		"keys":        "sk-good",
	})

	_, keyBody := h.admin(http.MethodPost, "/admin/keys", map[string]any{"name": "client", "accountId": brokeID})
	secret := keyBody["data"].(map[string]any)["key"].(string)

	response, chat := h.do(http.MethodPost, "/v1/chat/completions", chatBody(), secret)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("应换号后返回正常结果: %d %#v", response.StatusCode, chat)
	}
	if chat["id"] != "cmpl_1" {
		t.Fatalf("应返回健康账号的响应: %#v", chat)
	}
	if len(goodHits) != 1 || goodHits[0] != "sk-good" {
		t.Fatalf("应命中健康账号: %#v", goodHits)
	}

	_, list := h.admin(http.MethodGet, "/admin/accounts", nil)
	if broke := findAccount(list["data"].([]any), brokeID); broke == nil || broke["suspended"] != true {
		t.Fatalf("欠费账号应被自动暂停: %#v", list["data"])
	}
}

// TestStreamBalanceShortfallWithoutFallbackReturns503 覆盖「流还没开始下发就发现余额不足，
// 但没有别的账号可以接手」：此时必须回一个干净的 503，而不是把空的 SSE 流丢给调用方。
func TestStreamBalanceShortfallWithoutFallbackReturns503(t *testing.T) {
	h := newHarness(t)
	quota, used := 5000000.0, 0.0
	site := fakeSite(t, &quota, &used)

	mode := "error-first"
	hits := []string{}
	upstream := streamUpstream(t, &mode, &hits)

	groupID := h.createGroup("团队 A")
	_, body := h.admin(http.MethodPost, "/admin/accounts", map[string]any{
		"name":        "only",
		"groupId":     groupID,
		"baseUrl":     upstream.URL + "/v1",
		"siteUrl":     site.URL,
		"userId":      "1",
		"accessToken": "tok",
		"keys":        "sk-only",
	})
	accountID := body["data"].(map[string]any)["id"].(string)

	_, keyBody := h.admin(http.MethodPost, "/admin/keys", map[string]any{"name": "client"})
	secret := keyBody["data"].(map[string]any)["key"].(string)

	payload := chatBody()
	payload["stream"] = true
	response, raw := h.doRaw(http.MethodPost, "/v1/chat/completions", payload, secret)
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("无账号可接手时应返回 503: %d %s", response.StatusCode, raw)
	}
	if strings.Contains(raw, "text/event-stream") || strings.Contains(raw, "data: ") {
		t.Fatalf("不应下发半截 SSE: %s", raw)
	}
	if contentType := response.Header.Get("Content-Type"); !strings.Contains(contentType, "application/json") {
		t.Fatalf("失败响应应是 JSON: %s", contentType)
	}

	// 欠费账号仍然要被暂停。
	_, list := h.admin(http.MethodGet, "/admin/accounts", nil)
	if account := findAccount(list["data"].([]any), accountID); account == nil || account["suspended"] != true {
		t.Fatalf("欠费账号应被自动暂停: %#v", list["data"])
	}
}

// TestAccountRateLimitSwitchesAccount 验证账号级频率限制：
// 达到「一分钟 N 次」上限后，网关换用其它账号，而不是给调用方返回 429。
func TestAccountRateLimitSwitchesAccount(t *testing.T) {
	h := newHarness(t)

	limitedHits := []string{}
	spareHits := []string{}
	limitedUpstream := fakeUpstream(t, &limitedHits)
	spareUpstream := fakeUpstream(t, &spareHits)

	groupID := h.createGroup("限流分组")
	response, limitedBody := h.admin(http.MethodPost, "/admin/accounts", map[string]any{
		"name":            "limited",
		"groupId":         groupID,
		"baseUrl":         limitedUpstream.URL + "/v1",
		"keys":            "sk-limited",
		"rateLimitPerMin": 2,
	})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("创建限流账号失败: %d %#v", response.StatusCode, limitedBody)
	}
	limitedAccount := limitedBody["data"].(map[string]any)
	limitedID := limitedAccount["id"].(string)
	if limitedAccount["rateLimitPerMin"].(float64) != 2 {
		t.Fatalf("应回显账号频率限制: %#v", limitedAccount["rateLimitPerMin"])
	}

	_, keyBody := h.admin(http.MethodPost, "/admin/keys", map[string]any{"name": "client", "accountId": limitedID})
	secret := keyBody["data"].(map[string]any)["key"].(string)

	// 前两次请求用完该账号的每分钟配额。
	for index := 0; index < 2; index++ {
		if response, chat := h.do(http.MethodPost, "/v1/chat/completions", chatBody(), secret); response.StatusCode != http.StatusOK {
			t.Fatalf("第 %d 次请求应成功: %d %#v", index+1, response.StatusCode, chat)
		}
	}
	if len(limitedHits) != 2 {
		t.Fatalf("前两次请求应落在限流账号: %#v", limitedHits)
	}

	// 此时没有别的账号可换，达到上限只能返回 503（而不是把请求硬塞给已超限的账号）。
	if response, _ := h.do(http.MethodPost, "/v1/chat/completions", chatBody(), secret); response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("超限且无备用账号时应返回 503, got %d", response.StatusCode)
	}
	if len(limitedHits) != 2 {
		t.Fatalf("超限后不应继续调用该账号: %#v", limitedHits)
	}

	// 加入一个不限速的账号后，超限请求自动切过去。
	h.admin(http.MethodPost, "/admin/accounts", map[string]any{
		"name":    "spare",
		"groupId": groupID,
		"baseUrl": spareUpstream.URL + "/v1",
		"keys":    "sk-spare",
	})
	if response, chat := h.do(http.MethodPost, "/v1/chat/completions", chatBody(), secret); response.StatusCode != http.StatusOK {
		t.Fatalf("超限应自动换号成功: %d %#v", response.StatusCode, chat)
	}
	if len(limitedHits) != 2 {
		t.Fatalf("超限账号不应再被调用: %#v", limitedHits)
	}
	if len(spareHits) != 1 || spareHits[0] != "sk-spare" {
		t.Fatalf("应切换到未限速账号: %#v", spareHits)
	}

	// 粘性绑定不因一次限流而改写：限流只是「这一次不要用」。
	_, dashboard := h.admin(http.MethodGet, "/admin/dashboard", nil)
	if accountID := dashboard["keys"].([]any)[0].(map[string]any)["accountId"]; accountID != limitedID {
		t.Fatalf("限流换号不应改写常驻绑定: %#v", accountID)
	}
}

// TestAccountRateLimitValidation 验证频率限制的输入校验：留空表示不限制。
func TestAccountRateLimitValidation(t *testing.T) {
	h := newHarness(t)
	hits := []string{}
	upstream := fakeUpstream(t, &hits)
	groupID := h.createGroup("校验分组")

	if response, body := h.admin(http.MethodPost, "/admin/accounts", map[string]any{
		"name":            "bad",
		"groupId":         groupID,
		"baseUrl":         upstream.URL + "/v1",
		"keys":            "sk-bad",
		"rateLimitPerMin": 0,
	}); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("频率限制为 0 应报错: %d %#v", response.StatusCode, body)
	}

	_, body := h.admin(http.MethodPost, "/admin/accounts", map[string]any{
		"name":            "blank",
		"groupId":         groupID,
		"baseUrl":         upstream.URL + "/v1",
		"keys":            "sk-blank",
		"rateLimitPerMin": "",
	})
	if account := body["data"].(map[string]any); account["rateLimitPerMin"] != nil {
		t.Fatalf("留空应表示不限制: %#v", account["rateLimitPerMin"])
	}
}

// TestManualAccountSuspendAndEnable 验证管理员手动暂停/启用账号。
//
// 暂停后账号立即退出分配池且不删除任何数据，重新启用即恢复承接流量。
func TestManualAccountSuspendAndEnable(t *testing.T) {
	h := newHarness(t)
	hits := []string{}
	upstream := fakeUpstream(t, &hits)

	groupID := h.createGroup("手动启停")
	_, body := h.admin(http.MethodPost, "/admin/accounts", map[string]any{
		"name":    "solo",
		"groupId": groupID,
		"baseUrl": upstream.URL + "/v1",
		"keys":    "sk-solo",
	})
	accountID := body["data"].(map[string]any)["id"].(string)

	_, keyBody := h.admin(http.MethodPost, "/admin/keys", map[string]any{"name": "client"})
	secret := keyBody["data"].(map[string]any)["key"].(string)

	response, suspended := h.admin(http.MethodPost, "/admin/accounts/"+accountID+"/enable", map[string]any{"enabled": false})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("暂停账号失败: %d %#v", response.StatusCode, suspended)
	}
	account := suspended["data"].(map[string]any)
	if account["suspended"] != true || account["enabled"] != false || account["usable"] != false {
		t.Fatalf("暂停后账号应不可用: %#v", account)
	}
	if account["apiCount"].(float64) != 1 {
		t.Fatalf("暂停应保留上游 API: %#v", account)
	}

	if response, _ := h.do(http.MethodPost, "/v1/chat/completions", chatBody(), secret); response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("暂停后应无可用账号: %d", response.StatusCode)
	}
	if len(hits) != 0 {
		t.Fatalf("暂停账号不应被调用: %#v", hits)
	}

	if response, enabled := h.admin(http.MethodPost, "/admin/accounts/"+accountID+"/enable", map[string]any{"enabled": true}); response.StatusCode != http.StatusOK {
		t.Fatalf("启用账号失败: %d %#v", response.StatusCode, enabled)
	} else if account := enabled["data"].(map[string]any); account["suspended"] != false || account["enabled"] != true {
		t.Fatalf("启用后应解除暂停: %#v", account)
	}

	if response, chat := h.do(http.MethodPost, "/v1/chat/completions", chatBody(), secret); response.StatusCode != http.StatusOK {
		t.Fatalf("启用后应恢复承接流量: %d %#v", response.StatusCode, chat)
	}
	if len(hits) != 1 {
		t.Fatalf("启用后账号应被调用一次: %#v", hits)
	}
}

// TestModelRoutingPrefersAccountDeclaringModel 端到端验证：
// 一个账号明确勾选了 claude，另一个账号没勾任何模型（“什么都收”），
// 请求 claude 必须落到明确声明的那个账号，而不是余额更高的兜底账号。
func TestModelRoutingPrefersAccountDeclaringModel(t *testing.T) {
	h := newHarness(t)
	declaredQuota, declaredUsed := 5000000.0, 0.0
	looseQuota, looseUsed := 500000000.0, 0.0
	declaredSite := fakeSite(t, &declaredQuota, &declaredUsed)
	looseSite := fakeSite(t, &looseQuota, &looseUsed)
	declaredHits := []string{}
	looseHits := []string{}
	declaredUpstream := fakeUpstream(t, &declaredHits)
	looseUpstream := fakeUpstream(t, &looseHits)

	groupID := h.createGroup("团队 A")

	// 明确勾选 claude-3-opus 的账号，余额较低。
	if response, body := h.admin(http.MethodPost, "/admin/accounts", map[string]any{
		"name":           "claude-declared",
		"groupId":        groupID,
		"baseUrl":        declaredUpstream.URL + "/v1",
		"siteUrl":        declaredSite.URL,
		"userId":         "1",
		"accessToken":    "tok",
		"keys":           "sk-declared",
		"selectedModels": []string{"claude-3-opus"},
	}); response.StatusCode != http.StatusCreated {
		t.Fatalf("创建声明账号失败: %d %#v", response.StatusCode, body)
	}

	// 不勾任何模型的兜底账号，余额高得多（排序上本来更占优）。
	if response, body := h.admin(http.MethodPost, "/admin/accounts", map[string]any{
		"name":        "catch-all",
		"groupId":     groupID,
		"baseUrl":     looseUpstream.URL + "/v1",
		"siteUrl":     looseSite.URL,
		"userId":      "2",
		"accessToken": "tok",
		"keys":        "sk-loose",
	}); response.StatusCode != http.StatusCreated {
		t.Fatalf("创建兜底账号失败: %d %#v", response.StatusCode, body)
	}

	_, keyBody := h.admin(http.MethodPost, "/admin/keys", map[string]any{"name": "client", "groupId": groupID})
	secret := keyBody["data"].(map[string]any)["key"].(string)

	claudePayload := map[string]any{
		"model":    "claude-3-opus",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	for index := 0; index < 3; index++ {
		if response, chat := h.do(http.MethodPost, "/v1/chat/completions", claudePayload, secret); response.StatusCode != http.StatusOK {
			t.Fatalf("第 %d 次 claude 调用失败: %d %#v", index+1, response.StatusCode, chat)
		}
	}
	if len(declaredHits) != 3 {
		t.Fatalf("claude 请求应全部落到声明该模型的账号: %#v", declaredHits)
	}
	if len(looseHits) != 0 {
		t.Fatalf("不应落到没声明模型的兜底账号: %#v", looseHits)
	}

	// 没有账号声明的模型才回退到兜底账号。
	if response, chat := h.do(http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "gemini-1.5-pro",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, secret); response.StatusCode != http.StatusOK {
		t.Fatalf("无人声明的模型应回退到兜底账号: %d %#v", response.StatusCode, chat)
	}
	if len(looseHits) != 1 || looseHits[0] != "sk-loose" {
		t.Fatalf("回退应命中兜底账号: %#v", looseHits)
	}
	if len(declaredHits) != 3 {
		t.Fatalf("回退不应打扰声明账号: %#v", declaredHits)
	}
}

// TestPasswordResetWithWhitespaceStillLogsIn 复现报障：超管在 /manage 重置口令后登不进去。
//
// 从密码管理器粘贴的口令常带尾随空格或换行；前端不做 trim，
// 若服务端写入与校验的归一化规则不一致，账户就会被锁死。
func TestPasswordResetWithWhitespaceStillLogsIn(t *testing.T) {
	h := newHarness(t)
	if response, _ := h.login(testSuperUser, testSuperPassword); response.StatusCode != http.StatusOK {
		t.Fatalf("超管登录失败")
	}

	_, created := h.do(http.MethodPost, "/admin/users", map[string]any{
		"user": "viewer", "password": "viewer-password", "role": "admin",
	}, "")
	viewerID := created["data"].(map[string]any)["id"].(string)

	// 超管重置他人口令：新口令带尾随空格。
	if response, body := h.do(http.MethodPost, "/admin/users/"+viewerID+"/password", map[string]any{
		"password": "viewer-reset-1  ", "confirm": "viewer-reset-1  ",
	}, ""); response.StatusCode != http.StatusOK {
		t.Fatalf("重置口令失败: %d %#v", response.StatusCode, body)
	}
	superCSRF := h.csrf
	for _, candidate := range []string{"viewer-reset-1", "viewer-reset-1  "} {
		if response, body := h.login("viewer", candidate); response.StatusCode != http.StatusOK {
			t.Fatalf("口令 %q 应可登录: %d %#v", candidate, response.StatusCode, body)
		}
	}
	h.csrf = superCSRF

	// 超管改自己的密码：新口令带尾随换行，之后用干净口令登录。
	if response, _ := h.login(testSuperUser, testSuperPassword); response.StatusCode != http.StatusOK {
		t.Fatalf("超管重新登录失败")
	}
	if response, body := h.do(http.MethodPost, "/admin/password", map[string]any{
		"current": testSuperPassword, "next": "super-reset-1\n",
	}, ""); response.StatusCode != http.StatusOK {
		t.Fatalf("超管改密失败: %d %#v", response.StatusCode, body)
	}
	h.csrf = ""
	if response, body := h.login(testSuperUser, "super-reset-1"); response.StatusCode != http.StatusOK {
		t.Fatalf("超管改密后应能用新口令登录: %d %#v", response.StatusCode, body)
	}
}

// TestSetupPasswordWithWhitespaceLogsIn 覆盖 /setup 创建超管时带空白的口令。
func TestSetupPasswordWithWhitespaceLogsIn(t *testing.T) {
	h := newBareHarness(t)
	if response, body := h.do(http.MethodPost, "/admin/setup", map[string]any{
		"user": " Digital Gleam ", "password": " setup-password-1 ", "confirm": " setup-password-1 ",
	}, ""); response.StatusCode != http.StatusCreated {
		t.Fatalf("初始化失败: %d %#v", response.StatusCode, body)
	}
	if response, body := h.login("Digital Gleam", "setup-password-1"); response.StatusCode != http.StatusOK {
		t.Fatalf("初始化后应可登录: %d %#v", response.StatusCode, body)
	}
}

// TestScriptValidateEndpoint 覆盖脚本校验端点：保存后账号配置不可回显，
// 因此脚本必须在提交前就能验证，而校验不能真的发请求、也不能回显凭据原文。
func TestScriptValidateEndpoint(t *testing.T) {
	h := newHarness(t)
	balanceScript := `({
  request: {
    url: "{{baseUrl}}/console/quota",
    method: "GET",
    headers: { "X-Token": "{{accessToken}}" }
  },
  extractor: function (response) {
    if (!response.ok) {
      return { isValid: false, invalidMessage: response.reason };
    }
    return {
      planName: response.plan,
      remaining: response.left,
      used: response.spent,
      unit: "USD",
      extra: "节点 " + response.node
    };
  }
})`

	response, body := h.admin(http.MethodPost, "/admin/scripts/validate", map[string]any{
		"script":      balanceScript,
		"queryUrl":    "https://console.example.com",
		"accessToken": "tok-abcdef123456",
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("校验请求失败: %d %#v", response.StatusCode, body)
	}
	data := body["data"].(map[string]any)
	if data["ok"] != true {
		t.Fatalf("合法脚本应校验通过: %#v", data)
	}
	if data["url"] != "https://console.example.com/console/quota" || data["method"] != "GET" {
		t.Fatalf("回显的请求信息错误: %#v", data)
	}
	if data["hasBody"] != false {
		t.Fatalf("GET 脚本不应有请求体: %#v", data)
	}
	headers := data["headers"].(map[string]any)
	if token, _ := headers["X-Token"].(string); !strings.Contains(token, "****") || strings.Contains(token, "abcdef") {
		t.Fatalf("凭据应遮蔽后回显: %#v", headers)
	}

	_, bad := h.admin(http.MethodPost, "/admin/scripts/validate", map[string]any{
		"script": "({ request: { url: \"{{baseUrl}}/x\", method: \"GET\" } })",
	})
	badData := bad["data"].(map[string]any)
	if badData["ok"] != false {
		t.Fatalf("缺少 extractor 应校验失败: %#v", badData)
	}
	if message, _ := badData["error"].(string); !strings.Contains(message, "extractor") {
		t.Fatalf("应说明缺少 extractor: %#v", badData)
	}

	// 危险脚本必须在校验阶段就被拒绝，而不是等到真正查询时。
	_, danger := h.admin(http.MethodPost, "/admin/scripts/validate", map[string]any{
		"script": "({ request: { url: require('http') } })",
	})
	if danger["data"].(map[string]any)["ok"] != false {
		t.Fatalf("危险脚本应被拒绝: %#v", danger)
	}
}

// TestScriptedAccountQueriesBalance 覆盖「用脚本查站点总余额」的完整链路：
// 创建时立刻查一次、余额进入看板汇总、脚本源码不回显。
func TestScriptedAccountQueriesBalance(t *testing.T) {
	h := newHarness(t)
	hits := []string{}
	upstream := fakeUpstream(t, &hits)

	var gotPath, gotToken string
	left := 6.5
	console := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotToken = r.Header.Get("X-Token")
		_, _ = fmt.Fprintf(w, `{"ok":true,"plan":"pro","left":%f,"spent":3.5,"node":"hk-1"}`, left)
	}))
	t.Cleanup(console.Close)

	balanceScript := `({
  request: {
    url: "{{baseUrl}}/console/quota",
    method: "GET",
    headers: { "X-Token": "{{accessToken}}" }
  },
  extractor: function (response) {
    if (!response.ok) {
      return { isValid: false, invalidMessage: response.reason };
    }
    return {
      planName: response.plan,
      remaining: response.left,
      used: response.spent,
      unit: "USD",
      extra: "节点 " + response.node
    };
  }
})`

	groupID := h.createGroup("脚本组")
	response, body := h.admin(http.MethodPost, "/admin/accounts", map[string]any{
		"name":        "scripted",
		"groupId":     groupID,
		"baseUrl":     upstream.URL + "/v1",
		"queryUrl":    console.URL,
		"accessToken": "tok-console",
		"queryScript": balanceScript,
		"keys":        "sk-s1\nsk-s2",
	})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("创建脚本账号失败: %d %#v", response.StatusCode, body)
	}
	account := body["data"].(map[string]any)
	if gotPath != "/console/quota" || gotToken != "tok-console" {
		t.Fatalf("脚本请求未按配置发出: %q %q", gotPath, gotToken)
	}
	if account["balance"].(float64) != 6.5 || account["usedAmount"].(float64) != 3.5 {
		t.Fatalf("脚本余额未写入账号: %#v", account)
	}
	if account["planName"] != "pro" || account["balanceExtra"] != "节点 hk-1" {
		t.Fatalf("脚本附加信息未写入: %#v", account)
	}
	if account["hasQueryScript"] != true || account["hasQueryUrl"] != true {
		t.Fatalf("应标记脚本与查询地址已配置: %#v", account)
	}
	if account["unlimited"] != false || account["hasBalanceQuery"] != true {
		t.Fatalf("配了脚本就不该按无限额度处理: %#v", account)
	}
	if account["checkError"] != "" {
		t.Fatalf("脚本查询不应报错: %#v", account["checkError"])
	}
	for field, value := range account {
		if text, ok := value.(string); ok && strings.Contains(text, "extractor") {
			t.Fatalf("字段 %s 泄露了脚本源码", field)
		}
	}

	// 站点总余额查询：脚本账号要计入「上游查询得到」这一口径。
	_, total := h.admin(http.MethodPost, "/admin/accounts/balance-query", nil)
	totals := total["data"].(map[string]any)["totals"].(map[string]any)
	accounts := totals["accounts"].(map[string]any)
	if int(accounts["scripted"].(float64)) != 1 || int(accounts["queried"].(float64)) != 1 {
		t.Fatalf("看板应统计脚本查询账号: %#v", accounts)
	}
	if int(accounts["scriptBroken"].(float64)) != 0 || int(accounts["unlimited"].(float64)) != 0 {
		t.Fatalf("脚本正常时不应计入异常或无限额度: %#v", accounts)
	}
	balance := totals["balance"].(map[string]any)
	if balance["queriedBalance"].(float64) != 6.5 || balance["total"].(float64) != 6.5 {
		t.Fatalf("脚本余额应计入站点总余额: %#v", balance)
	}

	// 余额掉到下限以下时照常自动暂停，脚本口径与内置口径行为一致。
	left = 0.2
	if response, _ := h.admin(http.MethodPost, "/admin/accounts/refresh-all", nil); response.StatusCode != http.StatusOK {
		t.Fatalf("刷新失败: %d", response.StatusCode)
	}
	_, list := h.admin(http.MethodGet, "/admin/accounts", nil)
	suspended := findAccount(list["data"].([]any), account["id"].(string))
	if suspended == nil || suspended["suspended"] != true {
		t.Fatalf("余额触及下限应自动暂停: %#v", suspended)
	}
}
