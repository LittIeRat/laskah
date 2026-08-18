package script

import (
	"encoding/json"
	"strings"
	"testing"
)

const newAPIScript = `({
  request: {
    url: "{{baseUrl}}/api/user/self",
    method: "GET",
    headers: {
      "Content-Type": "application/json",
      "Authorization": "Bearer {{accessToken}}",
      "User-Agent": "cc-switch/1.0",
      "New-Api-User": "{{userId}}"
    },
  },
  extractor: function (response) {
    if (response.success && response.data) {
      return {
        planName: response.data.group || "默认套餐",
        remaining: response.data.quota / 500000,
        used: response.data.used_quota / 500000,
        total: (response.data.quota + response.data.used_quota) / 500000,
        unit: "USD",
      };
    }
    return {
      isValid: false,
      invalidMessage: response.message || "查询失败"
    };
  },
})`

const usageScript = `({
  request: {
    url: "{{baseUrl}}/api/usage",
    method: "POST",
    headers: {
      "Authorization": "Bearer {{apiKey}}",
      "User-Agent": "cc-switch/1.0"
    }
  },
  extractor: function(response) {
    return {
      isValid: !response.error,
      remaining: response.balance,
      unit: "USD"
    };
  }
})`

func mustParse(t *testing.T, source string) *Program {
	t.Helper()
	program, err := Parse(source)
	if err != nil {
		t.Fatalf("脚本应解析成功: %v", err)
	}
	return program
}

func decode(t *testing.T, raw string) any {
	t.Helper()
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		t.Fatalf("测试数据不是合法 JSON: %v", err)
	}
	return value
}

func TestNewAPIScriptBuildsRequestAndExtracts(t *testing.T) {
	program := mustParse(t, newAPIScript)
	if program.Method() != "GET" {
		t.Fatalf("方法应为 GET: %s", program.Method())
	}

	request, err := program.BuildRequest(map[string]string{
		"baseUrl":     "https://api.newapi.com",
		"accessToken": "tok-123",
		"userId":      "114514",
	})
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	if request.URL != "https://api.newapi.com/api/user/self" {
		t.Fatalf("地址替换错误: %s", request.URL)
	}
	if request.Headers["Authorization"] != "Bearer tok-123" {
		t.Fatalf("令牌未替换: %#v", request.Headers)
	}
	if request.Headers["New-Api-User"] != "114514" {
		t.Fatalf("用户 ID 未替换: %#v", request.Headers)
	}

	result, err := program.Extract(decode(t, `{"success":true,"data":{"group":"vip","quota":2500000,"used_quota":500000}}`))
	if err != nil {
		t.Fatalf("提取失败: %v", err)
	}
	if !result.HasRemaining || result.Remaining != 5 {
		t.Fatalf("剩余额度应为 5: %#v", result)
	}
	if !result.HasUsed || result.Used != 1 {
		t.Fatalf("已用额度应为 1: %#v", result)
	}
	if !result.HasTotal || result.Total != 6 {
		t.Fatalf("总额度应为 6: %#v", result)
	}
	if result.PlanName != "vip" || result.Unit != "USD" {
		t.Fatalf("套餐或单位错误: %#v", result)
	}
	if result.HasValid {
		t.Fatalf("脚本没返回 isValid，不应视为已声明: %#v", result)
	}
}

func TestNewAPIScriptFallbackBranch(t *testing.T) {
	program := mustParse(t, newAPIScript)
	result, err := program.Extract(decode(t, `{"success":false,"message":"令牌无效"}`))
	if err != nil {
		t.Fatalf("提取失败: %v", err)
	}
	if !result.HasValid || result.IsValid {
		t.Fatalf("应判定为失效: %#v", result)
	}
	if result.InvalidMessage != "令牌无效" {
		t.Fatalf("失效原因错误: %#v", result)
	}
	// message 缺失时走 || 的兜底分支。
	fallback, err := program.Extract(decode(t, `{"success":false}`))
	if err != nil {
		t.Fatalf("提取失败: %v", err)
	}
	if fallback.InvalidMessage != "查询失败" {
		t.Fatalf("应使用默认失效原因: %#v", fallback)
	}
}

func TestUsageScriptUsesAPIKey(t *testing.T) {
	program := mustParse(t, usageScript)
	request, err := program.BuildRequest(map[string]string{
		"baseUrl": "https://api.example.com",
		"apiKey":  "sk-abc",
	})
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	if request.Method != "POST" {
		t.Fatalf("方法应为 POST: %s", request.Method)
	}
	if request.Headers["Authorization"] != "Bearer sk-abc" {
		t.Fatalf("密钥未替换: %#v", request.Headers)
	}

	result, err := program.Extract(decode(t, `{"balance":12.5}`))
	if err != nil {
		t.Fatalf("提取失败: %v", err)
	}
	if !result.HasValid || !result.IsValid {
		t.Fatalf("应判定为有效: %#v", result)
	}
	if result.Remaining != 12.5 {
		t.Fatalf("余额错误: %#v", result)
	}
	if result.HasTotal || result.HasUsed {
		t.Fatalf("脚本未返回 total/used，不应视为已声明: %#v", result)
	}
}

func TestScriptSupportsModernSyntax(t *testing.T) {
	source := `({
  request: { url: "{{baseUrl}}/v1/quota", method: "POST", body: { scope: "all" } },
  extractor: (response) => {
    const rows = response?.items ?? [];
    const total = rows.map(row => row.amount).reduce((acc, item) => acc + item, 0);
    let plan = "";
    for (const row of rows) {
      if (row.name) plan = plan ? plan + "," + row.name : row.name;
    }
    return {
      remaining: Math.max(total, 0),
      planName: plan || "空",
      extra: ` + "`共 ${rows.length} 项，单位 ${(total).toFixed(2)}`" + `,
      unit: "USD",
    };
  },
})`
	program := mustParse(t, source)
	request, err := program.BuildRequest(map[string]string{"baseUrl": "https://q.example.com"})
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	if request.Body != `{"scope":"all"}` {
		t.Fatalf("对象 body 应被序列化为 JSON: %s", request.Body)
	}

	result, err := program.Extract(decode(t, `{"items":[{"amount":1.5,"name":"a"},{"amount":2,"name":"b"}]}`))
	if err != nil {
		t.Fatalf("提取失败: %v", err)
	}
	if result.Remaining != 3.5 {
		t.Fatalf("求和错误: %#v", result)
	}
	if result.PlanName != "a,b" {
		t.Fatalf("for-of 拼接错误: %#v", result)
	}
	if result.Extra != "共 2 项，单位 3.50" {
		t.Fatalf("模板字符串错误: %q", result.Extra)
	}

	// 可选链在字段缺失时应安全返回空数组分支。
	empty, err := program.Extract(decode(t, `{}`))
	if err != nil {
		t.Fatalf("提取失败: %v", err)
	}
	if empty.Remaining != 0 || empty.PlanName != "空" {
		t.Fatalf("缺字段时应走兜底: %#v", empty)
	}
}

func TestScriptRejectsDangerousInput(t *testing.T) {
	cases := map[string]string{
		"缺少 request":   `({ extractor: function (r) { return {}; } })`,
		"缺少 extractor": `({ request: { url: "https://a.com" } })`,
		"非法方法":         `({ request: { url: "https://a.com", method: "DELETE" }, extractor: function (r) { return {}; } })`,
		"未知变量":         `({ request: { url: "{{secretToken}}/x" }, extractor: function (r) { return {}; } })`,
		"require":      `({ request: { url: require("fs") }, extractor: function (r) { return {}; } })`,
		"process":      `({ request: { url: process.env.HOME }, extractor: function (r) { return {}; } })`,
		"fetch":        `({ request: { url: "https://a.com" }, extractor: function (r) { return fetch("https://b.com"); } })`,
		"while 循环":     `({ request: { url: "https://a.com" }, extractor: function (r) { while (true) {} } })`,
		"多个语句":         `({ request: { url: "https://a.com" }, extractor: function (r) { return {}; } }); ({})`,
		"空脚本":          "   ",
	}
	for name, source := range cases {
		if _, err := Parse(source); err == nil {
			t.Fatalf("%s 应被拒绝", name)
		}
	}
}

func TestScriptRejectsOversizedSource(t *testing.T) {
	padding := strings.Repeat("/* padding */", MaxSourceBytes)
	if _, err := Parse(padding + `({ request: { url: "https://a.com" }, extractor: function (r) { return {}; } })`); err == nil {
		t.Fatal("超长脚本应被拒绝")
	}
}

func TestScriptStepLimitStopsRunawayLoop(t *testing.T) {
	// for-of 嵌套遍历同一个大数组：语法合法，但求值步数会撞上限。
	source := `({
  request: { url: "https://a.com" },
  extractor: function (response) {
    var total = 0;
    for (const outer of response.items) {
      for (const inner of response.items) {
        total = total + outer * inner;
      }
    }
    return { remaining: total };
  },
})`
	program := mustParse(t, source)
	items := make([]any, 700)
	for index := range items {
		items[index] = float64(index)
	}
	if _, err := program.Extract(map[string]any{"items": items}); err == nil {
		t.Fatal("超出步数上限应报错")
	}
}

func TestScriptRejectsNonHTTPTarget(t *testing.T) {
	program := mustParse(t, `({ request: { url: "{{baseUrl}}/api/usage" }, extractor: function (r) { return {}; } })`)
	if _, err := program.BuildRequest(map[string]string{"baseUrl": "file:///etc"}); err == nil {
		t.Fatal("非 http(s) 地址应被拒绝")
	}
}

func TestScriptExtractIsStateless(t *testing.T) {
	// 顶层对象每次执行都重新构造：脚本内的赋值不能跨调用累积。
	program := mustParse(t, `({
  request: { url: "https://a.com" },
  extractor: function (response) {
    var counter = 0;
    counter = counter + 1;
    return { remaining: counter };
  },
})`)
	for round := 0; round < 3; round++ {
		result, err := program.Extract(map[string]any{})
		if err != nil {
			t.Fatalf("提取失败: %v", err)
		}
		if result.Remaining != 1 {
			t.Fatalf("第 %d 次执行结果应保持一致: %#v", round+1, result)
		}
	}
}
