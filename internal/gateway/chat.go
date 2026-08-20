package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"laskah/internal/balancer"
	"laskah/internal/httpx"
	"laskah/internal/store"
	"laskah/internal/tokenizer"
)

// retryableStatus 列出值得切换提供商重试的状态码。
var retryableStatus = map[int]bool{
	408: true, 409: true, 425: true, 429: true,
	500: true, 502: true, 503: true, 504: true, 522: true, 524: true,
}

// BalanceRefresher 在请求路径上按需刷新账号余额。
//
// 返回值表示刷新后该账号是否仍可承接流量；用接口而不是直接依赖 accounts 包，
// 既解开包依赖，也让测试可以注入假实现。
type BalanceRefresher interface {
	RefreshForRequest(ctx context.Context, accountID string) bool
}

// AccountSuspender 在上游明确报告余额不足时立即暂停账号。
//
// 与定时/请求前的额度查询互补：有些站点余额查询接口有缓存或延迟，
// 真正的“这一次请求都付不起”只有上游报错才知道，此时必须立刻把账号踢出池子。
// 暂停而不是删除：账号、上游 API 与统计全部保留，管理员充值后重新启用即可。
type AccountSuspender interface {
	SuspendAccount(accountID, reason string) bool
}

// balanceExhaustedMarkers 是上游“余额/额度不足”的判定文案。
//
// 只在上游返回业务错误时匹配，且刻意不含 "rate limit"/"too many requests" 这类
// 限流文案，避免把临时限流误判成余额耗尽而删掉正常账号。
var balanceExhaustedMarkers = []string{
	"余额不足",
	"余额不够",
	"余额已用尽",
	"余额已耗尽",
	"额度不足",
	"额度不够",
	"额度已用尽",
	"额度已耗尽",
	"额度已用完",
	"配额不足",
	// New API 的预扣费失败：请求前按预估价格扣费，扣不动就说明连这一次都付不起。
	"预扣费失败",
	"预扣费额度失败",
	"预扣费额度不足",
	"预扣额度失败",
	"预扣费用失败",
	"欠费",
	"请充值",
	"insufficient balance",
	"insufficient_balance",
	"insufficient quota",
	"insufficient_quota",
	"insufficient_user_quota",
	"insufficient funds",
	"not enough balance",
	"not enough quota",
	"balance is not enough",
	"quota is not enough",
	"exceeded your current quota",
	"quota exceeded",
	"billing hard limit",
	"out of credits",
	"no credits",
	"credit balance is too low",
	"arrearage",
}

// 额度数字提取：用于识别“剩余 X，需要 Y”这类只报金额、不含“不足”字样的文案，
// 例如 New API 的「预扣费额度失败, 用户剩余额度: ＄0.182898, 需要预扣费额度: ＄0.290486」。
//
// 两条正则都要求关键词与数字之间只隔少量非数字字符，避免跨句误配。
var (
	remainingAmountPattern = regexp.MustCompile(`(?:剩余|可用|当前|remaining|current)[^0-9]{0,16}([0-9]+(?:\.[0-9]+)?)`)
	requiredAmountPattern  = regexp.MustCompile(`(?:需要|所需|需扣|require[sd]?|need(?:s|ed)?)[^0-9]{0,16}([0-9]+(?:\.[0-9]+)?)`)
)

// normalizeErrorText 统一大小写并把全角字符折叠成半角。
//
// 上游常用全角符号（＄、：、，）包裹金额，不折叠会让金额与关键词之间的
// 字节距离被放大，也让 marker 匹配漏掉半角写法。
func normalizeErrorText(text string) string {
	var builder strings.Builder
	builder.Grow(len(text))
	for _, ch := range text {
		switch {
		case ch >= 0xFF01 && ch <= 0xFF5E:
			builder.WriteRune(ch - 0xFEE0)
		case ch == 0x3000:
			builder.WriteRune(' ')
		default:
			builder.WriteRune(ch)
		}
	}
	return strings.ToLower(builder.String())
}

// hasBalanceShortfall 判断文案里是否出现“剩余额度小于本次所需额度”。
//
// 只在同时给出两个金额且剩余确实更少时才成立，因此不会把
// “剩余额度 10，本次消耗 0.2”这类正常提示误判成余额耗尽。
func hasBalanceShortfall(normalized string) bool {
	// 先要求文案确实在谈额度/余额，避免把无关数字凑成一对。
	if !strings.Contains(normalized, "额度") &&
		!strings.Contains(normalized, "余额") &&
		!strings.Contains(normalized, "balance") &&
		!strings.Contains(normalized, "quota") &&
		!strings.Contains(normalized, "credit") {
		return false
	}

	remaining, okRemaining := firstAmount(remainingAmountPattern, normalized)
	required, okRequired := firstAmount(requiredAmountPattern, normalized)
	if !okRemaining || !okRequired {
		return false
	}
	return remaining < required
}

func firstAmount(pattern *regexp.Regexp, text string) (float64, bool) {
	match := pattern.FindStringSubmatch(text)
	if len(match) < 2 {
		return 0, false
	}
	value, err := strconv.ParseFloat(match[1], 64)
	if err != nil {
		return 0, false
	}
	return value, true
}

// IsBalanceExhausted 判断上游错误是否属于“账号余额不足以完成一次请求”。
//
// 状态码先做粗筛：只有付费/权限/参数类错误才可能是余额问题，
// 5xx 与网络错误一律不判为余额耗尽，避免上游抖动导致误删账号。
func IsBalanceExhausted(status int, body string) bool {
	switch status {
	case http.StatusBadRequest, http.StatusPaymentRequired, http.StatusForbidden,
		http.StatusTooManyRequests, http.StatusUnauthorized:
	default:
		return false
	}
	return matchesBalanceExhaustion(body)
}

// matchesBalanceExhaustion 只看文案，不看状态码。
//
// 供两处使用：状态码粗筛之后的 IsBalanceExhausted，以及流式响应里
// HTTP 状态已经是 200、余额不足只能从 SSE 的 error 事件里读出来的场景。
func matchesBalanceExhaustion(body string) bool {
	normalized := normalizeErrorText(body)
	for _, marker := range balanceExhaustedMarkers {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return hasBalanceShortfall(normalized)
}

// balanceExhaustedInPayload 判断一个已解析的 JSON 响应体是否在 error 字段里报余额不足。
//
// 刻意只看 error 字段：模型正文完全可能出现“余额不足”这类字样，
// 拿整个响应体做关键词匹配会把正常回答误判成账号欠费。
func balanceExhaustedInPayload(payload map[string]any) (string, bool) {
	if payload == nil {
		return "", false
	}
	raw, exists := payload["error"]
	if !exists || raw == nil {
		return "", false
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return "", false
	}
	text := string(encoded)
	if !matchesBalanceExhaustion(text) {
		return "", false
	}
	return text, true
}

// balanceStreamError 表示流式响应中途发现上游账号余额不足。
//
// 流式响应的 HTTP 状态码早在第一个字节前就是 200，余额不足只能从 SSE 事件里读到，
// 因此需要一个专门的错误类型把这个信号带回请求主流程做「截断 + 换号」。
type balanceStreamError struct {
	detail string
	// streamed 表示是否已经有内容下发给调用方。
	// 未下发时可以完全透明地换号重试；已下发时只能截断并结束这一次输出。
	streamed bool
}

func (e *balanceStreamError) Error() string {
	return "上游账号余额不足: " + e.detail
}

// maxAccountAttempts 限制单次鉴权中重新挑选账号的次数。
//
// 每次挑选可能触发一次上游额度查询，因此必须设上限，
// 否则大量已耗尽的账号会把单个请求拖成一串串行网络调用。
const maxAccountAttempts = 3

// Gateway 处理 OpenAI 兼容的推理请求。
type Gateway struct {
	Store    *store.Store
	Balancer *balancer.Balancer
	Limiter  *balancer.RateLimiter
	Upstream *Upstream

	// Refresher 可为 nil，此时跳过请求时余额刷新。
	Refresher BalanceRefresher

	// Suspender 可为 nil，此时不做余额不足自动暂停。
	Suspender AccountSuspender

	// PublicModels 决定不带密钥访问 /v1/models 时是否列出全站模型目录。
	//
	// 打开（默认）时匿名请求得到全部可用账号提供的模型并集，只含模型名，
	// 便于客户端探活与人工确认支持范围；关闭时退回空列表加提示。
	PublicModels bool
}

// New 创建网关处理器。
//
// PublicModels 默认开启：模型名不是机密，而「先建密钥才能看有什么模型」会明显拖慢接入。
func New(dataStore *store.Store, lb *balancer.Balancer, limiter *balancer.RateLimiter, upstream *Upstream) *Gateway {
	return &Gateway{Store: dataStore, Balancer: lb, Limiter: limiter, Upstream: upstream, PublicModels: true}
}

// SetPublicModels 设置匿名模型目录开关。
func (g *Gateway) SetPublicModels(enabled bool) {
	g.PublicModels = enabled
}

// SetRefresher 注入请求时余额刷新实现。
func (g *Gateway) SetRefresher(refresher BalanceRefresher) {
	g.Refresher = refresher
}

// SetSuspender 注入余额不足自动暂停实现。
func (g *Gateway) SetSuspender(suspender AccountSuspender) {
	g.Suspender = suspender
}

// suspendExhaustedAccount 在上游报余额不足时暂停该账号，返回是否已暂停。
func (g *Gateway) suspendExhaustedAccount(accountID string, detail string) bool {
	if g.Suspender == nil || accountID == "" {
		return false
	}
	return g.Suspender.SuspendAccount(accountID, "上游报余额不足自动暂停: "+truncateReason(detail))
}

// accountGate 构造账号级频率限制的准入判定。
//
// 用 Peek 而不是 Allow：这里只是在候选池里试探，被跳过的账号不该白白扣掉配额。
// 真正的记账发生在账号确定之后（recordAccountHit），与实际发出的上游请求一一对应。
func (g *Gateway) accountGate() store.AccountGate {
	if g.Limiter == nil {
		return nil
	}
	now := time.Now()
	return func(account *store.Account) bool {
		limit := account.RateLimit()
		if limit <= 0 {
			return true
		}
		return g.Limiter.Peek(balancer.AccountBucket(account.ID), limit, now).Allowed
	}
}

// recordAccountHit 给已确定的账号记一次请求，用于账号级频率限制计数。
func (g *Gateway) recordAccountHit(accountID string) {
	if g.Limiter == nil || accountID == "" {
		return
	}
	limit := 0
	g.Store.View(func(data *store.Data) {
		if account := data.FindAccount(accountID); account != nil {
			limit = account.RateLimit()
		}
	})
	if limit <= 0 {
		return
	}
	g.Limiter.Allow(balancer.AccountBucket(accountID), limit, time.Now())
}

// truncateReason 截断上游报错文本，避免把大段响应写进暂停原因。
func truncateReason(text string) string {
	trimmed := strings.TrimSpace(text)
	runes := []rune(trimmed)
	if len(runes) <= 120 {
		return trimmed
	}
	return string(runes[:120]) + "…"
}

// AuthError 表示密钥鉴权失败。
type AuthError struct {
	Status  int
	Message string
}

func (e *AuthError) Error() string {
	return e.Message
}

// Session 是一次请求的鉴权上下文。
type Session struct {
	KeyID     string
	AccountID string

	// Model 是本次请求指定的模型，为空表示不限模型。
	// 账号与上游都按它筛选，确保请求只落到真正提供该模型的账号上。
	Model       string
	ProviderIDs []string
}

// Authenticate 校验网关密钥，并在需要时自动分配可用账号。
func (g *Gateway) Authenticate(ctx context.Context, secret string) (Session, *AuthError) {
	return g.AuthenticateForModel(ctx, secret, "")
}

// AuthenticateForModel 校验网关密钥，并分配一个能承接指定模型的账号。
//
// 分配流程：命中密钥 → 检查密钥状态 → 挑选支持该模型、未达频率上限的账号 →
// 若该账号余额数据过期则先刷新再确认。刷新后账号若已耗尽或被自动暂停，
// 就重新挑选下一个账号，最多重试 maxAccountAttempts 次，
// 从而避免“余额已经用完却继续往同一个账号打请求”。
//
// 返回成功即视为这一次请求会真的打到该账号，因此顺带记一次账号级频率计数。
//
// model 为空表示不限模型（用于 /v1/models 之类不针对单一模型的请求）。
func (g *Gateway) AuthenticateForModel(ctx context.Context, secret, model string) (Session, *AuthError) {
	if strings.TrimSpace(secret) == "" {
		return Session{}, &AuthError{Status: http.StatusUnauthorized, Message: "缺少 API Key，请在 Authorization: Bearer <key> 中提供"}
	}

	for attempt := 0; attempt < maxAccountAttempts; attempt++ {
		session, staleAccount, authErr := g.assignSession(secret, model)
		if authErr != nil {
			return Session{}, authErr
		}
		if staleAccount == "" {
			g.recordAccountHit(session.AccountID)
			return session, nil
		}
		// 余额数据已过期：先查一次再决定是否放行。
		if g.Refresher == nil || g.Refresher.RefreshForRequest(ctx, staleAccount) {
			g.recordAccountHit(session.AccountID)
			return session, nil
		}
		if ctx.Err() != nil {
			return Session{}, &AuthError{Status: http.StatusServiceUnavailable, Message: "请求已取消"}
		}
	}

	return Session{}, &AuthError{Status: http.StatusServiceUnavailable, Message: noAccountMessage(model)}
}

// ValidateKey 只校验密钥本身，不分配账号，返回密钥 ID。
//
// 供请求先解析出 model 再分配账号的流程使用：既保证非法密钥仍然优先返回 401/403，
// 又让账号分配能拿到模型信息，避免把请求交给不支持该模型的账号。
func (g *Gateway) ValidateKey(secret string) (string, *AuthError) {
	if strings.TrimSpace(secret) == "" {
		return "", &AuthError{Status: http.StatusUnauthorized, Message: "缺少 API Key，请在 Authorization: Bearer <key> 中提供"}
	}
	var (
		keyID   string
		authErr *AuthError
	)
	g.Store.View(func(data *store.Data) {
		key := data.FindKeyBySecret(secret)
		if key == nil {
			authErr = &AuthError{Status: http.StatusUnauthorized, Message: "API Key 无效"}
			return
		}
		if stateErr := keyStateError(key); stateErr != nil {
			authErr = stateErr
			return
		}
		keyID = key.ID
	})
	if authErr != nil {
		return "", authErr
	}
	return keyID, nil
}

// keyStateError 把密钥状态映射成鉴权错误，nil 表示状态正常。
func keyStateError(key *store.APIKey) *AuthError {
	switch key.State(time.Now()) {
	case store.KeyDisabled:
		return &AuthError{Status: http.StatusForbidden, Message: "API Key 已禁用"}
	case store.KeyExpired:
		return &AuthError{Status: http.StatusForbidden, Message: "API Key 已过期"}
	case store.KeyQuotaExceeded:
		return &AuthError{Status: http.StatusTooManyRequests, Message: "API Key 配额已用尽"}
	}
	return nil
}

// noAccountMessage 生成“无可用账号”的提示，指定模型时说明是模型维度无账号可用。
func noAccountMessage(model string) string {
	if model == "" {
		return "没有可用账号（余额耗尽已暂停、达到频率限制或未配置 API）"
	}
	return "没有账号可以承接模型 " + model + "（未配置该模型、余额耗尽已暂停、达到频率限制或上游全部在冷却中）"
}

// assignSession 在一次写锁内完成密钥校验与账号分配。
//
// 第二个返回值非空表示所选账号的余额数据已过期，调用方需要先刷新再决定是否使用。
func (g *Gateway) assignSession(secret, model string) (Session, string, *AuthError) {
	var (
		session      Session
		staleAccount string
		authErr      *AuthError
	)
	gate := g.accountGate()

	g.Store.Mutate(func(data *store.Data) {
		key := data.FindKeyBySecret(secret)
		if key == nil {
			authErr = &AuthError{Status: http.StatusUnauthorized, Message: "API Key 无效"}
			return
		}
		if stateErr := keyStateError(key); stateErr != nil {
			authErr = stateErr
			return
		}

		session.KeyID = key.ID
		session.Model = model
		session.ProviderIDs = append([]string{}, key.ProviderIDs...)
		if len(data.Accounts) == 0 {
			return
		}

		// gate 让「已达每分钟频率上限」的账号本次不参与分配，直接换下一个账号，
		// 而不是给调用方返回 429。
		account := data.AssignAccountGated(key, model, gate)
		if account == nil {
			authErr = &AuthError{Status: http.StatusServiceUnavailable, Message: noAccountMessage(model)}
			return
		}
		session.AccountID = account.ID
		if g.Refresher != nil && account.NeedsRequestRefresh(time.Now()) {
			staleAccount = account.ID
		}
	})

	if authErr != nil {
		return Session{}, "", authErr
	}
	return session, staleAccount, nil
}

// HandleChatCompletions 是 /v1/chat/completions 的入口。
func (g *Gateway) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	g.handleInference(w, r, chatSpec())
}

// HandleResponses 是 /v1/responses 的入口（OpenAI Responses 兼容）。
func (g *Gateway) HandleResponses(w http.ResponseWriter, r *http.Request) {
	g.handleInference(w, r, responsesSpec())
}

// HandleMessages 是 /v1/messages 的入口（Anthropic Messages 兼容）。
//
// 很多客户端（Claude Code、Anthropic SDK）只会用 x-api-key + /v1/messages 接入，
// 只提供 OpenAI 形态会让它们在连通性探测阶段就拿到 404。
func (g *Gateway) HandleMessages(w http.ResponseWriter, r *http.Request) {
	g.handleInference(w, r, messagesSpec())
}

// handleInference 是两个推理端点共享的主流程。
//
// 顺序刻意是「先验密钥 → 再读 model → 最后分配账号」：
// 账号分配必须知道请求的是哪个模型，才能只挑真正提供该模型的账号，
// 否则会把请求交给不支持它的账号，白跑一次上游甚至误伤统计。
//
// token 用量一律以本站自己的估算为准（上游自报值只留作对照），
// 因此账号计费与密钥配额不会被谎报用量的站点带偏。
func (g *Gateway) handleInference(w http.ResponseWriter, r *http.Request, spec inferenceSpec) {
	secret := httpx.BearerToken(r)
	keyID, authErr := g.ValidateKey(secret)
	if authErr != nil {
		httpx.Error(w, authErr.Status, authErr.Message, nil)
		return
	}

	body, err := httpx.ReadJSONObject(r)
	if err != nil {
		httpx.Error(w, httpx.StatusOf(err, http.StatusBadRequest), err.Error(), nil)
		return
	}

	if message := spec.validate(body); message != "" {
		httpx.Error(w, http.StatusBadRequest, message, nil)
		return
	}
	// 入站协议先归一到内部 OpenAI 形态：之后的账号分配、限流、计量与换号
	// 只认这一种形态，新增入口协议不会让主流程分叉。
	if spec.inbound != nil {
		body = spec.inbound(body)
	}
	model := strings.TrimSpace(store.MustString(body["model"]))
	if model == "" {
		httpx.Error(w, http.StatusBadRequest, "model 字段不能为空", nil)
		return
	}

	// 输入 token 在发出请求前就能算出来，与上游是否返回 usage 无关。
	promptTokens := countPromptTokens(body)
	// 调用方声明的输出上限：上游忽略它时由本站兜底截断。
	outputLimit := outputTokenLimit(body)

	var (
		allowsModel bool
		rateLimit   int
	)
	g.Store.View(func(data *store.Data) {
		key := data.FindKeyByID(keyID)
		if key == nil {
			return
		}
		allowsModel = key.AllowsModel(model)
		if key.RateLimitPerMin != nil {
			rateLimit = *key.RateLimitPerMin
		}
	})
	if !allowsModel {
		httpx.Error(w, http.StatusForbidden, "当前 API Key 不允许调用模型 "+model, nil)
		return
	}

	// 限速在账号分配之前判定：被限流的请求不该触发余额查询等上游动作。
	if rateLimit > 0 {
		decision := g.Limiter.Allow(keyID, rateLimit, time.Now())
		if !decision.Allowed {
			seconds := int(decision.RetryAfter.Seconds())
			if seconds < 1 {
				seconds = 1
			}
			w.Header().Set("Retry-After", fmt.Sprint(seconds))
			g.recordKeyUsage(keyID, false, Usage{})
			httpx.Error(w, http.StatusTooManyRequests, "触发速率限制，请稍后再试", nil)
			return
		}
	}

	session, authErr := g.AuthenticateForModel(r.Context(), secret, model)
	if authErr != nil {
		httpx.Error(w, authErr.Status, authErr.Message, nil)
		return
	}
	keyID = session.KeyID

	candidates := g.orderedProviders(model, session)
	if len(candidates) == 0 {
		httpx.Error(w, http.StatusServiceUnavailable, "没有可用的上游提供商（检查模型匹配、启用状态与冷却状态）", nil)
		return
	}

	maxAttempts := g.Store.MaxRetries()
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	if maxAttempts > len(candidates) {
		maxAttempts = len(candidates)
	}

	stream := asBool(body["stream"])
	attempts := []any{}
	// accountSwitches 给“余额不足暂停后换账号”设上限，
	// 避免大批账号同时欠费时把一次请求拖成长串串行重试。
	accountSwitches := 0

	// switchAccount 在“账号连这一次请求都付不起”时立刻暂停该账号并换一个账号，
	// 成功时已就地刷新 session / candidates / maxAttempts，调用方只需重置循环下标。
	switchAccount := func(detail string) bool {
		if accountSwitches >= maxAccountAttempts {
			return false
		}
		if !g.suspendExhaustedAccount(session.AccountID, detail) {
			return false
		}
		accountSwitches++
		retrySession, retryErr := g.AuthenticateForModel(r.Context(), secret, model)
		if retryErr != nil {
			return false
		}
		retryCandidates := g.orderedProviders(model, retrySession)
		if len(retryCandidates) == 0 {
			return false
		}
		session = retrySession
		keyID = session.KeyID
		candidates = retryCandidates
		maxAttempts = g.Store.MaxRetries()
		if maxAttempts < 1 {
			maxAttempts = 1
		}
		if maxAttempts > len(candidates) {
			maxAttempts = len(candidates)
		}
		return true
	}

	for index := 0; index < maxAttempts; index++ {
		provider := candidates[index]
		g.adjustInflight(provider.ID, 1)

		response, sendErr := spec.send(g.Upstream, r.Context(), provider, body)
		if sendErr != nil {
			g.adjustInflight(provider.ID, -1)
			g.reportFailure(provider.ID, sendErr)
			attempts = append(attempts, map[string]any{"provider": provider.Name, "error": sendErr.Error()})
			if errors.Is(sendErr, context.Canceled) {
				return
			}
			continue
		}

		if response.HTTP.StatusCode < 200 || response.HTTP.StatusCode >= 300 {
			snippet := ReadErrorBody(response.HTTP)
			response.HTTP.Body.Close()
			response.Cancel()
			g.adjustInflight(provider.ID, -1)
			statusErr := fmt.Errorf("HTTP %d %s", response.HTTP.StatusCode, snippet)
			g.reportFailure(provider.ID, statusErr)
			attempts = append(attempts, map[string]any{"provider": provider.Name, "status": response.HTTP.StatusCode, "error": snippet})

			// 上游明确说“这一次请求的余额都不够”时立刻暂停该账号，
			// 并换一个账号重试，让调用方感知不到这次切换。
			if IsBalanceExhausted(response.HTTP.StatusCode, snippet) && switchAccount(snippet) {
				index = -1
				continue
			}

			if retryableStatus[response.HTTP.StatusCode] && index < maxAttempts-1 {
				continue
			}
			g.recordKeyUsage(keyID, false, Usage{})
			status := response.HTTP.StatusCode
			if status == http.StatusUnauthorized || status == http.StatusForbidden {
				status = http.StatusBadGateway
			}
			httpx.Error(w, status, "上游返回错误: "+firstNonEmptyText(snippet, fmt.Sprint(response.HTTP.StatusCode)), map[string]any{
				"provider": provider.Name,
				"attempts": attempts,
			})
			return
		}

		w.Header().Set("X-Lb-Provider", provider.Name)
		w.Header().Set("X-Lb-Provider-Id", provider.ID)
		w.Header().Set("X-Lb-Attempt", fmt.Sprint(index+1))
		w.Header().Set("X-Lb-Upstream-Model", provider.UpstreamModel(model))
		w.Header().Set("X-Lb-Endpoint", spec.endpoint)

		if stream {
			result, streamErr := g.pipeStream(w, response, provider, model, spec, streamOptions{
				outputLimit:  outputLimit,
				promptTokens: promptTokens,
			})
			response.HTTP.Body.Close()
			response.Cancel()
			g.adjustInflight(provider.ID, -1)
			usage := localUsage(promptTokens, result.completionTokens, result.upstream.TotalTokens)

			// 流式响应里余额不足只能从 SSE 的 error 事件读到（HTTP 状态早已是 200）。
			var balanceErr *balanceStreamError
			if errors.As(streamErr, &balanceErr) {
				g.reportFailure(provider.ID, streamErr)
				attempts = append(attempts, map[string]any{"provider": provider.Name, "error": balanceErr.Error()})
				switched := switchAccount(balanceErr.detail)
				if switched && !balanceErr.streamed {
					// 还没给调用方写过任何字节，可以完全透明地换号重来。
					index = -1
					continue
				}
				// 已经下发过内容：立刻截断并正常收尾，不让调用方干等或读到半截 SSE。
				g.recordKeyUsage(keyID, false, usage)
				if balanceErr.streamed {
					spec.truncate(w, model)
					return
				}
				httpx.Error(w, http.StatusServiceUnavailable, noAccountMessage(model), map[string]any{"attempts": attempts})
				return
			}

			if streamErr != nil {
				g.reportFailure(provider.ID, streamErr)
				g.recordKeyUsage(keyID, false, usage)
				return
			}
			g.reportSuccess(provider.ID, time.Since(response.StartedAt), usage)
			g.recordKeyUsage(keyID, true, usage)
			return
		}

		payload, upstreamUsage, decodeErr := g.decodeResponse(response, provider, model, spec)
		response.HTTP.Body.Close()
		response.Cancel()
		g.adjustInflight(provider.ID, -1)
		if decodeErr == nil {
			// 有些上游用 HTTP 200 包着 error 字段返回余额不足，状态码粗筛拦不住。
			if detail, exhausted := balanceExhaustedInPayload(payload); exhausted {
				g.reportFailure(provider.ID, errors.New("上游账号余额不足"))
				attempts = append(attempts, map[string]any{"provider": provider.Name, "error": detail})
				if switchAccount(detail) {
					index = -1
					continue
				}
				g.recordKeyUsage(keyID, false, Usage{})
				httpx.Error(w, http.StatusServiceUnavailable, noAccountMessage(model), map[string]any{"attempts": attempts})
				return
			}
		}
		if decodeErr != nil {
			g.reportFailure(provider.ID, decodeErr)
			attempts = append(attempts, map[string]any{"provider": provider.Name, "error": decodeErr.Error()})
			if index < maxAttempts-1 {
				continue
			}
			g.recordKeyUsage(keyID, false, Usage{})
			httpx.Error(w, http.StatusBadGateway, "上游响应解析失败: "+decodeErr.Error(), map[string]any{"attempts": attempts})
			return
		}

		// 上游忽略 max_tokens 时本站兜底截断：否则调用方按上限做的预算与展示全部失真，
		// 计费也会按上游多吐的部分收钱。截断后 finish_reason 改成 length，语义与 OpenAI 一致。
		enforceOutputLimit(payload, outputLimit)

		// 输出 token 由响应正文自行统计，然后覆盖上游给出的 usage：
		// 下游看到的用量与本站计费口径一致，不会因上游谎报而对不上账。
		usage := localUsage(promptTokens, tokenizer.CountCompletionPayload(payload), upstreamUsage.TotalTokens)
		payload["usage"] = usageBody(usage)

		if spec.outbound != nil {
			payload = spec.outbound(payload, model)
		}

		g.reportSuccess(provider.ID, time.Since(response.StartedAt), usage)
		g.recordKeyUsage(keyID, true, usage)
		httpx.JSON(w, http.StatusOK, payload)
		return
	}

	g.recordKeyUsage(keyID, false, Usage{})
	httpx.Error(w, http.StatusBadGateway, "所有上游提供商均失败", map[string]any{"attempts": attempts})
}

// HandleEmbeddings 转发向量化请求到 OpenAI 兼容提供商。
//
// 与 chat 一致：先验密钥、再读 model、最后按模型分配账号。
func (g *Gateway) HandleEmbeddings(w http.ResponseWriter, r *http.Request) {
	secret := httpx.BearerToken(r)
	if _, authErr := g.ValidateKey(secret); authErr != nil {
		httpx.Error(w, authErr.Status, authErr.Message, nil)
		return
	}

	body, err := httpx.ReadJSONObject(r)
	if err != nil {
		httpx.Error(w, httpx.StatusOf(err, http.StatusBadRequest), err.Error(), nil)
		return
	}
	model := strings.TrimSpace(store.MustString(body["model"]))
	if model == "" {
		httpx.Error(w, http.StatusBadRequest, "model 字段不能为空", nil)
		return
	}

	// 向量化只有输入 token，本地按 input 字段自算，同样不采信上游自报用量。
	inputTokens := tokenizer.CountContent(body["input"])

	session, authErr := g.AuthenticateForModel(r.Context(), secret, model)
	if authErr != nil {
		httpx.Error(w, authErr.Status, authErr.Message, nil)
		return
	}
	keyID := session.KeyID

	candidates := g.orderedProviders(model, session)
	if len(candidates) == 0 {
		httpx.Error(w, http.StatusServiceUnavailable, "没有可用的上游提供商（检查模型匹配、启用状态与冷却状态）", nil)
		return
	}
	provider := candidates[0]

	payload := map[string]any{}
	for key, value := range body {
		payload[key] = value
	}
	payload["model"] = provider.UpstreamModel(model)

	encoded, marshalErr := json.Marshal(payload)
	if marshalErr != nil {
		httpx.Error(w, http.StatusInternalServerError, marshalErr.Error(), nil)
		return
	}

	target := JoinURL(provider.BaseURL, "/embeddings")
	request, requestErr := http.NewRequestWithContext(r.Context(), http.MethodPost, target, strings.NewReader(string(encoded)))
	if requestErr != nil {
		httpx.Error(w, http.StatusInternalServerError, requestErr.Error(), nil)
		return
	}
	request.Header = AuthHeaders(provider)

	response, sendErr := g.Upstream.client.Do(request)
	if sendErr != nil {
		g.reportFailure(provider.ID, sendErr)
		httpx.Error(w, http.StatusBadGateway, "上游请求失败: "+sendErr.Error(), nil)
		return
	}
	defer response.Body.Close()

	raw, readErr := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if readErr != nil {
		g.reportFailure(provider.ID, readErr)
		httpx.Error(w, http.StatusBadGateway, "读取上游响应失败: "+readErr.Error(), nil)
		return
	}

	if response.StatusCode >= 200 && response.StatusCode < 300 {
		usage := localUsage(inputTokens, 0, 0)
		g.reportSuccess(provider.ID, 0, usage)
		g.recordKeyUsage(keyID, true, usage)
	} else {
		g.reportFailure(provider.ID, fmt.Errorf("HTTP %d", response.StatusCode))
		g.recordKeyUsage(keyID, false, Usage{})
		// 向量化没有多上游重试，但账号既然连这一次都付不起，也应立即退出分配池。
		if snippet := string(raw); IsBalanceExhausted(response.StatusCode, snippet) {
			g.suspendExhaustedAccount(session.AccountID, snippet)
		}
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(raw)
}

// orderedProviders 先按账号收敛候选范围，再在账号内做负载均衡排序。
//
// 候选只来自会话绑定的账号，绝不跨账号混用：Balancer 再按模型匹配、
// 冷却状态与策略排序，因此最终只会打到「确实提供该模型」的那些 Key 上。
func (g *Gateway) orderedProviders(model string, session Session) []*store.Provider {
	snapshot := []*store.Provider{}
	g.Store.View(func(data *store.Data) {
		if session.AccountID != "" {
			snapshot = append(snapshot, data.AccountProviders(session.AccountID)...)
			return
		}
		snapshot = append(snapshot, data.Providers...)
	})
	ordered := g.Balancer.Order(snapshot, balancer.Criteria{Model: model, ProviderIDs: session.ProviderIDs})
	return preferDeclaredModel(ordered, model)
}

// preferDeclaredModel 把“明确声明该模型”的上游排到前面。
//
// Balancer 已按策略排好序，这里只做一次稳定的两分区：模型列表留空的上游
// 属于“什么都收”，真实提供情况未知，应该留作兜底而不是首选。
// 全部都是同一类时返回原切片，不产生额外分配。
func preferDeclaredModel(providers []*store.Provider, model string) []*store.Provider {
	if model == "" || len(providers) <= 1 {
		return providers
	}
	declared := make([]*store.Provider, 0, len(providers))
	fallback := []*store.Provider{}
	for _, provider := range providers {
		if provider.ExplicitlySupportsModel(model) {
			declared = append(declared, provider)
			continue
		}
		fallback = append(fallback, provider)
	}
	if len(declared) == 0 || len(fallback) == 0 {
		return providers
	}
	return append(declared, fallback...)
}

// decodeResponse 读取非流式响应，返回响应体与上游自报用量。
//
// 第二个返回值只是上游自己声称的用量，仅用于对照；真正计费的 token
// 由调用方用 tokenizer 依据请求与响应正文自行统计。
func (g *Gateway) decodeResponse(response *Response, provider *store.Provider, model string, spec inferenceSpec) (map[string]any, Usage, error) {
	raw, err := io.ReadAll(io.LimitReader(response.HTTP.Body, 64<<20))
	if err != nil {
		return nil, Usage{}, err
	}
	decoded := map[string]any{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, Usage{}, fmt.Errorf("非法 JSON 响应: %w", err)
	}

	payload := decoded
	if spec.anthropic && provider.Type == store.TypeAnthropic {
		payload = FromAnthropicResponse(decoded, model)
	} else {
		payload["model"] = model
	}
	return payload, usageFromMap(payload["usage"]), nil
}

// sseBalanceError 从一行 SSE 文本里识别“账号余额不足”。
//
// 流式响应的 HTTP 状态码在第一个字节前就已经是 200，余额不足只会以
// SSE 的 error 事件出现，因此必须逐行看一眼 error 字段。
func sseBalanceError(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "data:") {
		return "", false
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	if payload == "" || payload == "[DONE]" || !strings.HasPrefix(payload, "{") {
		return "", false
	}
	decoded := map[string]any{}
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		return "", false
	}
	return balanceExhaustedInPayload(decoded)
}

// finishTruncatedStream 在流式输出中途换号时给调用方一个明确的收尾。
//
// 已经下发的内容撤不回来，但绝不能让连接就这么挂着：补一个
// finish_reason=length 的 chunk 再发 [DONE]，标准 OpenAI 客户端会正常结束读取。
func finishTruncatedStream(w http.ResponseWriter, model string) {
	chunk := map[string]any{
		"id":      fmt.Sprintf("chatcmpl_%d", time.Now().UnixNano()),
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": "length",
		}},
	}
	if encoded, err := json.Marshal(chunk); err == nil {
		_, _ = io.WriteString(w, "data: "+string(encoded)+"\n\n")
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// streamOptions 是一次流式转发的可选约束。
type streamOptions struct {
	// outputLimit 是调用方声明的输出上限，0 表示未声明。
	outputLimit int64

	// promptTokens 是本站算出的输入 token，供需要在首个事件里回报用量的协议使用。
	promptTokens int64
}

// streamResult 是一次流式转发的用量结果。
//
// completionTokens 是本站自己按下发正文累计出的输出 token；
// upstream 是上游自报的 usage，仅作对照，不参与计费。
type streamResult struct {
	completionTokens int64
	upstream         Usage

	// overLimit 表示上游无视 max_tokens，本站主动截断了输出。
	overLimit bool
}

// pipeStream 把上游 SSE 转发给调用方，同时在本地累计输出 token。
//
// 响应头刻意延迟到第一次真正下发内容时才写：在那之前发现余额不足，
// 还可以完全透明地换一个账号重试，调用方看不到任何异常。
//
// 输出 token 按「真正下发给调用方的正文」累计，因此与上游是否返回 usage 无关，
// 中途截断也只会计到截断处，不会替上游多算钱。
//
// 声明了输出上限时，累计量一旦越过硬上限就停止转发并收尾：有些中转站完全忽略
// max_tokens，不收口会让调用方的预算与展示全部失真，也会按多吐的部分计费。
func (g *Gateway) pipeStream(w http.ResponseWriter, response *Response, provider *store.Provider, model string, spec inferenceSpec, options streamOptions) (streamResult, error) {
	flusher, _ := w.(http.Flusher)
	reader := bufio.NewReader(response.HTTP.Body)
	result := streamResult{}
	var counter tokenizer.Counter
	converter := newAnthropicStreamConverter(model)
	useAnthropic := spec.anthropic && provider.Type == store.TypeAnthropic
	started := false
	hardCap := outputHardCap(options.outputLimit)

	var rewriter streamRewriter
	if spec.newRewriter != nil {
		rewriter = spec.newRewriter(model, options.promptTokens)
	}

	begin := func() {
		if started {
			return
		}
		started = true
		header := w.Header()
		header.Set("Content-Type", "text/event-stream; charset=utf-8")
		header.Set("Cache-Control", "no-cache, no-transform")
		header.Set("Connection", "keep-alive")
		header.Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
	}
	write := func(text string) error {
		begin()
		_, err := io.WriteString(w, text)
		return err
	}
	// emit 只接受内部形态的 chunk：先按内部形态累计 token，再按出站协议改写下发，
	// 这样计量口径与协议无关，新增出站协议不影响计费。
	emit := func(chunk string) error {
		if delta := spec.streamDelta(chunk); delta != "" {
			counter.Add(delta)
		}
		if rewriter == nil {
			return write(chunk)
		}
		for _, line := range rewriter.Rewrite(chunk) {
			if err := write(line); err != nil {
				return err
			}
		}
		return nil
	}
	finish := func(err error) (streamResult, error) {
		result.completionTokens = counter.Total()
		return result, err
	}
	// closeStream 补齐出站协议的收尾事件。
	closeStream := func(stopReason string) error {
		if rewriter == nil {
			return nil
		}
		if stopReason != "" {
			rewriter.SetStopReason(stopReason)
		}
		for _, line := range rewriter.Finish(counter.Total()) {
			if err := write(line); err != nil {
				return err
			}
		}
		return nil
	}

	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			// 余额不足要在转发之前拦下：已经写出去的内容无法撤回。
			if detail, exhausted := sseBalanceError(line); exhausted {
				return finish(&balanceStreamError{detail: detail, streamed: started})
			}
			if useAnthropic {
				for _, chunk := range converter.Convert(line) {
					if writeErr := emit(chunk); writeErr != nil {
						return finish(writeErr)
					}
				}
				if converter.usage.TotalTokens > 0 {
					result.upstream = converter.usage
				}
			} else {
				if parsed, ok := usageFromSSELine(line); ok {
					result.upstream = parsed
				}
				if writeErr := emit(line); writeErr != nil {
					return finish(writeErr)
				}
			}
			if started && flusher != nil {
				flusher.Flush()
			}
			// 越过硬上限就地收口：继续读只会替忽略 max_tokens 的上游多计费。
			if hardCap > 0 && counter.Total() > hardCap {
				result.overLimit = true
				if rewriter != nil {
					if closeErr := closeStream("max_tokens"); closeErr != nil {
						return finish(closeErr)
					}
				} else {
					begin()
					overLimit := spec.overLimit
					if overLimit == nil {
						overLimit = spec.truncate
					}
					overLimit(w, model)
				}
				return finish(nil)
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return finish(err)
		}
	}

	if useAnthropic && rewriter == nil {
		if err := write("data: [DONE]\n\n"); err != nil {
			return finish(err)
		}
	}
	if useAnthropic {
		result.upstream = converter.usage
	}
	if closeErr := closeStream(""); closeErr != nil {
		return finish(closeErr)
	}
	// 上游一个字节都没给（空流）时也要落地响应头，否则调用方收不到结束。
	begin()
	if flusher != nil {
		flusher.Flush()
	}
	return finish(nil)
}

// adjustInflight 只改内存计数，不触发落盘。
func (g *Gateway) adjustInflight(providerID string, delta int64) {
	g.Store.Mutate(func(data *store.Data) {
		if provider := data.FindProvider(providerID); provider != nil {
			provider.Inflight += delta
			if provider.Inflight < 0 {
				provider.Inflight = 0
			}
		}
	})
}

// reportSuccess 与 reportFailure 走缓冲写：统计信息由后台合并落盘，
// 避免每个请求都重写整个数据文件。
func (g *Gateway) reportSuccess(providerID string, latency time.Duration, usage Usage) {
	g.Store.Mutate(func(data *store.Data) {
		if provider := data.FindProvider(providerID); provider != nil {
			g.Balancer.ReportSuccess(provider, latency, usage)
		}
	})
}

func (g *Gateway) reportFailure(providerID string, err error) {
	g.Store.Mutate(func(data *store.Data) {
		if provider := data.FindProvider(providerID); provider != nil {
			g.Balancer.ReportFailure(provider, err)
		}
	})
}

// recordKeyUsage 记账一次调用：写入本地统计，并按账号计价方式扣费。
//
// token 数全部来自本站自己的估算，因此上游谎报用量不会影响计费与配额；
// 上游自报值另存 UpstreamTokens 供排查对照。
//
// 计费只对成功请求执行：失败的请求没有产出，向调用方收钱不合理，
// 但 requests/failure 仍然计数，方便看板判断账号健康度。
func (g *Gateway) recordKeyUsage(keyID string, ok bool, usage Usage) {
	exhausted := ""
	g.Store.Mutate(func(data *store.Data) {
		key := data.FindKeyByID(keyID)
		if key == nil {
			return
		}
		now := time.Now().UTC()
		key.Stats.Requests++
		if ok {
			key.Stats.Success++
		} else {
			key.Stats.Failure++
		}
		key.Stats.PromptTokens += usage.PromptTokens
		key.Stats.CompletionTokens += usage.CompletionTokens
		key.Stats.TotalTokens += usage.TotalTokens
		key.Stats.UpstreamTokens += usage.UpstreamTokens
		key.Stats.LastUsedAt = &now

		account := data.FindAccount(key.AccountID)
		if account == nil {
			return
		}
		account.Stats.Requests++
		if ok {
			account.Stats.Success++
		} else {
			account.Stats.Failure++
		}
		account.Stats.PromptTokens += usage.PromptTokens
		account.Stats.CompletionTokens += usage.CompletionTokens
		account.Stats.TotalTokens += usage.TotalTokens
		account.Stats.UpstreamTokens += usage.UpstreamTokens
		account.Stats.LastUsedAt = &now

		if !ok {
			return
		}
		calls := int64(1)
		if cost := account.Charge(usage.TotalTokens, calls); cost > 0 {
			key.Stats.Cost = round6(key.Stats.Cost + cost)
		}
		// 手动余额扣到下限时立刻退出分配池：下一次请求会自动换号。
		if account.AutoSuspend && account.Exhausted() && !account.Suspended {
			exhausted = account.ID
		}
	})

	if exhausted != "" && g.Suspender != nil {
		g.Suspender.SuspendAccount(exhausted, "本地计费余额触及下限自动暂停")
	}
}

// round6 把金额收敛到 6 位小数，避免浮点累加出现长尾误差。
func round6(value float64) float64 {
	scaled := value * 1e6
	if scaled >= 0 {
		scaled += 0.5
	} else {
		scaled -= 0.5
	}
	return float64(int64(scaled)) / 1e6
}

// usageFromMap 解析上游自报的 usage 字段。
//
// 结果只用于对照与日志：本站计费一律走 tokenizer 自算的数字。
func usageFromMap(value any) Usage {
	source, ok := value.(map[string]any)
	if !ok {
		return Usage{}
	}
	usage := Usage{}
	if prompt, ok := asInt(source["prompt_tokens"]); ok {
		usage.PromptTokens = prompt
	} else if prompt, ok := asInt(source["input_tokens"]); ok {
		usage.PromptTokens = prompt
	}
	if completion, ok := asInt(source["completion_tokens"]); ok {
		usage.CompletionTokens = completion
	} else if completion, ok := asInt(source["output_tokens"]); ok {
		usage.CompletionTokens = completion
	}
	if total, ok := asInt(source["total_tokens"]); ok {
		usage.TotalTokens = total
	} else {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	return usage
}

func usageFromSSELine(line string) (Usage, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "data:") {
		return Usage{}, false
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	if payload == "" || payload == "[DONE]" {
		return Usage{}, false
	}
	decoded := map[string]any{}
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		return Usage{}, false
	}
	if decoded["usage"] == nil {
		return Usage{}, false
	}
	usage := usageFromMap(decoded["usage"])
	if usage.TotalTokens == 0 && usage.PromptTokens == 0 && usage.CompletionTokens == 0 {
		return Usage{}, false
	}
	return usage, true
}

func firstNonEmptyText(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
