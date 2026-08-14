package admin

import (
	"net/http"
	"strings"
	"time"

	"laskah/internal/httpx"
	"laskah/internal/store"
)

func (h *Handler) handleKeyCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		groupFilter := strings.TrimSpace(r.URL.Query().Get("groupId"))
		list := []any{}
		totals := map[string]any{}
		h.Store.View(func(data *store.Data) {
			for _, key := range data.Keys {
				if groupFilter != "" && key.GroupID != groupFilter {
					continue
				}
				list = append(list, store.PublicKey(key, false))
			}
			totals = data.AccountTotals()
		})
		httpx.JSON(w, http.StatusOK, map[string]any{"data": list, "totals": totals})
	case http.MethodPost:
		input := store.KeyInput{}
		if err := httpx.DecodeJSON(r, &input); err != nil {
			httpx.Error(w, httpx.StatusOf(err, http.StatusBadRequest), err.Error(), nil)
			return
		}
		key, verr := store.BuildKey(input)
		if verr != nil {
			httpx.Error(w, http.StatusBadRequest, verr.Error(), nil)
			return
		}
		if key.GroupID != "" {
			missing := false
			h.Store.View(func(data *store.Data) {
				missing = data.FindGroup(key.GroupID) == nil
			})
			if missing {
				httpx.Error(w, http.StatusBadRequest, "指定的分组不存在", nil)
				return
			}
		}
		if err := h.Store.Update(func(data *store.Data) error {
			data.Keys = append(data.Keys, key)
			data.AssignAccount(key)
			return nil
		}); err != nil {
			httpx.Error(w, http.StatusInternalServerError, err.Error(), nil)
			return
		}
		httpx.JSON(w, http.StatusCreated, map[string]any{"data": store.PublicKey(key, true)})
	default:
		httpx.Error(w, http.StatusMethodNotAllowed, "不支持的方法", nil)
	}
}

func (h *Handler) handleKeyItem(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/keys/"), "/")
	if rest == "" {
		h.handleKeyCollection(w, r)
		return
	}
	segments := strings.Split(rest, "/")
	resource := segments[0]
	action := ""
	if len(segments) > 1 {
		action = segments[1]
	}

	switch {
	case resource == "bulk" && r.Method == http.MethodPost:
		h.handleKeyBulk(w, r)
	case resource == "batch" && r.Method == http.MethodDelete:
		h.handleKeyBatchDelete(w, r)
	case action == "reveal" && r.Method == http.MethodGet:
		h.handleKeyReveal(w, resource)
	case action == "reset-usage" && r.Method == http.MethodPost:
		h.handleKeyResetUsage(w, resource)
	case action == "" && (r.Method == http.MethodPatch || r.Method == http.MethodPut):
		h.handleKeyPatch(w, r, resource)
	case action == "" && r.Method == http.MethodDelete:
		h.handleKeyDelete(w, resource)
	default:
		httpx.Error(w, http.StatusNotFound, "未知的管理接口: "+r.URL.Path, nil)
	}
}

func (h *Handler) handleKeyBulk(w http.ResponseWriter, r *http.Request) {
	payload := struct {
		Count    int            `json:"count"`
		Template store.KeyInput `json:"template"`
	}{}
	if err := httpx.DecodeJSON(r, &payload); err != nil {
		httpx.Error(w, httpx.StatusOf(err, http.StatusBadRequest), err.Error(), nil)
		return
	}

	keys, verr := store.BuildKeyBatch(payload.Count, payload.Template)
	if verr != nil {
		httpx.Error(w, http.StatusBadRequest, verr.Error(), nil)
		return
	}
	if groupID := strings.TrimSpace(payload.Template.GroupID); groupID != "" {
		missing := false
		h.Store.View(func(data *store.Data) {
			missing = data.FindGroup(groupID) == nil
		})
		if missing {
			httpx.Error(w, http.StatusBadRequest, "指定的分组不存在", nil)
			return
		}
	}
	if err := h.Store.Update(func(data *store.Data) error {
		data.Keys = append(data.Keys, keys...)
		data.Reindex()
		for _, key := range keys {
			data.AssignAccount(key)
		}
		return nil
	}); err != nil {
		httpx.Error(w, http.StatusInternalServerError, err.Error(), nil)
		return
	}

	list := make([]any, 0, len(keys))
	for _, key := range keys {
		list = append(list, store.PublicKey(key, true))
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{"data": list})
}

func (h *Handler) handleKeyReveal(w http.ResponseWriter, id string) {
	var view map[string]any
	h.Store.View(func(data *store.Data) {
		if key := data.FindKeyByID(id); key != nil {
			view = store.PublicKey(key, true)
		}
	})
	if view == nil {
		httpx.Error(w, http.StatusNotFound, "Key 不存在", nil)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"data": view})
}

func (h *Handler) handleKeyResetUsage(w http.ResponseWriter, id string) {
	var view map[string]any
	if err := h.Store.Update(func(data *store.Data) error {
		key := data.FindKeyByID(id)
		if key == nil {
			return nil
		}
		key.Stats = store.KeyStats{}
		key.UpdatedAt = time.Now().UTC()
		view = store.PublicKey(key, false)
		return nil
	}); err != nil {
		httpx.Error(w, http.StatusInternalServerError, err.Error(), nil)
		return
	}
	if view == nil {
		httpx.Error(w, http.StatusNotFound, "Key 不存在", nil)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"data": view})
}

func (h *Handler) handleKeyPatch(w http.ResponseWriter, r *http.Request, id string) {
	patch := store.KeyInput{}
	if err := httpx.DecodeJSON(r, &patch); err != nil {
		httpx.Error(w, httpx.StatusOf(err, http.StatusBadRequest), err.Error(), nil)
		return
	}

	var (
		view     map[string]any
		notFound bool
		verr     *store.ValidationError
	)
	if err := h.Store.Update(func(data *store.Data) error {
		existing := data.FindKeyByID(id)
		if existing == nil {
			notFound = true
			return nil
		}
		merged := mergeKeyInput(existing, patch)
		built, buildErr := store.BuildKey(merged)
		if buildErr != nil {
			verr = buildErr
			return nil
		}
		existing.Name = built.Name
		existing.AccountID = built.AccountID
		existing.GroupID = built.GroupID
		existing.Enabled = built.Enabled
		existing.AllowedModels = built.AllowedModels
		existing.ProviderIDs = built.ProviderIDs
		existing.Tags = built.Tags
		existing.QuotaTokens = built.QuotaTokens
		existing.RateLimitPerMin = built.RateLimitPerMin
		existing.ExpiresAt = built.ExpiresAt
		existing.Note = built.Note
		existing.UpdatedAt = time.Now().UTC()
		view = store.PublicKey(existing, false)
		return nil
	}); err != nil {
		httpx.Error(w, http.StatusInternalServerError, err.Error(), nil)
		return
	}
	switch {
	case notFound:
		httpx.Error(w, http.StatusNotFound, "Key 不存在", nil)
	case verr != nil:
		httpx.Error(w, http.StatusBadRequest, verr.Error(), nil)
	default:
		httpx.JSON(w, http.StatusOK, map[string]any{"data": view})
	}
}

// mergeKeyInput 把补丁合并到现有密钥，未提供的字段保持不变。
func mergeKeyInput(existing *store.APIKey, patch store.KeyInput) store.KeyInput {
	merged := store.KeyInput{
		ID:            existing.ID,
		AccountID:     existing.AccountID,
		GroupID:       existing.GroupID,
		Name:          existing.Name,
		AllowedModels: existing.AllowedModels,
		ProviderIDs:   existing.ProviderIDs,
		Tags:          existing.Tags,
		Note:          existing.Note,
	}
	if existing.QuotaTokens != nil {
		merged.QuotaTokens = *existing.QuotaTokens
	}
	if existing.RateLimitPerMin != nil {
		merged.RateLimitPerMin = *existing.RateLimitPerMin
	}
	if existing.ExpiresAt != nil {
		merged.ExpiresAt = existing.ExpiresAt.Format(time.RFC3339)
	}

	if strings.TrimSpace(patch.Name) != "" {
		merged.Name = patch.Name
	}
	if patch.AccountID != "" {
		merged.AccountID = patch.AccountID
	}
	if patch.GroupID != "" {
		merged.GroupID = patch.GroupID
	}
	if patch.AllowedModels != nil {
		merged.AllowedModels = patch.AllowedModels
	}
	if patch.ProviderIDs != nil {
		merged.ProviderIDs = patch.ProviderIDs
	}
	if patch.Tags != nil {
		merged.Tags = patch.Tags
	}
	if patch.QuotaTokens != nil {
		merged.QuotaTokens = patch.QuotaTokens
	}
	if patch.RateLimitPerMin != nil {
		merged.RateLimitPerMin = patch.RateLimitPerMin
	}
	if patch.ExpiresAt != "" {
		merged.ExpiresAt = patch.ExpiresAt
	}
	if strings.TrimSpace(patch.Note) != "" {
		merged.Note = patch.Note
	}
	if patch.Enabled != nil {
		merged.Enabled = patch.Enabled
	} else {
		enabled := existing.Enabled
		merged.Enabled = &enabled
	}
	return merged
}

func (h *Handler) handleKeyDelete(w http.ResponseWriter, id string) {
	removed := 0
	if err := h.Store.Update(func(data *store.Data) error {
		removed = data.RemoveKeys([]string{id})
		return nil
	}); err != nil {
		httpx.Error(w, http.StatusInternalServerError, err.Error(), nil)
		return
	}
	if removed == 0 {
		httpx.Error(w, http.StatusNotFound, "Key 不存在", nil)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) handleKeyBatchDelete(w http.ResponseWriter, r *http.Request) {
	payload := struct {
		IDs []string `json:"ids"`
	}{}
	if err := httpx.DecodeJSON(r, &payload); err != nil {
		httpx.Error(w, httpx.StatusOf(err, http.StatusBadRequest), err.Error(), nil)
		return
	}
	removed := 0
	if err := h.Store.Update(func(data *store.Data) error {
		removed = data.RemoveKeys(payload.IDs)
		return nil
	}); err != nil {
		httpx.Error(w, http.StatusInternalServerError, err.Error(), nil)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"ok": true, "removed": removed})
}
