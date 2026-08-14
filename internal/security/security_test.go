package security

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPasswordHashRoundTrip(t *testing.T) {
	encoded, err := HashPassword("123456")
	if err != nil {
		t.Fatalf("散列失败: %v", err)
	}
	if strings.Contains(encoded, "123456") {
		t.Fatalf("散列不应包含明文口令")
	}
	if !VerifyPassword(encoded, "123456") {
		t.Fatalf("正确口令应校验通过")
	}
	if VerifyPassword(encoded, "1234567") || VerifyPassword(encoded, "") {
		t.Fatalf("错误口令应校验失败")
	}
	if VerifyPassword("plain-text", "123456") {
		t.Fatalf("非法散列格式应校验失败")
	}

	second, err := HashPassword("123456")
	if err != nil {
		t.Fatalf("散列失败: %v", err)
	}
	if second == encoded {
		t.Fatalf("相同口令应因随机盐得到不同散列")
	}
	if _, err := HashPassword(""); err == nil {
		t.Fatalf("空口令应报错")
	}
}

func TestCipherSealOpen(t *testing.T) {
	salt, err := NewSalt()
	if err != nil {
		t.Fatalf("生成盐失败: %v", err)
	}
	decoded, err := DecodeSalt(salt)
	if err != nil {
		t.Fatalf("解析盐失败: %v", err)
	}
	cipher, err := NewCipher("master-secret", decoded)
	if err != nil {
		t.Fatalf("创建 cipher 失败: %v", err)
	}

	sealed, err := cipher.Seal("sk-super-secret")
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	if strings.Contains(sealed, "sk-super-secret") || !IsSealed(sealed) {
		t.Fatalf("密文不应包含明文: %s", sealed)
	}
	opened, err := cipher.Open(sealed)
	if err != nil || opened != "sk-super-secret" {
		t.Fatalf("解密结果错误: %q %v", opened, err)
	}

	// 未加密的历史值应原样返回，便于平滑升级。
	if plain, err := cipher.Open("legacy-plain"); err != nil || plain != "legacy-plain" {
		t.Fatalf("兼容明文失败: %q %v", plain, err)
	}

	other, err := NewCipher("another-secret", decoded)
	if err != nil {
		t.Fatalf("创建 cipher 失败: %v", err)
	}
	if _, err := other.Open(sealed); err == nil {
		t.Fatalf("换密钥后应解密失败")
	}

	if empty, err := cipher.Seal(""); err != nil || empty != "" {
		t.Fatalf("空串应原样返回")
	}
	if _, err := NewCipher("", decoded); err == nil {
		t.Fatalf("空主密钥应报错")
	}
	if _, err := NewCipher("secret", []byte("short")); err == nil {
		t.Fatalf("过短的盐应报错")
	}
}

func TestSessionStoreLifecycle(t *testing.T) {
	store := NewSessionStore(time.Hour, time.Hour)
	token, session, err := store.Issue("usr_1", "Digital Gleam", "super")
	if err != nil {
		t.Fatalf("签发会话失败: %v", err)
	}
	if session.CSRF == "" || session.User != "Digital Gleam" || !session.IsSuper() {
		t.Fatalf("会话内容异常: %#v", session)
	}
	if found, ok := store.Lookup(token); !ok || found.User != "Digital Gleam" || found.UserID != "usr_1" {
		t.Fatalf("会话查找失败")
	}
	if _, ok := store.Lookup("not-a-token"); ok {
		t.Fatalf("伪造令牌不应通过")
	}

	store.Revoke(token)
	if _, ok := store.Lookup(token); ok {
		t.Fatalf("注销后不应仍然有效")
	}

	expired := NewSessionStore(time.Millisecond, time.Millisecond)
	staleToken, _, _ := expired.Issue("usr_1", "u", "admin")
	time.Sleep(5 * time.Millisecond)
	if _, ok := expired.Lookup(staleToken); ok {
		t.Fatalf("过期会话应失效")
	}

	multi := NewSessionStore(time.Hour, time.Hour)
	first, _, _ := multi.Issue("usr_1", "u", "admin")
	multi.Issue("usr_1", "u", "admin")
	multi.RevokeAll()
	if _, ok := multi.Lookup(first); ok || multi.Count() != 0 {
		t.Fatalf("RevokeAll 应清空全部会话")
	}

	// RevokeUser 只应踢掉目标账户的会话。
	scoped := NewSessionStore(time.Hour, time.Hour)
	mine, _, _ := scoped.Issue("usr_1", "a", "admin")
	other, _, _ := scoped.Issue("usr_2", "b", "admin")
	scoped.RevokeUser("usr_1")
	if _, ok := scoped.Lookup(mine); ok {
		t.Fatalf("目标账户会话应被注销")
	}
	if _, ok := scoped.Lookup(other); !ok {
		t.Fatalf("其他账户会话不应受影响")
	}
}

func TestSessionCookieAttributes(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest("POST", "/admin/login", nil)
	request.Header.Set("X-Forwarded-Proto", "https")
	SetSessionCookie(recorder, request, "token-value", time.Hour)

	raw := recorder.Header().Get("Set-Cookie")
	for _, expected := range []string{"HttpOnly", "Secure", "SameSite=Strict", "Path=/"} {
		if !strings.Contains(raw, expected) {
			t.Fatalf("Cookie 缺少 %s: %s", expected, raw)
		}
	}

	cleared := httptest.NewRecorder()
	ClearSessionCookie(cleared, request)
	if !strings.Contains(cleared.Header().Get("Set-Cookie"), "Max-Age=0") {
		t.Fatalf("清除 Cookie 应设置 Max-Age=0: %s", cleared.Header().Get("Set-Cookie"))
	}
}

func TestThrottleLocksAfterFailures(t *testing.T) {
	throttle := NewThrottle(3, time.Minute, 50*time.Millisecond)
	if ok, _ := throttle.Check("1.2.3.4"); !ok {
		t.Fatalf("初始应允许尝试")
	}
	throttle.Fail("1.2.3.4")
	throttle.Fail("1.2.3.4")
	if ok, _ := throttle.Check("1.2.3.4"); !ok {
		t.Fatalf("未达阈值前应允许尝试")
	}
	throttle.Fail("1.2.3.4")
	ok, wait := throttle.Check("1.2.3.4")
	if ok || wait <= 0 {
		t.Fatalf("达到阈值应锁定并给出等待时长: %v %v", ok, wait)
	}
	if allowed, _ := throttle.Check("5.6.7.8"); !allowed {
		t.Fatalf("锁定不应影响其他来源")
	}

	time.Sleep(60 * time.Millisecond)
	if allowed, _ := throttle.Check("1.2.3.4"); !allowed {
		t.Fatalf("锁定到期后应恢复")
	}

	throttle.Fail("9.9.9.9")
	throttle.Reset("9.9.9.9")
	if allowed, _ := throttle.Check("9.9.9.9"); !allowed {
		t.Fatalf("Reset 后应允许尝试")
	}
	throttle.Prune()
}

func TestClientIPRespectsProxyTrust(t *testing.T) {
	request := httptest.NewRequest("GET", "/", nil)
	request.RemoteAddr = "10.0.0.9:5555"
	request.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")

	if ip := ClientIP(request, false); ip != "10.0.0.9" {
		t.Fatalf("不信任代理时应使用 RemoteAddr, got %s", ip)
	}
	if ip := ClientIP(request, true); ip != "203.0.113.7" {
		t.Fatalf("信任代理时应使用首个 XFF, got %s", ip)
	}
}

func TestHashTokenAndConstantTimeEqual(t *testing.T) {
	digest := HashToken("token-value")
	if digest == "token-value" || len(digest) < 20 {
		t.Fatalf("摘要异常: %s", digest)
	}
	if digest != HashToken("token-value") {
		t.Fatalf("摘要应稳定")
	}
	if !ConstantTimeEqual("abc", "abc") || ConstantTimeEqual("abc", "abd") || ConstantTimeEqual("", "") {
		t.Fatalf("常量时间比较行为异常")
	}

	token, err := RandomToken(4)
	if err != nil || len(token) < 20 {
		t.Fatalf("随机令牌应有下限长度: %q %v", token, err)
	}
}
