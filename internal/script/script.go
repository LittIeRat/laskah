package script

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// MaxSourceBytes 限制脚本源码体积。
//
// 额度脚本本身只有几十行，给到 16 KiB 已经非常宽裕；
// 设上限是为了让解析开销与内存占用都有确定的天花板。
const MaxSourceBytes = 16 << 10

// Placeholders 是脚本里可用的模板变量。
//
// 与 cc-switch 的写法保持一致：{{baseUrl}}、{{apiKey}}、{{accessToken}}、{{userId}}。
// 变量在发请求前替换，脚本本身拿不到凭据明文，也无法把它们拼进任意表达式求值。
var Placeholders = []string{"baseUrl", "apiKey", "accessToken", "userId"}

var placeholderPattern = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_]+)\s*\}\}`)

// Request 是脚本声明的一次 HTTP 请求。
type Request struct {
	URL     string
	Method  string
	Headers map[string]string
	Body    string
}

// Result 是 extractor 的返回值，字段全部可选。
type Result struct {
	HasValid       bool
	IsValid        bool
	InvalidMessage string

	HasRemaining bool
	Remaining    float64

	HasTotal bool
	Total    float64

	HasUsed bool
	Used    float64

	Unit     string
	PlanName string
	Extra    string
}

// Program 是一段已通过校验的额度查询脚本。
//
// 只保存源码与语法树：每次使用都在全新作用域里重新求值，
// 因此多个账号并发查询不会共享任何可变状态。
type Program struct {
	source string
	ast    node
	method string
}

// Source 返回原始脚本源码。
func (p *Program) Source() string { return p.source }

// Method 返回脚本声明的 HTTP 方法，便于界面回显。
func (p *Program) Method() string { return p.method }

// Parse 解析并校验脚本，返回可执行的 Program。
//
// 校验会真正执行一次顶层表达式（不含 extractor 调用），因此语法错误、
// 缺字段、方法非法与未知模板变量都能在保存前暴露出来。
func Parse(source string) (*Program, error) {
	trimmed := strings.TrimSpace(source)
	if trimmed == "" {
		return nil, errors.New("脚本内容为空")
	}
	if len(trimmed) > MaxSourceBytes {
		return nil, fmt.Errorf("脚本长度超过上限 %d 字节", MaxSourceBytes)
	}

	ast, err := parseProgram(trimmed)
	if err != nil {
		return nil, err
	}

	config, err := evalConfig(ast)
	if err != nil {
		return nil, err
	}
	requestSpec, err := readRequest(config)
	if err != nil {
		return nil, err
	}
	if _, ok := config["extractor"]; !ok {
		return nil, errors.New("脚本缺少 extractor 函数")
	}
	if _, ok := config["extractor"].(*callable); !ok {
		return nil, errors.New("extractor 必须是函数")
	}
	if err := checkPlaceholders(requestSpec); err != nil {
		return nil, err
	}
	return &Program{source: trimmed, ast: ast, method: requestSpec.Method}, nil
}

// evalConfig 求值顶层表达式，要求结果是对象字面量。
func evalConfig(ast node) (map[string]any, error) {
	in := &interp{}
	value, err := in.eval(ast, newScope(nil))
	if err != nil {
		return nil, err
	}
	config, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("脚本必须求值为对象字面量，例如 ({ request: {...}, extractor: function (response) {...} })")
	}
	return config, nil
}

var allowedMethods = map[string]bool{"GET": true, "POST": true, "PUT": true, "PATCH": true, "HEAD": true}

// readRequest 从配置对象里取出 request 段。
func readRequest(config map[string]any) (Request, error) {
	raw, ok := config["request"]
	if !ok {
		return Request{}, errors.New("脚本缺少 request 配置")
	}
	spec, ok := raw.(map[string]any)
	if !ok {
		return Request{}, errors.New("request 必须是对象")
	}

	target := strings.TrimSpace(toString(orEmpty(spec["url"])))
	if target == "" {
		return Request{}, errors.New("request.url 不能为空")
	}

	method := strings.ToUpper(strings.TrimSpace(toString(orEmpty(spec["method"]))))
	if method == "" {
		method = "GET"
	}
	if !allowedMethods[method] {
		return Request{}, fmt.Errorf("不支持的请求方法 %s（仅 GET/POST/PUT/PATCH/HEAD）", method)
	}

	headers := map[string]string{}
	if rawHeaders, exists := spec["headers"]; exists && !isNullish(rawHeaders) {
		mapped, ok := rawHeaders.(map[string]any)
		if !ok {
			return Request{}, errors.New("request.headers 必须是对象")
		}
		for _, key := range sortedKeys(mapped) {
			name := strings.TrimSpace(key)
			if name == "" {
				continue
			}
			headers[name] = toString(orEmpty(mapped[key]))
		}
	}

	body := ""
	if rawBody, exists := spec["body"]; exists && !isNullish(rawBody) {
		encoded, err := encodeBody(rawBody)
		if err != nil {
			return Request{}, err
		}
		body = encoded
	}

	return Request{URL: target, Method: method, Headers: headers, Body: body}, nil
}

// encodeBody 把 body 归一化成请求正文文本。
func encodeBody(raw any) (string, error) {
	if text, ok := raw.(string); ok {
		return text, nil
	}
	encoder, _ := jsonObject()["stringify"].(*builtin)
	value, err := encoder.fn([]any{raw})
	if err != nil {
		return "", err
	}
	return toString(value), nil
}

func orEmpty(value any) any {
	if isNullish(value) {
		return ""
	}
	return value
}

// checkPlaceholders 拒绝未知的 {{变量}}，避免上线后才发现地址里带着没被替换的花括号。
func checkPlaceholders(request Request) error {
	known := map[string]bool{}
	for _, name := range Placeholders {
		known[name] = true
	}
	unknown := map[string]bool{}
	scan := func(text string) {
		for _, match := range placeholderPattern.FindAllStringSubmatch(text, -1) {
			if !known[match[1]] {
				unknown[match[1]] = true
			}
		}
	}
	scan(request.URL)
	scan(request.Body)
	for _, value := range request.Headers {
		scan(value)
	}
	if len(unknown) == 0 {
		return nil
	}
	names := make([]string, 0, len(unknown))
	for name := range unknown {
		names = append(names, name)
	}
	sort.Strings(names)
	return fmt.Errorf("未知的模板变量 {{%s}}，可用变量: %s",
		strings.Join(names, "}}、{{"), strings.Join(Placeholders, " / "))
}

// BuildRequest 用给定变量替换模板并返回可发送的请求。
//
// 替换后强制校验为 http(s) 绝对地址：脚本可以自由拼地址，但不能把请求
// 引到 file:// 之类的协议上。
func (p *Program) BuildRequest(vars map[string]string) (Request, error) {
	config, err := evalConfig(p.ast)
	if err != nil {
		return Request{}, err
	}
	request, err := readRequest(config)
	if err != nil {
		return Request{}, err
	}

	request.URL = substitute(request.URL, vars)
	request.Body = substitute(request.Body, vars)
	for name, value := range request.Headers {
		request.Headers[name] = substitute(value, vars)
	}

	parsed, parseErr := url.Parse(strings.TrimSpace(request.URL))
	if parseErr != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return Request{}, fmt.Errorf("request.url 必须是完整的 http(s) 地址，当前为: %s", request.URL)
	}
	request.URL = parsed.String()
	return request, nil
}

func substitute(text string, vars map[string]string) string {
	if text == "" || !strings.Contains(text, "{{") {
		return text
	}
	return placeholderPattern.ReplaceAllStringFunc(text, func(match string) string {
		name := placeholderPattern.FindStringSubmatch(match)[1]
		if value, ok := vars[name]; ok {
			return value
		}
		return ""
	})
}

// Extract 调用 extractor 解析上游响应。
//
// response 直接来自 json.Unmarshal 的结果；上游返回非对象（如数组或裸字符串）也照样传入，
// 由脚本自己判断。执行受步数与字符串长度上限约束，因此不会拖住调用方。
func (p *Program) Extract(response any) (Result, error) {
	config, err := evalConfig(p.ast)
	if err != nil {
		return Result{}, err
	}
	extractor, ok := config["extractor"].(*callable)
	if !ok {
		return Result{}, errors.New("extractor 必须是函数")
	}

	in := &interp{}
	value, err := in.invoke(extractor, []any{normalize(response)})
	if err != nil {
		return Result{}, err
	}
	mapped, ok := value.(map[string]any)
	if !ok {
		return Result{}, errors.New("extractor 必须返回对象")
	}
	return readResult(mapped), nil
}

func readResult(mapped map[string]any) Result {
	result := Result{}
	if raw, ok := mapped["isValid"]; ok && !isNullish(raw) {
		result.HasValid = true
		result.IsValid = truthy(raw)
	}
	if raw, ok := mapped["invalidMessage"]; ok && !isNullish(raw) {
		result.InvalidMessage = strings.TrimSpace(toString(raw))
	}
	if value, ok := numberField(mapped, "remaining"); ok {
		result.HasRemaining = true
		result.Remaining = value
	}
	if value, ok := numberField(mapped, "total"); ok {
		result.HasTotal = true
		result.Total = value
	}
	if value, ok := numberField(mapped, "used"); ok {
		result.HasUsed = true
		result.Used = value
	}
	if raw, ok := mapped["unit"]; ok && !isNullish(raw) {
		result.Unit = strings.TrimSpace(toString(raw))
	}
	if raw, ok := mapped["planName"]; ok && !isNullish(raw) {
		result.PlanName = strings.TrimSpace(toString(raw))
	}
	if raw, ok := mapped["extra"]; ok && !isNullish(raw) {
		result.Extra = strings.TrimSpace(toString(raw))
	}
	return result
}

// numberField 读取数字字段，NaN 与无穷视为未提供。
func numberField(mapped map[string]any, key string) (float64, bool) {
	raw, ok := mapped[key]
	if !ok || isNullish(raw) {
		return 0, false
	}
	value := toNumber(raw)
	if value != value || value > 1e18 || value < -1e18 {
		return 0, false
	}
	return value, true
}
