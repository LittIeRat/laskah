package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildProviderRequiresBaseURL(t *testing.T) {
	_, verr := BuildProvider(ProviderInput{Name: "no-url"})
	if verr == nil {
		t.Fatalf("缺少 baseUrl 时应当报错")
	}
	if !strings.Contains(verr.Error(), "baseUrl") {
		t.Fatalf("错误信息应提到 baseUrl, got %q", verr.Error())
	}
}

func TestBuildProviderNormalizesInput(t *testing.T) {
	provider, verr := BuildProvider(ProviderInput{
		BaseURLSnake: "api.example.com/v1/",
		APIKeySnake:  "  sk-upstream  ",
		ModelField:   "gpt-4o-mini, deepseek-chat",
		Tags:         "prod;cn",
	})
	if verr != nil {
		t.Fatalf("不应报错: %v", verr)
	}
	if provider.BaseURL != "https://api.example.com/v1" {
		t.Fatalf("baseUrl 归一化失败: %s", provider.BaseURL)
	}
	if provider.APIKey != "sk-upstream" {
		t.Fatalf("apiKey 未去空白: %q", provider.APIKey)
	}
	if provider.Name != "api.example.com" {
		t.Fatalf("默认名称应取主机名, got %s", provider.Name)
	}
	if provider.Type != TypeOpenAI {
		t.Fatalf("默认协议应为 openai, got %s", provider.Type)
	}
	if len(provider.Models) != 2 || provider.Models[0] != "gpt-4o-mini" {
		t.Fatalf("模型解析失败: %#v", provider.Models)
	}
	if provider.Paths.Chat != "/chat/completions" {
		t.Fatalf("默认 chat 路径应为 /chat/completions: %#v", provider.Paths)
	}
	if !provider.Enabled || provider.Weight != 1 || provider.TimeoutMS != 120000 {
		t.Fatalf("默认值错误: %#v", provider)
	}
}

func TestBuildProviderValidation(t *testing.T) {
	if _, verr := BuildProvider(ProviderInput{BaseURL: "https://a.com", Type: "cohere"}); verr == nil {
		t.Fatalf("非法协议类型应报错")
	}
	if _, verr := BuildProvider(ProviderInput{BaseURL: "https://a.com", Weight: "abc"}); verr == nil {
		t.Fatalf("非法 weight 应报错")
	}
	if _, verr := BuildProvider(ProviderInput{BaseURL: "https://a.com", TimeoutMS: 10}); verr == nil {
		t.Fatalf("过小的 timeoutMs 应报错")
	}
}

func TestSupportsModelWildcardAndAlias(t *testing.T) {
	provider := &Provider{
		Models:   []string{"gpt-4*", "deepseek-chat"},
		ModelMap: map[string]string{"fast": "gpt-4o-mini"},
	}
	cases := map[string]bool{
		"gpt-4o":        true,
		"gpt-4o-mini":   true,
		"deepseek-chat": true,
		"fast":          true,
		"claude-3":      false,
	}
	for model, want := range cases {
		if got := provider.SupportsModel(model); got != want {
			t.Fatalf("SupportsModel(%q)=%v, want %v", model, got, want)
		}
	}
	if provider.UpstreamModel("fast") != "gpt-4o-mini" {
		t.Fatalf("别名映射失败")
	}
	if (&Provider{}).SupportsModel("anything") != true {
		t.Fatalf("未配置模型时应接受任意模型")
	}
}

func TestBuildKeyValidation(t *testing.T) {
	if _, verr := BuildKey(KeyInput{QuotaTokens: "abc"}); verr == nil {
		t.Fatalf("非法 quotaTokens 应报错")
	}
	if _, verr := BuildKey(KeyInput{RateLimitPerMin: 0}); verr == nil {
		t.Fatalf("非正 rateLimitPerMin 应报错")
	}
	if _, verr := BuildKey(KeyInput{ExpiresAt: "not-a-time"}); verr == nil {
		t.Fatalf("非法过期时间应报错")
	}

	key, verr := BuildKey(KeyInput{Name: " team ", Prefix: "sk-team", QuotaTokens: 1000, RateLimitPerMin: 30, ExpiresAt: "2030-01-02"})
	if verr != nil {
		t.Fatalf("不应报错: %v", verr)
	}
	if key.Name != "team" || !strings.HasPrefix(key.Key, "sk-team-") {
		t.Fatalf("密钥字段错误: %#v", key)
	}
	if key.KeyHash == "" || key.KeyMasked == "" || strings.Contains(key.KeyMasked, key.Key) {
		t.Fatalf("应生成摘要与掩码: %q %q", key.KeyHash, key.KeyMasked)
	}
	if *key.QuotaTokens != 1000 || *key.RateLimitPerMin != 30 || key.ExpiresAt == nil {
		t.Fatalf("可选字段解析失败: %#v", key)
	}
}

func TestBuildKeyBatch(t *testing.T) {
	if _, verr := BuildKeyBatch(0, KeyInput{}); verr == nil {
		t.Fatalf("count=0 应报错")
	}
	if _, verr := BuildKeyBatch(501, KeyInput{}); verr == nil {
		t.Fatalf("count 超上限应报错")
	}

	keys, verr := BuildKeyBatch(12, KeyInput{Name: "team", Prefix: "sk-lb"})
	if verr != nil {
		t.Fatalf("不应报错: %v", verr)
	}
	if len(keys) != 12 || keys[0].Name != "team-01" || keys[11].Name != "team-12" {
		t.Fatalf("批量命名错误: %s .. %s", keys[0].Name, keys[11].Name)
	}
	seen := map[string]bool{}
	for _, key := range keys {
		if seen[key.Key] {
			t.Fatalf("批量密钥不应重复")
		}
		seen[key.Key] = true
	}

	single, _ := BuildKeyBatch(1, KeyInput{Name: "solo"})
	if single[0].Name != "solo" {
		t.Fatalf("单个创建不应加序号: %s", single[0].Name)
	}
}

func TestKeyStateAndMask(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Hour)
	quota := int64(100)

	if (&APIKey{Enabled: true}).State(now) != KeyActive {
		t.Fatalf("应为 active")
	}
	if (&APIKey{}).State(now) != KeyDisabled {
		t.Fatalf("应为 disabled")
	}
	if (&APIKey{Enabled: true, ExpiresAt: &past}).State(now) != KeyExpired {
		t.Fatalf("应为 expired")
	}
	exceeded := &APIKey{Enabled: true, QuotaTokens: &quota, Stats: KeyStats{TotalTokens: 100}}
	if exceeded.State(now) != KeyQuotaExceeded {
		t.Fatalf("应为 quota_exceeded")
	}

	if MaskKey("") != "" {
		t.Fatalf("空密钥应返回空串")
	}
	masked := MaskKey("sk-lb-abcdefghijklmn")
	if strings.Contains(masked, "abcdefghij") || !strings.Contains(masked, "******") {
		t.Fatalf("掩码失败: %s", masked)
	}
	if MaskKey("short") != "sh******" {
		t.Fatalf("短密钥掩码失败: %s", MaskKey("short"))
	}
}

func TestKeyAllowsModel(t *testing.T) {
	limited := &APIKey{AllowedModels: []string{"gpt-4o-mini"}}
	if !limited.AllowsModel("gpt-4o-mini") || limited.AllowsModel("gpt-4o") {
		t.Fatalf("白名单判定错误")
	}
	if !(&APIKey{}).AllowsModel("anything") {
		t.Fatalf("未配置白名单时应全部允许")
	}
	if !(&APIKey{AllowedModels: []string{"*"}}).AllowsModel("anything") {
		t.Fatalf("* 应允许全部模型")
	}
}

func TestStorePersistenceAndLookup(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", "")
	t.Setenv("MASTER_KEY", "unit-test-master-key")
	file := filepath.Join(t.TempDir(), "nested", "db.json")

	first := New(file)
	if err := first.Load(); err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if first.AdminToken() == "" {
		t.Fatalf("应自动生成管理令牌")
	}
	if !first.NeedsSetup() {
		t.Fatalf("首次加载应处于待初始化状态")
	}
	if first.AdminUser() != "" {
		t.Fatalf("未初始化时不应存在管理员账户, got %s", first.AdminUser())
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("数据文件应被创建: %v", err)
	}

	provider, _ := BuildProvider(ProviderInput{BaseURL: "https://p.example.com", Name: "p1", APIKey: "sk-upstream-secret"})
	key, _ := BuildKey(KeyInput{Name: "k1"})
	if err := first.Update(func(data *Data) error {
		data.Providers = append(data.Providers, provider)
		data.Keys = append(data.Keys, key)
		return nil
	}); err != nil {
		t.Fatalf("Update 失败: %v", err)
	}
	token := first.AdminToken()
	secret := key.Key

	second := New(file)
	if err := second.Load(); err != nil {
		t.Fatalf("重新 Load 失败: %v", err)
	}
	if second.AdminToken() != token {
		t.Fatalf("令牌应持久化")
	}
	second.View(func(data *Data) {
		if len(data.Providers) != 1 || len(data.Keys) != 1 {
			t.Fatalf("数据未持久化: %d providers, %d keys", len(data.Providers), len(data.Keys))
		}
		if data.Providers[0].APIKey != "sk-upstream-secret" {
			t.Fatalf("上游密钥应可解密还原: %q", data.Providers[0].APIKey)
		}
		if data.FindKeyBySecret(secret) == nil {
			t.Fatalf("按明文密钥查找失败")
		}
		if data.FindKeyBySecret("wrong") != nil {
			t.Fatalf("错误密钥不应命中")
		}
		if data.FindProvider(provider.ID) == nil || data.FindKeyByID(key.ID) == nil {
			t.Fatalf("按 ID 查找失败")
		}
	})
}

func TestStoreEncryptsSecretsOnDisk(t *testing.T) {
	t.Setenv("MASTER_KEY", "unit-test-master-key")
	file := filepath.Join(t.TempDir(), "db.json")

	dataStore := New(file)
	if err := dataStore.Load(); err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	provider, _ := BuildProvider(ProviderInput{BaseURL: "https://p.example.com", APIKey: "sk-plain-upstream"})
	account, _ := BuildAccount(AccountInput{Name: "acct", BaseURL: "https://p.example.com/v1", AccessToken: "tok-plain-access"})
	gatewayKey, _ := BuildKey(KeyInput{Name: "k"})
	if err := dataStore.Update(func(data *Data) error {
		data.Providers = append(data.Providers, provider)
		data.Accounts = append(data.Accounts, account)
		data.Keys = append(data.Keys, gatewayKey)
		return nil
	}); err != nil {
		t.Fatalf("Update 失败: %v", err)
	}

	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("读取数据文件失败: %v", err)
	}
	text := string(raw)
	for _, secret := range []string{"sk-plain-upstream", "tok-plain-access", gatewayKey.Key} {
		if strings.Contains(text, secret) {
			t.Fatalf("敏感字段不应以明文落盘: %s", secret)
		}
	}
	if !strings.Contains(text, "enc.v1.") {
		t.Fatalf("落盘内容应包含密文标记")
	}
	// 管理员口令只保存散列。
	if strings.Contains(text, "\"123456\"") {
		t.Fatalf("口令不应以明文落盘")
	}

	// 换掉主密钥后应因无法解密而拒绝加载，而不是静默丢数据。
	t.Setenv("MASTER_KEY", "another-master-key")
	broken := New(file)
	if err := broken.Load(); err == nil {
		t.Fatalf("更换主密钥后应报错")
	}
}

func TestStoreFlushBuffersMutations(t *testing.T) {
	t.Setenv("MASTER_KEY", "unit-test-master-key")
	file := filepath.Join(t.TempDir(), "db.json")
	dataStore := New(file)
	if err := dataStore.Load(); err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	key, _ := BuildKey(KeyInput{Name: "k"})
	if err := dataStore.Update(func(data *Data) error {
		data.Keys = append(data.Keys, key)
		return nil
	}); err != nil {
		t.Fatalf("Update 失败: %v", err)
	}

	if flushed, err := dataStore.Flush(); err != nil || flushed {
		t.Fatalf("无变更时不应写盘: %v %v", flushed, err)
	}

	dataStore.Mutate(func(data *Data) {
		data.Keys[0].Stats.Requests = 7
	})

	// Mutate 只改内存，落盘发生在 Flush。
	raw, _ := os.ReadFile(file)
	snapshot := map[string]any{}
	_ = json.Unmarshal(raw, &snapshot)
	if strings.Contains(string(raw), "\"requests\":7") {
		t.Fatalf("Mutate 不应立即落盘")
	}

	if flushed, err := dataStore.Flush(); err != nil || !flushed {
		t.Fatalf("有变更时应写盘: %v %v", flushed, err)
	}
	reloaded := New(file)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("重新 Load 失败: %v", err)
	}
	reloaded.View(func(data *Data) {
		if data.Keys[0].Stats.Requests != 7 {
			t.Fatalf("Flush 后统计应持久化: %#v", data.Keys[0].Stats)
		}
	})
}

func TestStoreVerifyAdminAndPasswordChange(t *testing.T) {
	t.Setenv("MASTER_KEY", "unit-test-master-key")
	file := filepath.Join(t.TempDir(), "db.json")
	dataStore := New(file)
	if err := dataStore.Load(); err != nil {
		t.Fatalf("Load 失败: %v", err)
	}

	super, err := dataStore.CreateSuperAdmin("Digital Gleam", "sup3r-secret")
	if err != nil {
		t.Fatalf("创建超级管理员失败: %v", err)
	}
	if !super.IsSuper() || dataStore.NeedsSetup() {
		t.Fatalf("初始化后应存在超级管理员")
	}
	if _, err := dataStore.CreateSuperAdmin("Another", "sup3r-secret"); err == nil {
		t.Fatalf("不应允许重复初始化")
	}

	if _, ok := dataStore.VerifyAdmin("Digital Gleam", "sup3r-secret"); !ok {
		t.Fatalf("超级管理员凭据应可登录")
	}
	if _, ok := dataStore.VerifyAdmin("digital gleam", "sup3r-secret"); ok {
		t.Fatalf("账户名应区分大小写")
	}
	if _, ok := dataStore.VerifyAdmin("Digital Gleam", "sup3r-secretx"); ok {
		t.Fatalf("错误口令应拒绝")
	}

	if err := dataStore.SetAdminPassword(super.ID, "short"); err == nil {
		t.Fatalf("过短口令应被拒绝")
	}
	if err := dataStore.SetAdminPassword(super.ID, "new-password-1"); err != nil {
		t.Fatalf("改密失败: %v", err)
	}
	if _, ok := dataStore.VerifyAdmin("Digital Gleam", "sup3r-secret"); ok {
		t.Fatalf("旧口令应失效")
	}
	if _, ok := dataStore.VerifyAdmin("Digital Gleam", "new-password-1"); !ok {
		t.Fatalf("新口令应生效")
	}

	reloaded := New(file)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("重新 Load 失败: %v", err)
	}
	if _, ok := reloaded.VerifyAdmin("Digital Gleam", "new-password-1"); !ok {
		t.Fatalf("改密应持久化")
	}

	// 落盘的数据文件里不能出现明文账户名。
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("读取数据文件失败: %v", err)
	}
	if strings.Contains(string(raw), "Digital Gleam") {
		t.Fatalf("账户名不应以明文落盘")
	}
}

func TestStoreAdminUserLifecycle(t *testing.T) {
	t.Setenv("MASTER_KEY", "unit-test-master-key")
	dataStore := New(filepath.Join(t.TempDir(), "db.json"))
	if err := dataStore.Load(); err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if _, err := dataStore.CreateAdminUser("early", "password-1", RoleAdmin, ""); err == nil {
		t.Fatalf("未初始化时不应允许新增账户")
	}

	super, err := dataStore.CreateSuperAdmin("root-user", "root-password")
	if err != nil {
		t.Fatalf("创建超级管理员失败: %v", err)
	}
	viewer, err := dataStore.CreateAdminUser("viewer", "viewer-password", RoleAdmin, "只看看板")
	if err != nil {
		t.Fatalf("创建管理员失败: %v", err)
	}
	if viewer.IsSuper() {
		t.Fatalf("普通管理员不应是超管")
	}
	if _, err := dataStore.CreateAdminUser("viewer", "viewer-password", RoleAdmin, ""); err == nil {
		t.Fatalf("重复账户名应被拒绝")
	}

	if err := dataStore.SetAdminEnabled(viewer.ID, false); err != nil {
		t.Fatalf("禁用账户失败: %v", err)
	}
	if _, ok := dataStore.VerifyAdmin("viewer", "viewer-password"); ok {
		t.Fatalf("禁用后不应能登录")
	}
	if err := dataStore.SetAdminEnabled(super.ID, false); err == nil {
		t.Fatalf("不应允许禁用超级管理员")
	}
	if err := dataStore.RemoveAdminUser(super.ID); err == nil {
		t.Fatalf("不应允许删除最后一个超级管理员")
	}
	if err := dataStore.RemoveAdminUser(viewer.ID); err != nil {
		t.Fatalf("删除账户失败: %v", err)
	}
	if len(dataStore.AdminUsers()) != 1 {
		t.Fatalf("删除后应只剩超级管理员")
	}
}

func TestStoreRecoversFromCorruptFile(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", "fixed-token")
	t.Setenv("MASTER_KEY", "unit-test-master-key")
	dir := t.TempDir()
	file := filepath.Join(dir, "db.json")
	if err := os.WriteFile(file, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("写入损坏文件失败: %v", err)
	}

	dataStore := New(file)
	if err := dataStore.Load(); err != nil {
		t.Fatalf("损坏文件应可恢复: %v", err)
	}
	if dataStore.AdminToken() != "fixed-token" {
		t.Fatalf("应使用环境变量令牌, got %s", dataStore.AdminToken())
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取目录失败: %v", err)
	}
	backup := false
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".corrupt-") {
			backup = true
		}
	}
	if !backup {
		t.Fatalf("应保留损坏文件备份")
	}
}

func TestPublicViewsHideSecrets(t *testing.T) {
	provider, _ := BuildProvider(ProviderInput{BaseURL: "https://p.example.com", APIKey: "sk-secret-value-1234"})
	view := PublicProvider(provider)
	if _, leaked := view["apiKey"]; leaked {
		t.Fatalf("上游视图不应包含明文密钥")
	}
	if view["hasApiKey"] != true {
		t.Fatalf("hasApiKey 应为 true")
	}

	key, _ := BuildKey(KeyInput{Name: "k"})
	if _, leaked := PublicKey(key, false)["key"]; leaked {
		t.Fatalf("默认视图不应包含明文密钥")
	}
	if PublicKey(key, true)["key"] != key.Key {
		t.Fatalf("reveal 视图应包含明文密钥")
	}
}

func TestSplitListAndMustString(t *testing.T) {
	if got := SplitList("a, b;c d"); len(got) != 4 {
		t.Fatalf("字符串分割失败: %#v", got)
	}
	if got := SplitList([]any{"a", "", nil, "b"}); len(got) != 2 {
		t.Fatalf("数组分割失败: %#v", got)
	}
	if got := SplitList(nil); len(got) != 0 {
		t.Fatalf("nil 应返回空切片")
	}
	if MustString(nil) != "" || MustString("x") != "x" || MustString(float64(12)) != "12" || MustString(true) != "true" {
		t.Fatalf("MustString 转换错误")
	}
}

// TestPasswordNormalizationKeepsLoginWorking 复现“重置口令后登不进去”的报障。
//
// 历史实现里写入口令时剪掉首尾空白、校验时却按原串比较，
// 粘贴带尾随空格或换行的口令就会造成账户被锁死。
func TestPasswordNormalizationKeepsLoginWorking(t *testing.T) {
	t.Setenv("MASTER_KEY", "unit-test-master-key")
	dataStore := New(filepath.Join(t.TempDir(), "db.json"))
	if err := dataStore.Load(); err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	super, err := dataStore.CreateSuperAdmin("Digital Gleam", "  sup3r-secret  ")
	if err != nil {
		t.Fatalf("创建超级管理员失败: %v", err)
	}
	// 创建时带空白，登录时无论带不带空白都应该通过。
	for _, candidate := range []string{"sup3r-secret", "  sup3r-secret  ", "sup3r-secret\n"} {
		if _, ok := dataStore.VerifyAdmin("Digital Gleam", candidate); !ok {
			t.Fatalf("口令 %q 应可登录", candidate)
		}
	}

	// 重置口令时带尾随换行，用户随后输入的干净口令必须能登录。
	if err := dataStore.SetAdminPassword(super.ID, "reset-password-1\n"); err != nil {
		t.Fatalf("重置口令失败: %v", err)
	}
	if _, ok := dataStore.VerifyAdmin("Digital Gleam", "reset-password-1"); !ok {
		t.Fatalf("重置后的口令应生效")
	}
	if _, ok := dataStore.VerifyAdmin("Digital Gleam", "reset-password-1\n"); !ok {
		t.Fatalf("带尾随换行的同一口令也应生效")
	}
	if _, ok := dataStore.VerifyAdmin("Digital Gleam", "sup3r-secret"); ok {
		t.Fatalf("旧口令必须失效")
	}

	// 归一化后长度不足的口令要被拒绝，避免“7 个字符 + 空格”绕过下限。
	if err := dataStore.SetAdminPassword(super.ID, "  short  "); err == nil {
		t.Fatalf("归一化后过短的口令应被拒绝")
	}
}

// TestResetAdminPasswordByName 覆盖命令行自救入口。
func TestResetAdminPasswordByName(t *testing.T) {
	t.Setenv("MASTER_KEY", "unit-test-master-key")
	file := filepath.Join(t.TempDir(), "db.json")
	dataStore := New(file)
	if err := dataStore.Load(); err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if _, err := dataStore.CreateSuperAdmin("Digital Gleam", "sup3r-secret"); err != nil {
		t.Fatalf("创建超级管理员失败: %v", err)
	}
	viewer, err := dataStore.CreateAdminUser("viewer", "viewer-password", RoleAdmin, "")
	if err != nil {
		t.Fatalf("创建管理员失败: %v", err)
	}
	if err := dataStore.SetAdminEnabled(viewer.ID, false); err != nil {
		t.Fatalf("禁用账户失败: %v", err)
	}

	if _, err := dataStore.ResetAdminPasswordByName("nobody", "whatever-1"); err == nil {
		t.Fatalf("未知账户应报错")
	}
	if _, err := dataStore.ResetAdminPasswordByName("viewer", "tiny"); err == nil {
		t.Fatalf("过短口令应报错")
	}

	// 重置会顺带把账户重新启用，避免“口令对了但账户禁用”的二次卡死。
	restored, err := dataStore.ResetAdminPasswordByName(" viewer ", "cli-reset-password")
	if err != nil {
		t.Fatalf("命令行重置失败: %v", err)
	}
	if !restored.Enabled {
		t.Fatalf("重置后账户应为启用状态")
	}
	if _, ok := dataStore.VerifyAdmin("viewer", "cli-reset-password"); !ok {
		t.Fatalf("命令行重置后的口令应可登录")
	}

	// 必须已经落盘：命令行改完口令后服务是重新启动的。
	reloaded := New(file)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("重新 Load 失败: %v", err)
	}
	if _, ok := reloaded.VerifyAdmin("viewer", "cli-reset-password"); !ok {
		t.Fatalf("命令行重置应持久化")
	}
}
