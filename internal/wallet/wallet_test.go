package wallet

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFetchFromUserSelf(t *testing.T) {
	var gotAuth, gotUser, gotContentType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/status":
			_, _ = fmt.Fprint(w, `{"data":{"quota_per_unit":500000}}`)
		case "/api/user/self":
			gotAuth = r.Header.Get("Authorization")
			gotUser = r.Header.Get("New-Api-User")
			gotContentType = r.Header.Get("Content-Type")
			_, _ = fmt.Fprint(w, `{"success":true,"data":{"group":"vip","quota":5000000,"used_quota":2500000}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	snapshot := NewClient().Fetch(context.Background(), Credentials{
		BaseURL:     server.URL,
		UserID:      "114514",
		AccessToken: "tok-abc",
		Timeout:     5 * time.Second,
	})
	if snapshot.Err != nil {
		t.Fatalf("查询应成功: %v", snapshot.Err)
	}
	if snapshot.Balance != 10 || snapshot.UsedAmount != 5 || snapshot.TotalAmount != 15 {
		t.Fatalf("额度换算错误: %#v", snapshot)
	}
	if snapshot.PlanName != "vip" || snapshot.Currency != "USD" || snapshot.Source != "/api/user/self" {
		t.Fatalf("快照字段错误: %#v", snapshot)
	}
	if gotAuth != "Bearer tok-abc" || gotUser != "114514" || gotContentType != "application/json" {
		t.Fatalf("请求头错误: %q %q %q", gotAuth, gotUser, gotContentType)
	}
}

func TestFetchUsesCustomQuotaPerUnit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/status":
			_, _ = fmt.Fprint(w, `{"data":{"quota_per_unit":1000}}`)
		case "/api/user/self":
			_, _ = fmt.Fprint(w, `{"success":true,"data":{"quota":2000,"used_quota":1000}}`)
		}
	}))
	defer server.Close()

	snapshot := NewClient().Fetch(context.Background(), Credentials{BaseURL: server.URL, AccessToken: "tok"})
	if snapshot.Err != nil || snapshot.Balance != 2 || snapshot.UsedAmount != 1 {
		t.Fatalf("自定义换算单位未生效: %#v", snapshot)
	}
	if snapshot.QuotaPerUnit != 1000 {
		t.Fatalf("应回传站点换算单位: %#v", snapshot)
	}
	if snapshot.PlanName != "默认套餐" {
		t.Fatalf("缺少分组时应回落默认套餐: %#v", snapshot)
	}
}

func TestFetchFallsBackToUsageEndpoint(t *testing.T) {
	var method, auth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/user/self":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = fmt.Fprint(w, `{"success":false,"message":"invalid token"}`)
		case "/api/usage":
			method = r.Method
			auth = r.Header.Get("Authorization")
			_, _ = fmt.Fprint(w, `{"balance":7.5,"used":2.5}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	snapshot := NewClient().Fetch(context.Background(), Credentials{
		BaseURL:     server.URL,
		AccessToken: "bad-token",
		APIKey:      "sk-upstream",
	})
	if snapshot.Err != nil {
		t.Fatalf("应回落到 /api/usage: %v", snapshot.Err)
	}
	if snapshot.Balance != 7.5 || snapshot.UsedAmount != 2.5 || snapshot.Source != "/api/usage" {
		t.Fatalf("回落结果错误: %#v", snapshot)
	}
	if method != http.MethodPost || auth != "Bearer sk-upstream" {
		t.Fatalf("/api/usage 请求方式错误: %s %s", method, auth)
	}
}

func TestFetchWithoutCredentials(t *testing.T) {
	snapshot := NewClient().Fetch(context.Background(), Credentials{BaseURL: "https://api.newapi.com"})
	if snapshot.Err == nil {
		t.Fatalf("缺少凭据应报错")
	}
	if !strings.Contains(snapshot.Err.Error(), "凭据") {
		t.Fatalf("错误信息应提示凭据缺失: %v", snapshot.Err)
	}

	empty := NewClient().Fetch(context.Background(), Credentials{})
	if empty.Err == nil || !strings.Contains(empty.Err.Error(), "请求地址") {
		t.Fatalf("缺少请求地址应报错: %v", empty.Err)
	}
}

func TestFetchReportsUpstreamFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/user/self" {
			_, _ = fmt.Fprint(w, `{"success":false,"message":"额度不足"}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	snapshot := NewClient().Fetch(context.Background(), Credentials{BaseURL: server.URL, AccessToken: "tok"})
	if snapshot.Err == nil || !strings.Contains(snapshot.Err.Error(), "额度不足") {
		t.Fatalf("应透出上游错误信息: %v", snapshot.Err)
	}
	if snapshot.CheckedAt.IsZero() {
		t.Fatalf("失败也应记录检查时间")
	}
}

func TestFetchUsageErrorField(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/usage" {
			_, _ = fmt.Fprint(w, `{"error":{"message":"key disabled"}}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	snapshot := NewClient().Fetch(context.Background(), Credentials{BaseURL: server.URL, APIKey: "sk-x"})
	if snapshot.Err == nil || !strings.Contains(snapshot.Err.Error(), "key disabled") {
		t.Fatalf("应识别 error 字段: %v", snapshot.Err)
	}
}
