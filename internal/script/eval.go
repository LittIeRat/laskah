package script

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// maxSteps 限制单次执行的求值步数。
//
// 解释器没有 while 与计数循环，但 for-of 嵌套仍可能放大工作量；
// 步数上限保证脚本一定会在有界时间内结束，不会拖住额度查询。
const maxSteps = 200000

// maxStringLen 限制字符串拼接结果长度，避免内存被脚本撑爆。
const maxStringLen = 1 << 20

// undefined 是 JS undefined 的内部表示。
//
// 用独立类型区分 undefined 与 null：extractor 常写 `response.data.group || "默认"`，
// 两者都要按 falsy 处理，但 JSON 序列化时 undefined 字段应当消失而 null 应当保留。
type undefinedValue struct{}

var undefinedVal = undefinedValue{}

// callable 是脚本内定义的函数。
type callable struct {
	params []string
	body   []stmt
	expr   node
	scope  *scope
}

// builtin 是宿主提供的原生函数。
type builtin struct {
	name string
	fn   func(args []any) (any, error)
}

type scope struct {
	vars   map[string]any
	parent *scope
}

func newScope(parent *scope) *scope {
	return &scope{vars: map[string]any{}, parent: parent}
}

func (s *scope) lookup(name string) (any, bool) {
	for cursor := s; cursor != nil; cursor = cursor.parent {
		if value, ok := cursor.vars[name]; ok {
			return value, true
		}
	}
	return nil, false
}

func (s *scope) assign(name string, value any) bool {
	for cursor := s; cursor != nil; cursor = cursor.parent {
		if _, ok := cursor.vars[name]; ok {
			cursor.vars[name] = value
			return true
		}
	}
	return false
}

// interp 是一次执行的解释器状态。
type interp struct {
	steps int
}

// returnSignal 承载 return 语句的值，仅在解释器内部流转。
type returnSignal struct{ value any }

func (r *returnSignal) Error() string { return "return" }

func (in *interp) tick() error {
	in.steps++
	if in.steps > maxSteps {
		return fmt.Errorf("脚本执行步数超过上限 %d，已终止", maxSteps)
	}
	return nil
}

func (in *interp) eval(n node, sc *scope) (any, error) {
	if err := in.tick(); err != nil {
		return nil, err
	}
	switch typed := n.(type) {
	case *numLit:
		return typed.value, nil
	case *strLit:
		return typed.value, nil
	case *boolLit:
		return typed.value, nil
	case *nullLit:
		return nil, nil
	case *undefLit:
		return undefinedVal, nil
	case *tmplLit:
		return in.evalTemplate(typed, sc)
	case *ident:
		if value, ok := sc.lookup(typed.name); ok {
			return value, nil
		}
		if value, ok := globalValue(typed.name); ok {
			return value, nil
		}
		return nil, fmt.Errorf("未定义的变量: %s", typed.name)
	case *objLit:
		result := map[string]any{}
		for index, key := range typed.keys {
			value, err := in.eval(typed.values[index], sc)
			if err != nil {
				return nil, err
			}
			result[key] = value
		}
		return result, nil
	case *arrLit:
		result := make([]any, 0, len(typed.items))
		for _, item := range typed.items {
			value, err := in.eval(item, sc)
			if err != nil {
				return nil, err
			}
			result = append(result, value)
		}
		return result, nil
	case *fnLit:
		return &callable{params: typed.params, body: typed.body, expr: typed.expr, scope: sc}, nil
	case *unary:
		return in.evalUnary(typed, sc)
	case *binary:
		return in.evalBinary(typed, sc)
	case *logical:
		return in.evalLogical(typed, sc)
	case *cond:
		test, err := in.eval(typed.test, sc)
		if err != nil {
			return nil, err
		}
		if truthy(test) {
			return in.eval(typed.cons, sc)
		}
		return in.eval(typed.alt, sc)
	case *member:
		value, _, err := in.evalMember(typed, sc)
		return value, err
	case *call:
		return in.evalCall(typed, sc)
	case *assign:
		return in.evalAssign(typed, sc)
	}
	return nil, errors.New("不支持的表达式")
}

func (in *interp) evalTemplate(t *tmplLit, sc *scope) (any, error) {
	var out strings.Builder
	for index, chunk := range t.chunks {
		out.WriteString(chunk)
		if index < len(t.exprs) {
			value, err := in.eval(t.exprs[index], sc)
			if err != nil {
				return nil, err
			}
			out.WriteString(toString(value))
		}
		if out.Len() > maxStringLen {
			return nil, errors.New("字符串长度超过上限")
		}
	}
	return out.String(), nil
}

func (in *interp) evalUnary(u *unary, sc *scope) (any, error) {
	arg, err := in.eval(u.arg, sc)
	if err != nil {
		return nil, err
	}
	switch u.op {
	case "!":
		return !truthy(arg), nil
	case "-":
		return -toNumber(arg), nil
	case "+":
		return toNumber(arg), nil
	case "typeof":
		return typeOf(arg), nil
	}
	return nil, fmt.Errorf("不支持的一元运算符 %s", u.op)
}

func (in *interp) evalLogical(l *logical, sc *scope) (any, error) {
	left, err := in.eval(l.left, sc)
	if err != nil {
		return nil, err
	}
	switch l.op {
	case "&&":
		if !truthy(left) {
			return left, nil
		}
	case "||":
		if truthy(left) {
			return left, nil
		}
	case "??":
		if !isNullish(left) {
			return left, nil
		}
	}
	return in.eval(l.right, sc)
}

func (in *interp) evalBinary(b *binary, sc *scope) (any, error) {
	left, err := in.eval(b.left, sc)
	if err != nil {
		return nil, err
	}
	right, err := in.eval(b.right, sc)
	if err != nil {
		return nil, err
	}
	switch b.op {
	case "+":
		_, leftIsString := left.(string)
		_, rightIsString := right.(string)
		if leftIsString || rightIsString {
			joined := toString(left) + toString(right)
			if len(joined) > maxStringLen {
				return nil, errors.New("字符串长度超过上限")
			}
			return joined, nil
		}
		return toNumber(left) + toNumber(right), nil
	case "-":
		return toNumber(left) - toNumber(right), nil
	case "*":
		return toNumber(left) * toNumber(right), nil
	case "/":
		return toNumber(left) / toNumber(right), nil
	case "%":
		return math.Mod(toNumber(left), toNumber(right)), nil
	case "===", "==":
		return looseEqual(left, right, b.op == "==="), nil
	case "!==", "!=":
		return !looseEqual(left, right, b.op == "!=="), nil
	case "<", "<=", ">", ">=":
		return compare(left, right, b.op), nil
	}
	return nil, fmt.Errorf("不支持的运算符 %s", b.op)
}

// evalMember 求值属性访问，同时返回宿主对象供方法调用绑定 this。
func (in *interp) evalMember(m *member, sc *scope) (any, any, error) {
	object, err := in.eval(m.object, sc)
	if err != nil {
		return nil, nil, err
	}
	if isNullish(object) {
		if m.optional {
			return undefinedVal, nil, nil
		}
		return nil, nil, fmt.Errorf("无法读取 %s 的属性: 值为 %s", describeTarget(m), toString(object))
	}
	key := m.name
	if m.computed {
		computed, err := in.eval(m.prop, sc)
		if err != nil {
			return nil, nil, err
		}
		key = toString(computed)
	}
	value, err := getProperty(object, key)
	if err != nil {
		return nil, nil, err
	}
	return value, object, nil
}

func describeTarget(m *member) string {
	if m.computed {
		return "计算属性"
	}
	return m.name
}

func (in *interp) evalCall(c *call, sc *scope) (any, error) {
	var (
		callee  any
		thisArg any
		err     error
	)
	if asMember, ok := c.callee.(*member); ok {
		callee, thisArg, err = in.evalMember(asMember, sc)
	} else {
		callee, err = in.eval(c.callee, sc)
	}
	if err != nil {
		return nil, err
	}
	if isNullish(callee) {
		if c.optional {
			return undefinedVal, nil
		}
		return nil, errors.New("调用了不存在的函数")
	}
	args := make([]any, 0, len(c.args))
	for _, item := range c.args {
		value, err := in.eval(item, sc)
		if err != nil {
			return nil, err
		}
		args = append(args, value)
	}
	if items, ok := thisArg.([]any); ok {
		if asBuiltin, isBuiltin := callee.(*builtin); isBuiltin {
			if handled, result, err := in.arrayMethod(items, asBuiltin.name, args); handled {
				return result, err
			}
		}
	}
	return in.invoke(callee, args)
}

// invoke 调用脚本函数或宿主内建函数。
func (in *interp) invoke(callee any, args []any) (any, error) {
	if err := in.tick(); err != nil {
		return nil, err
	}
	switch fn := callee.(type) {
	case *builtin:
		return fn.fn(args)
	case *callable:
		local := newScope(fn.scope)
		for index, name := range fn.params {
			if index < len(args) {
				local.vars[name] = args[index]
				continue
			}
			local.vars[name] = undefinedVal
		}
		if fn.expr != nil {
			return in.eval(fn.expr, local)
		}
		if err := in.execBlock(fn.body, local); err != nil {
			signal := &returnSignal{}
			if errors.As(err, &signal) {
				return signal.value, nil
			}
			return nil, err
		}
		return undefinedVal, nil
	}
	return nil, errors.New("目标不是可调用的函数")
}

func (in *interp) evalAssign(a *assign, sc *scope) (any, error) {
	value, err := in.eval(a.value, sc)
	if err != nil {
		return nil, err
	}
	if a.op != "=" {
		current, err := in.eval(a.target, sc)
		if err != nil {
			return nil, err
		}
		combined, err := in.evalBinaryValues(strings.TrimSuffix(a.op, "="), current, value)
		if err != nil {
			return nil, err
		}
		value = combined
	}
	switch target := a.target.(type) {
	case *ident:
		if !sc.assign(target.name, value) {
			return nil, fmt.Errorf("未声明的变量: %s", target.name)
		}
		return value, nil
	case *member:
		object, err := in.eval(target.object, sc)
		if err != nil {
			return nil, err
		}
		key := target.name
		if target.computed {
			computed, err := in.eval(target.prop, sc)
			if err != nil {
				return nil, err
			}
			key = toString(computed)
		}
		switch holder := object.(type) {
		case map[string]any:
			holder[key] = value
			return value, nil
		case []any:
			index, convErr := strconv.Atoi(key)
			if convErr != nil || index < 0 || index >= len(holder) {
				return nil, errors.New("数组下标越界")
			}
			holder[index] = value
			return value, nil
		}
		return nil, errors.New("只能给对象或数组元素赋值")
	}
	return nil, errors.New("赋值目标无效")
}

// evalBinaryValues 对已求值的两个操作数应用二元运算，供复合赋值复用。
func (in *interp) evalBinaryValues(op string, left, right any) (any, error) {
	switch op {
	case "+":
		_, leftIsString := left.(string)
		_, rightIsString := right.(string)
		if leftIsString || rightIsString {
			joined := toString(left) + toString(right)
			if len(joined) > maxStringLen {
				return nil, errors.New("字符串长度超过上限")
			}
			return joined, nil
		}
		return toNumber(left) + toNumber(right), nil
	case "-":
		return toNumber(left) - toNumber(right), nil
	case "*":
		return toNumber(left) * toNumber(right), nil
	case "/":
		return toNumber(left) / toNumber(right), nil
	case "%":
		return math.Mod(toNumber(left), toNumber(right)), nil
	}
	return nil, fmt.Errorf("不支持的复合赋值 %s=", op)
}

func (in *interp) execBlock(body []stmt, sc *scope) error {
	for _, item := range body {
		if err := in.exec(item, sc); err != nil {
			return err
		}
	}
	return nil
}

func (in *interp) exec(s stmt, sc *scope) error {
	if err := in.tick(); err != nil {
		return err
	}
	switch typed := s.(type) {
	case *blockStmt:
		return in.execBlock(typed.body, newScope(sc))
	case *exprStmt:
		_, err := in.eval(typed.expr, sc)
		return err
	case *varDecl:
		for index, name := range typed.names {
			value := any(undefinedVal)
			if typed.inits[index] != nil {
				evaluated, err := in.eval(typed.inits[index], sc)
				if err != nil {
					return err
				}
				value = evaluated
			}
			sc.vars[name] = value
		}
		return nil
	case *retStmt:
		value := any(undefinedVal)
		if typed.arg != nil {
			evaluated, err := in.eval(typed.arg, sc)
			if err != nil {
				return err
			}
			value = evaluated
		}
		return &returnSignal{value: value}
	case *ifStmt:
		test, err := in.eval(typed.test, sc)
		if err != nil {
			return err
		}
		if truthy(test) {
			return in.exec(typed.cons, newScope(sc))
		}
		if typed.alt != nil {
			return in.exec(typed.alt, newScope(sc))
		}
		return nil
	case *forOfStmt:
		return in.execForOf(typed, sc)
	}
	return errors.New("不支持的语句")
}

func (in *interp) execForOf(f *forOfStmt, sc *scope) error {
	iterable, err := in.eval(f.iter, sc)
	if err != nil {
		return err
	}
	items, err := toIterable(iterable)
	if err != nil {
		return err
	}
	for _, item := range items {
		if err := in.tick(); err != nil {
			return err
		}
		local := newScope(sc)
		local.vars[f.name] = item
		if err := in.exec(f.body, local); err != nil {
			return err
		}
	}
	return nil
}

func toIterable(value any) ([]any, error) {
	switch typed := value.(type) {
	case []any:
		return typed, nil
	case string:
		items := make([]any, 0, len(typed))
		for _, ch := range typed {
			items = append(items, string(ch))
		}
		return items, nil
	}
	return nil, errors.New("只能遍历数组或字符串")
}

// truthy 实现 JS 的真值判定。
func truthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case undefinedValue:
		return false
	case bool:
		return typed
	case float64:
		return typed != 0 && !math.IsNaN(typed)
	case string:
		return typed != ""
	}
	return true
}

func isNullish(value any) bool {
	if value == nil {
		return true
	}
	_, undef := value.(undefinedValue)
	return undef
}

func typeOf(value any) string {
	switch value.(type) {
	case nil:
		return "object"
	case undefinedValue:
		return "undefined"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case string:
		return "string"
	case *callable, *builtin:
		return "function"
	}
	return "object"
}

// toNumber 实现 JS 的数字转换，无法转换时得到 NaN。
func toNumber(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case bool:
		if typed {
			return 1
		}
		return 0
	case nil:
		return 0
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return 0
		}
		parsed, err := strconv.ParseFloat(trimmed, 64)
		if err != nil {
			return math.NaN()
		}
		return parsed
	}
	return math.NaN()
}

// toString 实现 JS 的字符串转换。
func toString(value any) string {
	switch typed := value.(type) {
	case nil:
		return "null"
	case undefinedValue:
		return "undefined"
	case bool:
		if typed {
			return "true"
		}
		return "false"
	case float64:
		return formatNumber(typed)
	case string:
		return typed
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			if isNullish(item) {
				parts = append(parts, "")
				continue
			}
			parts = append(parts, toString(item))
		}
		return strings.Join(parts, ",")
	case *callable, *builtin:
		return "function"
	}
	return "[object Object]"
}

func formatNumber(value float64) string {
	switch {
	case math.IsNaN(value):
		return "NaN"
	case math.IsInf(value, 1):
		return "Infinity"
	case math.IsInf(value, -1):
		return "-Infinity"
	case value == math.Trunc(value) && math.Abs(value) < 1e21:
		return strconv.FormatFloat(value, 'f', -1, 64)
	}
	return strconv.FormatFloat(value, 'g', -1, 64)
}

func looseEqual(left, right any, strict bool) bool {
	if isNullish(left) || isNullish(right) {
		if strict {
			_, leftUndef := left.(undefinedValue)
			_, rightUndef := right.(undefinedValue)
			return leftUndef == rightUndef && isNullish(left) && isNullish(right)
		}
		return isNullish(left) && isNullish(right)
	}
	if strict && typeOf(left) != typeOf(right) {
		return false
	}
	switch leftTyped := left.(type) {
	case string:
		if rightTyped, ok := right.(string); ok {
			return leftTyped == rightTyped
		}
		if strict {
			return false
		}
		return toNumber(left) == toNumber(right)
	case float64:
		return leftTyped == toNumber(right)
	case bool:
		if rightTyped, ok := right.(bool); ok {
			return leftTyped == rightTyped
		}
		if strict {
			return false
		}
		return toNumber(left) == toNumber(right)
	}
	// 对象与数组按引用比较，与 JS 一致。
	return sameReference(left, right)
}

func sameReference(left, right any) bool {
	leftMap, leftIsMap := left.(map[string]any)
	rightMap, rightIsMap := right.(map[string]any)
	if leftIsMap && rightIsMap {
		if len(leftMap) == 0 && len(rightMap) == 0 {
			return false
		}
		return fmt.Sprintf("%p", leftMap) == fmt.Sprintf("%p", rightMap)
	}
	leftSlice, leftIsSlice := left.([]any)
	rightSlice, rightIsSlice := right.([]any)
	if leftIsSlice && rightIsSlice {
		if len(leftSlice) == 0 && len(rightSlice) == 0 {
			return false
		}
		return fmt.Sprintf("%p", leftSlice) == fmt.Sprintf("%p", rightSlice)
	}
	return left == right
}

func compare(left, right any, op string) bool {
	leftStr, leftIsString := left.(string)
	rightStr, rightIsString := right.(string)
	if leftIsString && rightIsString {
		switch op {
		case "<":
			return leftStr < rightStr
		case "<=":
			return leftStr <= rightStr
		case ">":
			return leftStr > rightStr
		default:
			return leftStr >= rightStr
		}
	}
	leftNum, rightNum := toNumber(left), toNumber(right)
	if math.IsNaN(leftNum) || math.IsNaN(rightNum) {
		return false
	}
	switch op {
	case "<":
		return leftNum < rightNum
	case "<=":
		return leftNum <= rightNum
	case ">":
		return leftNum > rightNum
	default:
		return leftNum >= rightNum
	}
}

// normalize 把 encoding/json 解出的值转成解释器使用的类型。
func normalize(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			result[key] = normalize(item)
		}
		return result
	case []any:
		result := make([]any, 0, len(typed))
		for _, item := range typed {
			result = append(result, normalize(item))
		}
		return result
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return typed.String()
		}
		return parsed
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	case float32:
		return float64(typed)
	}
	return value
}

// exportValue 把解释器的值转成可 JSON 序列化的普通 Go 值。
func exportValue(value any) any {
	switch typed := value.(type) {
	case undefinedValue:
		return nil
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			if _, undef := item.(undefinedValue); undef {
				continue
			}
			result[key] = exportValue(item)
		}
		return result
	case []any:
		result := make([]any, 0, len(typed))
		for _, item := range typed {
			result = append(result, exportValue(item))
		}
		return result
	case *callable, *builtin:
		return nil
	}
	return value
}

// sortedKeys 保证对象键遍历顺序稳定，避免同一脚本每次执行结果不同。
func sortedKeys(source map[string]any) []string {
	keys := make([]string, 0, len(source))
	for key := range source {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
