package script

import (
	"errors"
	"fmt"
	"sort"
)

// arrayMethod 实现需要回调能力的数组方法。
//
// 这些方法必须由解释器执行（回调是脚本函数），因此不能像 join / includes
// 那样在 builtins 里用纯 Go 闭包完成。handled 为 false 表示该名字不由这里负责。
func (in *interp) arrayMethod(items []any, name string, args []any) (bool, any, error) {
	switch name {
	case "map":
		callback := argAt(args, 0)
		result := make([]any, 0, len(items))
		for index, item := range items {
			value, err := in.invoke(callback, []any{item, float64(index)})
			if err != nil {
				return true, nil, err
			}
			result = append(result, value)
		}
		return true, result, nil
	case "filter":
		callback := argAt(args, 0)
		result := []any{}
		for index, item := range items {
			value, err := in.invoke(callback, []any{item, float64(index)})
			if err != nil {
				return true, nil, err
			}
			if truthy(value) {
				result = append(result, item)
			}
		}
		return true, result, nil
	case "find":
		callback := argAt(args, 0)
		for index, item := range items {
			value, err := in.invoke(callback, []any{item, float64(index)})
			if err != nil {
				return true, nil, err
			}
			if truthy(value) {
				return true, item, nil
			}
		}
		return true, undefinedVal, nil
	case "some", "every":
		callback := argAt(args, 0)
		for index, item := range items {
			value, err := in.invoke(callback, []any{item, float64(index)})
			if err != nil {
				return true, nil, err
			}
			if name == "some" && truthy(value) {
				return true, true, nil
			}
			if name == "every" && !truthy(value) {
				return true, false, nil
			}
		}
		return true, name == "every", nil
	case "forEach":
		callback := argAt(args, 0)
		for index, item := range items {
			if _, err := in.invoke(callback, []any{item, float64(index)}); err != nil {
				return true, nil, err
			}
		}
		return true, undefinedVal, nil
	case "reduce":
		callback := argAt(args, 0)
		var (
			acc   any
			start int
		)
		if len(args) > 1 {
			acc = args[1]
		} else {
			if len(items) == 0 {
				return true, nil, errors.New("对空数组调用 reduce 需要提供初始值")
			}
			acc = items[0]
			start = 1
		}
		for index := start; index < len(items); index++ {
			value, err := in.invoke(callback, []any{acc, items[index], float64(index)})
			if err != nil {
				return true, nil, err
			}
			acc = value
		}
		return true, acc, nil
	case "sort":
		result := make([]any, len(items))
		copy(result, items)
		comparator := argAt(args, 0)
		var sortErr error
		sort.SliceStable(result, func(i, j int) bool {
			if sortErr != nil {
				return false
			}
			if isNullish(comparator) {
				return toString(result[i]) < toString(result[j])
			}
			value, err := in.invoke(comparator, []any{result[i], result[j]})
			if err != nil {
				sortErr = err
				return false
			}
			return toNumber(value) < 0
		})
		if sortErr != nil {
			return true, nil, sortErr
		}
		return true, result, nil
	case "concat":
		result := make([]any, 0, len(items)+len(args))
		result = append(result, items...)
		for _, item := range args {
			if nested, ok := item.([]any); ok {
				result = append(result, nested...)
				continue
			}
			result = append(result, item)
		}
		if len(result) > maxArrayLen {
			return true, nil, fmt.Errorf("数组长度超过上限 %d", maxArrayLen)
		}
		return true, result, nil
	case "push":
		// 只能原地追加到已有容量里没有意义，脚本拿不到新切片头；
		// 因此明确拒绝，让作者改用 concat，避免出现「push 了但没生效」的困惑。
		return true, nil, errors.New("不支持 push，请改用 concat 生成新数组")
	}
	return false, nil, nil
}

// maxArrayLen 限制脚本构造的数组长度。
const maxArrayLen = 100000
