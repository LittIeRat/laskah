package store

import (
	"strings"
	"time"

	"laskah/internal/security"
)

// Role 是管理面角色。
//
// 只有两级：超级管理员可访问全部页面与接口；管理员仅能查看数据看板。
type Role string

// 支持的角色取值。
const (
	RoleSuper Role = "super"
	RoleAdmin Role = "admin"
)

// MaxAdminUsers 限制管理员数量，避免登录索引无界增长。
const MaxAdminUsers = 64

// minPasswordLength 是管理员口令的最小长度。
const minPasswordLength = 8

// ValidRole 判断角色取值是否受支持。
func ValidRole(value string) bool {
	switch Role(strings.TrimSpace(value)) {
	case RoleSuper, RoleAdmin:
		return true
	default:
		return false
	}
}

// AdminUser 是一个管理面账户。
//
// 账户名本身也是敏感信息：落盘只保留 AES-GCM 密文（SealedUsername）与
// SHA-256 摘要（UsernameHash）。摘要用于登录时 O(1) 定位账户，
// 因此即使数据文件泄露也无法直接读出超级管理员的账户名。
type AdminUser struct {
	ID             string     `json:"id"`
	Username       string     `json:"-"`
	SealedUsername string     `json:"username"`
	UsernameHash   string     `json:"usernameHash"`
	Password       string     `json:"password"`
	Role           Role       `json:"role"`
	Enabled        bool       `json:"enabled"`
	Note           string     `json:"note"`
	CreatedAt      time.Time  `json:"createdAt"`
	UpdatedAt      time.Time  `json:"updatedAt"`
	PasswordSetAt  time.Time  `json:"passwordSetAt"`
	LastLoginAt    *time.Time `json:"lastLoginAt"`

	sealedFrom string
}

// NormalizeUsername 统一账户名的空白处理。
//
// 不做大小写折叠：账户名区分大小写可以让暴力猜测的搜索空间更大。
func NormalizeUsername(raw string) string {
	return strings.TrimSpace(raw)
}

// BuildAdminUser 校验输入并生成管理员账户。
func BuildAdminUser(username, password string, role Role, note string) (*AdminUser, *ValidationError) {
	verr := &ValidationError{}

	name := NormalizeUsername(username)
	switch {
	case name == "":
		verr.Errorf("账户名不能为空")
	case len([]rune(name)) < 3:
		verr.Errorf("账户名至少 3 个字符")
	case len([]rune(name)) > 48:
		verr.Errorf("账户名不能超过 48 个字符")
	}

	if len([]rune(password)) < minPasswordLength {
		verr.Errorf("密码至少 %d 个字符", minPasswordLength)
	}
	if !ValidRole(string(role)) {
		verr.Errorf("角色只能是 super 或 admin")
	}
	if verr.HasErrors() {
		return nil, verr
	}

	hashed, err := security.HashPassword(password)
	if err != nil {
		verr.Errorf("%s", err.Error())
		return nil, verr
	}

	now := time.Now().UTC()
	return &AdminUser{
		ID:            NewID("usr"),
		Username:      name,
		UsernameHash:  security.HashToken(name),
		Password:      hashed,
		Role:          role,
		Enabled:       true,
		Note:          strings.TrimSpace(note),
		CreatedAt:     now,
		UpdatedAt:     now,
		PasswordSetAt: now,
	}, nil
}

// IsSuper 判断是否为超级管理员。
func (u *AdminUser) IsSuper() bool {
	return u != nil && u.Role == RoleSuper
}

// PublicAdminUser 返回管理员的脱敏视图。
//
// 只有超级管理员能看到账户名；其余场景一律用掩码，避免账户名被旁路读取。
func PublicAdminUser(u *AdminUser, revealName bool) map[string]any {
	name := MaskUsername(u.Username)
	if revealName {
		name = u.Username
	}
	return map[string]any{
		"id":          u.ID,
		"username":    name,
		"role":        u.Role,
		"enabled":     u.Enabled,
		"note":        u.Note,
		"createdAt":   u.CreatedAt,
		"updatedAt":   u.UpdatedAt,
		"lastLoginAt": u.LastLoginAt,
		"isSuper":     u.IsSuper(),
	}
}

// MaskUsername 只保留账户名首尾字符。
func MaskUsername(name string) string {
	runes := []rune(name)
	switch {
	case len(runes) == 0:
		return ""
	case len(runes) <= 2:
		return string(runes[:1]) + "***"
	default:
		return string(runes[:1]) + "***" + string(runes[len(runes)-1:])
	}
}

// FindAdminByHash 按账户名摘要查找账户。
func (d *Data) FindAdminByHash(digest string) *AdminUser {
	if digest == "" {
		return nil
	}
	if d.adminByHash != nil {
		return d.adminByHash[digest]
	}
	for _, user := range d.Config.Users {
		if security.ConstantTimeEqual(user.UsernameHash, digest) {
			return user
		}
	}
	return nil
}

// FindAdminByID 按 ID 查找账户。
func (d *Data) FindAdminByID(id string) *AdminUser {
	if id == "" {
		return nil
	}
	for _, user := range d.Config.Users {
		if user.ID == id {
			return user
		}
	}
	return nil
}

// SuperAdmin 返回首个启用中的超级管理员。
func (d *Data) SuperAdmin() *AdminUser {
	for _, user := range d.Config.Users {
		if user.IsSuper() {
			return user
		}
	}
	return nil
}

// CountSuperAdmins 统计超级管理员数量，用于阻止删掉最后一个。
func (d *Data) CountSuperAdmins() int {
	count := 0
	for _, user := range d.Config.Users {
		if user.IsSuper() {
			count++
		}
	}
	return count
}

// RemoveAdminUser 删除指定账户，返回是否删除成功。
func (d *Data) RemoveAdminUser(id string) bool {
	kept := make([]*AdminUser, 0, len(d.Config.Users))
	removed := false
	for _, user := range d.Config.Users {
		if user.ID == id {
			removed = true
			continue
		}
		kept = append(kept, user)
	}
	d.Config.Users = kept
	if removed {
		d.reindexAdmins()
	}
	return removed
}

// reindexAdmins 重建账户名摘要索引。
func (d *Data) reindexAdmins() {
	d.adminByHash = make(map[string]*AdminUser, len(d.Config.Users))
	for _, user := range d.Config.Users {
		if user.UsernameHash != "" {
			d.adminByHash[user.UsernameHash] = user
		}
	}
}
