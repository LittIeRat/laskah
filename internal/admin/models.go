package admin

import (
	"net/http"
	"sort"
	"strings"

	"laskah/internal/httpx"
	"laskah/internal/store"
)

// maxProbeModels 限制返回给界面的模型数量，避免超大列表拖慢前端渲染。
const maxProbeModels = 500

// handleModelProbe 用用户填写的 base url + 任一 API Key 拉取模型列表，供界面勾选。
//
// 只做一次性探测，不落盘任何凭据。
func (h *Handler) handleModelProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpx.Error(w, http.StatusMethodNotAllowed, "仅支持 POST", nil)
		return
	}

	payload := struct {
		BaseURL string `json:"baseUrl"`
		APIKey  string `json:"apiKey"`
		Keys    string `json:"keys"`
	}{}
	if err := httpx.DecodeJSON(r, &payload); err != nil {
		httpx.Error(w, httpx.StatusOf(err, http.StatusBadRequest), err.Error(), nil)
		return
	}

	baseURL := store.NormalizeBaseURL(payload.BaseURL)
	if baseURL == "" {
		httpx.Error(w, http.StatusBadRequest, "请先填写 base url", nil)
		return
	}

	apiKey := strings.TrimSpace(payload.APIKey)
	if apiKey == "" {
		if keys := parseKeyLines(payload.Keys, nil); len(keys) > 0 {
			apiKey = keys[0]
		}
	}
	if apiKey == "" {
		httpx.Error(w, http.StatusBadRequest, "请先粘贴至少一个 API Key", nil)
		return
	}

	probe := store.Provider{
		Name:      store.HostLabel(baseURL),
		Type:      store.TypeOpenAI,
		BaseURL:   baseURL,
		APIKey:    apiKey,
		Paths:     store.DefaultPaths(store.TypeOpenAI),
		TimeoutMS: 20000,
	}
	result := h.Upstream.ListModels(r.Context(), &probe)
	if !result.OK {
		message := result.Error
		if message == "" {
			message = "模型列表获取失败"
		}
		httpx.Error(w, http.StatusBadGateway, "获取模型列表失败: "+message, map[string]any{"status": result.Status})
		return
	}

	models := dedupeSorted(result.Models)
	if len(models) > maxProbeModels {
		models = models[:maxProbeModels]
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"data":      models,
		"latencyMs": result.LatencyMS,
		"baseUrl":   baseURL,
	})
}

func dedupeSorted(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		result = append(result, trimmed)
	}
	sort.Strings(result)
	return result
}
