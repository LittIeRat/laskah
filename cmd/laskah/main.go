// Command laskah 启动 API 负载均衡网关。
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"laskah/internal/server"
)

func main() {
	options := server.Options{
		DataFile:         envString("DATA_FILE", defaultDataFile()),
		Strategy:         envString("STRATEGY", ""),
		MaxRetries:       envInt("MAX_RETRIES", 0),
		Cooldown:         time.Duration(envInt("COOLDOWN_MS", 30000)) * time.Millisecond,
		FailureThreshold: envInt("FAILURE_THRESHOLD", 3),
		BalanceInterval:  time.Duration(envInt("BALANCE_INTERVAL_MS", 60000)) * time.Millisecond,
		AllowOrigin:      envString("ALLOW_ORIGIN", ""),
		TrustProxy:       envBool("TRUST_PROXY", false),
	}

	app, err := server.New(options)
	if err != nil {
		log.Fatalf("初始化失败: %v", err)
	}
	defer app.Close()

	host := envString("HOST", "0.0.0.0")
	port := envInt("PORT", 8787)

	httpServer := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", host, port),
		Handler:           app.Handler,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	printBanner(app, host, port)

	shutdownCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-shutdownCtx.Done()
		graceCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(graceCtx); err != nil {
			log.Printf("关闭服务出错: %v", err)
		}
	}()

	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("监听失败: %v", err)
	}
	fmt.Println("服务已停止")
}

// printBanner 输出访问入口与关键路径。
//
// 不打印任何账户名、口令或管理令牌：超级管理员账户名本身也是敏感信息，
// 只在初始化页面由管理员自己保存，避免凭据落进终端记录或日志采集。
func printBanner(app *server.App, host string, port int) {
	display := host
	if display == "0.0.0.0" || display == "" {
		display = "127.0.0.1"
	}
	base := fmt.Sprintf("http://%s:%d", display, port)

	masterKey := app.Store.KeyFile()
	if strings.TrimSpace(os.Getenv("MASTER_KEY")) != "" {
		masterKey = "来自 MASTER_KEY 环境变量（未落盘）"
	}

	status := "已初始化"
	entry := base + "/login"
	if app.Store.NeedsSetup() {
		status = "等待创建超级管理员"
		entry = base + "/setup"
	}

	fmt.Printf(`
  Laskah API 负载均衡网关已启动
  入口:          %s
  数据看板:      %s/dashboard
  分组与账号:    %s/manage
  OpenAI 兼容:   %s/v1/chat/completions
  初始化状态:    %s
  数据文件:      %s
  主密钥:        %s
  提示: 首次访问入口页创建超级管理员并妥善保存凭据；生产环境建议设置 MASTER_KEY 让主密钥不落盘。

`, entry, base, base, base, status, app.Store.File(), masterKey)
}

func defaultDataFile() string {
	executable, err := os.Executable()
	if err != nil {
		return filepath.Join("data", "db.json")
	}
	return filepath.Join(filepath.Dir(executable), "data", "db.json")
}

func envString(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return parsed
}

func envBool(name string, fallback bool) bool {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	switch raw {
	case "":
		return fallback
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
