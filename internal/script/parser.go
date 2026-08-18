package script

import (
	"fmt"
	"strconv"
)

// node 是表达式节点。
type node interface{}

// stmt 是语句节点。
type stmt interface{}

type numLit struct{ value float64 }
type strLit struct{ value string }
type boolLit struct{ value bool }
type nullLit struct{}
type undefLit struct{}
type tmplLit struct {
	chunks []string
	exprs  []node
}
type ident struct{ name string }
type objLit struct {
	keys   []string
	values []node
}
type arrLit struct{ items []node }
type member struct {
	object   node
	prop     node
	name     string
	computed bool
	optional bool
}
type call struct {
	callee   node
	args     []node
	optional bool
}
type unary struct {
	op  string
	arg node
}
type binary struct {
	op          string
	left, right node
}
type logical struct {
	op          string
	left, right node
}
type cond struct{ test, cons, alt node }
type assign struct {
	target node
	op     string
	value  node
}
type fnLit struct {
	params []string
	body   []stmt
	expr   node
}

type varDecl struct {
	names []string
	inits []node
}
type retStmt struct{ arg node }
type ifStmt struct {
	test node
	cons stmt
	alt  stmt
}
type blockStmt struct{ body []stmt }
type exprStmt struct{ expr node }
type forOfStmt struct {
	name string
	iter node
	body stmt
}

// maxNodes 限制单个脚本的节点数量，挡住靠体积做资源消耗的输入。
const maxNodes = 4000

type parser struct {
	tokens []token
	pos    int
	nodes  int
}

// parseProgram 把源码解析成一个顶层表达式。
//
// 只接受「单个表达式」形态（约定用 () 包裹的对象字面量），
// 因此脚本无法声明顶层语句，也没有任何可以持续存在的状态。
func parseProgram(src string) (node, error) {
	tokens, err := tokenize(src)
	if err != nil {
		return nil, err
	}
	p := &parser{tokens: tokens}
	expr, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	p.skipPunct(";")
	if p.cur().kind != tokEOF {
		return nil, fmt.Errorf("第 %d 行: 脚本必须是单个表达式（整体用 () 包裹）", p.cur().line)
	}
	return expr, nil
}

func (p *parser) cur() token  { return p.tokens[p.pos] }
func (p *parser) peek() token { return p.tokens[min(p.pos+1, len(p.tokens)-1)] }

func (p *parser) count() error {
	p.nodes++
	if p.nodes > maxNodes {
		return fmt.Errorf("脚本过于复杂（节点数超过 %d）", maxNodes)
	}
	return nil
}

func (p *parser) isPunct(text string) bool {
	return p.cur().kind == tokPunct && p.cur().text == text
}

func (p *parser) isKeyword(word string) bool {
	return p.cur().kind == tokIdent && p.cur().text == word
}

func (p *parser) skipPunct(text string) bool {
	if p.isPunct(text) {
		p.pos++
		return true
	}
	return false
}

func (p *parser) expectPunct(text string) error {
	if !p.skipPunct(text) {
		return fmt.Errorf("第 %d 行: 期望 %q", p.cur().line, text)
	}
	return nil
}

func (p *parser) parseExpression() (node, error) {
	if err := p.count(); err != nil {
		return nil, err
	}
	return p.parseAssign()
}

var assignOps = map[string]bool{"=": true, "+=": true, "-=": true, "*=": true, "/=": true, "%=": true}

func (p *parser) parseAssign() (node, error) {
	left, err := p.parseConditional()
	if err != nil {
		return nil, err
	}
	if p.cur().kind != tokPunct || !assignOps[p.cur().text] {
		return left, nil
	}
	switch left.(type) {
	case *ident, *member:
	default:
		return nil, fmt.Errorf("第 %d 行: 赋值目标必须是变量或属性", p.cur().line)
	}
	op := p.cur().text
	p.pos++
	value, err := p.parseAssign()
	if err != nil {
		return nil, err
	}
	return &assign{target: left, op: op, value: value}, nil
}

func (p *parser) parseConditional() (node, error) {
	test, err := p.parseLogicalOr()
	if err != nil {
		return nil, err
	}
	if !p.isPunct("?") {
		return test, nil
	}
	p.pos++
	cons, err := p.parseAssign()
	if err != nil {
		return nil, err
	}
	if err := p.expectPunct(":"); err != nil {
		return nil, err
	}
	alt, err := p.parseAssign()
	if err != nil {
		return nil, err
	}
	return &cond{test: test, cons: cons, alt: alt}, nil
}

func (p *parser) parseLogicalOr() (node, error) {
	left, err := p.parseLogicalAnd()
	if err != nil {
		return nil, err
	}
	for p.isPunct("||") || p.isPunct("??") {
		op := p.cur().text
		p.pos++
		right, err := p.parseLogicalAnd()
		if err != nil {
			return nil, err
		}
		if err := p.count(); err != nil {
			return nil, err
		}
		left = &logical{op: op, left: left, right: right}
	}
	return left, nil
}

func (p *parser) parseLogicalAnd() (node, error) {
	left, err := p.parseEquality()
	if err != nil {
		return nil, err
	}
	for p.isPunct("&&") {
		p.pos++
		right, err := p.parseEquality()
		if err != nil {
			return nil, err
		}
		if err := p.count(); err != nil {
			return nil, err
		}
		left = &logical{op: "&&", left: left, right: right}
	}
	return left, nil
}

func (p *parser) parseBinaryLevel(ops []string, next func() (node, error)) (node, error) {
	left, err := next()
	if err != nil {
		return nil, err
	}
	for {
		matched := ""
		for _, op := range ops {
			if p.isPunct(op) {
				matched = op
				break
			}
		}
		if matched == "" {
			return left, nil
		}
		p.pos++
		right, err := next()
		if err != nil {
			return nil, err
		}
		if err := p.count(); err != nil {
			return nil, err
		}
		left = &binary{op: matched, left: left, right: right}
	}
}

func (p *parser) parseEquality() (node, error) {
	return p.parseBinaryLevel([]string{"===", "!==", "==", "!="}, p.parseRelational)
}

func (p *parser) parseRelational() (node, error) {
	return p.parseBinaryLevel([]string{"<=", ">=", "<", ">"}, p.parseAdditive)
}

func (p *parser) parseAdditive() (node, error) {
	return p.parseBinaryLevel([]string{"+", "-"}, p.parseMultiplicative)
}

func (p *parser) parseMultiplicative() (node, error) {
	return p.parseBinaryLevel([]string{"*", "/", "%"}, p.parseUnary)
}

func (p *parser) parseUnary() (node, error) {
	if p.isPunct("!") || p.isPunct("-") || p.isPunct("+") || p.isKeyword("typeof") {
		op := p.cur().text
		p.pos++
		arg, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		if err := p.count(); err != nil {
			return nil, err
		}
		return &unary{op: op, arg: arg}, nil
	}
	return p.parsePostfix()
}

func (p *parser) parsePostfix() (node, error) {
	expr, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	for {
		if err := p.count(); err != nil {
			return nil, err
		}
		switch {
		case p.isPunct("."):
			p.pos++
			if p.cur().kind != tokIdent {
				return nil, fmt.Errorf("第 %d 行: 点号后需要属性名", p.cur().line)
			}
			expr = &member{object: expr, name: p.cur().text}
			p.pos++
		case p.isPunct("?."):
			p.pos++
			switch {
			case p.isPunct("("):
				args, err := p.parseArgs()
				if err != nil {
					return nil, err
				}
				expr = &call{callee: expr, args: args, optional: true}
			case p.isPunct("["):
				p.pos++
				prop, err := p.parseExpression()
				if err != nil {
					return nil, err
				}
				if err := p.expectPunct("]"); err != nil {
					return nil, err
				}
				expr = &member{object: expr, prop: prop, computed: true, optional: true}
			default:
				if p.cur().kind != tokIdent {
					return nil, fmt.Errorf("第 %d 行: ?. 后需要属性名", p.cur().line)
				}
				expr = &member{object: expr, name: p.cur().text, optional: true}
				p.pos++
			}
		case p.isPunct("["):
			p.pos++
			prop, err := p.parseExpression()
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct("]"); err != nil {
				return nil, err
			}
			expr = &member{object: expr, prop: prop, computed: true}
		case p.isPunct("("):
			args, err := p.parseArgs()
			if err != nil {
				return nil, err
			}
			expr = &call{callee: expr, args: args}
		default:
			return expr, nil
		}
	}
}

func (p *parser) parseArgs() ([]node, error) {
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	args := []node{}
	for !p.isPunct(")") {
		arg, err := p.parseAssign()
		if err != nil {
			return nil, err
		}
		args = append(args, arg)
		if !p.skipPunct(",") {
			break
		}
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	return args, nil
}

// bannedIdents 是被拒绝的标识符：它们在真实 JS 里能触达宿主或制造副作用。
//
// 这里的解释器本身没有实现它们，拒绝只是为了在解析期就给出明确报错，
// 而不是等到运行时报一句含糊的“未定义”。
var bannedIdents = map[string]bool{
	"require": true, "import": true, "process": true, "globalThis": true, "global": true,
	"eval": true, "Function": true, "fetch": true, "XMLHttpRequest": true, "WebSocket": true,
	"setTimeout": true, "setInterval": true, "Buffer": true, "window": true, "document": true,
	"new": true, "class": true, "async": true, "await": true, "yield": true, "this": true,
	"try": true, "catch": true, "throw": true, "while": true, "do": true, "switch": true,
	"delete": true, "void": true, "in": true, "instanceof": true,
}

func (p *parser) parsePrimary() (node, error) {
	if err := p.count(); err != nil {
		return nil, err
	}
	tok := p.cur()
	switch tok.kind {
	case tokNumber:
		p.pos++
		return &numLit{value: tok.num}, nil
	case tokString:
		p.pos++
		return &strLit{value: tok.str}, nil
	case tokTemplate:
		p.pos++
		exprs := make([]node, 0, len(tok.exprs))
		for _, source := range tok.exprs {
			sub, err := parseProgram(source)
			if err != nil {
				return nil, err
			}
			exprs = append(exprs, sub)
		}
		return &tmplLit{chunks: tok.chunks, exprs: exprs}, nil
	case tokIdent:
		switch tok.text {
		case "true", "false":
			p.pos++
			return &boolLit{value: tok.text == "true"}, nil
		case "null":
			p.pos++
			return &nullLit{}, nil
		case "undefined":
			p.pos++
			return &undefLit{}, nil
		case "function":
			return p.parseFunction()
		}
		if bannedIdents[tok.text] {
			return nil, fmt.Errorf("第 %d 行: 不允许使用 %s", tok.line, tok.text)
		}
		// 单参数箭头函数：x => ...
		if p.peek().kind == tokPunct && p.peek().text == "=>" {
			p.pos += 2
			return p.parseArrowBody([]string{tok.text})
		}
		p.pos++
		return &ident{name: tok.text}, nil
	case tokPunct:
		switch tok.text {
		case "(":
			if params, ok := p.tryArrowParams(); ok {
				return p.parseArrowBody(params)
			}
			p.pos++
			expr, err := p.parseExpression()
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct(")"); err != nil {
				return nil, err
			}
			return expr, nil
		case "{":
			return p.parseObject()
		case "[":
			return p.parseArray()
		}
	}
	return nil, fmt.Errorf("第 %d 行: 无法解析的记号", tok.line)
}

// tryArrowParams 判断 ( 开头是否为箭头函数参数表，是则消耗到 => 之后。
func (p *parser) tryArrowParams() ([]string, bool) {
	depth := 0
	index := p.pos
	for index < len(p.tokens) {
		tok := p.tokens[index]
		if tok.kind == tokEOF {
			return nil, false
		}
		if tok.kind == tokPunct {
			switch tok.text {
			case "(":
				depth++
			case ")":
				depth--
				if depth == 0 {
					next := p.tokens[min(index+1, len(p.tokens)-1)]
					if next.kind != tokPunct || next.text != "=>" {
						return nil, false
					}
					params := []string{}
					for cursor := p.pos + 1; cursor < index; cursor++ {
						inner := p.tokens[cursor]
						switch {
						case inner.kind == tokIdent:
							params = append(params, inner.text)
						case inner.kind == tokPunct && inner.text == ",":
						default:
							return nil, false
						}
					}
					p.pos = index + 2
					return params, true
				}
			}
		}
		index++
	}
	return nil, false
}

// parseArrowBody 解析箭头函数体：既支持表达式体也支持花括号语句体。
func (p *parser) parseArrowBody(params []string) (node, error) {
	if p.isPunct("{") {
		body, err := p.parseBlock()
		if err != nil {
			return nil, err
		}
		return &fnLit{params: params, body: body}, nil
	}
	expr, err := p.parseAssign()
	if err != nil {
		return nil, err
	}
	return &fnLit{params: params, expr: expr}, nil
}

func (p *parser) parseFunction() (node, error) {
	p.pos++
	if p.cur().kind == tokIdent {
		// 具名函数表达式：名字对本解释器没有意义，直接忽略。
		p.pos++
	}
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	params := []string{}
	for !p.isPunct(")") {
		if p.cur().kind != tokIdent {
			return nil, fmt.Errorf("第 %d 行: 参数名无效", p.cur().line)
		}
		params = append(params, p.cur().text)
		p.pos++
		if !p.skipPunct(",") {
			break
		}
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	body, err := p.parseBlock()
	if err != nil {
		return nil, err
	}
	return &fnLit{params: params, body: body}, nil
}

func (p *parser) parseObject() (node, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	keys := []string{}
	values := []node{}
	for !p.isPunct("}") {
		tok := p.cur()
		var key string
		switch tok.kind {
		case tokIdent:
			key = tok.text
		case tokString:
			key = tok.str
		case tokNumber:
			key = strconv.FormatFloat(tok.num, 'g', -1, 64)
		default:
			return nil, fmt.Errorf("第 %d 行: 对象键名无效", tok.line)
		}
		p.pos++
		if err := p.expectPunct(":"); err != nil {
			return nil, err
		}
		value, err := p.parseAssign()
		if err != nil {
			return nil, err
		}
		keys = append(keys, key)
		values = append(values, value)
		if !p.skipPunct(",") {
			break
		}
	}
	if err := p.expectPunct("}"); err != nil {
		return nil, err
	}
	return &objLit{keys: keys, values: values}, nil
}

func (p *parser) parseArray() (node, error) {
	if err := p.expectPunct("["); err != nil {
		return nil, err
	}
	items := []node{}
	for !p.isPunct("]") {
		item, err := p.parseAssign()
		if err != nil {
			return nil, err
		}
		items = append(items, item)
		if !p.skipPunct(",") {
			break
		}
	}
	if err := p.expectPunct("]"); err != nil {
		return nil, err
	}
	return &arrLit{items: items}, nil
}

func (p *parser) parseBlock() ([]stmt, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	body := []stmt{}
	for !p.isPunct("}") {
		if p.cur().kind == tokEOF {
			return nil, fmt.Errorf("第 %d 行: 花括号没有闭合", p.cur().line)
		}
		item, err := p.parseStatement()
		if err != nil {
			return nil, err
		}
		body = append(body, item)
	}
	if err := p.expectPunct("}"); err != nil {
		return nil, err
	}
	return body, nil
}

func (p *parser) parseStatement() (stmt, error) {
	if err := p.count(); err != nil {
		return nil, err
	}
	switch {
	case p.isPunct("{"):
		body, err := p.parseBlock()
		if err != nil {
			return nil, err
		}
		return &blockStmt{body: body}, nil
	case p.isPunct(";"):
		p.pos++
		return &blockStmt{}, nil
	case p.isKeyword("var"), p.isKeyword("let"), p.isKeyword("const"):
		return p.parseVarDecl()
	case p.isKeyword("return"):
		p.pos++
		if p.isPunct(";") || p.isPunct("}") {
			p.skipPunct(";")
			return &retStmt{}, nil
		}
		arg, err := p.parseExpression()
		if err != nil {
			return nil, err
		}
		p.skipPunct(";")
		return &retStmt{arg: arg}, nil
	case p.isKeyword("if"):
		return p.parseIf()
	case p.isKeyword("for"):
		return p.parseForOf()
	}
	if p.cur().kind == tokIdent && bannedIdents[p.cur().text] {
		return nil, fmt.Errorf("第 %d 行: 不允许使用 %s", p.cur().line, p.cur().text)
	}
	expr, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	p.skipPunct(";")
	return &exprStmt{expr: expr}, nil
}

func (p *parser) parseVarDecl() (stmt, error) {
	p.pos++
	decl := &varDecl{}
	for {
		if p.cur().kind != tokIdent {
			return nil, fmt.Errorf("第 %d 行: 变量名无效", p.cur().line)
		}
		name := p.cur().text
		p.pos++
		var init node
		if p.skipPunct("=") {
			value, err := p.parseAssign()
			if err != nil {
				return nil, err
			}
			init = value
		}
		decl.names = append(decl.names, name)
		decl.inits = append(decl.inits, init)
		if !p.skipPunct(",") {
			break
		}
	}
	p.skipPunct(";")
	return decl, nil
}

func (p *parser) parseIf() (stmt, error) {
	p.pos++
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	test, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	cons, err := p.parseStatement()
	if err != nil {
		return nil, err
	}
	result := &ifStmt{test: test, cons: cons}
	if p.isKeyword("else") {
		p.pos++
		alt, err := p.parseStatement()
		if err != nil {
			return nil, err
		}
		result.alt = alt
	}
	return result, nil
}

// parseForOf 只支持 for (const item of list) 形态。
//
// 计数循环与 while 都能写出不易察觉的长循环，而额度脚本只需要遍历数组，
// 因此故意不支持，配合执行步数上限把最坏情况压住。
func (p *parser) parseForOf() (stmt, error) {
	p.pos++
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	if p.isKeyword("var") || p.isKeyword("let") || p.isKeyword("const") {
		p.pos++
	}
	if p.cur().kind != tokIdent {
		return nil, fmt.Errorf("第 %d 行: for 循环变量名无效", p.cur().line)
	}
	name := p.cur().text
	p.pos++
	if !p.isKeyword("of") {
		return nil, fmt.Errorf("第 %d 行: 只支持 for (item of list) 形式的循环", p.cur().line)
	}
	p.pos++
	iter, err := p.parseExpression()
	if err != nil {
		return nil, err
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	body, err := p.parseStatement()
	if err != nil {
		return nil, err
	}
	return &forOfStmt{name: name, iter: iter, body: body}, nil
}
