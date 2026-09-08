package main

// A GitHub Actions expression evaluator, present so this gate can DECIDE a step's guard
// instead of restating it.
//
// The alternative - matching the text of `if:` against a pattern - cannot answer the only
// question that matters ("would this step run?"), because the answer depends on values a
// shell produces at runtime. A guard that reads `steps.plan.outputs.publish == 'true'`
// looks identical whether the planning script sets publish=false on a dispatch or sets it
// to true; the text is the same and the release is not. So the planning script is RUN
// (see shell.go) and its real outputs are fed to this evaluator.
//
// It is deliberately fail-closed. A context path that the run did not produce, a function
// this evaluator does not implement, or a syntax it cannot parse is an ERROR that reds the
// gate - never a silently-false guard, which would read as "that publishing step does not
// run" and is exactly the wrong way to be wrong.

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// --- values -------------------------------------------------------------------------
//
// An expression value is one of: string, float64, bool, nil. That is GitHub's own set
// minus objects/arrays, which no guard in a release workflow needs.

func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case float64:
		return t != 0 && !math.IsNaN(t)
	case string:
		return t != ""
	default:
		return false
	}
}

// stringify renders a value the way GitHub interpolates it into a string.
func stringify(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		if t == math.Trunc(t) && !math.IsInf(t, 0) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'g', -1, 64)
	case string:
		return t
	default:
		return fmt.Sprintf("%v", t)
	}
}

func toNumber(v any) float64 {
	switch t := v.(type) {
	case nil:
		return 0
	case bool:
		if t {
			return 1
		}
		return 0
	case float64:
		return t
	case string:
		if strings.TrimSpace(t) == "" {
			return 0
		}
		n, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return math.NaN()
		}
		return n
	default:
		return math.NaN()
	}
}

// looseEqual is GitHub's `==`: strings compare case-insensitively, and operands of
// different types are coerced to numbers (a non-numeric string becomes NaN, which is
// equal to nothing, including itself).
func looseEqual(a, b any) bool {
	as, aok := a.(string)
	bs, bok := b.(string)
	if aok && bok {
		return strings.EqualFold(as, bs)
	}
	if a == nil && b == nil {
		return true
	}
	an, bn := toNumber(a), toNumber(b)
	if math.IsNaN(an) || math.IsNaN(bn) {
		return false
	}
	return an == bn
}

// --- lexer --------------------------------------------------------------------------

type tokKind int

const (
	tokEOF tokKind = iota
	tokIdent
	tokString
	tokNumber
	tokOp
)

type token struct {
	kind tokKind
	text string
	num  float64
}

// operators, LONGEST FIRST: "!=" must be matched before "!", or `a != b` lexes as a
// negation and the comparison silently disappears.
var operators = []string{"==", "!=", "<=", ">=", "&&", "||", "!", "(", ")", ",", "<", ">"}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9') || c == '-' || c == '.' || c == '*'
}

func lex(src string) ([]token, error) {
	var out []token
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '\'':
			s, next, err := lexString(src, i)
			if err != nil {
				return nil, err
			}
			out = append(out, token{kind: tokString, text: s})
			i = next
		case c >= '0' && c <= '9':
			j := i
			for j < len(src) && ((src[j] >= '0' && src[j] <= '9') || src[j] == '.') {
				j++
			}
			n, err := strconv.ParseFloat(src[i:j], 64)
			if err != nil {
				return nil, fmt.Errorf("not a number: %q", src[i:j])
			}
			out = append(out, token{kind: tokNumber, text: src[i:j], num: n})
			i = j
		case isIdentStart(c):
			j := i
			for j < len(src) && isIdentPart(src[j]) {
				j++
			}
			out = append(out, token{kind: tokIdent, text: src[i:j]})
			i = j
		default:
			op := ""
			for _, cand := range operators {
				if strings.HasPrefix(src[i:], cand) {
					op = cand
					break
				}
			}
			if op == "" {
				return nil, fmt.Errorf("unexpected character %q at offset %d", string(c), i)
			}
			out = append(out, token{kind: tokOp, text: op})
			i += len(op)
		}
	}
	return append(out, token{kind: tokEOF}), nil
}

func lexString(src string, start int) (string, int, error) {
	var sb strings.Builder
	for j := start + 1; ; {
		if j >= len(src) {
			return "", 0, fmt.Errorf("unterminated string literal")
		}
		if src[j] == '\'' {
			if j+1 < len(src) && src[j+1] == '\'' { // '' is an escaped quote
				sb.WriteByte('\'')
				j += 2
				continue
			}
			return sb.String(), j + 1, nil
		}
		sb.WriteByte(src[j])
		j++
	}
}

// --- context ------------------------------------------------------------------------

// evalCtx carries the values a guard may read and the job's success state.
//
// success is what `success()` returns, and - crucially - what a guard with NO status
// function gets implicitly: GitHub inserts `success() &&` in front of every `if:` that
// does not mention one. That implicit wrap is the whole reason a failing gate leaves
// `:latest` alone, so this evaluator models it rather than assuming it.
type evalCtx struct {
	vars    map[string]any
	success bool
}

func (c evalCtx) lookup(path string) (any, error) {
	parts := strings.Split(path, ".")
	var cur any = c.vars
	for i, p := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("expression reads %q, but %q is not an object", path, strings.Join(parts[:i], "."))
		}
		v, ok := m[p]
		if !ok {
			return nil, fmt.Errorf("expression reads %q, which this run did not produce (no %q under %q)", path, p, strings.Join(parts[:i], "."))
		}
		cur = v
	}
	if _, isMap := cur.(map[string]any); isMap {
		return nil, fmt.Errorf("expression reads %q, which is an object, not a value", path)
	}
	return cur, nil
}

// --- parser -------------------------------------------------------------------------

type parser struct {
	toks []token
	pos  int
	ctx  evalCtx
}

func (p *parser) peek() token { return p.toks[p.pos] }

func (p *parser) acceptOp(op string) bool {
	if p.toks[p.pos].kind == tokOp && p.toks[p.pos].text == op {
		p.pos++
		return true
	}
	return false
}

// Evaluate runs one GitHub expression (without the ${{ }} wrapper) against ctx.
func Evaluate(src string, ctx evalCtx) (any, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, fmt.Errorf("cannot parse expression %q: %w", src, err)
	}
	p := &parser{toks: toks, ctx: ctx}
	v, err := p.or()
	if err != nil {
		return nil, fmt.Errorf("cannot evaluate expression %q: %w", src, err)
	}
	if p.peek().kind != tokEOF {
		return nil, fmt.Errorf("cannot parse expression %q: trailing input at %q", src, p.peek().text)
	}
	return v, nil
}

// or/and return the OPERAND, not a boolean - that is JavaScript's rule and GitHub's.
func (p *parser) or() (any, error) {
	left, err := p.and()
	if err != nil {
		return nil, err
	}
	for p.acceptOp("||") {
		right, err := p.and()
		if err != nil {
			return nil, err
		}
		if !truthy(left) {
			left = right
		}
	}
	return left, nil
}

func (p *parser) and() (any, error) {
	left, err := p.compare()
	if err != nil {
		return nil, err
	}
	for p.acceptOp("&&") {
		right, err := p.compare()
		if err != nil {
			return nil, err
		}
		if truthy(left) {
			left = right
		}
	}
	return left, nil
}

func (p *parser) compare() (any, error) {
	left, err := p.unary()
	if err != nil {
		return nil, err
	}
	for _, op := range []string{"==", "!=", "<=", ">=", "<", ">"} {
		if !p.acceptOp(op) {
			continue
		}
		right, err := p.unary()
		if err != nil {
			return nil, err
		}
		switch op {
		case "==":
			return looseEqual(left, right), nil
		case "!=":
			return !looseEqual(left, right), nil
		}
		ln, rn := toNumber(left), toNumber(right)
		if math.IsNaN(ln) || math.IsNaN(rn) {
			return false, nil
		}
		switch op {
		case "<":
			return ln < rn, nil
		case "<=":
			return ln <= rn, nil
		case ">":
			return ln > rn, nil
		default:
			return ln >= rn, nil
		}
	}
	return left, nil
}

func (p *parser) unary() (any, error) {
	if p.acceptOp("!") {
		v, err := p.unary()
		if err != nil {
			return nil, err
		}
		return !truthy(v), nil
	}
	return p.primary()
}

func (p *parser) primary() (any, error) {
	t := p.peek()
	switch t.kind {
	case tokOp:
		if p.acceptOp("(") {
			v, err := p.or()
			if err != nil {
				return nil, err
			}
			if !p.acceptOp(")") {
				return nil, fmt.Errorf("missing closing parenthesis")
			}
			return v, nil
		}
		return nil, fmt.Errorf("unexpected operator %q", t.text)
	case tokString:
		p.pos++
		return t.text, nil
	case tokNumber:
		p.pos++
		return t.num, nil
	case tokIdent:
		p.pos++
		switch strings.ToLower(t.text) {
		case "true":
			return true, nil
		case "false":
			return false, nil
		case "null":
			return nil, nil
		}
		if p.peek().kind == tokOp && p.peek().text == "(" {
			return p.call(t.text)
		}
		return p.ctx.lookup(t.text)
	default:
		return nil, fmt.Errorf("unexpected end of expression")
	}
}

func (p *parser) call(name string) (any, error) {
	if !p.acceptOp("(") {
		return nil, fmt.Errorf("internal: call without an open parenthesis")
	}
	var args []any
	if !p.acceptOp(")") {
		for {
			v, err := p.or()
			if err != nil {
				return nil, err
			}
			args = append(args, v)
			if p.acceptOp(",") {
				continue
			}
			if p.acceptOp(")") {
				break
			}
			return nil, fmt.Errorf("malformed argument list for %s()", name)
		}
	}
	return p.apply(strings.ToLower(name), args)
}

func (p *parser) apply(name string, args []any) (any, error) {
	want := func(n int) error {
		if len(args) != n {
			return fmt.Errorf("%s() takes %d argument(s), got %d", name, n, len(args))
		}
		return nil
	}
	switch name {
	case "success":
		if err := want(0); err != nil {
			return nil, err
		}
		return p.ctx.success, nil
	case "failure":
		if err := want(0); err != nil {
			return nil, err
		}
		return !p.ctx.success, nil
	case "always":
		if err := want(0); err != nil {
			return nil, err
		}
		return true, nil
	case "cancelled":
		if err := want(0); err != nil {
			return nil, err
		}
		return false, nil
	case "startswith", "endswith", "contains":
		if err := want(2); err != nil {
			return nil, err
		}
		a, b := strings.ToLower(stringify(args[0])), strings.ToLower(stringify(args[1]))
		switch name {
		case "startswith":
			return strings.HasPrefix(a, b), nil
		case "endswith":
			return strings.HasSuffix(a, b), nil
		default:
			return strings.Contains(a, b), nil
		}
	default:
		// Fail-closed: an unimplemented function must not evaluate to false, which would
		// read as "this publishing step does not run".
		return nil, fmt.Errorf("this gate does not implement the expression function %s(); it cannot decide whether the step runs, so it refuses to guess", name)
	}
}

// --- conditions and interpolation ----------------------------------------------------

var statusFuncs = map[string]bool{"success": true, "failure": true, "always": true, "cancelled": true}

// ConditionRuns answers "would a step carrying this `if:` run, in this job state?".
//
// An empty condition, or one that names no status function, is implicitly `success() &&
// (...)`: that implicit wrap is what makes a failed gate leave the floating tag where it
// was, so it is modelled here rather than assumed.
func ConditionRuns(cond string, ctx evalCtx) (bool, error) {
	cond = strings.TrimSpace(cond)
	if cond == "" {
		return ctx.success, nil
	}
	if strings.HasPrefix(cond, "${{") && strings.HasSuffix(cond, "}}") {
		cond = strings.TrimSpace(cond[3 : len(cond)-2])
	}
	toks, err := lex(cond)
	if err != nil {
		return false, fmt.Errorf("cannot parse condition %q: %w", cond, err)
	}
	implicit := true
	for _, t := range toks {
		if t.kind == tokIdent && statusFuncs[strings.ToLower(t.text)] {
			implicit = false
			break
		}
	}
	v, err := Evaluate(cond, ctx)
	if err != nil {
		return false, err
	}
	if implicit && !ctx.success {
		return false, nil
	}
	return truthy(v), nil
}

// Interpolate replaces every ${{ … }} span in s with the value it evaluates to.
func Interpolate(s string, ctx evalCtx) (string, error) {
	var sb strings.Builder
	for {
		i := strings.Index(s, "${{")
		if i < 0 {
			sb.WriteString(s)
			return sb.String(), nil
		}
		j := strings.Index(s[i:], "}}")
		if j < 0 {
			return "", fmt.Errorf("unterminated ${{ … }} in %q", s)
		}
		sb.WriteString(s[:i])
		v, err := Evaluate(strings.TrimSpace(s[i+3:i+j]), ctx)
		if err != nil {
			return "", err
		}
		sb.WriteString(stringify(v))
		s = s[i+j+2:]
	}
}
