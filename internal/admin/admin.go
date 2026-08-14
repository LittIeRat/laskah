// Package admin 提供管理面接口：登录鉴权、分组、账号、网关密钥与数据看板。
package admin

import (
	"net/http"
	"strings"
	"time"

	"laskah/internal/accounts"
	"laskah/internal/balancer"
	"laskah/internal/gateway"
	"laskah/internal/httpx"
	"laskah/internal/security"
	"laskah/internal/store"
)

// sessionTTL 是会话绝对有效期，sessionIdle 是空闲超时。
const (
	sessionTTL  = 8 * time.Hour
	sessionIdle = 90 * time.Minute
)

// Handler 聚合管理面依赖。
type Handler struct {
	Store      *store.Store
	Balancer   *balancer.Balancer
	Upstream   *gateway.Upstream
	Accounts   *accounts.Manager
	Sessions   *security.SessionStore
	Throttle   *security.Throttle
	TrustProxy bool
}

// New 创建管理面处理器。
func New(dataStore *store.Store, lb *balancer.Balancer, upstream *gateway.Upstream, manager *accounts.Manager, trustProxy bool) *Handler {
	return &Handler{
		Store:      dataStore,
		Balancer:   lb,
		Upstream:   upstream,
		Accounts:   manager,
		Sessions:   security.NewSessionStore(sessionTTL, sessionIdle),
		Throttle:   security.NewThrottle(5, 10*time.Minute, 15*time.Minute),
		TrustProxy: trustProxy,
	}
}

// Register 把管理路由注册到 mux。
//
// guard 只要求登录（看板视图对管理员开放）；super 额外要求超级管理员，
// 分组、账号、密钥、设置与账户管理全部走 super。
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/admin/setup", h.handleSetup)
	mux.HandleFunc("/admin/login", h.handleLogin)
	mux.HandleFunc("/admin/logout", h.handleLogout)
	mux.HandleFunc("/admin/session", h.handleSession)
	mux.HandleFunc("/admin/password", h.guard(h.handlePassword))
	mux.HandleFunc("/admin/dashboard", h.guard(h.handleDashboard))
	mux.HandleFunc("/admin/settings", h.super(h.handleSettings))
	mux.HandleFunc("/admin/users", h.super(h.handleUserCollection))
	mux.HandleFunc("/admin/users/", h.super(h.handleUserItem))
	mux.HandleFunc("/admin/groups", h.super(h.handleGroupCollection))
	mux.HandleFunc("/admin/groups/", h.super(h.handleGroupItem))
	mux.HandleFunc("/admin/accounts", h.super(h.handleAccountCollection))
	mux.HandleFunc("/admin/accounts/", h.super(h.handleAccountItem))
	mux.HandleFunc("/admin/models/probe", h.super(h.handleModelProbe))
	mux.HandleFunc("/admin/keys", h.super(h.handleKeyCollection))
	mux.HandleFunc("/admin/keys/", h.super(h.handleKeyItem))
}

// Authorized 判断请求是否已登录，供页面路由做门控。
func (h *Handler) Authorized(r *http.Request) bool {
	_, ok := h.authorize(r)
	return ok
}

// AuthorizedSuper 判断请求是否具备超级管理员权限。
//
// Bearer 管理令牌视为超级权限：它只在服务端配置里，本身等价于 root。
func (h *Handler) AuthorizedSuper(r *http.Request) bool {
	session, ok := h.authorize(r)
	if !ok {
		return false
	}
	return session == nil || session.IsSuper()
}

// PruneSessions 清理过期会话与失败计数，由后台定时调用。
func (h *Handler) PruneSessions() {
	h.Sessions.Prune()
	h.Throttle.Prune()
}

// authorize 校验会话 Cookie 或管理令牌。
//
// 会话用于浏览器，令牌用于脚本；两者都不接受查询参数传递，避免出现在日志或 Referer 中。
func (h *Handler) authorize(r *http.Request) (*security.Session, bool) {
	if token := security.SessionToken(r); token != "" {
		if session, ok := h.Sessions.Lookup(token); ok {
			return session, true
		}
	}
	if bearer := httpx.BearerToken(r); bearer != "" {
		if security.ConstantTimeEqual(bearer, h.Store.AdminToken()) {
			return nil, true
		}
	}
	return nil, false
}

// guard 组合鉴权与 CSRF 校验。
//
// 使用 Cookie 会话的写请求必须携带匹配的 X-CSRF-Token；
// 使用 Bearer 令牌的调用不依赖 Cookie，因此不受 CSRF 影响。
func (h *Handler) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, ok := h.authorize(r)
		if !ok {
			httpx.Error(w, http.StatusUnauthorized, "请先登录管理面", nil)
			return
		}
		if session != nil && isWrite(r.Method) {
			if !security.ConstantTimeEqual(r.Header.Get(security.CSRFHeader), session.CSRF) {
				httpx.Error(w, http.StatusForbidden, "CSRF 校验失败，请刷新页面重试", nil)
				return
			}
		}
		next(w, r)
	}
}

// super 在 guard 之上要求超级管理员权限。
//
// 权限判定只依据服务端会话中的角色，普通管理员访问一律 403，
// 因此即使直接构造请求或改地址栏也无法触达管理接口。
func (h *Handler) super(next http.HandlerFunc) http.HandlerFunc {
	return h.guard(func(w http.ResponseWriter, r *http.Request) {
		session, _ := h.authorize(r)
		if session != nil && !session.IsSuper() {
			httpx.Error(w, http.StatusForbidden, "仅超级管理员可访问该功能", nil)
			return
		}
		next(w, r)
	})
}

func isWrite(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// handleSetup 创建首个超级管理员。
//
// GET 只回报是否需要初始化（不泄露任何账户信息）；POST 仅在未初始化时可用，
// 且同样受登录限速保护，避免被用来做暴力探测。
func (h *Handler) handleSetup(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		httpx.JSON(w, http.StatusOK, map[string]any{"needsSetup": h.Store.NeedsSetup()})
	case http.MethodPost:
		source := security.ClientIP(r, h.TrustProxy)
		if allowed, wait := h.Throttle.Check(source); !allowed {
			w.Header().Set("Retry-After", itoa(int(wait.Seconds())+1))
			httpx.Error(w, http.StatusTooManyRequests, "尝试次数过多，请稍后再试", nil)
			return
		}
		if !h.Store.NeedsSetup() {
			httpx.Error(w, http.StatusConflict, "服务已初始化，无法重复创建超级管理员", nil)
			return
		}

		payload := struct {
			User     string `json:"user"`
			Password string `json:"password"`
			Confirm  string `json:"confirm"`
		}{}
		if err := httpx.DecodeJSON(r, &payload); err != nil {
			httpx.Error(w, httpx.StatusOf(err, http.StatusBadRequest), err.Error(), nil)
			return
		}
		if payload.Confirm != "" && payload.Confirm != payload.Password {
			httpx.Error(w, http.StatusBadRequest, "两次输入的密码不一致", nil)
			return
		}

		user, err := h.Store.CreateSuperAdmin(payload.User, payload.Password)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, err.Error(), nil)
			return
		}
		httpx.JSON(w, http.StatusCreated, map[string]any{
			"ok":   true,
			"data": store.PublicAdminUser(user, true),
		})
	default:
		httpx.Error(w, http.StatusMethodNotAllowed, "不支持的方法", nil)
	}
}

func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpx.Error(w, http.StatusMethodNotAllowed, "仅支持 POST", nil)
		return
	}
	if h.Store.NeedsSetup() {
		httpx.Error(w, http.StatusConflict, "服务尚未初始化，请先创建超级管理员", nil)
		return
	}

	source := security.ClientIP(r, h.TrustProxy)
	if allowed, wait := h.Throttle.Check(source); !allowed {
		seconds := int(wait.Seconds()) + 1
		w.Header().Set("Retry-After", itoa(seconds))
		httpx.Error(w, http.StatusTooManyRequests, "尝试次数过多，请稍后再试", nil)
		return
	}

	payload := struct {
		User     string `json:"user"`
		Password string `json:"password"`
	}{}
	if err := httpx.DecodeJSON(r, &payload); err != nil {
		httpx.Error(w, httpx.StatusOf(err, http.StatusBadRequest), err.Error(), nil)
		return
	}

	account, verified := h.Store.VerifyAdmin(payload.User, payload.Password)
	if !verified {
		h.Throttle.Fail(source)
		// 不区分“账户不存在”“账户被禁用”与“口令错误”，避免账户名枚举。
		httpx.Error(w, http.StatusUnauthorized, "账户或密码错误", nil)
		return
	}
	h.Throttle.Reset(source)

	token, session, err := h.Sessions.Issue(account.ID, account.Username, string(account.Role))
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, err.Error(), nil)
		return
	}
	security.SetSessionCookie(w, r, token, sessionTTL)
	httpx.JSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"user":      session.User,
		"role":      session.Role,
		"isSuper":   session.IsSuper(),
		"home":      homeFor(session),
		"csrfToken": session.CSRF,
		"expiresAt": session.ExpiresAt,
	})
}

// homeFor 返回该角色登录后的落地页。
//
// 普通管理员只有看板权限，因此固定落到 /dashboard。
func homeFor(session *security.Session) string {
	if session == nil || session.IsSuper() {
		return "/dashboard"
	}
	return "/dashboard"
}

func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpx.Error(w, http.StatusMethodNotAllowed, "仅支持 POST", nil)
		return
	}
	h.Sessions.Revoke(security.SessionToken(r))
	security.ClearSessionCookie(w, r)
	httpx.JSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) handleSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpx.Error(w, http.StatusMethodNotAllowed, "仅支持 GET", nil)
		return
	}
	session, ok := h.authorize(r)
	if !ok {
		httpx.JSON(w, http.StatusOK, map[string]any{"authenticated": false})
		return
	}
	// 令牌调用没有会话实体，按超级权限对待。
	view := map[string]any{
		"authenticated": true,
		"user":          "",
		"role":          string(store.RoleSuper),
		"isSuper":       true,
	}
	if session != nil {
		view["user"] = session.User
		view["role"] = session.Role
		view["isSuper"] = session.IsSuper()
		view["csrfToken"] = session.CSRF
		view["expiresAt"] = session.ExpiresAt
	}
	view["home"] = homeFor(session)
	httpx.JSON(w, http.StatusOK, view)
}

func (h *Handler) handlePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpx.Error(w, http.StatusMethodNotAllowed, "仅支持 POST", nil)
		return
	}
	session, _ := h.authorize(r)
	if session == nil {
		httpx.Error(w, http.StatusForbidden, "管理令牌不能用于改密，请用浏览器会话登录", nil)
		return
	}

	payload := struct {
		Current string `json:"current"`
		Next    string `json:"next"`
	}{}
	if err := httpx.DecodeJSON(r, &payload); err != nil {
		httpx.Error(w, httpx.StatusOf(err, http.StatusBadRequest), err.Error(), nil)
		return
	}
	// 改密同样先核验当前口令，避免会话被劫持后直接锁死账户。
	if _, ok := h.Store.VerifyAdmin(session.User, payload.Current); !ok {
		httpx.Error(w, http.StatusUnauthorized, "当前密码错误", nil)
		return
	}
	if err := h.Store.SetAdminPassword(session.UserID, strings.TrimSpace(payload.Next)); err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error(), nil)
		return
	}
	// 改密后只强制该账户重新登录，不影响其他管理员。
	h.Sessions.RevokeUser(session.UserID)
	security.ClearSessionCookie(w, r)
	httpx.JSON(w, http.StatusOK, map[string]any{"ok": true, "message": "密码已更新，请重新登录"})
}

func (h *Handler) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		httpx.JSON(w, http.StatusOK, map[string]any{"data": map[string]any{
			"strategy":   h.Store.Strategy(),
			"maxRetries": h.Store.MaxRetries(),
			"strategies": balancer.Strategies,
		}})
	case http.MethodPatch, http.MethodPut:
		payload := struct {
			Strategy   string `json:"strategy"`
			MaxRetries any    `json:"maxRetries"`
		}{}
		if err := httpx.DecodeJSON(r, &payload); err != nil {
			httpx.Error(w, httpx.StatusOf(err, http.StatusBadRequest), err.Error(), nil)
			return
		}
		strategy := strings.TrimSpace(payload.Strategy)
		if strategy != "" && !balancer.ValidStrategy(strategy) {
			httpx.Error(w, http.StatusBadRequest, "不支持的负载均衡策略: "+strategy, nil)
			return
		}
		retries := 0
		if text := store.MustString(payload.MaxRetries); text != "" {
			parsed, err := parsePositiveInt(text)
			if err != nil || parsed > 10 {
				httpx.Error(w, http.StatusBadRequest, "maxRetries 需要是 1-10 的整数", nil)
				return
			}
			retries = parsed
		}
		if err := h.Store.SetSettings(strategy, retries); err != nil {
			httpx.Error(w, http.StatusInternalServerError, err.Error(), nil)
			return
		}
		if strategy != "" {
			h.Balancer.SetStrategy(strategy)
		}
		httpx.JSON(w, http.StatusOK, map[string]any{"data": map[string]any{
			"strategy":   h.Store.Strategy(),
			"maxRetries": h.Store.MaxRetries(),
		}})
	default:
		httpx.Error(w, http.StatusMethodNotAllowed, "不支持的方法", nil)
	}
}

// handleDashboard 返回看板数据：分组余额、总消耗 tokens 与金额。
//
// 普通管理员只拿到汇总数据：网关密钥列表属于超管范围，
// 因此这里按角色裁剪响应，而不是依赖前端隐藏。
func (h *Handler) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpx.Error(w, http.StatusMethodNotAllowed, "仅支持 GET", nil)
		return
	}
	isSuper := h.AuthorizedSuper(r)

	totals := map[string]any{}
	keys := []any{}
	h.Store.View(func(data *store.Data) {
		totals = data.AccountTotals()
		if !isSuper {
			return
		}
		for _, key := range data.Keys {
			keys = append(keys, store.PublicKey(key, false))
		}
	})
	httpx.JSON(w, http.StatusOK, map[string]any{
		"data":     totals,
		"keys":     keys,
		"isSuper":  isSuper,
		"strategy": h.Store.Strategy(),
	})
}
