package store

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"laskah/internal/security"
)

// NewID 生成带前缀的随机标识。
func NewID(prefix string) string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return prefix + "_" + strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return prefix + "_" + hex.EncodeToString(buf)
}

// NewGatewayKey 生成下游调用使用的网关密钥。
func NewGatewayKey(prefix string) string {
	cleaned := sanitizePrefix(prefix)
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return cleaned + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return cleaned + "-" + base64.RawURLEncoding.EncodeToString(buf)
}

func sanitizePrefix(prefix string) string {
	var out strings.Builder
	for _, ch := range prefix {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9', ch == '-', ch == '_':
			out.WriteRune(ch)
		}
	}
	if out.Len() == 0 {
		return "sk-lb"
	}
	return out.String()
}

// SplitList 把逗号、分号、空白分隔的输入统一成字符串切片。
func SplitList(value any) []string {
	result := []string{}
	switch typed := value.(type) {
	case nil:
		return result
	case []string:
		for _, item := range typed {
			if trimmed := strings.TrimSpace(item); trimmed != "" {
				result = append(result, trimmed)
			}
		}
	case []any:
		for _, item := range typed {
			if item == nil {
				continue
			}
			if trimmed := strings.TrimSpace(fmt.Sprint(item)); trimmed != "" {
				result = append(result, trimmed)
			}
		}
	case string:
		for _, part := range strings.FieldsFunc(typed, func(r rune) bool {
			return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
		}) {
			if trimmed := strings.TrimSpace(part); trimmed != "" {
				result = append(result, trimmed)
			}
		}
	default:
		if trimmed := strings.TrimSpace(fmt.Sprint(typed)); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func toFloat(value any, fallback float64) (float64, bool) {
	switch typed := value.(type) {
	case nil:
		return fallback, true
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return fallback, true
		}
		parsed, err := strconv.ParseFloat(trimmed, 64)
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

// BuildProvider 校验输入并生成规范化的提供商对象。
func BuildProvider(input ProviderInput) (*Provider, *ValidationError) {
	verr := &ValidationError{}

	rawBase := firstNonEmpty(input.BaseURL, input.BaseURLSnake, input.URLField, input.Endpoint)
	baseURL := NormalizeBaseURL(rawBase)
	if baseURL == "" {
		verr.Errorf("baseUrl 不能为空")
	} else if parsed, err := url.Parse(baseURL); err != nil || parsed.Host == "" {
		verr.Errorf("baseUrl 格式无效: %s", rawBase)
	}

	providerType := strings.ToLower(strings.TrimSpace(input.Type))
	if providerType == "" {
		providerType = string(TypeOpenAI)
	}
	if !ValidProviderType(providerType) {
		verr.Errorf("type 必须是 openai / anthropic / gemini")
	}

	weight, ok := toFloat(input.Weight, 1)
	if !ok || weight <= 0 {
		verr.Errorf("weight 必须是正数")
		weight = 1
	}

	priorityValue, ok := toFloat(input.Priority, 0)
	if !ok {
		verr.Errorf("priority 必须是数字")
	}

	timeout, ok := toFloat(input.TimeoutMS, 120000)
	if !ok || timeout < 1000 {
		verr.Errorf("timeoutMs 至少为 1000")
		timeout = 120000
	}

	if verr.HasErrors() {
		return nil, verr
	}

	models := SplitList(input.Models)
	if len(models) == 0 {
		models = SplitList(input.ModelField)
	}

	name := strings.TrimSpace(input.Name)
	if name == "" {
		name = HostLabel(baseURL)
	}

	apiKey := strings.TrimSpace(firstNonEmpty(input.APIKey, input.APIKeySnake, input.KeyField, input.TokenField))

	now := time.Now().UTC()
	paths := DefaultPaths(ProviderType(providerType))
	if input.Paths != nil {
		if strings.TrimSpace(input.Paths.Chat) != "" {
			paths.Chat = strings.TrimSpace(input.Paths.Chat)
		}
		if strings.TrimSpace(input.Paths.Models) != "" {
			paths.Models = strings.TrimSpace(input.Paths.Models)
		}
	}

	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}

	provider := &Provider{
		ID:        firstNonEmpty(input.ID, NewID("prov")),
		AccountID: strings.TrimSpace(input.AccountID),
		Name:      name,
		Type:      ProviderType(providerType),
		BaseURL:   baseURL,
		APIKey:    apiKey,
		Models:    models,
		ModelMap:  copyStringMap(input.ModelMap),
		Headers:   copyStringMap(input.Headers),
		Paths:     paths,
		Weight:    weight,
		Priority:  int(priorityValue),
		TimeoutMS: int64(timeout),
		Enabled:   enabled,
		Tags:      SplitList(input.Tags),
		Note:      strings.TrimSpace(input.Note),
		CreatedAt: now,
		UpdatedAt: now,
	}
	return provider, nil
}

// BuildKey 校验输入并生成网关密钥。
func BuildKey(input KeyInput) (*APIKey, *ValidationError) {
	verr := &ValidationError{}

	name := strings.TrimSpace(input.Name)
	if name == "" {
		name = "key"
	}

	var quota *int64
	if !isBlank(input.QuotaTokens) {
		value, ok := toFloat(input.QuotaTokens, 0)
		if !ok || value < 0 {
			verr.Errorf("quotaTokens 必须是非负数字")
		} else {
			converted := int64(value)
			quota = &converted
		}
	}

	var rateLimit *int
	if !isBlank(input.RateLimitPerMin) {
		value, ok := toFloat(input.RateLimitPerMin, 0)
		if !ok || value <= 0 {
			verr.Errorf("rateLimitPerMin 必须是正数")
		} else {
			converted := int(value)
			rateLimit = &converted
		}
	}

	var expiresAt *time.Time
	if trimmed := strings.TrimSpace(input.ExpiresAt); trimmed != "" {
		parsed, err := parseTime(trimmed)
		if err != nil {
			verr.Errorf("expiresAt 时间格式无效: %s", trimmed)
		} else {
			utc := parsed.UTC()
			expiresAt = &utc
		}
	}

	if verr.HasErrors() {
		return nil, verr
	}

	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}

	now := time.Now().UTC()
	secret := NewGatewayKey(input.Prefix)
	return &APIKey{
		ID:              firstNonEmpty(input.ID, NewID("key")),
		AccountID:       strings.TrimSpace(input.AccountID),
		GroupID:         strings.TrimSpace(input.GroupID),
		Name:            name,
		Key:             secret,
		KeyHash:         security.HashToken(secret),
		KeyMasked:       MaskKey(secret),
		Enabled:         enabled,
		AllowedModels:   SplitList(input.AllowedModels),
		ProviderIDs:     SplitList(input.ProviderIDs),
		Tags:            SplitList(input.Tags),
		QuotaTokens:     quota,
		RateLimitPerMin: rateLimit,
		ExpiresAt:       expiresAt,
		Note:            strings.TrimSpace(input.Note),
		CreatedAt:       now,
		UpdatedAt:       now,
	}, nil
}

// BuildKeyBatch 按模板批量生成密钥，名称自动加序号。
func BuildKeyBatch(count int, template KeyInput) ([]*APIKey, *ValidationError) {
	verr := &ValidationError{}
	if count < 1 || count > 500 {
		verr.Errorf("count 需要是 1-500 的整数")
		return nil, verr
	}
	baseName := strings.TrimSpace(template.Name)
	if baseName == "" {
		baseName = "key"
	}
	width := len(strconv.Itoa(count))
	keys := make([]*APIKey, 0, count)
	for index := 1; index <= count; index++ {
		item := template
		item.ID = ""
		if count == 1 {
			item.Name = baseName
		} else {
			item.Name = fmt.Sprintf("%s-%0*d", baseName, width, index)
		}
		built, buildErr := BuildKey(item)
		if buildErr != nil {
			return nil, buildErr
		}
		keys = append(keys, built)
	}
	return keys, nil
}

func parseTime(value string) (time.Time, error) {
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("无法解析时间: %s", value)
}

func isBlank(value any) bool {
	if value == nil {
		return true
	}
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text) == ""
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func copyStringMap(source map[string]string) map[string]string {
	result := map[string]string{}
	for key, value := range source {
		trimmedKey := strings.TrimSpace(key)
		if trimmedKey == "" {
			continue
		}
		result[trimmedKey] = strings.TrimSpace(value)
	}
	return result
}
