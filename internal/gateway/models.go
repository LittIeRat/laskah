package gateway

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"laskah/internal/httpx"
	"laskah/internal/store"
)

// modelOwner 是 /v1/models 中 owned_by 的取值。
//
// 网关对下游屏蔽真实上游身份，因此统一署名为本站，
// 避免通过模型列表反推出账号数量或供应商站点。
const modelOwner = "laskah"

// ModelEntry 是严格遵循 OpenAI 规范的模型对象。
//
// 字段顺序与命名都对齐 OpenAI /v1/models：
//
//	{"id":"gpt-4o","object":"model","created":1700000000,"owned_by":"..."}
//
// 用具名结构体而不是 map，保证 JSON 键固定、无多余字段、类型稳定。
type ModelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// ModelList 是 /v1/models 的响应体。
type ModelList struct {
	Object string       `json:"object"`
	Data   []ModelEntry `json:"data"`
}

// AnonymousModelList 是未携带密钥时的响应体。
//
// 保持 object/data 结构不变，确保 OpenAI 客户端仍能正常解析；
// 额外的 hint 只是给用手动打开链接的人看的说明。
type AnonymousModelList struct {
	Object string       `json:"object"`
	Data   []ModelEntry `json:"data"`
	Hint   string       `json:"hint"`
}

// anonymousModelsHint 说明匿名访问看不到模型的原因与正确用法。
const anonymousModelsHint = "未提供 API Key，返回空列表。请在请求头带上 Authorization: Bearer <本站 API Key> 以查看该密钥可调用的模型。"

// HandleModels 处理 /v1/models 与 /v1/models/{id}。
//
// 列表严格按 OpenAI 规范输出：object=list，data 按 id 升序排列，
// 每项只含 id / object / created / owned_by 四个规范字段。
// 列表覆盖「该密钥能落到的全部账号」，而不只是当前绑定的那一个：
// 请求某个模型时网关会自动切到提供它的账号，因此可调用范围就是分组内的并集。
//
// 未携带任何密钥（浏览器直接打开该地址）时返回 200 + 空 data，并在 hint 里说明要带密钥：
// 直接回 401 会让人以为服务坏了，但把全站模型并集暴露给匿名访问者
// 等于泄露了上游供货范围，因此空列表是这里唯一站得住的折中。
// 带了密钥但密钥无效/禁用/过期时仍然照实返回 401/403，不做任何遮掩。
func (g *Gateway) HandleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		httpx.Error(w, http.StatusMethodNotAllowed, "仅支持 GET", nil)
		return
	}

	requested := requestedModelID(r.URL.Path)
	secret := httpx.BearerToken(r)

	// 完全没带密钥（例如浏览器直接打开该地址）时给出可解析的空列表而不是 401。
	// 带了密钥但无效仍然按 401 处理：那是明确的鉴权失败，不该被静默成空结果。
	if strings.TrimSpace(secret) == "" {
		if requested != "" {
			httpx.Error(w, http.StatusNotFound, "模型不存在或当前密钥无权访问: "+requested, nil)
			return
		}
		httpx.JSON(w, http.StatusOK, AnonymousModelList{
			Object: "list",
			Data:   []ModelEntry{},
			Hint:   anonymousModelsHint,
		})
		return
	}

	// 只验密钥、不分配账号：列举模型不该触发余额查询或改动粘性绑定。
	keyID, authErr := g.ValidateKey(secret)
	if authErr != nil {
		httpx.Error(w, authErr.Status, authErr.Message, nil)
		return
	}

	entries := g.availableModels(keyID)

	if requested == "" {
		httpx.JSON(w, http.StatusOK, ModelList{Object: "list", Data: entries})
		return
	}
	for _, entry := range entries {
		if entry.ID == requested {
			httpx.JSON(w, http.StatusOK, entry)
			return
		}
	}
	httpx.Error(w, http.StatusNotFound, "模型不存在或当前密钥无权访问: "+requested, nil)
}

// requestedModelID 从 /v1/models/{id} 提取模型名，列表请求返回空串。
//
// 模型名允许包含斜杠（如 `meta/llama-3`），因此保留剩余全部路径段。
func requestedModelID(path string) string {
	trimmed := strings.TrimSuffix(path, "/")
	for _, prefix := range []string{"/v1/models", "/models"} {
		if !strings.HasPrefix(trimmed, prefix) {
			continue
		}
		rest := strings.TrimPrefix(trimmed, prefix)
		return strings.Trim(rest, "/")
	}
	return ""
}

// availableModels 汇总当前密钥真正能调用到的模型。
//
// 只统计“会被选中的上游”：启用中的 API、密钥允许的模型、
// 以及密钥所在分组内当前可用的账号，确保列表与实际可用范围一致。
func (g *Gateway) availableModels(keyID string) []ModelEntry {
	type modelMeta struct {
		created int64
	}
	found := map[string]modelMeta{}

	g.Store.View(func(data *store.Data) {
		key := data.FindKeyByID(keyID)
		allowedProviders := map[string]bool{}
		if key != nil {
			for _, id := range key.ProviderIDs {
				allowedProviders[id] = true
			}
		}

		for _, provider := range g.reachableProviders(data, key) {
			if !provider.Enabled {
				continue
			}
			if len(allowedProviders) > 0 && !allowedProviders[provider.ID] {
				continue
			}
			created := provider.CreatedAt.Unix()
			if created <= 0 {
				created = time.Now().Unix()
			}
			for _, name := range providerModelNames(provider) {
				if key != nil && !key.AllowsModel(name) {
					continue
				}
				if existing, seen := found[name]; !seen || created < existing.created {
					found[name] = modelMeta{created: created}
				}
			}
		}
	})

	ids := make([]string, 0, len(found))
	for name := range found {
		ids = append(ids, name)
	}
	sort.Strings(ids)

	entries := make([]ModelEntry, 0, len(ids))
	for _, id := range ids {
		entries = append(entries, ModelEntry{
			ID:      id,
			Object:  "model",
			Created: found[id].created,
			OwnedBy: modelOwner,
		})
	}
	return entries
}

// reachableProviders 返回该密钥可能落到的上游集合。
//
// 覆盖分组内全部可用账号，而不是只看当前绑定的账号：
// 网关会为不同模型自动切换到提供该模型的账号，列表必须与这个行为一致，
// 否则下游会看不到自己其实能调用的模型。
func (g *Gateway) reachableProviders(data *store.Data, key *store.APIKey) []*store.Provider {
	groupID := ""
	if key != nil {
		groupID = key.GroupID
	}
	accounts := data.UsableAccounts(groupID)
	if len(accounts) == 0 {
		return nil
	}
	result := []*store.Provider{}
	for _, account := range accounts {
		result = append(result, data.AccountProviders(account.ID)...)
	}
	return result
}

// providerModelNames 展开上游声明的模型名与别名，剔除通配符条目。
//
// 通配符（如 `gpt-4*`）只用于请求期匹配，不能作为可枚举的模型 id 暴露给下游。
func providerModelNames(provider *store.Provider) []string {
	names := make([]string, 0, len(provider.Models)+len(provider.ModelMap))
	seen := map[string]bool{}

	appendName := func(raw string) {
		name := strings.TrimSpace(raw)
		if name == "" || name == "*" || strings.Contains(name, "*") || seen[name] {
			return
		}
		seen[name] = true
		names = append(names, name)
	}

	for _, name := range provider.Models {
		appendName(name)
	}
	for alias := range provider.ModelMap {
		appendName(alias)
	}
	return names
}
