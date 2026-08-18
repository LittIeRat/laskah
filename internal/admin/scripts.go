package admin

import (
	"net/http"
	"strings"

	"laskah/internal/httpx"
	"laskah/internal/script"
)

// handleScriptValidate 校验一段额度查询脚本，并回显它将要发出的请求。
//
// 保存后账号配置不可回显，因此脚本必须在提交前就能验证：否则写错一个字段
// 只能靠「创建账号 → 看余额报错 → 删号重建」来发现。
// 这里不会真的发请求，只做解析与模板替换，用占位值展示最终地址与请求头。
func (h *Handler) handleScriptValidate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpx.Error(w, http.StatusMethodNotAllowed, "仅支持 POST", nil)
		return
	}

	payload := struct {
		Script      string `json:"script"`
		BaseURL     string `json:"baseUrl"`
		QueryURL    string `json:"queryUrl"`
		UserID      string `json:"userId"`
		AccessToken string `json:"accessToken"`
		APIKey      string `json:"apiKey"`
	}{}
	if err := httpx.DecodeJSON(r, &payload); err != nil {
		httpx.Error(w, httpx.StatusOf(err, http.StatusBadRequest), err.Error(), nil)
		return
	}

	program, err := script.Parse(payload.Script)
	if err != nil {
		httpx.JSON(w, http.StatusOK, map[string]any{"data": map[string]any{
			"ok":    false,
			"error": err.Error(),
		}})
		return
	}

	base := strings.TrimRight(strings.TrimSpace(payload.BaseURL), "/")
	if custom := strings.TrimRight(strings.TrimSpace(payload.QueryURL), "/"); custom != "" {
		base = custom
	}
	if base == "" {
		base = "https://example.com"
	}
	request, err := program.BuildRequest(map[string]string{
		"baseUrl":     base,
		"userId":      firstNonBlank(payload.UserID, "<用户 ID>"),
		"accessToken": maskPreview(payload.AccessToken, "<访问令牌>"),
		"apiKey":      maskPreview(payload.APIKey, "<上游 API Key>"),
	})
	if err != nil {
		httpx.JSON(w, http.StatusOK, map[string]any{"data": map[string]any{
			"ok":    false,
			"error": err.Error(),
		}})
		return
	}

	httpx.JSON(w, http.StatusOK, map[string]any{"data": map[string]any{
		"ok":           true,
		"method":       request.Method,
		"url":          request.URL,
		"headers":      request.Headers,
		"hasBody":      request.Body != "",
		"placeholders": script.Placeholders,
	}})
}

func firstNonBlank(value, fallback string) string {
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		return trimmed
	}
	return fallback
}

// maskPreview 回显凭据时只保留头尾，避免校验结果把令牌原文打到界面与日志里。
func maskPreview(value, fallback string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return fallback
	}
	if len(trimmed) <= 8 {
		return "****"
	}
	return trimmed[:4] + "****" + trimmed[len(trimmed)-2:]
}
