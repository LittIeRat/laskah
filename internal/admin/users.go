package admin

import (
	"net/http"
	"strings"

	"laskah/internal/httpx"
	"laskah/internal/store"
)

// handleUserCollection 管理管理员账户：GET 列表，POST 新建。
//
// 只有超级管理员能触达（路由已包 super），因此列表里可以显示明文账户名，
// 但落盘依然是密文 + 摘要，泄露数据文件也读不出账户名。
func (h *Handler) handleUserCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list := []any{}
		for _, user := range h.Store.AdminUsers() {
			list = append(list, store.PublicAdminUser(user, true))
		}
		httpx.JSON(w, http.StatusOK, map[string]any{"data": list, "max": store.MaxAdminUsers})
	case http.MethodPost:
		payload := struct {
			User     string `json:"user"`
			Password string `json:"password"`
			Role     string `json:"role"`
			Note     string `json:"note"`
		}{}
		if err := httpx.DecodeJSON(r, &payload); err != nil {
			httpx.Error(w, httpx.StatusOf(err, http.StatusBadRequest), err.Error(), nil)
			return
		}
		role := store.Role(strings.TrimSpace(payload.Role))
		if role == "" {
			role = store.RoleAdmin
		}
		user, err := h.Store.CreateAdminUser(payload.User, payload.Password, role, payload.Note)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, err.Error(), nil)
			return
		}
		httpx.JSON(w, http.StatusCreated, map[string]any{"data": store.PublicAdminUser(user, true)})
	default:
		httpx.Error(w, http.StatusMethodNotAllowed, "不支持的方法", nil)
	}
}

// handleUserItem 处理 /admin/users/{id}[/password|/enable]。
func (h *Handler) handleUserItem(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/users/"), "/")
	if rest == "" {
		h.handleUserCollection(w, r)
		return
	}
	segments := strings.Split(rest, "/")
	id := segments[0]
	action := ""
	if len(segments) > 1 {
		action = segments[1]
	}

	switch {
	case action == "password" && r.Method == http.MethodPost:
		h.resetUserPassword(w, r, id)
	case action == "enable" && (r.Method == http.MethodPost || r.Method == http.MethodPatch):
		h.setUserEnabled(w, r, id)
	case action == "" && r.Method == http.MethodDelete:
		h.deleteUser(w, id)
	default:
		httpx.Error(w, http.StatusMethodNotAllowed, "账户只支持改密、启停与删除", nil)
	}
}

// resetUserPassword 由超级管理员重置任意账户口令，并踢掉该账户的现有会话。
func (h *Handler) resetUserPassword(w http.ResponseWriter, r *http.Request, id string) {
	payload := struct {
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
	if err := h.Store.SetAdminPassword(id, strings.TrimSpace(payload.Password)); err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error(), nil)
		return
	}
	h.Sessions.RevokeUser(id)
	httpx.JSON(w, http.StatusOK, map[string]any{"ok": true, "message": "口令已重置，该账户需重新登录"})
}

// setUserEnabled 启用或禁用账户；禁用后立即注销其会话。
func (h *Handler) setUserEnabled(w http.ResponseWriter, r *http.Request, id string) {
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
	if err := h.Store.SetAdminEnabled(id, enabled); err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error(), nil)
		return
	}
	if !enabled {
		h.Sessions.RevokeUser(id)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": enabled})
}

func (h *Handler) deleteUser(w http.ResponseWriter, id string) {
	if err := h.Store.RemoveAdminUser(id); err != nil {
		httpx.Error(w, http.StatusBadRequest, err.Error(), nil)
		return
	}
	h.Sessions.RevokeUser(id)
	httpx.JSON(w, http.StatusOK, map[string]any{"ok": true})
}
