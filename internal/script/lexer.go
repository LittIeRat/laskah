// Package script 解析并执行管理员自填的额度查询脚本。
//
// 脚本形态与 cc-switch 一致：({ request: {...}, extractor: function (response) {...} })。
// 这里内置一个最小化解释器，只覆盖「取字段、算数字、拼字符串」这点需求：
// 没有网络、文件、定时器、全局对象，也不支持 new / class / 正则 / 异步 / import，
// 因此脚本既拿不到宿主进程，也无法在解析阶段之外产生副作用。
package script

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

type tokenKind int

const (
	tokEOF tokenKind = iota
	tokIdent
	tokNumber
	tokString
	tokTemplate
	tokPunct
)

// token 是词法单元。chunks/exprs 只在模板字符串时使用。
type token struct {
	kind   tokenKind
	text   string
	num    float64
	str    string
	chunks []string
	exprs  []string
	line   int
}

// punctuators 必须按长度倒序排列，否则 === 会被切成 == 与 =。
var punctuators = []string{
	"===", "!==",
	"?.", "??", "=>", "==", "!=", "<=", ">=", "&&", "||",
	"+=", "-=", "*=", "/=", "%=",
	"{", "}", "(", ")", "[", "]", ",", ";", ":", ".", "?", "=", "!", "<", ">", "+", "-", "*", "/", "%",
}

type lexer struct {
	src  string
	pos  int
	line int
}

// tokenize 把脚本源码切成词法单元序列。
func tokenize(src string) ([]token, error) {
	lx := &lexer{src: src, line: 1}
	tokens := []token{}
	for {
		if err := lx.skipSpace(); err != nil {
			return nil, err
		}
		if lx.pos >= len(lx.src) {
			tokens = append(tokens, token{kind: tokEOF, line: lx.line})
			return tokens, nil
		}
		next, err := lx.next()
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, next)
	}
}

func (l *lexer) skipSpace() error {
	for l.pos < len(l.src) {
		ch := l.src[l.pos]
		switch {
		case ch == 10:
			l.line++
			l.pos++
		case ch == 32 || ch == 9 || ch == 13:
			l.pos++
		case strings.HasPrefix(l.src[l.pos:], "//"):
			for l.pos < len(l.src) && l.src[l.pos] != 10 {
				l.pos++
			}
		case strings.HasPrefix(l.src[l.pos:], "/*"):
			end := strings.Index(l.src[l.pos+2:], "*/")
			if end < 0 {
				return fmt.Errorf("第 %d 行: 注释没有闭合", l.line)
			}
			l.line += strings.Count(l.src[l.pos:l.pos+end+4], "\n")
			l.pos += end + 4
		default:
			return nil
		}
	}
	return nil
}

func (l *lexer) next() (token, error) {
	ch := l.src[l.pos]
	switch {
	case ch == 39 || ch == 34:
		return l.readString(ch)
	case ch == 96:
		return l.readTemplate()
	case ch >= 48 && ch <= 57:
		return l.readNumber()
	case ch == 46 && l.pos+1 < len(l.src) && l.src[l.pos+1] >= 48 && l.src[l.pos+1] <= 57:
		return l.readNumber()
	case isIdentStart(l.src[l.pos:]):
		return l.readIdent(), nil
	}
	for _, symbol := range punctuators {
		if strings.HasPrefix(l.src[l.pos:], symbol) {
			line := l.line
			l.pos += len(symbol)
			return token{kind: tokPunct, text: symbol, line: line}, nil
		}
	}
	return token{}, fmt.Errorf("第 %d 行: 不支持的字符 %q", l.line, string(rune(ch)))
}

func isIdentStart(rest string) bool {
	if rest == "" {
		return false
	}
	r, _ := utf8.DecodeRuneInString(rest)
	return r == 95 || r == 36 || unicode.IsLetter(r)
}

func isIdentPart(rest string) bool {
	if rest == "" {
		return false
	}
	r, _ := utf8.DecodeRuneInString(rest)
	return r == 95 || r == 36 || unicode.IsLetter(r) || unicode.IsDigit(r)
}

func (l *lexer) readIdent() token {
	start := l.pos
	for l.pos < len(l.src) && isIdentPart(l.src[l.pos:]) {
		_, size := utf8.DecodeRuneInString(l.src[l.pos:])
		l.pos += size
	}
	return token{kind: tokIdent, text: l.src[start:l.pos], line: l.line}
}

func (l *lexer) readNumber() (token, error) {
	start := l.pos
	if strings.HasPrefix(strings.ToLower(l.src[l.pos:]), "0x") {
		l.pos += 2
		for l.pos < len(l.src) && isHexDigit(l.src[l.pos]) {
			l.pos++
		}
		value, err := strconv.ParseUint(l.src[start+2:l.pos], 16, 64)
		if err != nil {
			return token{}, fmt.Errorf("第 %d 行: 无效的十六进制数字 %s", l.line, l.src[start:l.pos])
		}
		return token{kind: tokNumber, num: float64(value), line: l.line}, nil
	}
	for l.pos < len(l.src) && (l.src[l.pos] >= 48 && l.src[l.pos] <= 57 || l.src[l.pos] == 46) {
		l.pos++
	}
	if l.pos < len(l.src) && (l.src[l.pos] == 101 || l.src[l.pos] == 69) {
		l.pos++
		if l.pos < len(l.src) && (l.src[l.pos] == 43 || l.src[l.pos] == 45) {
			l.pos++
		}
		for l.pos < len(l.src) && l.src[l.pos] >= 48 && l.src[l.pos] <= 57 {
			l.pos++
		}
	}
	value, err := strconv.ParseFloat(l.src[start:l.pos], 64)
	if err != nil {
		return token{}, fmt.Errorf("第 %d 行: 无效的数字 %s", l.line, l.src[start:l.pos])
	}
	return token{kind: tokNumber, num: value, line: l.line}, nil
}

func isHexDigit(ch byte) bool {
	return ch >= 48 && ch <= 57 || ch >= 97 && ch <= 102 || ch >= 65 && ch <= 70
}

func (l *lexer) readString(quote byte) (token, error) {
	line := l.line
	l.pos++
	var out strings.Builder
	for l.pos < len(l.src) {
		ch := l.src[l.pos]
		switch ch {
		case quote:
			l.pos++
			return token{kind: tokString, str: out.String(), line: line}, nil
		case 92:
			decoded, size, err := decodeEscape(l.src[l.pos:], line)
			if err != nil {
				return token{}, err
			}
			out.WriteString(decoded)
			l.pos += size
		case 10:
			return token{}, fmt.Errorf("第 %d 行: 字符串不能跨行", line)
		default:
			out.WriteByte(ch)
			l.pos++
		}
	}
	return token{}, fmt.Errorf("第 %d 行: 字符串没有闭合", line)
}

// decodeEscape 解析一个反斜杠转义，返回解码结果与消耗的字节数。
func decodeEscape(rest string, line int) (string, int, error) {
	if len(rest) < 2 {
		return "", 0, fmt.Errorf("第 %d 行: 转义序列不完整", line)
	}
	switch rest[1] {
	case 110:
		return "\n", 2, nil
	case 116:
		return "\t", 2, nil
	case 114:
		return "\r", 2, nil
	case 98:
		return "\b", 2, nil
	case 102:
		return "\f", 2, nil
	case 48:
		return "\x00", 2, nil
	case 117:
		if len(rest) < 6 {
			return "", 0, fmt.Errorf("第 %d 行: \\u 转义不完整", line)
		}
		code, err := strconv.ParseUint(rest[2:6], 16, 32)
		if err != nil {
			return "", 0, fmt.Errorf("第 %d 行: 无效的 \\u 转义", line)
		}
		return string(rune(code)), 6, nil
	case 120:
		if len(rest) < 4 {
			return "", 0, fmt.Errorf("第 %d 行: \\x 转义不完整", line)
		}
		code, err := strconv.ParseUint(rest[2:4], 16, 32)
		if err != nil {
			return "", 0, fmt.Errorf("第 %d 行: 无效的 \\x 转义", line)
		}
		return string(rune(code)), 4, nil
	default:
		_, size := utf8.DecodeRuneInString(rest[1:])
		return rest[1 : 1+size], 1 + size, nil
	}
}

// readTemplate 读取模板字符串，${} 内的表达式源码原样保留，交给解析器递归处理。
func (l *lexer) readTemplate() (token, error) {
	line := l.line
	l.pos++
	chunks := []string{}
	exprs := []string{}
	var current strings.Builder
	for l.pos < len(l.src) {
		ch := l.src[l.pos]
		switch {
		case ch == 96:
			l.pos++
			chunks = append(chunks, current.String())
			return token{kind: tokTemplate, chunks: chunks, exprs: exprs, line: line}, nil
		case ch == 92:
			decoded, size, err := decodeEscape(l.src[l.pos:], line)
			if err != nil {
				return token{}, err
			}
			current.WriteString(decoded)
			l.pos += size
		case ch == 36 && l.pos+1 < len(l.src) && l.src[l.pos+1] == 123:
			source, size, err := l.readTemplateExpr(line)
			if err != nil {
				return token{}, err
			}
			chunks = append(chunks, current.String())
			current.Reset()
			exprs = append(exprs, source)
			l.pos += size
		default:
			if ch == 10 {
				l.line++
			}
			current.WriteByte(ch)
			l.pos++
		}
	}
	return token{}, fmt.Errorf("第 %d 行: 模板字符串没有闭合", line)
}

// readTemplateExpr 按花括号配对切出 ${...} 的内部源码，跳过其中的字符串字面量。
func (l *lexer) readTemplateExpr(line int) (string, int, error) {
	depth := 0
	start := l.pos + 2
	index := l.pos + 1
	for index < len(l.src) {
		ch := l.src[index]
		switch ch {
		case 123:
			depth++
			index++
		case 125:
			depth--
			if depth == 0 {
				return l.src[start:index], index + 1 - l.pos, nil
			}
			index++
		case 39, 34:
			quote := ch
			index++
			for index < len(l.src) && l.src[index] != quote {
				if l.src[index] == 92 {
					index++
				}
				index++
			}
			index++
		default:
			index++
		}
	}
	return "", 0, fmt.Errorf("第 %d 行: 模板表达式没有闭合", line)
}
