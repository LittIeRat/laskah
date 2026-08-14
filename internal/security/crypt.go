// Package security 提供口令校验、会话管理与静态数据加密能力。
package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// sealPrefix 标记一段密文由本程序加密，便于识别历史明文数据。
const sealPrefix = "enc.v1."

// keyIterations 是派生数据加密密钥的 PBKDF2 迭代次数。
const keyIterations = 210000

// Cipher 使用 AES-256-GCM 加解密敏感字段。
type Cipher struct {
	aead cipher.AEAD
}

// NewCipher 由主密钥与盐派生数据密钥。
func NewCipher(secret string, salt []byte) (*Cipher, error) {
	if strings.TrimSpace(secret) == "" {
		return nil, errors.New("加密主密钥不能为空")
	}
	if len(salt) < 16 {
		return nil, errors.New("加密盐长度不足")
	}
	key, err := pbkdf2.Key(sha256.New, secret, salt, keyIterations, 32)
	if err != nil {
		return nil, fmt.Errorf("派生数据密钥失败: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("初始化 AES 失败: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("初始化 GCM 失败: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// Seal 加密明文，空串原样返回。
func (c *Cipher) Seal(plaintext string) (string, error) {
	if c == nil || c.aead == nil {
		return plaintext, nil
	}
	if plaintext == "" {
		return "", nil
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("生成随机数失败: %w", err)
	}
	sealed := c.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return sealPrefix + base64.RawURLEncoding.EncodeToString(sealed), nil
}

// Open 解密密文；对未加密的历史值直接返回，便于平滑升级。
func (c *Cipher) Open(value string) (string, error) {
	if !strings.HasPrefix(value, sealPrefix) {
		return value, nil
	}
	if c == nil || c.aead == nil {
		return "", errors.New("缺少数据密钥，无法解密")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, sealPrefix))
	if err != nil {
		return "", fmt.Errorf("密文编码非法: %w", err)
	}
	nonceSize := c.aead.NonceSize()
	if len(raw) <= nonceSize {
		return "", errors.New("密文长度不足")
	}
	plaintext, err := c.aead.Open(nil, raw[:nonceSize], raw[nonceSize:], nil)
	if err != nil {
		return "", errors.New("密文校验失败，数据密钥可能已更换")
	}
	return string(plaintext), nil
}

// IsSealed 判断一个字段是否已被加密。
func IsSealed(value string) bool {
	return strings.HasPrefix(value, sealPrefix)
}

// NewSalt 生成一段随机盐并以 base64 返回。
func NewSalt() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成盐失败: %w", err)
	}
	return base64.RawStdEncoding.EncodeToString(buf), nil
}

// DecodeSalt 解析 base64 盐。
func DecodeSalt(value string) ([]byte, error) {
	return base64.RawStdEncoding.DecodeString(value)
}

// ResolveSecret 依次尝试环境变量与密钥文件，必要时生成新的主密钥。
//
// 密钥文件与数据文件分离保存，权限收紧到 0600，避免数据文件本身泄露即等于凭据泄露。
func ResolveSecret(envValue, keyFile string) (string, error) {
	if trimmed := strings.TrimSpace(envValue); trimmed != "" {
		return trimmed, nil
	}
	if keyFile == "" {
		return "", errors.New("未提供主密钥，且没有可用的密钥文件路径")
	}
	raw, err := os.ReadFile(keyFile)
	if err == nil {
		if trimmed := strings.TrimSpace(string(raw)); trimmed != "" {
			return trimmed, nil
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("读取密钥文件失败: %w", err)
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成主密钥失败: %w", err)
	}
	secret := base64.RawURLEncoding.EncodeToString(buf)
	if err := os.MkdirAll(filepath.Dir(keyFile), 0o700); err != nil {
		return "", fmt.Errorf("创建密钥目录失败: %w", err)
	}
	if err := os.WriteFile(keyFile, []byte(secret+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("写入密钥文件失败: %w", err)
	}
	return secret, nil
}
