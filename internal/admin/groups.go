package admin

import (
	"context"
	"net/http"
	"strings"
	"time"

	"laskah/internal/httpx"
	"laskah/internal/store"
)

// maxGroups 限制分组数量，避免界面与看板被无限膨胀的数据拖慢。
const maxGroups = 200

// trimAction 把 "{id}/refresh" 拆成 id 与动作命中标记。
func trimAction(path, action string) (string, bool) {
	suffix := "/" + action
	if !strings.HasSuffix(path, suffix) {
		return "", false
	}
	id := strings.TrimSuffix(path, suffix)
	if id == "" || strings.Contains(id, "/") {
		return "", false
	}
	return id, true
}

// handleGroupRefresh 手动刷新分组内全部账号的余额。
func (h *Handler) handleGroupRefresh(w http.ResponseWriter, r *http.Request, groupID string) {
	if h.Accounts == nil {
		httpx.Error(w, http.StatusServiceUnavailable, "额度查询组件未就绪", nil)
		return
	}

	var (
		missing bool
		ids     []string
	)
	h.Store.View(func(data *store.Data) {
		if data.FindGroup(groupID) == nil {
			missing = true
			return
		}
		for _, account := range data.GroupAccounts(groupID) {
			ids = append(ids, account.ID)
		}
	})
	if missing {
		httpx.Error(w, http.StatusNotFound, "分组不存在", nil)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), manualRefreshBudget)
	defer cancel()
	results := h.Accounts.RefreshIDs(ctx, ids)
	summary := map[string]any{}
	h.Store.View(func(data *store.Data) {
		summary = store.PublicGroup(orEmptyGroup(data.FindGroup(groupID)), data.GroupSummary(groupID))
	})
	httpx.JSON(w, http.StatusOK, map[string]any{"data": results, "group": summary})
}

// orEmptyGroup 避免分组在刷新过程中被级联删除时出现空指针。
func orEmptyGroup(group *store.Group) *store.Group {
	if group != nil {
		return group
	}
	return &store.Group{}
}

// handleGroupEnable 启用或禁用分组。
//
// 禁用后该分组内的账号立即退出分配池（UsableAccounts 会过滤掉），
// 但账号与余额数据保留，重新启用即可恢复承接流量。
func (h *Handler) handleGroupEnable(w http.ResponseWriter, r *http.Request, groupID string) {
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
		group := data.FindGroup(groupID)
		if group == nil {
			missing = true
			return nil
		}
		group.Enabled = enabled
		group.UpdatedAt = time.Now().UTC()
		return nil
	}); err != nil {
		httpx.Error(w, http.StatusInternalServerError, err.Error(), nil)
		return
	}
	if missing {
		httpx.Error(w, http.StatusNotFound, "分组不存在", nil)
		return
	}

	view := map[string]any{}
	h.Store.View(func(data *store.Data) {
		view = store.PublicGroup(orEmptyGroup(data.FindGroup(groupID)), data.GroupSummary(groupID))
	})
	httpx.JSON(w, http.StatusOK, map[string]any{"data": view})
}

func (h *Handler) handleGroupCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list := []any{}
		h.Store.View(func(data *store.Data) {
			for _, group := range data.Groups {
				list = append(list, store.PublicGroup(group, data.GroupSummary(group.ID)))
			}
		})
		httpx.JSON(w, http.StatusOK, map[string]any{"data": list})
	case http.MethodPost:
		input := store.GroupInput{}
		if err := httpx.DecodeJSON(r, &input); err != nil {
			httpx.Error(w, httpx.StatusOf(err, http.StatusBadRequest), err.Error(), nil)
			return
		}
		group, verr := store.BuildGroup(input)
		if verr != nil {
			httpx.Error(w, http.StatusBadRequest, verr.Error(), nil)
			return
		}

		var (
			duplicate bool
			overflow  bool
		)
		if err := h.Store.Update(func(data *store.Data) error {
			if data.FindGroupByName(group.Name) != nil {
				duplicate = true
				return nil
			}
			if len(data.Groups) >= maxGroups {
				overflow = true
				return nil
			}
			data.Groups = append(data.Groups, group)
			return nil
		}); err != nil {
			httpx.Error(w, http.StatusInternalServerError, err.Error(), nil)
			return
		}
		switch {
		case duplicate:
			httpx.Error(w, http.StatusConflict, "分组名称已存在: "+group.Name, nil)
		case overflow:
			httpx.Error(w, http.StatusBadRequest, "分组数量已达上限", nil)
		default:
			view := map[string]any{}
			h.Store.View(func(data *store.Data) {
				view = store.PublicGroup(group, data.GroupSummary(group.ID))
			})
			httpx.JSON(w, http.StatusCreated, map[string]any{"data": view})
		}
	default:
		httpx.Error(w, http.StatusMethodNotAllowed, "不支持的方法", nil)
	}
}

func (h *Handler) handleGroupItem(w http.ResponseWriter, r *http.Request) {
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/groups/"), "/")
	if id == "" {
		h.handleGroupCollection(w, r)
		return
	}
	if action, ok := trimAction(id, "refresh"); ok {
		if r.Method != http.MethodPost {
			httpx.Error(w, http.StatusMethodNotAllowed, "仅支持 POST", nil)
			return
		}
		h.handleGroupRefresh(w, r, action)
		return
	}
	if action, ok := trimAction(id, "enable"); ok {
		if r.Method != http.MethodPost && r.Method != http.MethodPatch {
			httpx.Error(w, http.StatusMethodNotAllowed, "仅支持 POST/PATCH", nil)
			return
		}
		h.handleGroupEnable(w, r, action)
		return
	}
	if strings.Contains(id, "/") {
		httpx.Error(w, http.StatusNotFound, "未知的管理接口: "+r.URL.Path, nil)
		return
	}

	switch r.Method {
	case http.MethodGet:
		var view map[string]any
		h.Store.View(func(data *store.Data) {
			group := data.FindGroup(id)
			if group == nil {
				return
			}
			view = store.PublicGroup(group, data.GroupSummary(group.ID))
			accountViews := []any{}
			for _, account := range data.GroupAccounts(group.ID) {
				accountViews = append(accountViews, store.PublicAccount(account, data.CountAccountKeys(account.ID), data.KeysUsingAccount(account.ID)))
			}
			view["accountList"] = accountViews
		})
		if view == nil {
			httpx.Error(w, http.StatusNotFound, "分组不存在", nil)
			return
		}
		httpx.JSON(w, http.StatusOK, map[string]any{"data": view})
	case http.MethodDelete:
		removed := 0
		if err := h.Store.Update(func(data *store.Data) error {
			removed = data.RemoveGroups([]string{id})
			return nil
		}); err != nil {
			httpx.Error(w, http.StatusInternalServerError, err.Error(), nil)
			return
		}
		if removed == 0 {
			httpx.Error(w, http.StatusNotFound, "分组不存在", nil)
			return
		}
		httpx.JSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		httpx.Error(w, http.StatusMethodNotAllowed, "分组只支持查看与删除", nil)
	}
}
