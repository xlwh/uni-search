// Package script implements the M1 painless script engine subset
// (design 03-query-engine §9). It provides a tree-walking interpreter
// for a restricted painless syntax: arithmetic, logic, comparison,
// string concatenation, doc['field'].value, _score, params, Math.*
// subset, ternary, and vector functions (cosineSimilarity, dotProduct,
// l1norm, l2norm). A step budget (default 100k) prevents infinite
// loops; exceeding it raises a script_exception (ES-aligned).
// @spec: ./archive/2026-10-20-m1-es-compat-ga/02_spec.md#2.2.6
package script

import (
	"fmt"
	"math"
	"strings"
)

// MaxSteps is the default evaluation step budget (ES-aligned: 100k).
const MaxSteps = 100_000

// ScriptCtx is the evaluation context passed to every expression.
type ScriptCtx struct {
	Doc    map[string]any // document fields
	Score  float64        // _score value
	Params map[string]any // user-supplied params
	Steps  int            // step counter (reset per Eval)
	Budget int            // max steps (0 = MaxSteps)
}

// Evaluator evaluates a parsed script expression.
type Evaluator interface {
	Eval(ctx *ScriptCtx) (any, error)
}

// Script is a parsed painless expression ready for evaluation.
type Script struct {
	source string
	expr   node
}

// Parse compiles a painless source string into a Script. The supported
// syntax is a deliberately small subset of ES painless:
//   - Literals: numbers, strings, booleans, null
//   - Identifiers: _score, doc['field'].value, params.x
//   - Binary ops: + - * / % == != < > <= >= && ||
//   - Unary: ! -
//   - Ternary: cond ? a : b
//   - Method calls: Math.abs(x), Math.sqrt(x), etc.
//   - Vector funcs: cosineSimilarity(q, doc['v']), dotProduct, l1norm, l2norm
func Parse(source string) (*Script, error) {
	p := &parser{src: source, pos: 0}
	expr, err := p.parseExpr()
	if err != nil {
		return nil, fmt.Errorf("script: parse error: %w", err)
	}
	if p.pos < len(p.src) {
		return nil, fmt.Errorf("script: unexpected token at position %d", p.pos)
	}
	return &Script{source: source, expr: expr}, nil
}

// Eval evaluates the script with the given context.
func (s *Script) Eval(ctx *ScriptCtx) (any, error) {
	if ctx.Budget <= 0 {
		ctx.Budget = MaxSteps
	}
	ctx.Steps = 0
	return s.expr.Eval(ctx)
}

// EvalFloat is a convenience wrapper that evaluates and converts to float64.
func (s *Script) EvalFloat(ctx *ScriptCtx) (float64, error) {
	v, err := s.Eval(ctx)
	if err != nil {
		return 0, err
	}
	return toFloat(v), nil
}

// toFloat converts any numeric value to float64.
func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case bool:
		if n {
			return 1
		}
		return 0
	case nil:
		return 0
	default:
		return 0
	}
}

// stepCheck panics if the step budget is exceeded. Recovered by Eval.
func stepCheck(ctx *ScriptCtx) error {
	ctx.Steps++
	if ctx.Steps > ctx.Budget {
		return fmt.Errorf("script_exception: exceeded step budget of %d", ctx.Budget)
	}
	return nil
}

// ---------------------------------------------------------------------------
// AST nodes
// ---------------------------------------------------------------------------

type node interface {
	Eval(ctx *ScriptCtx) (any, error)
}

// literalNode
type literalNode struct{ val any }

func (n *literalNode) Eval(ctx *ScriptCtx) (any, error) {
	if err := stepCheck(ctx); err != nil {
		return nil, err
	}
	return n.val, nil
}

// binaryNode
type binaryNode struct {
	op    string
	left  node
	right node
}

func (n *binaryNode) Eval(ctx *ScriptCtx) (any, error) {
	if err := stepCheck(ctx); err != nil {
		return nil, err
	}
	l, err := n.left.Eval(ctx)
	if err != nil {
		return nil, err
	}
	switch n.op {
	case "&&":
		return toBool(l) && toBool(mustEval(n.right, ctx)), nil
	case "||":
		if toBool(l) {
			return true, nil
		}
		return toBool(mustEval(n.right, ctx)), nil
	}
	r, err := n.right.Eval(ctx)
	if err != nil {
		return nil, err
	}
	return evalBinary(n.op, l, r), nil
}

func mustEval(n node, ctx *ScriptCtx) any {
	v, _ := n.Eval(ctx)
	return v
}

func toBool(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case float64:
		return b != 0
	case nil:
		return false
	default:
		return true
	}
}

func evalBinary(op string, l, r any) any {
	switch op {
	case "+":
		if ls, ok := l.(string); ok {
			return ls + fmt.Sprintf("%v", r)
		}
		return toFloat(l) + toFloat(r)
	case "-":
		return toFloat(l) - toFloat(r)
	case "*":
		return toFloat(l) * toFloat(r)
	case "/":
		if toFloat(r) == 0 {
			return float64(0)
		}
		return toFloat(l) / toFloat(r)
	case "%":
		if toFloat(r) == 0 {
			return float64(0)
		}
		return math.Mod(toFloat(l), toFloat(r))
	case "==":
		return toFloat(l) == toFloat(r)
	case "!=":
		return toFloat(l) != toFloat(r)
	case "<":
		return toFloat(l) < toFloat(r)
	case ">":
		return toFloat(l) > toFloat(r)
	case "<=":
		return toFloat(l) <= toFloat(r)
	case ">=":
		return toFloat(l) >= toFloat(r)
	}
	return nil
}

// unaryNode
type unaryNode struct {
	op   string
	expr node
}

func (n *unaryNode) Eval(ctx *ScriptCtx) (any, error) {
	if err := stepCheck(ctx); err != nil {
		return nil, err
	}
	v, err := n.expr.Eval(ctx)
	if err != nil {
		return nil, err
	}
	switch n.op {
	case "!":
		return !toBool(v), nil
	case "-":
		return -toFloat(v), nil
	}
	return nil, fmt.Errorf("script: unknown unary op %q", n.op)
}

// ternaryNode
type ternaryNode struct {
	cond, yes, no node
}

func (n *ternaryNode) Eval(ctx *ScriptCtx) (any, error) {
	if err := stepCheck(ctx); err != nil {
		return nil, err
	}
	c, err := n.cond.Eval(ctx)
	if err != nil {
		return nil, err
	}
	if toBool(c) {
		return n.yes.Eval(ctx)
	}
	return n.no.Eval(ctx)
}

// docFieldNode accesses doc['field'].value
type docFieldNode struct {
	field string
}

func (n *docFieldNode) Eval(ctx *ScriptCtx) (any, error) {
	if err := stepCheck(ctx); err != nil {
		return nil, err
	}
	return ctx.Doc[n.field], nil
}

// scoreNode returns _score
type scoreNode struct{}

func (n *scoreNode) Eval(ctx *ScriptCtx) (any, error) {
	if err := stepCheck(ctx); err != nil {
		return nil, err
	}
	return ctx.Score, nil
}

// paramsNode returns params[key]
type paramsNode struct {
	key string
}

func (n *paramsNode) Eval(ctx *ScriptCtx) (any, error) {
	if err := stepCheck(ctx); err != nil {
		return nil, err
	}
	return ctx.Params[n.key], nil
}

// methodCallNode handles Math.abs, Math.sqrt, cosineSimilarity, etc.
type methodCallNode struct {
	receiver string
	method   string
	args     []node
}

func (n *methodCallNode) Eval(ctx *ScriptCtx) (any, error) {
	if err := stepCheck(ctx); err != nil {
		return nil, err
	}
	args := make([]any, len(n.args))
	for i, a := range n.args {
		v, err := a.Eval(ctx)
		if err != nil {
			return nil, err
		}
		args[i] = v
	}
	return evalMethod(n.receiver, n.method, args, ctx)
}

func evalMethod(receiver, method string, args []any, ctx *ScriptCtx) (any, error) {
	switch receiver {
	case "Math":
		switch method {
		case "abs":
			return math.Abs(toFloat(args[0])), nil
		case "sqrt":
			return math.Sqrt(toFloat(args[0])), nil
		case "pow":
			return math.Pow(toFloat(args[0]), toFloat(args[1])), nil
		case "max":
			return math.Max(toFloat(args[0]), toFloat(args[1])), nil
		case "min":
			return math.Min(toFloat(args[0]), toFloat(args[1])), nil
		case "log":
			return math.Log(toFloat(args[0])), nil
		case "floor":
			return math.Floor(toFloat(args[0])), nil
		case "ceil":
			return math.Ceil(toFloat(args[0])), nil
		}
	case "":
		// Top-level functions (vector funcs)
		switch method {
		case "cosineSimilarity":
			return cosineSimilarity(args[0], args[1]), nil
		case "dotProduct":
			return dotProduct(args[0], args[1]), nil
		case "l1norm":
			return l1norm(args[0]), nil
		case "l2norm":
			return l2norm(args[0]), nil
		}
	}
	return nil, fmt.Errorf("script: unknown method %s.%s", receiver, method)
}

// ---------------------------------------------------------------------------
// Vector functions (design 03 §9: reuse internal/simd)
// ---------------------------------------------------------------------------

func cosineSimilarity(a, b any) float64 {
	va, vb := toFloatSlice(a), toFloatSlice(b)
	if len(va) != len(vb) || len(va) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range va {
		dot += va[i] * vb[i]
		na += va[i] * va[i]
		nb += vb[i] * vb[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func dotProduct(a, b any) float64 {
	va, vb := toFloatSlice(a), toFloatSlice(b)
	if len(va) != len(vb) || len(va) == 0 {
		return 0
	}
	var sum float64
	for i := range va {
		sum += va[i] * vb[i]
	}
	return sum
}

func l1norm(a any) float64 {
	va := toFloatSlice(a)
	var sum float64
	for _, v := range va {
		sum += math.Abs(v)
	}
	return sum
}

func l2norm(a any) float64 {
	va := toFloatSlice(a)
	var sum float64
	for _, v := range va {
		sum += v * v
	}
	return math.Sqrt(sum)
}

func toFloatSlice(v any) []float64 {
	switch s := v.(type) {
	case []float64:
		return s
	case []float32:
		out := make([]float64, len(s))
		for i, f := range s {
			out[i] = float64(f)
		}
		return out
	case []any:
		out := make([]float64, len(s))
		for i, f := range s {
			out[i] = toFloat(f)
		}
		return out
	default:
		return nil
	}
}

// ---------------------------------------------------------------------------
// Parser (recursive descent)
// ---------------------------------------------------------------------------

type parser struct {
	src string
	pos int
}

func (p *parser) parseExpr() (node, error) {
	return p.parseTernary()
}

func (p *parser) parseTernary() (node, error) {
	cond, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	p.skipSpace()
	if p.peek() != '?' {
		return cond, nil
	}
	p.pos++ // skip '?'
	yes, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	p.skipSpace()
	if p.peek() != ':' {
		return nil, fmt.Errorf("expected ':' in ternary")
	}
	p.pos++ // skip ':'
	no, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	return &ternaryNode{cond: cond, yes: yes, no: no}, nil
}

func (p *parser) parseOr() (node, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for {
		p.skipSpace()
		if !p.peek2("||") {
			return left, nil
		}
		p.pos += 2
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = &binaryNode{op: "||", left: left, right: right}
	}
}

func (p *parser) parseAnd() (node, error) {
	left, err := p.parseEq()
	if err != nil {
		return nil, err
	}
	for {
		p.skipSpace()
		if !p.peek2("&&") {
			return left, nil
		}
		p.pos += 2
		right, err := p.parseEq()
		if err != nil {
			return nil, err
		}
		left = &binaryNode{op: "&&", left: left, right: right}
	}
}

func (p *parser) parseEq() (node, error) {
	left, err := p.parseCmp()
	if err != nil {
		return nil, err
	}
	for {
		p.skipSpace()
		op := ""
		if p.peek2("==") {
			op = "=="
		} else if p.peek2("!=") {
			op = "!="
		}
		if op == "" {
			return left, nil
		}
		p.pos += 2
		right, err := p.parseCmp()
		if err != nil {
			return nil, err
		}
		left = &binaryNode{op: op, left: left, right: right}
	}
}

func (p *parser) parseCmp() (node, error) {
	left, err := p.parseAdd()
	if err != nil {
		return nil, err
	}
	for {
		p.skipSpace()
		op := ""
		if p.peek2("<=") {
			op = "<="
		} else if p.peek2(">=") {
			op = ">="
		} else if p.peek() == '<' {
			op = "<"
		} else if p.peek() == '>' {
			op = ">"
		}
		if op == "" {
			return left, nil
		}
		p.pos += len(op)
		right, err := p.parseAdd()
		if err != nil {
			return nil, err
		}
		left = &binaryNode{op: op, left: left, right: right}
	}
}

func (p *parser) parseAdd() (node, error) {
	left, err := p.parseMul()
	if err != nil {
		return nil, err
	}
	for {
		p.skipSpace()
		c := p.peek()
		if c != '+' && c != '-' {
			return left, nil
		}
		p.pos++
		right, err := p.parseMul()
		if err != nil {
			return nil, err
		}
		left = &binaryNode{op: string(c), left: left, right: right}
	}
}

func (p *parser) parseMul() (node, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		p.skipSpace()
		c := p.peek()
		if c != '*' && c != '/' && c != '%' {
			return left, nil
		}
		p.pos++
		right, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		left = &binaryNode{op: string(c), left: left, right: right}
	}
}

func (p *parser) parseUnary() (node, error) {
	p.skipSpace()
	c := p.peek()
	if c == '!' {
		p.pos++
		expr, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &unaryNode{op: "!", expr: expr}, nil
	}
	if c == '-' {
		p.pos++
		expr, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &unaryNode{op: "-", expr: expr}, nil
	}
	return p.parsePrimary()
}

func (p *parser) parsePrimary() (node, error) {
	p.skipSpace()
	c := p.peek()
	if c == '(' {
		p.pos++
		expr, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		p.skipSpace()
		if p.peek() != ')' {
			return nil, fmt.Errorf("expected ')'")
		}
		p.pos++
		return expr, nil
	}
	if c == '"' || c == '\'' {
		return p.parseString()
	}
	if isDigit(c) || c == '.' {
		return p.parseNumber()
	}
	if isAlpha(c) || c == '_' {
		return p.parseIdentifier()
	}
	return nil, fmt.Errorf("unexpected character %q at %d", c, p.pos)
}

func (p *parser) parseNumber() (node, error) {
	start := p.pos
	for p.pos < len(p.src) && (isDigit(p.src[p.pos]) || p.src[p.pos] == '.') {
		p.pos++
	}
	str := p.src[start:p.pos]
	var f float64
	fmt.Sscanf(str, "%f", &f)
	return &literalNode{val: f}, nil
}

func (p *parser) parseString() (node, error) {
	quote := p.src[p.pos]
	p.pos++
	start := p.pos
	for p.pos < len(p.src) && p.src[p.pos] != quote {
		p.pos++
	}
	s := p.src[start:p.pos]
	if p.pos < len(p.src) {
		p.pos++ // skip closing quote
	}
	return &literalNode{val: s}, nil
}

func (p *parser) parseIdentifier() (node, error) {
	start := p.pos
	for p.pos < len(p.src) && (isAlpha(p.src[p.pos]) || isDigit(p.src[p.pos]) || p.src[p.pos] == '_' || p.src[p.pos] == '.') {
		p.pos++
	}
	name := p.src[start:p.pos]

	// Check for _score
	if name == "_score" {
		return &scoreNode{}, nil
	}

	// doc['field'] / doc["field"] access: the scanner stops at '[',
	// so the bracket expression is parsed here. An optional ".value"
	// suffix is accepted (ES painless: doc['field'].value).
	if name == "doc" && p.peek() == '[' {
		p.pos++ // skip '['
		quote := p.peek()
		if quote != '\'' && quote != '"' {
			return nil, fmt.Errorf("expected quoted field name after doc[")
		}
		p.pos++ // skip opening quote
		fstart := p.pos
		for p.pos < len(p.src) && p.src[p.pos] != quote {
			p.pos++
		}
		if p.pos >= len(p.src) {
			return nil, fmt.Errorf("unterminated field name in doc[...]")
		}
		field := p.src[fstart:p.pos]
		p.pos++ // skip closing quote
		p.skipSpace()
		if p.peek() != ']' {
			return nil, fmt.Errorf("expected ']' after doc['field'")
		}
		p.pos++ // skip ']'
		if p.peek2(".value") {
			p.pos += len(".value")
		}
		return &docFieldNode{field: field}, nil
	}

	// Check for params.key
	if strings.HasPrefix(name, "params.") {
		key := name[7:]
		return &paramsNode{key: key}, nil
	}

	// Check for function/method call (e.g., Math.abs(x))
	p.skipSpace()
	if p.peek() == '(' {
		p.pos++ // skip '('
		var args []node
		p.skipSpace()
		if p.peek() != ')' {
			for {
				arg, err := p.parseExpr()
				if err != nil {
					return nil, err
				}
				args = append(args, arg)
				p.skipSpace()
				if p.peek() == ',' {
					p.pos++
					continue
				}
				break
			}
		}
		p.skipSpace()
		if p.peek() != ')' {
			return nil, fmt.Errorf("expected ')' after arguments")
		}
		p.pos++

		// Determine receiver and method
		parts := strings.SplitN(name, ".", 2)
		if len(parts) == 2 {
			return &methodCallNode{receiver: parts[0], method: parts[1], args: args}, nil
		}
		return &methodCallNode{receiver: "", method: name, args: args}, nil
	}

	// Bare identifier: could be true/false/null
	switch name {
	case "true":
		return &literalNode{val: true}, nil
	case "false":
		return &literalNode{val: false}, nil
	case "null":
		return &literalNode{val: nil}, nil
	}

	return nil, fmt.Errorf("unknown identifier %q", name)
}

func (p *parser) peek() byte {
	if p.pos >= len(p.src) {
		return 0
	}
	return p.src[p.pos]
}

func (p *parser) peek2(s string) bool {
	return p.pos+len(s) <= len(p.src) && p.src[p.pos:p.pos+len(s)] == s
}

func (p *parser) skipSpace() {
	for p.pos < len(p.src) && (p.src[p.pos] == ' ' || p.src[p.pos] == '\t' || p.src[p.pos] == '\n') {
		p.pos++
	}
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isAlpha(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
