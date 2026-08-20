package accounts

import (
	"path/filepath"
	"testing"
	"time"

	"laskah/internal/store"
	"laskah/internal/wallet"
)

// TestRefreshClearsFalsePositiveUpstreamSuspension 校验：
// 账号曾因上游一句“余额不足”被请求路径暂停，但真实余额刷新成功且余额仍高于下限时，
// 管理器会自动恢复该账号，避免管理员必须手工点启用。
func TestRefreshClearsFalsePositiveUpstreamSuspension(t *testing.T) {
	t.Setenv("MASTER_KEY", "unit-test-master-key")
	st := store.New(filepath.Join(t.TempDir(), "db.json"))
	if err := st.Load(); err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	mgr := New(st, wallet.NewClient())

	checked := time.Date(2026, 8, 20, 7, 0, 0, 0, time.UTC)
	if err := st.Update(func(data *store.Data) error {
		data.Accounts = []*store.Account{{
			ID:            "acct_1",
			Name:          "claude-oner",
			Enabled:       true,
			AutoSuspend:   true,
			Balance:       581.483696,
			Currency:      "USD",
			Suspended:     true,
			SuspendReason: "上游报余额不足自动暂停: 余额不足",
		}}
		data.Providers = []*store.Provider{{
			ID:        "prov_1",
			Name:      "prov-1",
			APIKey:    "sk-test",
			BaseURL:   "https://example.com",
			AccountID: "acct_1",
		}}
		return nil
	}); err != nil {
		t.Fatalf("写入测试数据失败: %v", err)
	}

	result := mgr.apply("acct_1", "claude-oner", wallet.Snapshot{
		Balance:     581.483696,
		UsedAmount:  10,
		TotalAmount: 591.483696,
		PlanName:    "test",
		Source:      "/api/user/self",
		CheckedAt:   checked,
	})

	if suspended, _ := result["suspended"].(bool); suspended {
		t.Fatalf("真实余额高于下限时应自动恢复: %#v", result)
	}
	st.View(func(data *store.Data) {
		account := data.FindAccount("acct_1")
		if account == nil {
			t.Fatal("账号不存在")
		}
		if account.Suspended {
			t.Fatalf("账号仍处于暂停状态: %#v", account)
		}
		if account.SuspendReason != "" {
			t.Fatalf("恢复后应清空暂停原因: %q", account.SuspendReason)
		}
	})
}
