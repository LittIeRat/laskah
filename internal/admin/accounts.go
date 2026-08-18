package admin

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"laskah/internal/httpx"
	"laskah/internal/store"
)

// manualRefreshBudget 是「手动刷新全部余额 / 查询总余额」的整体上限。
//
// 单账号超时最长 300 秒，账号一多就可能把管理端请求挂到浏览器超时；
// 这里给整批操作一个封顶值，超出的账号沿用旧余额并在报告里计为失败。
const manualRefreshBudget = 3 * time.Minute

func (h *Handler) handleAccountCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		groupFilter := strings.TrimSpace(r.URL.Query().Get("groupId"))
		list := []any{}
		removed := []any{}
		totals := map[string]any{}
		h.Store.View(func(data *store.Data) {
			for _, account := range data.Accounts {
				if groupFilter != "" && account.GroupID != groupFilter {
					continue
				}
				list = append(list, store.PublicAccount(account, data.CountAccountKeys(account.ID), data.KeysUsingAccount(account.ID)))
			}
			for _, item := range data.RemovedAccounts {
				if groupFilter != "" && item.GroupID != groupFilter {
					continue
				}
				removed = append(removed, item)
			}
			totals = data.AccountTotals()
		})
		httpx.JSON(w, http.StatusOK, map[string]any{"data": list, "removed": removed, "totals": totals})
	case http.MethodPost:
		h.createAccount(w, r)
	default:
		httpx.Error(w, http.StatusMethodNotAllowed, "不支持的方法", nil)
	}
}

// accountPayload 是 /manage 表单提交的账号创建请求。
//
// keys 是批量粘贴的上游 API Key 文本，每行一个；selectedModels 是用户获取模型列表后勾选的模型。
type accountPayload struct {
	store.AccountInput
	Keys      string `json:"keys"`
	KeyList   any    `json:"keyList"`
	Selected  any    `json:"selectedModels"`
	TimeoutMS any    `json:"timeoutMs"`
	// Script 是 balanceScript 的别名，容忍界面早期字段名。
	Script string `json:"balanceScript"`
}

func (h *Handler) createAccount(w http.ResponseWriter, r *http.Request) {
	payload := accountPayload{}
	if err := httpx.DecodeJSON(r, &payload); err != nil {
		httpx.Error(w, httpx.StatusOf(err, http.StatusBadRequest), err.Error(), nil)
		return
	}

	input := payload.AccountInput
	if input.Models == nil {
		input.Models = payload.Selected
	}
	if strings.TrimSpace(input.QueryScript) == "" {
		input.QueryScript = payload.Script
	}
	account, verr := store.BuildAccount(input)
	if verr != nil {
		httpx.Error(w, http.StatusBadRequest, verr.Error(), nil)
		return
	}

	apiKeys := parseKeyLines(payload.Keys, payload.KeyList)
	if len(apiKeys) == 0 {
		httpx.Error(w, http.StatusBadRequest, "请至少粘贴一个 API Key", nil)
		return
	}

	groupMissing := false
	h.Store.View(func(data *store.Data) {
		if account.GroupID == "" || data.FindGroup(account.GroupID) == nil {
			groupMissing = true
		}
	})
	if groupMissing {
		httpx.Error(w, http.StatusBadRequest, "请先选择一个已存在的用户分组", nil)
		return
	}

	models := store.SplitList(account.Models)
	created := 0
	skipped := []any{}
	errorList := []string{}

	if err := h.Store.Update(func(data *store.Data) error {
		data.Accounts = append(data.Accounts, account)
		for index, apiKey := range apiKeys {
			if created >= store.MaxKeysPerAccount {
				skipped = append(skipped, map[string]any{
					"line":   index + 1,
					"reason": "超出单账号 " + strconv.Itoa(store.MaxKeysPerAccount) + " 个 API 上限",
				})
				continue
			}
			provider, buildErr := store.BuildProvider(store.ProviderInput{
				Name:      account.Name + "#" + strconv.Itoa(created+1),
				AccountID: account.ID,
				BaseURL:   account.BaseURL,
				APIKey:    apiKey,
				Models:    models,
				TimeoutMS: payload.TimeoutMS,
				// 账号填了完整端点地址时优先使用它，否则按 baseUrl 拼默认后缀。
				Paths: endpointPaths(account),
			})
			if buildErr != nil {
				errorList = append(errorList, "第 "+strconv.Itoa(index+1)+" 条: "+buildErr.Error())
				continue
			}
			data.Providers = append(data.Providers, provider)
			created++
		}
		return nil
	}); err != nil {
		httpx.Error(w, http.StatusInternalServerError, err.Error(), nil)
		return
	}

	if created == 0 {
		_ = h.Store.Update(func(data *store.Data) error {
			data.RemoveAccounts([]string{account.ID}, "创建时没有任何有效 API")
			return nil
		})
		httpx.Error(w, http.StatusBadRequest, "没有任何有效的 API Key: "+strings.Join(errorList, "; "), nil)
		return
	}

	// 创建后立刻查一次额度，让界面马上能看到余额或凭据错误。
	//
	// 走请求路径的「短等待 + 后台继续」语义：额度站点挂在 Cloudflare 后面时，
	// 单次查询可能要几十秒，不能让「创建账号」这个写操作一直挂着不返回。
	// 等待窗口内查完就带上余额返回，没查完则先返回账号，结果由后台写回。
	h.refreshAccountBalanceSoon(r, account.ID)

	view := map[string]any{}
	h.Store.View(func(data *store.Data) {
		if current := data.FindAccount(account.ID); current != nil {
			view = store.PublicAccount(current, data.CountAccountKeys(current.ID), data.KeysUsingAccount(current.ID))
		}
	})
	httpx.JSON(w, http.StatusCreated, map[string]any{
		"data":    view,
		"created": created,
		"skipped": skipped,
		"errors":  errorList,
	})
}

// endpointPaths 把账号自填的完整端点地址转成上游路径覆盖。
//
// 全部留空时返回 nil，让 BuildProvider 使用协议默认路径。
func endpointPaths(account *store.Account) *store.Paths {
	if account.Endpoints.Chat == "" && account.Endpoints.Models == "" && account.Endpoints.Responses == "" {
		return nil
	}
	paths := account.Endpoints
	return &paths
}

// parseKeyLines 从批量粘贴文本或数组中提取 API Key，忽略空行、注释与重复项。
//
// 必须先按行切分再处理注释：如果一开始就按全部空白字符切分，
// “# 备注”这类注释的正文会被当成一个独立的 Key 混进结果。
func parseKeyLines(text string, list any) []string {
	seen := map[string]bool{}
	result := []string{}

	appendKey := func(raw string) {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			return
		}
		// 容忍粘贴时带上引号或标点。
		trimmed = strings.Trim(trimmed, "\"',;")
		if trimmed == "" || seen[trimmed] {
			return
		}
		seen[trimmed] = true
		result = append(result, trimmed)
	}

	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "//") {
			continue
		}
		// 同一行内允许用空白、逗号或分号分隔多个 Key。
		for _, field := range strings.FieldsFunc(trimmed, func(r rune) bool {
			return r == '\t' || r == ' ' || r == ',' || r == ';'
		}) {
			appendKey(field)
		}
	}
	for _, item := range store.SplitList(list) {
		appendKey(item)
	}
	return result
}

func (h *Handler) handleAccountItem(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/accounts/"), "/")
	if rest == "" {
		h.handleAccountCollection(w, r)
		return
	}
	segments := strings.Split(rest, "/")
	resource := segments[0]
	action := ""
	if len(segments) > 1 {
		action = segments[1]
	}

	switch {
	case resource == "totals" && r.Method == http.MethodGet:
		h.handleAccountTotals(w)
	case resource == "refresh-all" && r.Method == http.MethodPost:
		h.handleRefreshAll(w, r)
	case resource == "balance-query" && r.Method == http.MethodPost:
		h.handleBalanceQuery(w, r)
	case resource == "batch" && r.Method == http.MethodDelete:
		h.handleAccountBatchDelete(w, r)
	case action == "refresh" && r.Method == http.MethodPost:
		h.handleAccountRefresh(w, r, resource)
	case action == "enable" && r.Method == http.MethodPost:
		h.handleAccountEnable(w, r, resource)
	case (action == "balance" || action == "") && r.Method == http.MethodGet:
		h.handleAccountBalance(w, resource)
	case action == "" && r.Method == http.MethodDelete:
		h.handleAccountDelete(w, resource)
	default:
		// 账号配置保存后不可修改、不可回显，故不提供 PATCH/PUT。
		httpx.Error(w, http.StatusMethodNotAllowed, "账号保存后只能查询余额、启停或删除", nil)
	}
}

// handleAccountEnable 启用或暂停账号。
//
// 启用同时解除「余额耗尽自动暂停」与「管理员手动禁用」两种状态，
// 并立刻查一次余额：充值后重新启用应当马上看到真实余额，
// 否则账号会带着旧的耗尽数据进入分配池，第一批请求仍会被上游拒绝。
// 这不属于修改配置，因此不违反「保存后不可修改」的约束。
func (h *Handler) handleAccountEnable(w http.ResponseWriter, r *http.Request, id string) {
	payload := struct {
		Enabled *bool `json:"enabled"`
	}{}
	if err := httpx.DecodeJSON(r, &payload); err != nil {
		httpx.Error(w, httpx.StatusOf(err, http.StatusBadRequest), err.Error(), nil)
		return
	}

	enabled := true
	if payload.Enabled != nil {
		enabled = *payload.Enabled
	}

	missing := false
	if err := h.Store.Update(func(data *store.Data) error {
		account := data.FindAccount(id)
		if account == nil {
			missing = true
			return nil
		}
		if enabled {
			account.Resume()
			return nil
		}
		account.Enabled = false
		account.Suspend("管理员手动暂停")
		return nil
	}); err != nil {
		httpx.Error(w, http.StatusInternalServerError, err.Error(), nil)
		return
	}
	if missing {
		httpx.Error(w, http.StatusNotFound, "账号不存在", nil)
		return
	}

	refresh := map[string]any{}
	if enabled {
		if result := h.refreshAccountBalance(r, id); result != nil {
			refresh = result
		}
	}

	var view map[string]any
	h.Store.View(func(data *store.Data) {
		if account := data.FindAccount(id); account != nil {
			view = store.PublicAccount(account, data.CountAccountKeys(account.ID), data.KeysUsingAccount(account.ID))
		}
	})
	if view == nil {
		httpx.Error(w, http.StatusNotFound, "账号不存在", nil)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"data": view, "refresh": refresh})
}

func (h *Handler) handleAccountTotals(w http.ResponseWriter) {
	totals := map[string]any{}
	h.Store.View(func(data *store.Data) {
		totals = data.AccountTotals()
	})
	httpx.JSON(w, http.StatusOK, map[string]any{"data": totals})
}

func (h *Handler) handleAccountBalance(w http.ResponseWriter, id string) {
	var view map[string]any
	h.Store.View(func(data *store.Data) {
		account := data.FindAccount(id)
		if account == nil {
			return
		}
		view = store.PublicAccount(account, data.CountAccountKeys(account.ID), data.KeysUsingAccount(account.ID))
	})
	if view == nil {
		httpx.Error(w, http.StatusNotFound, "账号不存在", nil)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"data": view})
}

func (h *Handler) handleAccountRefresh(w http.ResponseWriter, r *http.Request, id string) {
	result := h.refreshAccountBalance(r, id)
	if result == nil {
		httpx.Error(w, http.StatusNotFound, "账号不存在", nil)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"data": result})
}

func (h *Handler) handleRefreshAll(w http.ResponseWriter, r *http.Request) {
	if h.Accounts == nil {
		httpx.Error(w, http.StatusServiceUnavailable, "额度查询组件未就绪", nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), manualRefreshBudget)
	defer cancel()
	results := h.Accounts.RefreshAll(ctx)
	totals := map[string]any{}
	h.Store.View(func(data *store.Data) {
		totals = data.AccountTotals()
	})
	httpx.JSON(w, http.StatusOK, map[string]any{"data": results, "totals": totals})
}

// handleBalanceQuery 先刷新全部账号，再返回一份「总余额」汇总。
//
// 与 refresh-all 的区别是它按余额来源把结果拆开：上游查询到的余额、本地手动扣减的余额、
// 无限额度账号数量、以及查询失败的账号数量。总余额只有配上失败数才有意义——
// 少查到一个账号就少一笔钱，直接给一个总数会让人误判额度。
func (h *Handler) handleBalanceQuery(w http.ResponseWriter, r *http.Request) {
	if h.Accounts == nil {
		httpx.Error(w, http.StatusServiceUnavailable, "额度查询组件未就绪", nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), manualRefreshBudget)
	defer cancel()
	results := h.Accounts.RefreshAll(ctx)

	failed := 0
	deleted := 0
	suspended := 0
	for _, item := range results {
		row, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if text, _ := row["error"].(string); text != "" {
			failed++
		}
		if flag, _ := row["deleted"].(bool); flag {
			deleted++
		}
		if flag, _ := row["suspended"].(bool); flag {
			suspended++
		}
	}

	totals := map[string]any{}
	groups := []any{}
	h.Store.View(func(data *store.Data) {
		totals = data.AccountTotals()
		for _, group := range data.Groups {
			groups = append(groups, store.PublicGroup(group, data.GroupSummary(group.ID)))
		}
	})

	httpx.JSON(w, http.StatusOK, map[string]any{
		"data": map[string]any{
			"queried":   len(results),
			"failed":    failed,
			"deleted":   deleted,
			"suspended": suspended,
			"totals":    totals,
			"groups":    groups,
		},
	})
}

func (h *Handler) handleAccountDelete(w http.ResponseWriter, id string) {
	removed := 0
	if err := h.Store.Update(func(data *store.Data) error {
		removed = len(data.RemoveAccounts([]string{id}, "管理员手动删除"))
		return nil
	}); err != nil {
		httpx.Error(w, http.StatusInternalServerError, err.Error(), nil)
		return
	}
	if removed == 0 {
		httpx.Error(w, http.StatusNotFound, "账号不存在", nil)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) handleAccountBatchDelete(w http.ResponseWriter, r *http.Request) {
	payload := struct {
		IDs []string `json:"ids"`
	}{}
	if err := httpx.DecodeJSON(r, &payload); err != nil {
		httpx.Error(w, httpx.StatusOf(err, http.StatusBadRequest), err.Error(), nil)
		return
	}
	removed := 0
	if err := h.Store.Update(func(data *store.Data) error {
		removed = len(data.RemoveAccounts(payload.IDs, "管理员批量删除"))
		return nil
	}); err != nil {
		httpx.Error(w, http.StatusInternalServerError, err.Error(), nil)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"ok": true, "removed": removed})
}

// refreshAccountBalance 触发一次额度查询，账号不存在时返回 nil。
//
// 单账号查询按账号自己的超时执行，这里只加一个封顶，避免管理端请求被无限挂住。
func (h *Handler) refreshAccountBalance(r *http.Request, id string) map[string]any {
	if h.Accounts == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), manualRefreshBudget)
	defer cancel()
	return h.Accounts.Refresh(ctx, id)
}

// refreshAccountBalanceSoon 触发一次额度查询，但最多只等一小段时间。
//
// 与 refreshAccountBalance 的区别：这里复用 RefreshForRequest 的合并 + 后台执行语义，
// 用于「创建账号」这类不该被慢额度接口拖住的写操作。
func (h *Handler) refreshAccountBalanceSoon(r *http.Request, id string) {
	if h.Accounts == nil {
		return
	}
	h.Accounts.RefreshForRequest(r.Context(), id)
}

func parsePositiveInt(text string) (int, error) {
	value, err := strconv.Atoi(strings.TrimSpace(text))
	if err != nil {
		return 0, err
	}
	if value < 1 {
		return 0, strconv.ErrRange
	}
	return value, nil
}

func itoa(value int) string {
	return strconv.Itoa(value)
}
