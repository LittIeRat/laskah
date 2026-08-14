package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"laskah/internal/security"
	"laskah/internal/store"
)

func TestParseKeyLines(t *testing.T) {
	cases := []struct {
		name   string
		text   string
		list   any
		expect []string
	}{
		{
			name:   "按行拆分并去掉空行",
			text:   "sk-a\n\nsk-b\r\nsk-c\n",
			expect: []string{"sk-a", "sk-b", "sk-c"},
		},
		{
			name:   "兼容逗号分号与制表符",
			text:   "sk-a, sk-b; sk-c\tsk-d",
			expect: []string{"sk-a", "sk-b", "sk-c", "sk-d"},
		},
		{
			name:   "去重并保持首次出现顺序",
			text:   "sk-b\nsk-a\nsk-b",
			expect: []string{"sk-b", "sk-a"},
		},
		{
			name:   "忽略注释行",
			text:   "# 备注\nsk-a\n// 说明\nsk-b",
			expect: []string{"sk-a", "sk-b"},
		},
		{
			name:   "剥掉粘贴带入的引号",
			text:   `"sk-a"` + "\n'sk-b',",
			expect: []string{"sk-a", "sk-b"},
		},
		{
			name:   "合并数组形式的输入",
			text:   "sk-a",
			list:   []any{"sk-b", " sk-a ", ""},
			expect: []string{"sk-a", "sk-b"},
		},
		{
			name:   "全为空白时返回空切片",
			text:   "  \n\t ",
			expect: []string{},
		},
	}

	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			got := parseKeyLines(item.text, item.list)
			if len(got) != len(item.expect) {
				t.Fatalf("数量不符: got %#v want %#v", got, item.expect)
			}
			for index := range got {
				if got[index] != item.expect[index] {
					t.Fatalf("第 %d 个不符: got %q want %q", index, got[index], item.expect[index])
				}
			}
		})
	}
}

func TestParseKeyLinesRespectsMaxCount(t *testing.T) {
	lines := make([]string, 0, 60)
	for index := 0; index < 60; index++ {
		lines = append(lines, "sk-"+itoa(index))
	}
	got := parseKeyLines(strings.Join(lines, "\n"), nil)
	if len(got) != 60 {
		t.Fatalf("解析阶段不截断，应返回 60 个: %d", len(got))
	}
}

// newGuardHandler 构造一个只依赖会话与令牌校验的最小处理器。
func newGuardHandler(t *testing.T) (*Handler, string, *security.Session) {
	t.Helper()
	return newRoleHandler(t, string(store.RoleSuper))
}

// newRoleHandler 按指定角色签发会话，用于验证权限分级。
func newRoleHandler(t *testing.T, role string) (*Handler, string, *security.Session) {
	t.Helper()
	handler := &Handler{
		Sessions: security.NewSessionStore(time.Hour, time.Hour),
		Throttle: security.NewThrottle(5, time.Minute, time.Minute),
	}
	token, session, err := handler.Sessions.Issue("usr_test", "Digital Gleam", role)
	if err != nil {
		t.Fatalf("签发会话失败: %v", err)
	}
	return handler, token, session
}

// TestSuperBlocksPlainAdmin 验证普通管理员访问超管接口一定被拒。
func TestSuperBlocksPlainAdmin(t *testing.T) {
	handler, token, session := newRoleHandler(t, string(store.RoleAdmin))
	called := 0
	guarded := handler.super(func(w http.ResponseWriter, r *http.Request) {
		called++
		w.WriteHeader(http.StatusOK)
	})

	request := httptest.NewRequest(http.MethodGet, "/admin/groups", nil)
	request.AddCookie(&http.Cookie{Name: security.SessionCookie, Value: token})
	request.Header.Set(security.CSRFHeader, session.CSRF)
	recorder := httptest.NewRecorder()
	guarded(recorder, request)
	if recorder.Code != http.StatusForbidden || called != 0 {
		t.Fatalf("普通管理员应 403, got %d", recorder.Code)
	}
	if handler.AuthorizedSuper(request) {
		t.Fatalf("普通管理员不应通过 AuthorizedSuper")
	}

	superHandler, superToken, superSession := newRoleHandler(t, string(store.RoleSuper))
	superGuarded := superHandler.super(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	superRequest := httptest.NewRequest(http.MethodGet, "/admin/groups", nil)
	superRequest.AddCookie(&http.Cookie{Name: security.SessionCookie, Value: superToken})
	superRequest.Header.Set(security.CSRFHeader, superSession.CSRF)
	recorder = httptest.NewRecorder()
	superGuarded(recorder, superRequest)
	if recorder.Code != http.StatusOK {
		t.Fatalf("超级管理员应放行, got %d", recorder.Code)
	}
	if !superHandler.AuthorizedSuper(superRequest) {
		t.Fatalf("超级管理员应通过 AuthorizedSuper")
	}
}

func TestGuardRequiresAuthentication(t *testing.T) {
	handler, _, _ := newGuardHandler(t)
	guarded := handler.guard(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	recorder := httptest.NewRecorder()
	guarded(recorder, httptest.NewRequest(http.MethodGet, "/admin/groups", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("匿名请求应 401, got %d", recorder.Code)
	}
}

func TestGuardEnforcesCSRFForCookieSessions(t *testing.T) {
	handler, token, session := newGuardHandler(t)
	called := 0
	guarded := handler.guard(func(w http.ResponseWriter, r *http.Request) {
		called++
		w.WriteHeader(http.StatusOK)
	})

	// 读请求不需要 CSRF 头。
	read := httptest.NewRequest(http.MethodGet, "/admin/groups", nil)
	read.AddCookie(&http.Cookie{Name: security.SessionCookie, Value: token})
	recorder := httptest.NewRecorder()
	guarded(recorder, read)
	if recorder.Code != http.StatusOK || called != 1 {
		t.Fatalf("带会话的 GET 应放行, got %d", recorder.Code)
	}

	// 写请求缺少 CSRF 头必须拒绝。
	missing := httptest.NewRequest(http.MethodPost, "/admin/groups", nil)
	missing.AddCookie(&http.Cookie{Name: security.SessionCookie, Value: token})
	recorder = httptest.NewRecorder()
	guarded(recorder, missing)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("缺少 CSRF 头应 403, got %d", recorder.Code)
	}

	// CSRF 头不匹配同样拒绝。
	wrong := httptest.NewRequest(http.MethodDelete, "/admin/groups/x", nil)
	wrong.AddCookie(&http.Cookie{Name: security.SessionCookie, Value: token})
	wrong.Header.Set(security.CSRFHeader, session.CSRF+"tampered")
	recorder = httptest.NewRecorder()
	guarded(recorder, wrong)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("CSRF 不匹配应 403, got %d", recorder.Code)
	}

	// 携带正确 CSRF 头放行。
	ok := httptest.NewRequest(http.MethodPost, "/admin/groups", nil)
	ok.AddCookie(&http.Cookie{Name: security.SessionCookie, Value: token})
	ok.Header.Set(security.CSRFHeader, session.CSRF)
	recorder = httptest.NewRecorder()
	guarded(recorder, ok)
	if recorder.Code != http.StatusOK || called != 2 {
		t.Fatalf("正确 CSRF 头应放行, got %d", recorder.Code)
	}
}

func TestIsWriteClassifiesMethods(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		if !isWrite(method) {
			t.Fatalf("%s 应被视为写请求", method)
		}
	}
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		if isWrite(method) {
			t.Fatalf("%s 不应被视为写请求", method)
		}
	}
}

func TestDedupeSorted(t *testing.T) {
	got := dedupeSorted([]string{" gpt-4o ", "gpt-4o", "", "gpt-4o-mini", "claude-3"})
	want := []string{"claude-3", "gpt-4o", "gpt-4o-mini"}
	if len(got) != len(want) {
		t.Fatalf("数量不符: %#v", got)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("第 %d 个不符: got %q want %q", index, got[index], want[index])
		}
	}
}

func TestParsePositiveInt(t *testing.T) {
	if value, err := parsePositiveInt(" 7 "); err != nil || value != 7 {
		t.Fatalf("应解析出 7: %d %v", value, err)
	}
	if _, err := parsePositiveInt("0"); err == nil {
		t.Fatalf("0 应被拒绝")
	}
	if _, err := parsePositiveInt("abc"); err == nil {
		t.Fatalf("非数字应被拒绝")
	}
}
