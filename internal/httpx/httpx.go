package httpx

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// maxBodyBytes 限制请求体大小，避免恶意超大请求。
const maxBodyBytes = 16 << 20

// StatusError 是带 HTTP 状态码的错误。
type StatusError struct {
	Status  int
	Message string
}

func (e *StatusError) Error() string {
	return e.Message
}

// StatusOf 提取错误携带的状态码，没有则返回兜底值。
func StatusOf(err error, fallback int) int {
	var statusErr *StatusError
	if errors.As(err, &statusErr) {
		return statusErr.Status
	}
	return fallback
}

// JSON 以 JSON 形式写出响应。
func JSON(w http.ResponseWriter, status int, payload any) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, "序列化响应失败", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(encoded)
}

// Error 以 OpenAI 风格错误结构写出响应。
func Error(w http.ResponseWriter, status int, message string, extra map[string]any) {
	errorBody := map[string]any{"message": message, "code": status}
	if status >= 500 {
		errorBody["type"] = "upstream_error"
	} else {
		errorBody["type"] = "invalid_request_error"
	}
	for key, value := range extra {
		errorBody[key] = value
	}
	JSON(w, status, map[string]any{"error": errorBody})
}

// ReadJSONObject 读取并解析 JSON 对象请求体。
func ReadJSONObject(r *http.Request) (map[string]any, error) {
	raw, err := readBody(r)
	if err != nil {
		return nil, err
	}
	if raw == "" {
		return map[string]any{}, nil
	}
	decoded := map[string]any{}
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return nil, &StatusError{Status: http.StatusBadRequest, Message: "JSON 请求体解析失败: " + err.Error()}
	}
	return decoded, nil
}

// DecodeJSON 把请求体解析到指定结构。
func DecodeJSON(r *http.Request, target any) error {
	raw, err := readBody(r)
	if err != nil {
		return err
	}
	if raw == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(raw), target); err != nil {
		return &StatusError{Status: http.StatusBadRequest, Message: "JSON 请求体解析失败: " + err.Error()}
	}
	return nil
}

func readBody(r *http.Request) (string, error) {
	if r.Body == nil {
		return "", nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return "", &StatusError{Status: http.StatusBadRequest, Message: "读取请求体失败: " + err.Error()}
	}
	if len(raw) > maxBodyBytes {
		return "", &StatusError{Status: http.StatusRequestEntityTooLarge, Message: "请求体过大"}
	}
	return strings.TrimSpace(string(raw)), nil
}

// BearerToken 从请求头提取令牌，兼容多种自定义头。
func BearerToken(r *http.Request) string {
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(header) > 7 && strings.EqualFold(header[:7], "Bearer ") {
		return strings.TrimSpace(header[7:])
	}
	for _, name := range []string{"X-Api-Key", "X-Admin-Token"} {
		if value := strings.TrimSpace(r.Header.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

// SecureEqual 是常量时间字符串比较。
func SecureEqual(a, b string) bool {
	if a == "" || b == "" || len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// SetCORS 写出跨域响应头。
func SetCORS(w http.ResponseWriter, allowOrigin string) {
	if allowOrigin == "" {
		allowOrigin = "*"
	}
	header := w.Header()
	header.Set("Access-Control-Allow-Origin", allowOrigin)
	header.Set("Access-Control-Allow-Headers", "authorization, content-type, x-api-key, x-admin-token, x-csrf-token")
	header.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, PUT, DELETE, OPTIONS")
	header.Set("Access-Control-Max-Age", "600")
}
