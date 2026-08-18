package script

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// globalValue 返回沙箱允许的全局对象。
//
// 白名单极窄：只有纯计算能力（Math / Number / String / JSON / Object / Array 的少数方法）。
// 没有网络、文件、时间、随机数与进程访问，脚本因此无法产生任何外部副作用，
// 也无法通过时间或随机性把结果变得不可复现。
func globalValue(name string) (any, bool) {
	switch name {
	case "Math":
		return mathObject(), true
	case "JSON":
		return jsonObject(), true
	case "Object":
		return objectObject(), true
	case "Array":
		return arrayObject(), true
	case "Number":
		return numberObject(), true
	case "String":
		return &builtin{name: "String", fn: func(args []any) (any, error) {
			return toString(argAt(args, 0)), nil
		}}, true
	case "Boolean":
		return &builtin{name: "Boolean", fn: func(args []any) (any, error) {
			return truthy(argAt(args, 0)), nil
		}}, true
	case "parseFloat":
		return &builtin{name: "parseFloat", fn: func(args []any) (any, error) {
			return parseLeadingNumber(toString(argAt(args, 0)), false), nil
		}}, true
	case "parseInt":
		return &builtin{name: "parseInt", fn: func(args []any) (any, error) {
			return parseLeadingNumber(toString(argAt(args, 0)), true), nil
		}}, true
	case "isNaN":
		return &builtin{name: "isNaN", fn: func(args []any) (any, error) {
			return math.IsNaN(toNumber(argAt(args, 0))), nil
		}}, true
	case "NaN":
		return math.NaN(), true
	case "Infinity":
		return math.Inf(1), true
	}
	return nil, false
}

func argAt(args []any, index int) any {
	if index < len(args) {
		return args[index]
	}
	return undefinedVal
}

// parseLeadingNumber 实现 parseFloat / parseInt 的前缀解析语义。
func parseLeadingNumber(raw string, integerOnly bool) float64 {
	trimmed := strings.TrimSpace(raw)
	end := 0
	seenDot := false
	seenDigit := false
	for index := 0; index < len(trimmed); index++ {
		ch := trimmed[index]
		switch {
		case ch == 43 || ch == 45:
			if index != 0 {
				goto done
			}
		case ch >= 48 && ch <= 57:
			seenDigit = true
		case ch == 46 && !integerOnly && !seenDot:
			seenDot = true
		default:
			goto done
		}
		end = index + 1
	}
done:
	if !seenDigit {
		return math.NaN()
	}
	parsed, err := strconv.ParseFloat(strings.TrimSuffix(trimmed[:end], "."), 64)
	if err != nil {
		return math.NaN()
	}
	if integerOnly {
		return math.Trunc(parsed)
	}
	return parsed
}

func mathObject() map[string]any {
	unary := func(name string, fn func(float64) float64) *builtin {
		return &builtin{name: name, fn: func(args []any) (any, error) {
			return fn(toNumber(argAt(args, 0))), nil
		}}
	}
	reduce := func(name string, pick func(float64, float64) float64, seed float64) *builtin {
		return &builtin{name: name, fn: func(args []any) (any, error) {
			result := seed
			for _, item := range args {
				value := toNumber(item)
				if math.IsNaN(value) {
					return math.NaN(), nil
				}
				result = pick(result, value)
			}
			return result, nil
		}}
	}
	return map[string]any{
		"abs":   unary("abs", math.Abs),
		"floor": unary("floor", math.Floor),
		"ceil":  unary("ceil", math.Ceil),
		"trunc": unary("trunc", math.Trunc),
		"sqrt":  unary("sqrt", math.Sqrt),
		"log":   unary("log", math.Log),
		"round": unary("round", func(value float64) float64 {
			return math.Floor(value + 0.5)
		}),
		"pow": &builtin{name: "pow", fn: func(args []any) (any, error) {
			return math.Pow(toNumber(argAt(args, 0)), toNumber(argAt(args, 1))), nil
		}},
		"max": reduce("max", math.Max, math.Inf(-1)),
		"min": reduce("min", math.Min, math.Inf(1)),
	}
}

func jsonObject() map[string]any {
	return map[string]any{
		"parse": &builtin{name: "parse", fn: func(args []any) (any, error) {
			text := toString(argAt(args, 0))
			if len(text) > maxStringLen {
				return nil, errors.New("JSON.parse 输入过长")
			}
			var decoded any
			if err := json.Unmarshal([]byte(text), &decoded); err != nil {
				return nil, fmt.Errorf("JSON.parse 失败: %w", err)
			}
			return normalize(decoded), nil
		}},
		"stringify": &builtin{name: "stringify", fn: func(args []any) (any, error) {
			encoded, err := json.Marshal(exportValue(argAt(args, 0)))
			if err != nil {
				return nil, fmt.Errorf("JSON.stringify 失败: %w", err)
			}
			if len(encoded) > maxStringLen {
				return nil, errors.New("JSON.stringify 结果过长")
			}
			return string(encoded), nil
		}},
	}
}

func objectObject() map[string]any {
	asMap := func(value any) map[string]any {
		if typed, ok := value.(map[string]any); ok {
			return typed
		}
		return map[string]any{}
	}
	return map[string]any{
		"keys": &builtin{name: "keys", fn: func(args []any) (any, error) {
			source := asMap(argAt(args, 0))
			result := []any{}
			for _, key := range sortedKeys(source) {
				result = append(result, key)
			}
			return result, nil
		}},
		"values": &builtin{name: "values", fn: func(args []any) (any, error) {
			source := asMap(argAt(args, 0))
			result := []any{}
			for _, key := range sortedKeys(source) {
				result = append(result, source[key])
			}
			return result, nil
		}},
		"entries": &builtin{name: "entries", fn: func(args []any) (any, error) {
			source := asMap(argAt(args, 0))
			result := []any{}
			for _, key := range sortedKeys(source) {
				result = append(result, []any{key, source[key]})
			}
			return result, nil
		}},
	}
}

func arrayObject() map[string]any {
	return map[string]any{
		"isArray": &builtin{name: "isArray", fn: func(args []any) (any, error) {
			_, ok := argAt(args, 0).([]any)
			return ok, nil
		}},
	}
}

func numberObject() map[string]any {
	return map[string]any{
		"isFinite": &builtin{name: "isFinite", fn: func(args []any) (any, error) {
			value := toNumber(argAt(args, 0))
			return !math.IsNaN(value) && !math.IsInf(value, 0), nil
		}},
		"isNaN": &builtin{name: "isNaN", fn: func(args []any) (any, error) {
			number, ok := argAt(args, 0).(float64)
			return ok && math.IsNaN(number), nil
		}},
		"parseFloat": &builtin{name: "parseFloat", fn: func(args []any) (any, error) {
			return parseLeadingNumber(toString(argAt(args, 0)), false), nil
		}},
	}
}

// getProperty 读取对象属性或返回绑定到该对象的内建方法。
func getProperty(object any, key string) (any, error) {
	switch typed := object.(type) {
	case map[string]any:
		if value, ok := typed[key]; ok {
			return value, nil
		}
		return undefinedVal, nil
	case []any:
		return arrayProperty(typed, key)
	case string:
		return stringProperty(typed, key)
	case float64:
		return numberProperty(typed, key)
	case bool:
		return undefinedVal, nil
	}
	return undefinedVal, nil
}

func arrayProperty(items []any, key string) (any, error) {
	if key == "length" {
		return float64(len(items)), nil
	}
	if index, err := strconv.Atoi(key); err == nil {
		if index < 0 || index >= len(items) {
			return undefinedVal, nil
		}
		return items[index], nil
	}
	switch key {
	case "join":
		return &builtin{name: "join", fn: func(args []any) (any, error) {
			sep := ","
			if len(args) > 0 {
				sep = toString(args[0])
			}
			parts := make([]string, 0, len(items))
			for _, item := range items {
				if isNullish(item) {
					parts = append(parts, "")
					continue
				}
				parts = append(parts, toString(item))
			}
			return strings.Join(parts, sep), nil
		}}, nil
	case "includes":
		return &builtin{name: "includes", fn: func(args []any) (any, error) {
			target := argAt(args, 0)
			for _, item := range items {
				if looseEqual(item, target, true) {
					return true, nil
				}
			}
			return false, nil
		}}, nil
	case "indexOf":
		return &builtin{name: "indexOf", fn: func(args []any) (any, error) {
			target := argAt(args, 0)
			for index, item := range items {
				if looseEqual(item, target, true) {
					return float64(index), nil
				}
			}
			return float64(-1), nil
		}}, nil
	case "slice":
		return &builtin{name: "slice", fn: func(args []any) (any, error) {
			start, end := sliceRange(len(items), args)
			result := make([]any, 0, end-start)
			result = append(result, items[start:end]...)
			return result, nil
		}}, nil
	case "map", "filter", "find", "some", "every", "reduce", "forEach", "sort", "concat", "push":
		// 这些高阶方法需要回调调用能力，由 interp 在 callMethod 中处理。
		return &builtin{name: key, fn: func(args []any) (any, error) {
			return nil, fmt.Errorf("数组方法 %s 需要在表达式中直接调用", key)
		}}, nil
	}
	return undefinedVal, nil
}

// sliceRange 归一化 slice/substring 的起止下标。
func sliceRange(length int, args []any) (int, int) {
	start := 0
	end := length
	if len(args) > 0 && !isNullish(args[0]) {
		start = clampIndex(int(toNumber(args[0])), length)
	}
	if len(args) > 1 && !isNullish(args[1]) {
		end = clampIndex(int(toNumber(args[1])), length)
	}
	if end < start {
		end = start
	}
	return start, end
}

func clampIndex(value, length int) int {
	if value < 0 {
		value += length
	}
	if value < 0 {
		return 0
	}
	if value > length {
		return length
	}
	return value
}

func stringProperty(text string, key string) (any, error) {
	if key == "length" {
		return float64(len([]rune(text))), nil
	}
	switch key {
	case "trim":
		return simpleString("trim", func(args []any) string { return strings.TrimSpace(text) }), nil
	case "toLowerCase":
		return simpleString("toLowerCase", func(args []any) string { return strings.ToLower(text) }), nil
	case "toUpperCase":
		return simpleString("toUpperCase", func(args []any) string { return strings.ToUpper(text) }), nil
	case "includes":
		return &builtin{name: "includes", fn: func(args []any) (any, error) {
			return strings.Contains(text, toString(argAt(args, 0))), nil
		}}, nil
	case "startsWith":
		return &builtin{name: "startsWith", fn: func(args []any) (any, error) {
			return strings.HasPrefix(text, toString(argAt(args, 0))), nil
		}}, nil
	case "endsWith":
		return &builtin{name: "endsWith", fn: func(args []any) (any, error) {
			return strings.HasSuffix(text, toString(argAt(args, 0))), nil
		}}, nil
	case "indexOf":
		return &builtin{name: "indexOf", fn: func(args []any) (any, error) {
			return float64(strings.Index(text, toString(argAt(args, 0)))), nil
		}}, nil
	case "split":
		return &builtin{name: "split", fn: func(args []any) (any, error) {
			sep := toString(argAt(args, 0))
			var parts []string
			if sep == "" {
				for _, ch := range text {
					parts = append(parts, string(ch))
				}
			} else {
				parts = strings.Split(text, sep)
			}
			result := make([]any, 0, len(parts))
			for _, part := range parts {
				result = append(result, part)
			}
			return result, nil
		}}, nil
	case "replace":
		return &builtin{name: "replace", fn: func(args []any) (any, error) {
			return strings.Replace(text, toString(argAt(args, 0)), toString(argAt(args, 1)), 1), nil
		}}, nil
	case "replaceAll":
		return &builtin{name: "replaceAll", fn: func(args []any) (any, error) {
			return strings.ReplaceAll(text, toString(argAt(args, 0)), toString(argAt(args, 1))), nil
		}}, nil
	case "slice", "substring":
		return &builtin{name: key, fn: func(args []any) (any, error) {
			runes := []rune(text)
			start, end := sliceRange(len(runes), args)
			return string(runes[start:end]), nil
		}}, nil
	case "toString":
		return simpleString("toString", func(args []any) string { return text }), nil
	}
	return undefinedVal, nil
}

func simpleString(name string, fn func(args []any) string) *builtin {
	return &builtin{name: name, fn: func(args []any) (any, error) {
		return fn(args), nil
	}}
}

func numberProperty(value float64, key string) (any, error) {
	switch key {
	case "toFixed":
		return &builtin{name: "toFixed", fn: func(args []any) (any, error) {
			digits := 0
			if len(args) > 0 {
				digits = int(toNumber(args[0]))
			}
			if digits < 0 || digits > 20 {
				return nil, errors.New("toFixed 的位数需要在 0-20 之间")
			}
			return strconv.FormatFloat(value, 'f', digits, 64), nil
		}}, nil
	case "toString":
		return simpleString("toString", func(args []any) string { return formatNumber(value) }), nil
	}
	return undefinedVal, nil
}
