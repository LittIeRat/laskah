package server

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"io"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"time"

	"laskah/internal/accounts"
	"laskah/internal/admin"
	"laskah/internal/balancer"
	"laskah/internal/gateway"
	"laskah/internal/httpx"
	"laskah/internal/store"
	"laskah/internal/wallet"
)

//go:embed all:web
var webAssets embed.FS

// Options 是服务构建参数。
type Options struct {
	DataFile         string
	BalanceInterval  time.Duration
	Strategy         string
	MaxRetries       int
	Cooldown         time.Duration
	FailureThreshold int
	AllowOrigin      string
	TrustProxy       bool

	// PublicModels 控制不带密钥访问 /v1/models 时是否返回全站模型目录。
	//
	// 用指针区分「没设置」与「显式设为 false」：默认开启，只有显式关掉才收敛成空列表。
	PublicModels *bool

	// RequestRefreshWait 是请求路径上等待余额刷新的上限，0 表示用默认值。
	//
	// 额度接口慢的时候，调用方不该陪着一起等；查询会在后台继续跑完。
	RequestRefreshWait time.Duration
}

// App 聚合服务运行所需的组件。
type App struct {
	Store    *store.Store
	Balancer *balancer.Balancer
	Limiter  *balancer.RateLimiter
	Gateway  *gateway.Gateway
	Accounts *accounts.Manager
	Admin    *admin.Handler
	Handler  http.Handler
	interval time.Duration
	stopCh   chan struct{}
}

// New 初始化数据、组件与路由。
func New(options Options) (*App, error) {
	dataStore := store.New(options.DataFile)
	if err := dataStore.Load(); err != nil {
		return nil, err
	}

	strategy := dataStore.Strategy()
	if options.Strategy != "" && balancer.ValidStrategy(options.Strategy) {
		strategy = options.Strategy
	}
	maxRetries := dataStore.MaxRetries()
	if options.MaxRetries > 0 {
		maxRetries = options.MaxRetries
	}
	if err := dataStore.SetSettings(strategy, maxRetries); err != nil {
		return nil, err
	}

	lb := balancer.New(strategy, options.Cooldown, options.FailureThreshold)
	limiter := balancer.NewRateLimiter()
	upstream := gateway.NewUpstream()
	gw := gateway.New(dataStore, lb, limiter, upstream)
	manager := accounts.New(dataStore, wallet.NewClient())
	manager.RequestWait = options.RequestRefreshWait
	// 请求路径上的余额刷新：账号余额数据过期时先查一次再分配流量。
	gw.SetRefresher(manager)
	// 上游明确报余额不足时立刻暂停该账号并换账号重试。
	gw.SetSuspender(manager)
	if options.PublicModels != nil {
		gw.SetPublicModels(*options.PublicModels)
	}
	adminHandler := admin.New(dataStore, lb, upstream, manager, options.TrustProxy)

	mux := http.NewServeMux()
	adminHandler.Register(mux)

	// 扫描间隔固定为 1 分钟量级，账号的“自动查询间隔”才是真正的节流条件。
	interval := options.BalanceInterval
	if interval <= 0 {
		interval = time.Minute
	}

	app := &App{
		Store:    dataStore,
		Balancer: lb,
		Limiter:  limiter,
		Gateway:  gw,
		Accounts: manager,
		Admin:    adminHandler,
		interval: interval,
		stopCh:   make(chan struct{}),
	}
	app.registerRoutes(mux)
	app.Handler = withMiddleware(mux, options.AllowOrigin)

	go app.maintenanceLoop()
	go app.balanceLoop()
	return app, nil
}

// Close 停止后台任务并落盘未保存的统计。
func (a *App) Close() {
	select {
	case <-a.stopCh:
	default:
		close(a.stopCh)
	}
	_, _ = a.Store.Flush()
}

// maintenanceLoop 定期清理限流窗口与过期会话，并合并落盘高频统计写入。
func (a *App) maintenanceLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-a.stopCh:
			return
		case now := <-ticker.C:
			a.Limiter.Prune(now)
			a.Admin.PruneSessions()
			if _, err := a.Store.Flush(); err != nil {
				log.Printf("落盘统计失败: %v", err)
			}
		}
	}
}

// balanceLoop 按各账号自身的“自动查询间隔”刷新额度，并暂停余额耗尽的账号。
//
// 间隔为 0 的账号不会被自动查询，因此这里只做到期扫描，
// 没有到期账号时不产生任何上游请求。
func (a *App) balanceLoop() {
	if a.Accounts == nil {
		return
	}
	ticker := time.NewTicker(a.interval)
	defer ticker.Stop()
	for {
		select {
		case <-a.stopCh:
			return
		case <-ticker.C:
			// 预算要能容下一整批慢站点：单账号超时最长 300 秒，2 分钟的总预算
			// 会在账号稍多时把后面的账号全部判成超时。
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			a.Accounts.RefreshDue(ctx)
			cancel()
			if suspended := a.Accounts.SweepExhausted(); len(suspended) > 0 {
				log.Printf("余额耗尽自动暂停账号: %s", strings.Join(suspended, ", "))
			}
		}
	}
}

func (a *App) registerRoutes(mux *http.ServeMux) {
	assets, err := fs.Sub(webAssets, "web")
	if err != nil {
		panic(err)
	}

	mux.HandleFunc("/health", a.handleHealth)
	mux.HandleFunc("/healthz", a.handleHealth)

	mux.HandleFunc("/setup", a.setupPage(assets))
	mux.HandleFunc("/login", a.loginPage(assets))
	// /manage 只对超级管理员开放：普通管理员即使手输地址也会被重定向回看板。
	mux.HandleFunc("/manage", a.superPage(assets, "manage.html"))
	mux.HandleFunc("/manage/", a.superPage(assets, "manage.html"))
	mux.HandleFunc("/dashboard", a.gatedPage(assets, "dashboard.html"))
	mux.HandleFunc("/dashboard/", a.gatedPage(assets, "dashboard.html"))

	// /keys 是旧地址，永久重定向到 /dashboard。
	mux.HandleFunc("/keys", redirectTo("/dashboard"))
	mux.HandleFunc("/keys/", redirectTo("/dashboard"))

	mux.HandleFunc("/chat/completions", a.Gateway.HandleChatCompletions)
	mux.HandleFunc("/v1/chat/completions", a.Gateway.HandleChatCompletions)
	mux.HandleFunc("/completions", a.handleLegacyCompletions)
	mux.HandleFunc("/v1/completions", a.handleLegacyCompletions)
	mux.HandleFunc("/embeddings", a.Gateway.HandleEmbeddings)
	mux.HandleFunc("/v1/embeddings", a.Gateway.HandleEmbeddings)
	// Responses 是 OpenAI 的新形态兼容接口，与 chat 走同一套账号分配与计费。
	mux.HandleFunc("/responses", a.Gateway.HandleResponses)
	mux.HandleFunc("/v1/responses", a.Gateway.HandleResponses)
	// Messages 是 Anthropic 兼容接口：Claude Code 与 Anthropic SDK 只认这个路径，
	// 缺了它这些客户端在连通性探测阶段就会拿到 404。
	mux.HandleFunc("/messages", a.Gateway.HandleMessages)
	mux.HandleFunc("/v1/messages", a.Gateway.HandleMessages)
	// 列表与单模型查询共用一个处理器：/v1/models 与 /v1/models/{id}。
	mux.HandleFunc("/models", a.Gateway.HandleModels)
	mux.HandleFunc("/models/", a.Gateway.HandleModels)
	mux.HandleFunc("/v1/models", a.Gateway.HandleModels)
	mux.HandleFunc("/v1/models/", a.Gateway.HandleModels)

	fileServer := http.FileServer(http.FS(assets))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			if a.Store.NeedsSetup() {
				http.Redirect(w, r, "/setup", http.StatusFound)
				return
			}
			http.Redirect(w, r, "/dashboard", http.StatusFound)
			return
		}
		if !isStaticAsset(r.URL.Path) {
			httpx.Error(w, http.StatusNotFound, "资源不存在", nil)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		fileServer.ServeHTTP(w, r)
	})
}

// isStaticAsset 白名单化静态资源后缀，杜绝目录遍历与非预期文件暴露。
func isStaticAsset(path string) bool {
	switch {
	case strings.HasSuffix(path, ".css"),
		strings.HasSuffix(path, ".js"),
		strings.HasSuffix(path, ".svg"),
		strings.HasSuffix(path, ".ico"),
		strings.HasSuffix(path, ".png"),
		strings.HasSuffix(path, ".woff2"):
		return true
	default:
		return false
	}
}

func redirectTo(target string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target, http.StatusMovedPermanently)
	}
}

// gatedPage 未登录时跳转登录页，避免管理界面结构对匿名访问者可见。
//
// 服务尚未初始化时一律先去 /setup 创建超级管理员。
func (a *App) gatedPage(assets fs.FS, name string) http.HandlerFunc {
	page := servePage(assets, name)
	return func(w http.ResponseWriter, r *http.Request) {
		if a.Store.NeedsSetup() {
			http.Redirect(w, r, "/setup", http.StatusFound)
			return
		}
		if !a.Admin.Authorized(r) {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		page(w, r)
	}
}

// superPage 只允许超级管理员打开页面。
//
// 权限判定完全在服务端：非超管请求拿不到页面 HTML，
// 因此普通管理员既看不到入口，也无法通过改地址栏进入。
func (a *App) superPage(assets fs.FS, name string) http.HandlerFunc {
	page := servePage(assets, name)
	return func(w http.ResponseWriter, r *http.Request) {
		if a.Store.NeedsSetup() {
			http.Redirect(w, r, "/setup", http.StatusFound)
			return
		}
		if !a.Admin.Authorized(r) {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		if !a.Admin.AuthorizedSuper(r) {
			http.Redirect(w, r, "/dashboard", http.StatusFound)
			return
		}
		page(w, r)
	}
}

// setupPage 只在未初始化时可见，初始化完成后重定向登录页。
func (a *App) setupPage(assets fs.FS) http.HandlerFunc {
	page := servePage(assets, "setup.html")
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.Store.NeedsSetup() {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		page(w, r)
	}
}

// loginPage 在未初始化时把访问者引导到初始化流程。
func (a *App) loginPage(assets fs.FS) http.HandlerFunc {
	page := servePage(assets, "login.html")
	return func(w http.ResponseWriter, r *http.Request) {
		if a.Store.NeedsSetup() {
			http.Redirect(w, r, "/setup", http.StatusFound)
			return
		}
		page(w, r)
	}
}

func servePage(assets fs.FS, name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, err := fs.ReadFile(assets, name)
		if err != nil {
			httpx.Error(w, http.StatusNotFound, "页面不存在: "+name, nil)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(raw)
	}
}

func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	// 健康检查对匿名可见，因此只暴露聚合数量，不泄露余额与配置细节。
	var accountCount, providers, keys, groups int
	a.Store.View(func(data *store.Data) {
		accountCount = len(data.Accounts)
		providers = len(data.Providers)
		keys = len(data.Keys)
		groups = len(data.Groups)
	})
	httpx.JSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"groups":    groups,
		"accounts":  accountCount,
		"providers": providers,
		"keys":      keys,
	})
}

func (a *App) handleLegacyCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := httpx.ReadJSONObject(r)
	if err != nil {
		httpx.Error(w, httpx.StatusOf(err, http.StatusBadRequest), err.Error(), nil)
		return
	}
	if prompt, ok := body["prompt"]; ok {
		if _, hasMessages := body["messages"]; !hasMessages {
			body["messages"] = []any{map[string]any{"role": "user", "content": store.MustString(prompt)}}
		}
		delete(body, "prompt")
	}
	encoded, marshalErr := json.Marshal(body)
	if marshalErr != nil {
		httpx.Error(w, http.StatusInternalServerError, marshalErr.Error(), nil)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(encoded))
	r.ContentLength = int64(len(encoded))
	a.Gateway.HandleChatCompletions(w, r)
}

// withMiddleware 统一处理 CORS、安全响应头与路径检查。
func withMiddleware(next http.Handler, allowOrigin string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w)
		httpx.SetCORS(w, allowOrigin)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if strings.Contains(r.URL.Path, "..") {
			httpx.Error(w, http.StatusBadRequest, "非法路径", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// setSecurityHeaders 收紧浏览器侧行为：禁止内联脚本、禁止被嵌套、禁止 MIME 嗅探。
//
// CSP 不含 unsafe-inline，因此所有页面脚本都必须是独立的 .js 文件。
func setSecurityHeaders(w http.ResponseWriter) {
	header := w.Header()
	header.Set("Content-Security-Policy", strings.Join([]string{
		"default-src 'none'",
		"script-src 'self'",
		"style-src 'self'",
		"img-src 'self' data:",
		"font-src 'self'",
		"connect-src 'self'",
		"form-action 'none'",
		"base-uri 'none'",
		"frame-ancestors 'none'",
	}, "; "))
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("X-Frame-Options", "DENY")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("Cross-Origin-Opener-Policy", "same-origin")
	header.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
}
