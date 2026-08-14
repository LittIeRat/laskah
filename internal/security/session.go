package security

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// SessionCookie 是会话 Cookie 名称。
const SessionCookie = "laskah_session"

// CSRFHeader 是校验 CSRF 时读取的请求头。
const CSRFHeader = "X-CSRF-Token"

// Session 是一次登录后的服务端会话。
//
// UserID 与 Role 在签发时固定：权限判定只看服务端会话，
// 不读取任何客户端可控字段，避免前端伪造角色越权。
type Session struct {
	UserID    string
	User      string
	Role      string
	CSRF      string
	CreatedAt time.Time
	ExpiresAt time.Time
	LastSeen  time.Time
}

// IsSuper 判断会话是否具备超级管理员权限。
func (s *Session) IsSuper() bool {
	return s != nil && s.Role == "super"
}

// SessionStore 在内存中保存会话，仅存令牌摘要，进程退出即全部失效。
type SessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	ttl      time.Duration
	idle     time.Duration
	maxCount int
}

// NewSessionStore 创建会话仓库。
func NewSessionStore(ttl, idle time.Duration) *SessionStore {
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}
	if idle <= 0 {
		idle = 2 * time.Hour
	}
	return &SessionStore{
		sessions: map[string]*Session{},
		ttl:      ttl,
		idle:     idle,
		maxCount: 512,
	}
}

// Issue 创建新会话并返回明文令牌与会话内容。
func (s *SessionStore) Issue(userID, user, role string) (string, *Session, error) {
	token, err := RandomToken(32)
	if err != nil {
		return "", nil, err
	}
	csrf, err := RandomToken(24)
	if err != nil {
		return "", nil, err
	}
	now := time.Now()
	session := &Session{
		UserID:    userID,
		User:      user,
		Role:      role,
		CSRF:      csrf,
		CreatedAt: now,
		ExpiresAt: now.Add(s.ttl),
		LastSeen:  now,
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	if len(s.sessions) >= s.maxCount {
		s.dropOldestLocked()
	}
	s.sessions[HashToken(token)] = session
	return token, session, nil
}

// Lookup 校验令牌并顺延空闲超时。
func (s *SessionStore) Lookup(token string) (*Session, bool) {
	if strings.TrimSpace(token) == "" {
		return nil, false
	}
	digest := HashToken(token)
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[digest]
	if !ok {
		return nil, false
	}
	if now.After(session.ExpiresAt) || now.Sub(session.LastSeen) > s.idle {
		delete(s.sessions, digest)
		return nil, false
	}
	session.LastSeen = now
	copied := *session
	return &copied, true
}

// Revoke 注销指定令牌。
func (s *SessionStore) Revoke(token string) {
	if token == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, HashToken(token))
}

// RevokeAll 注销全部会话，改口令后调用。
func (s *SessionStore) RevokeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = map[string]*Session{}
}

// RevokeUser 注销某个账户的全部会话，用于禁用或删除账户后立即断开其登录态。
func (s *SessionStore) RevokeUser(userID string) {
	if userID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for digest, session := range s.sessions {
		if session.UserID == userID {
			delete(s.sessions, digest)
		}
	}
}

// Prune 清理过期会话。
func (s *SessionStore) Prune() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(time.Now())
}

// Count 返回当前活跃会话数量。
func (s *SessionStore) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.sessions)
}

func (s *SessionStore) pruneLocked(now time.Time) {
	for digest, session := range s.sessions {
		if now.After(session.ExpiresAt) || now.Sub(session.LastSeen) > s.idle {
			delete(s.sessions, digest)
		}
	}
}

func (s *SessionStore) dropOldestLocked() {
	oldestDigest := ""
	var oldest time.Time
	for digest, session := range s.sessions {
		if oldestDigest == "" || session.LastSeen.Before(oldest) {
			oldestDigest = digest
			oldest = session.LastSeen
		}
	}
	if oldestDigest != "" {
		delete(s.sessions, oldestDigest)
	}
}

// SetSessionCookie 写出会话 Cookie，尽可能收紧属性。
func SetSessionCookie(w http.ResponseWriter, r *http.Request, token string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(ttl.Seconds()),
	})
}

// ClearSessionCookie 立即失效会话 Cookie。
func ClearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}

// SessionToken 从 Cookie 中读取会话令牌。
func SessionToken(r *http.Request) string {
	cookie, err := r.Cookie(SessionCookie)
	if err != nil {
		return ""
	}
	return cookie.Value
}

func isHTTPS(r *http.Request) bool {
	if r == nil {
		return false
	}
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// ClientIP 提取调用方 IP，用于登录限流。
//
// 只信任反向代理注入的首个 X-Forwarded-For，未部署代理时以 RemoteAddr 为准。
func ClientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
			first := strings.TrimSpace(strings.Split(forwarded, ",")[0])
			if first != "" {
				return first
			}
		}
		if realIP := strings.TrimSpace(r.Header.Get("X-Real-Ip")); realIP != "" {
			return realIP
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
