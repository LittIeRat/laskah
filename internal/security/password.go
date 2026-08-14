package security

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// passwordIterations 是口令散列的 PBKDF2 迭代次数。
const passwordIterations = 240000

// HashPassword 生成 pbkdf2-sha256 格式的口令散列。
func HashPassword(password string) (string, error) {
	if password == "" {
		return "", errors.New("口令不能为空")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("生成口令盐失败: %w", err)
	}
	digest, err := pbkdf2.Key(sha256.New, password, salt, passwordIterations, 32)
	if err != nil {
		return "", fmt.Errorf("计算口令散列失败: %w", err)
	}
	return strings.Join([]string{
		"pbkdf2-sha256",
		strconv.Itoa(passwordIterations),
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(digest),
	}, "$"), nil
}

// VerifyPassword 以常量时间比较校验口令。
func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iterations, err := strconv.Atoi(parts[1])
	if err != nil || iterations < 1000 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	expected, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	actual, err := pbkdf2.Key(sha256.New, password, salt, iterations, len(expected))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(expected, actual) == 1
}

// DummyHash 是一条真实格式的口令散列，用于账户不存在时执行等价耗时的假校验。
//
// 目的是抹平“账户存在 / 不存在”的响应时间差，防止通过时间侧信道枚举账户名。
const DummyHash = "pbkdf2-sha256$240000$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// HashToken 返回令牌的 SHA-256 摘要，用于只存摘要不存明文。
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// ConstantTimeEqual 常量时间比较两个字符串。
func ConstantTimeEqual(a, b string) bool {
	if len(a) != len(b) || a == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// RandomToken 生成指定字节数的随机 URL 安全令牌。
func RandomToken(size int) (string, error) {
	if size < 16 {
		size = 16
	}
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成随机令牌失败: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
