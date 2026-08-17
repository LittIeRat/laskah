package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"laskah/internal/security"
)

// ErrSetupDone 表示服务已完成初始化，不能再次创建超级管理员。
var ErrSetupDone = errors.New("超级管理员已创建，无法重复初始化")

// ErrNotSetup 表示服务尚未创建超级管理员。
var ErrNotSetup = errors.New("服务尚未初始化，请先创建超级管理员")

// Store 是基于单个 JSON 文件的并发安全数据仓库。
//
// 敏感字段（上游 API Key、账号访问令牌、网关密钥）以 AES-256-GCM 落盘，
// 内存中保留明文以避免热路径反复解密。
type Store struct {
	mu      sync.RWMutex
	file    string
	keyFile string
	data    *Data
	cipher  *security.Cipher
	dirty   bool
}

// New 创建仓库实例，尚未读取磁盘。
func New(file string) *Store {
	return &Store{file: file, keyFile: defaultKeyFile(file), data: newData()}
}

func defaultKeyFile(dataFile string) string {
	dir := filepath.Dir(dataFile)
	base := filepath.Base(dataFile)
	if ext := filepath.Ext(base); ext != "" {
		base = strings.TrimSuffix(base, ext)
	}
	return filepath.Join(dir, base+".master.key")
}

func newData() *Data {
	data := &Data{
		Version:         6,
		Config:          Config{Strategy: "weighted-random", MaxRetries: 3, CreatedAt: time.Now().UTC()},
		Groups:          []*Group{},
		Accounts:        []*Account{},
		Providers:       []*Provider{},
		Keys:            []*APIKey{},
		RemovedAccounts: []RemovedAccount{},
	}
	data.reindex()
	return data
}

// Load 读取磁盘数据，初始化加密与管理员凭据，并重建索引。
func (s *Store) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, err := os.ReadFile(s.file)
	switch {
	case err == nil:
		parsed := newData()
		if unmarshalErr := json.Unmarshal(raw, parsed); unmarshalErr != nil {
			backup := fmt.Sprintf("%s.corrupt-%d", s.file, time.Now().Unix())
			if renameErr := os.Rename(s.file, backup); renameErr != nil {
				return fmt.Errorf("数据文件损坏且无法备份: %w", renameErr)
			}
			parsed = newData()
		}
		s.data = parsed
	case os.IsNotExist(err):
		s.data = newData()
	default:
		return fmt.Errorf("读取数据文件失败: %w", err)
	}

	s.normalizeLocked()
	if err := s.initCipherLocked(); err != nil {
		return err
	}
	if err := s.unsealLocked(); err != nil {
		return err
	}
	if err := s.initCredentialsLocked(); err != nil {
		return err
	}
	s.data.reindex()
	return s.persistLocked()
}

func (s *Store) normalizeLocked() {
	data := s.data
	if data.Groups == nil {
		data.Groups = []*Group{}
	}
	if data.Accounts == nil {
		data.Accounts = []*Account{}
	}
	if data.RemovedAccounts == nil {
		data.RemovedAccounts = []RemovedAccount{}
	}
	if data.Providers == nil {
		data.Providers = []*Provider{}
	}
	if data.Keys == nil {
		data.Keys = []*APIKey{}
	}
	if data.Config.Strategy == "" {
		data.Config.Strategy = "weighted-random"
	}
	if data.Config.MaxRetries <= 0 {
		data.Config.MaxRetries = 3
	}
	if data.Config.CreatedAt.IsZero() {
		data.Config.CreatedAt = time.Now().UTC()
	}
	// v3 引入“请求时刷新”：历史数据默认开启，避免旧账号余额耗尽后继续接流量。
	legacyAccounts := data.Version < 3
	// v4 引入分组启用开关与多管理员：旧分组默认启用。
	legacyGroups := data.Version < 4
	// v5 把「余额耗尽自动删号」换成「自动暂停」：旧数据按原 autoDelete 开关迁移。
	legacyAutoDelete := data.Version < 5
	// v6 引入本地 token 计量、手动余额与自定义端点：旧账号一律按「不计价」迁移，
	// 余额来源与行为保持原样，避免升级后突然开始扣本地余额。
	legacyBilling := data.Version < 6
	if data.Version < 6 {
		data.Version = 6
	}
	if data.Config.Users == nil {
		data.Config.Users = []*AdminUser{}
	}
	for _, group := range data.Groups {
		if legacyGroups {
			group.Enabled = true
		}
	}

	for _, provider := range data.Providers {
		if provider.ModelMap == nil {
			provider.ModelMap = map[string]string{}
		}
		if provider.Headers == nil {
			provider.Headers = map[string]string{}
		}
		if provider.Models == nil {
			provider.Models = []string{}
		}
		if provider.Tags == nil {
			provider.Tags = []string{}
		}
		if provider.Paths.Chat == "" || provider.Paths.Models == "" || provider.Paths.Responses == "" {
			defaults := DefaultPaths(provider.Type)
			if provider.Paths.Chat == "" {
				provider.Paths.Chat = defaults.Chat
			}
			if provider.Paths.Models == "" {
				provider.Paths.Models = defaults.Models
			}
			if provider.Paths.Responses == "" {
				provider.Paths.Responses = defaults.Responses
			}
		}
		provider.CooldownUntil = time.Time{}
		provider.Inflight = 0
		provider.ConsecutiveFailures = 0
	}
	for _, account := range data.Accounts {
		if account.Models == nil {
			account.Models = []string{}
		}
		if account.Currency == "" {
			account.Currency = "USD"
		}
		if account.TimeoutSeconds <= 0 {
			account.TimeoutSeconds = DefaultQueryTimeoutSeconds
		}
		if account.RequestRefreshSec <= 0 {
			account.RequestRefreshSec = DefaultRequestRefreshSeconds
		}
		if account.RequestRefreshSec > MaxRequestRefreshSeconds {
			account.RequestRefreshSec = MaxRequestRefreshSeconds
		}
		if legacyAccounts {
			account.RefreshOnRequest = true
		}
		if legacyAutoDelete {
			// 旧字段缺失时默认开启：与 v4 之前 BuildAccount 的默认值一致。
			account.AutoSuspend = account.AutoDeleteLegacy == nil || *account.AutoDeleteLegacy
		}
		// 迁移后丢弃历史字段，避免继续写回磁盘。
		account.AutoDeleteLegacy = nil
		if account.RateLimitPerMin != nil && *account.RateLimitPerMin <= 0 {
			account.RateLimitPerMin = nil
		}
		if legacyBilling {
			account.BillingMode = BillingNone
			account.ManualBalance = false
		}
		if account.BillingMode == "" || !ValidBillingMode(string(account.BillingMode)) {
			account.BillingMode = BillingNone
		}
		if account.BillingMode == BillingNone {
			// 没有计价方式就无从扣减，手动余额必须同时关闭，否则账号会永久卡在当前余额。
			account.ManualBalance = false
		}
		if !account.Suspended {
			account.SuspendReason = ""
			account.SuspendedAt = nil
		}
	}
	for _, key := range data.Keys {
		if key.AllowedModels == nil {
			key.AllowedModels = []string{}
		}
		if key.ProviderIDs == nil {
			key.ProviderIDs = []string{}
		}
		if key.Tags == nil {
			key.Tags = []string{}
		}
	}
}

func (s *Store) initCipherLocked() error {
	if strings.TrimSpace(s.data.Config.EncryptionSalt) == "" {
		salt, err := security.NewSalt()
		if err != nil {
			return err
		}
		s.data.Config.EncryptionSalt = salt
	}
	salt, err := security.DecodeSalt(s.data.Config.EncryptionSalt)
	if err != nil {
		return fmt.Errorf("解析加密盐失败: %w", err)
	}
	secret, err := security.ResolveSecret(os.Getenv("MASTER_KEY"), s.keyFile)
	if err != nil {
		return err
	}
	cipher, err := security.NewCipher(secret, salt)
	if err != nil {
		return err
	}
	s.cipher = cipher
	return nil
}

func (s *Store) unsealLocked() error {
	for _, user := range s.data.Config.Users {
		plaintext, err := s.cipher.Open(user.SealedUsername)
		if err != nil {
			return fmt.Errorf("解密管理员账户名失败 (%s): %w", user.ID, err)
		}
		user.Username = plaintext
		if user.UsernameHash == "" && plaintext != "" {
			user.UsernameHash = security.HashToken(plaintext)
		}
	}
	for _, provider := range s.data.Providers {
		plaintext, err := s.cipher.Open(provider.SealedAPIKey)
		if err != nil {
			return fmt.Errorf("解密上游 API Key 失败 (%s): %w", provider.Name, err)
		}
		provider.APIKey = plaintext
	}
	for _, account := range s.data.Accounts {
		plaintext, err := s.cipher.Open(account.SealedAccessToken)
		if err != nil {
			return fmt.Errorf("解密账号访问令牌失败 (%s): %w", account.Name, err)
		}
		account.AccessToken = plaintext
	}
	for _, key := range s.data.Keys {
		plaintext, err := s.cipher.Open(key.SealedKey)
		if err != nil {
			return fmt.Errorf("解密网关密钥失败 (%s): %w", key.Name, err)
		}
		key.Key = plaintext
		if key.KeyHash == "" && plaintext != "" {
			key.KeyHash = security.HashToken(plaintext)
		}
		if key.KeyMasked == "" {
			key.KeyMasked = MaskKey(plaintext)
		}
	}
	return nil
}

// initCredentialsLocked 只准备管理令牌，不再写入任何默认管理员账户。
//
// 部署后首次访问必须由用户显式创建超级管理员（见 CreateSuperAdmin），
// 因此这里不能内置默认口令，避免出现开箱可登录的弱凭据。
// 环境变量 ADMIN_USER / ADMIN_PASSWORD 可用于无人值守初始化。
func (s *Store) initCredentialsLocked() error {
	if envToken := strings.TrimSpace(os.Getenv("ADMIN_TOKEN")); envToken != "" {
		s.data.Config.AdminToken = envToken
	} else if s.data.Config.AdminToken == "" {
		token, err := security.RandomToken(18)
		if err != nil {
			return err
		}
		s.data.Config.AdminToken = token
	}

	if len(s.data.Config.Users) > 0 {
		s.data.Config.Setup = true
		return nil
	}

	envUser := NormalizeUsername(os.Getenv("ADMIN_USER"))
	envPassword := os.Getenv("ADMIN_PASSWORD")
	if envUser == "" || envPassword == "" {
		s.data.Config.Setup = false
		return nil
	}

	user, verr := BuildAdminUser(envUser, envPassword, RoleSuper, "由环境变量初始化")
	if verr != nil {
		return fmt.Errorf("环境变量初始化超级管理员失败: %s", verr.Error())
	}
	s.data.Config.Users = append(s.data.Config.Users, user)
	s.data.Config.Setup = true
	s.data.reindexAdmins()
	return nil
}

// NeedsSetup 判断是否还没有创建超级管理员。
func (s *Store) NeedsSetup() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.data.Config.Users) == 0
}

// CreateSuperAdmin 创建首个超级管理员，仅在未初始化时可用。
func (s *Store) CreateSuperAdmin(username, password string) (*AdminUser, error) {
	user, verr := BuildAdminUser(username, password, RoleSuper, "初始超级管理员")
	if verr != nil {
		return nil, verr
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.data.Config.Users) > 0 {
		return nil, ErrSetupDone
	}
	s.data.Config.Users = append(s.data.Config.Users, user)
	s.data.Config.Setup = true
	s.data.reindexAdmins()
	if err := s.persistLocked(); err != nil {
		return nil, err
	}
	return user, nil
}

// CreateAdminUser 由超级管理员新增账户。
func (s *Store) CreateAdminUser(username, password string, role Role, note string) (*AdminUser, error) {
	user, verr := BuildAdminUser(username, password, role, note)
	if verr != nil {
		return nil, verr
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.data.Config.Users) == 0 {
		return nil, ErrNotSetup
	}
	if len(s.data.Config.Users) >= MaxAdminUsers {
		return nil, errors.New("管理员数量已达上限")
	}
	if s.data.FindAdminByHash(user.UsernameHash) != nil {
		return nil, errors.New("账户名已存在")
	}
	s.data.Config.Users = append(s.data.Config.Users, user)
	s.data.reindexAdmins()
	if err := s.persistLocked(); err != nil {
		return nil, err
	}
	return user, nil
}

// SetAdminEnabled 启用或禁用一个管理员账户。
func (s *Store) SetAdminEnabled(id string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	user := s.data.FindAdminByID(id)
	if user == nil {
		return errors.New("账户不存在")
	}
	if user.IsSuper() && !enabled {
		return errors.New("不能禁用超级管理员")
	}
	user.Enabled = enabled
	user.UpdatedAt = time.Now().UTC()
	return s.persistLocked()
}

// RemoveAdminUser 删除管理员账户，禁止删除最后一个超级管理员。
func (s *Store) RemoveAdminUser(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	user := s.data.FindAdminByID(id)
	if user == nil {
		return errors.New("账户不存在")
	}
	if user.IsSuper() && s.data.CountSuperAdmins() <= 1 {
		return errors.New("不能删除最后一个超级管理员")
	}
	if !s.data.RemoveAdminUser(id) {
		return errors.New("账户不存在")
	}
	return s.persistLocked()
}

// AdminUsers 返回账户列表的浅拷贝快照。
func (s *Store) AdminUsers() []*AdminUser {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*AdminUser, len(s.data.Config.Users))
	copy(result, s.data.Config.Users)
	return result
}

// File 返回数据文件路径。
func (s *Store) File() string {
	return s.file
}

// KeyFile 返回主密钥文件路径。
//
// 设置了 MASTER_KEY 环境变量时该文件不会被创建。
func (s *Store) KeyFile() string {
	return s.keyFile
}

// AdminToken 返回当前管理令牌。
func (s *Store) AdminToken() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.Config.AdminToken
}

// AdminUser 返回超级管理员账户名，未初始化时返回空串。
//
// 仅用于服务端内部（如启动横幅），不通过接口返回给未授权调用方。
func (s *Store) AdminUser() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if super := s.data.SuperAdmin(); super != nil {
		return super.Username
	}
	return ""
}

// VerifyAdmin 校验账户名与口令，成功时返回账户快照。
//
// 先按账户名摘要 O(1) 定位账户，再做常量时间口令比较；
// 账户不存在时仍执行一次假校验，避免用响应时间区分“账户不存在”与“口令错误”。
func (s *Store) VerifyAdmin(username, password string) (*AdminUser, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	name := NormalizeUsername(username)
	// 口令与写入时保持同一归一化规则，避免改密后登录被首尾空白卡住。
	secret := NormalizePassword(password)
	user := s.data.FindAdminByHash(security.HashToken(name))
	if user == nil {
		security.VerifyPassword(security.DummyHash, secret)
		return nil, false
	}
	if !user.Enabled {
		security.VerifyPassword(security.DummyHash, secret)
		return nil, false
	}
	if !security.VerifyPassword(user.Password, secret) {
		return nil, false
	}

	now := time.Now().UTC()
	user.LastLoginAt = &now
	s.dirty = true
	copied := *user
	return &copied, true
}

// ResetAdminPasswordByName 按账户名重置口令并确保账户可用。
//
// 专供命令行自救：超级管理员忘记口令或改密后登不进去时，
// 运维可以在服务器上直接改回一个已知口令，而不必清空数据重新初始化。
// 一并把账户置为启用状态，避免“口令对了但账户被禁用”的二次卡死。
func (s *Store) ResetAdminPasswordByName(username, password string) (*AdminUser, error) {
	name := NormalizeUsername(username)
	if name == "" {
		return nil, errors.New("账户名不能为空")
	}
	secret := NormalizePassword(password)
	if len([]rune(secret)) < minPasswordLength {
		return nil, fmt.Errorf("密码至少 %d 个字符", minPasswordLength)
	}
	hashed, err := security.HashPassword(secret)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	user := s.data.FindAdminByHash(security.HashToken(name))
	if user == nil {
		return nil, errors.New("账户不存在")
	}
	user.Password = hashed
	user.PasswordSetAt = time.Now().UTC()
	user.UpdatedAt = user.PasswordSetAt
	user.Enabled = true
	if err := s.persistLocked(); err != nil {
		return nil, err
	}
	copied := *user
	return &copied, nil
}

// SetAdminPassword 更新指定账户的口令散列。
func (s *Store) SetAdminPassword(id, password string) error {
	secret := NormalizePassword(password)
	if len([]rune(secret)) < minPasswordLength {
		return fmt.Errorf("密码至少 %d 个字符", minPasswordLength)
	}
	hashed, err := security.HashPassword(secret)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	user := s.data.FindAdminByID(id)
	if user == nil {
		return errors.New("账户不存在")
	}
	user.Password = hashed
	user.PasswordSetAt = time.Now().UTC()
	user.UpdatedAt = user.PasswordSetAt
	return s.persistLocked()
}

// Strategy 返回当前负载均衡策略。
func (s *Store) Strategy() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.Config.Strategy
}

// MaxRetries 返回单次请求最多尝试的上游数量。
func (s *Store) MaxRetries() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.Config.MaxRetries
}

// SetSettings 更新策略与重试次数并落盘。
func (s *Store) SetSettings(strategy string, maxRetries int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strategy != "" {
		s.data.Config.Strategy = strategy
	}
	if maxRetries > 0 {
		s.data.Config.MaxRetries = maxRetries
	}
	return s.persistLocked()
}

// Update 在写锁保护下修改数据并立即落盘。
func (s *Store) Update(fn func(data *Data) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := fn(s.data); err != nil {
		return err
	}
	s.data.reindex()
	return s.persistLocked()
}

// Mutate 在写锁保护下修改数据，仅标记脏位而不立即落盘。
//
// 用于请求计数这类高频写入：由后台 Flush 合并落盘，避免每次调用都重写整个数据文件。
func (s *Store) Mutate(fn func(data *Data)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s.data)
	s.dirty = true
}

// View 在读锁保护下访问数据。
func (s *Store) View(fn func(data *Data)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fn(s.data)
}

// Persist 主动落盘。
func (s *Store) Persist() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.persistLocked()
}

// Flush 仅在存在未落盘变更时写盘，返回是否实际写入。
func (s *Store) Flush() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return false, nil
	}
	if err := s.persistLocked(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) persistLocked() error {
	if err := s.sealLocked(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.file), 0o700); err != nil {
		return fmt.Errorf("创建数据目录失败: %w", err)
	}
	encoded, err := json.Marshal(s.data)
	if err != nil {
		return fmt.Errorf("序列化数据失败: %w", err)
	}
	tmp := s.file + ".tmp"
	if err := os.WriteFile(tmp, encoded, 0o600); err != nil {
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := os.Rename(tmp, s.file); err != nil {
		return fmt.Errorf("替换数据文件失败: %w", err)
	}
	s.dirty = false
	return nil
}

// sealLocked 只对发生变化的敏感字段重新加密，避免每次落盘都做无谓的密码学运算。
func (s *Store) sealLocked() error {
	if s.cipher == nil {
		return nil
	}
	for _, user := range s.data.Config.Users {
		if user.sealedFrom == user.Username && security.IsSealed(user.SealedUsername) {
			continue
		}
		sealed, err := s.cipher.Seal(user.Username)
		if err != nil {
			return err
		}
		user.SealedUsername = sealed
		user.sealedFrom = user.Username
	}
	for _, provider := range s.data.Providers {
		if provider.sealedFrom == provider.APIKey && security.IsSealed(provider.SealedAPIKey) {
			continue
		}
		sealed, err := s.cipher.Seal(provider.APIKey)
		if err != nil {
			return err
		}
		provider.SealedAPIKey = sealed
		provider.sealedFrom = provider.APIKey
	}
	for _, account := range s.data.Accounts {
		if account.sealedFrom == account.AccessToken && security.IsSealed(account.SealedAccessToken) {
			continue
		}
		sealed, err := s.cipher.Seal(account.AccessToken)
		if err != nil {
			return err
		}
		account.SealedAccessToken = sealed
		account.sealedFrom = account.AccessToken
	}
	for _, key := range s.data.Keys {
		if key.sealedFrom == key.Key && security.IsSealed(key.SealedKey) {
			continue
		}
		sealed, err := s.cipher.Seal(key.Key)
		if err != nil {
			return err
		}
		key.SealedKey = sealed
		key.sealedFrom = key.Key
	}
	return nil
}

// PublicProvider 返回上游 API 的脱敏视图。
func PublicProvider(p *Provider) map[string]any {
	var cooldown any
	if !p.CooldownUntil.IsZero() {
		cooldown = p.CooldownUntil
	}
	return map[string]any{
		"id":            p.ID,
		"accountId":     p.AccountID,
		"name":          p.Name,
		"type":          p.Type,
		"baseUrl":       p.BaseURL,
		"hasApiKey":     p.APIKey != "",
		"apiKeyMasked":  MaskKey(p.APIKey),
		"models":        p.Models,
		"modelMap":      p.ModelMap,
		"paths":         p.Paths,
		"weight":        p.Weight,
		"priority":      p.Priority,
		"timeoutMs":     p.TimeoutMS,
		"enabled":       p.Enabled,
		"tags":          p.Tags,
		"note":          p.Note,
		"createdAt":     p.CreatedAt,
		"updatedAt":     p.UpdatedAt,
		"stats":         p.Stats,
		"cooldownUntil": cooldown,
		"inflight":      p.Inflight,
	}
}

// PublicKey 返回密钥视图，reveal 为真时包含明文。
func PublicKey(k *APIKey, reveal bool) map[string]any {
	view := map[string]any{
		"id":              k.ID,
		"accountId":       k.AccountID,
		"groupId":         k.GroupID,
		"name":            k.Name,
		"keyMasked":       k.KeyMasked,
		"enabled":         k.Enabled,
		"allowedModels":   k.AllowedModels,
		"providerIds":     k.ProviderIDs,
		"tags":            k.Tags,
		"quotaTokens":     k.QuotaTokens,
		"rateLimitPerMin": k.RateLimitPerMin,
		"expiresAt":       k.ExpiresAt,
		"note":            k.Note,
		"createdAt":       k.CreatedAt,
		"updatedAt":       k.UpdatedAt,
		"stats":           k.Stats,
		"status":          k.State(time.Now()),
	}
	if reveal {
		view["key"] = k.Key
	}
	return view
}

// FindProvider 按 ID 查找上游 API。
func (d *Data) FindProvider(id string) *Provider {
	if d.providerByID != nil {
		return d.providerByID[id]
	}
	for _, provider := range d.Providers {
		if provider.ID == id {
			return provider
		}
	}
	return nil
}

// FindKeyByID 按 ID 查找密钥。
func (d *Data) FindKeyByID(id string) *APIKey {
	for _, key := range d.Keys {
		if key.ID == id {
			return key
		}
	}
	return nil
}

// FindKeyBySecret 按明文密钥的摘要索引查找，避免逐条比较明文。
func (d *Data) FindKeyBySecret(secret string) *APIKey {
	if secret == "" {
		return nil
	}
	digest := security.HashToken(secret)
	if d.keyByHash != nil {
		if key, ok := d.keyByHash[digest]; ok {
			return key
		}
		return nil
	}
	for _, key := range d.Keys {
		if security.ConstantTimeEqual(key.KeyHash, digest) {
			return key
		}
	}
	return nil
}

// RemoveProviders 删除指定 ID 的上游 API，返回删除数量。
func (d *Data) RemoveProviders(ids []string) int {
	wanted := map[string]bool{}
	for _, id := range ids {
		if strings.TrimSpace(id) != "" {
			wanted[id] = true
		}
	}
	if len(wanted) == 0 {
		return 0
	}
	kept := make([]*Provider, 0, len(d.Providers))
	removed := 0
	for _, provider := range d.Providers {
		if wanted[provider.ID] {
			removed++
			continue
		}
		kept = append(kept, provider)
	}
	d.Providers = kept
	if removed > 0 {
		d.reindex()
	}
	return removed
}

// RemoveKeys 删除指定 ID 的网关密钥，返回删除数量。
func (d *Data) RemoveKeys(ids []string) int {
	wanted := map[string]bool{}
	for _, id := range ids {
		if strings.TrimSpace(id) != "" {
			wanted[id] = true
		}
	}
	if len(wanted) == 0 {
		return 0
	}
	kept := make([]*APIKey, 0, len(d.Keys))
	removed := 0
	for _, key := range d.Keys {
		if wanted[key.ID] {
			removed++
			continue
		}
		kept = append(kept, key)
	}
	d.Keys = kept
	if removed > 0 {
		d.reindex()
	}
	return removed
}
