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
	"laskah/internal/store"
)

func main() {
	// 子命令在启动 HTTP 服务之前处理：它们只需要数据文件，不需要监听端口。
	if len(os.Args) > 1 {
		runSubcommand(os.Args[1], os.Args[2:])
		return
	}

	options := server.Options{
		DataFile:         envString("DATA_FILE", defaultDataFile()),
		Strategy:         envString("STRATEGY", ""),
		MaxRetries:       envInt("MAX_RETRIES", 0),
		Cooldown:         time.Duration(envInt("COOLDOWN_MS", 30000)) * time.Millisecond,
		FailureThreshold: envInt("FAILURE_THRESHOLD", 3),
		BalanceInterval:  time.Duration(envInt("BALANCE_INTERVAL_MS", 60000)) * time.Millisecond,
		AllowOrigin:      envString("ALLOW_ORIGIN", ""),
		TrustProxy:       envBool("TRUST_PROXY", false),
		PublicModels:     envBoolPtr("PUBLIC_MODELS"),
		// 请求路径上最多等这么久的余额刷新，超时后先放行、查询继续在后台跑完。
		RequestRefreshWait: time.Duration(envInt("REQUEST_REFRESH_WAIT_MS", 5000)) * time.Millisecond,
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

// runSubcommand 分派命令行子命令，未知子命令直接打印用法后退出。
func runSubcommand(name string, args []string) {
	switch name {
	case "reset-password":
		resetPassword(args)
	case "list-admins":
		listAdmins()
	case "-h", "--help", "help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: %s\n\n", name)
		printUsage()
		os.Exit(2)
	}
}

func printUsage() {
	fmt.Printf(`
  Laskah API 负载均衡网关

  用法:
    laskah                                  启动网关服务
    laskah list-admins                      列出管理员账户（账户名脱敏）
    laskah reset-password <账户名> <新口令>   重置指定管理员的口令并启用该账户

  说明:
    子命令读写与服务相同的数据文件（DATA_FILE，默认 %s）。
    若服务启动时设置了 MASTER_KEY，执行子命令也必须带上同一个 MASTER_KEY。
    重置口令前请先停止服务，避免运行中的进程把内存里的旧数据覆盖回磁盘。

`, defaultDataFile())
}

// openStore 打开与服务共用的数据文件。
func openStore() *store.Store {
	dataStore := store.New(envString("DATA_FILE", defaultDataFile()))
	if err := dataStore.Load(); err != nil {
		fmt.Fprintf(os.Stderr, "打开数据文件失败: %v\n", err)
		os.Exit(1)
	}
	return dataStore
}

// resetPassword 是忘记口令时的自救入口。
//
// 只能在服务器本机执行（需要数据文件与主密钥），因此不构成远程可用的后门。
func resetPassword(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "用法: laskah reset-password <账户名> <新口令>")
		os.Exit(2)
	}
	dataStore := openStore()
	user, err := dataStore.ResetAdminPasswordByName(args[0], strings.Join(args[1:], " "))
	if err != nil {
		fmt.Fprintf(os.Stderr, "重置失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("已重置 %s（角色 %s）的口令，请重启服务后用新口令登录。\n", store.MaskUsername(user.Username), user.Role)
}

// listAdmins 只输出脱敏账户名，用于确认要重置哪个账户。
func listAdmins() {
	dataStore := openStore()
	users := dataStore.AdminUsers()
	if len(users) == 0 {
		fmt.Println("尚未创建任何管理员，请访问 /setup 初始化。")
		return
	}
	fmt.Printf("共 %d 个账户:\n", len(users))
	for _, user := range users {
		state := "启用"
		if !user.Enabled {
			state = "禁用"
		}
		fmt.Printf("  %-14s 角色=%-5s 状态=%s 创建于 %s\n",
			store.MaskUsername(user.Username), user.Role, state, user.CreatedAt.Format(time.RFC3339))
	}
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

// envBoolPtr 读取三态布尔环境变量：未设置返回 nil，交由下游用默认值。
//
// 用于 PUBLIC_MODELS 这类「默认开启、只有显式关掉才生效」的开关。
func envBoolPtr(name string) *bool {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	if raw == "" {
		return nil
	}
	value := raw == "1" || raw == "true" || raw == "yes" || raw == "on"
	return &value
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
