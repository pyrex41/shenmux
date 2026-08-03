// Package shenmodel parses and evaluates the deliberately restricted, pure
// Shen subset used by shenmux. Generated guards embed the exact Shen source and
// route every state predicate and transition through this evaluator.
package shenmodel

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxCallDepth = 512
	maxSteps     = 1_000_000
)

// List is the runtime representation of a Shen list.
type List []any

type nodeKind uint8

const (
	nodeAtom nodeKind = iota
	nodeString
	nodeList
)

type node struct {
	kind     nodeKind
	text     string
	delim    byte
	children []node
}

type clause struct {
	patterns []node
	body     node
}

type function struct {
	name    string
	arity   int
	clauses []clause
}

// Program is an immutable executable model parsed from Shen source.
type Program struct {
	functions map[string]function
	datatypes map[string]struct{}
}

func MustParse(source string) *Program {
	program, err := Parse(source)
	if err != nil {
		panic(err)
	}
	return program
}

func Parse(source string) (*Program, error) {
	forms, err := parseAll(source)
	if err != nil {
		return nil, err
	}
	program := &Program{functions: make(map[string]function), datatypes: make(map[string]struct{})}
	for _, form := range forms {
		if form.kind != nodeList || form.delim != '(' || len(form.children) < 2 || form.children[0].kind != nodeAtom {
			continue
		}
		switch form.children[0].text {
		case "datatype":
			name := form.children[1]
			if name.kind != nodeAtom || name.text == "" {
				return nil, errors.New("datatype has invalid name")
			}
			if _, exists := program.datatypes[name.text]; exists {
				return nil, fmt.Errorf("duplicate datatype %q", name.text)
			}
			program.datatypes[name.text] = struct{}{}
		case "define":
			fn, err := parseFunction(form)
			if err != nil {
				return nil, err
			}
			if _, exists := program.functions[fn.name]; exists {
				return nil, fmt.Errorf("duplicate function %q", fn.name)
			}
			program.functions[fn.name] = fn
		}
	}
	return program, nil
}

func (p *Program) HasDatatype(name string) bool {
	_, ok := p.datatypes[name]
	return ok
}

func (p *Program) FunctionArity(name string) (int, bool) {
	fn, ok := p.functions[name]
	return fn.arity, ok
}

func (p *Program) Call(name string, args ...any) (any, error) {
	state := evalState{steps: maxSteps}
	return p.call(&state, 0, name, args)
}

type evalState struct{ steps int }

func (s *evalState) step() error {
	s.steps--
	if s.steps < 0 {
		return errors.New("Shen model evaluation step limit exceeded")
	}
	return nil
}

func (p *Program) call(state *evalState, depth int, name string, args []any) (any, error) {
	if err := state.step(); err != nil {
		return nil, err
	}
	if depth > maxCallDepth {
		return nil, errors.New("Shen model call depth exceeded")
	}
	fn, ok := p.functions[name]
	if !ok {
		return nil, fmt.Errorf("unknown Shen function %q", name)
	}
	if len(args) != fn.arity {
		return nil, fmt.Errorf("Shen function %s expects %d args, got %d", name, fn.arity, len(args))
	}
	for _, clause := range fn.clauses {
		env := make(map[string]any)
		matched := true
		for i, pattern := range clause.patterns {
			if !match(pattern, args[i], env) {
				matched = false
				break
			}
		}
		if matched {
			value, err := p.eval(state, depth+1, clause.body, env)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			return value, nil
		}
	}
	return nil, fmt.Errorf("Shen function %s has no matching clause", name)
}

func (p *Program) eval(state *evalState, depth int, expr node, env map[string]any) (any, error) {
	if err := state.step(); err != nil {
		return nil, err
	}
	switch expr.kind {
	case nodeString:
		return expr.text, nil
	case nodeAtom:
		if isVariable(expr.text) {
			value, ok := env[expr.text]
			if !ok {
				return nil, fmt.Errorf("unbound variable %s", expr.text)
			}
			return value, nil
		}
		switch expr.text {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
		if number, ok := parseNumber(expr.text); ok {
			return number, nil
		}
		return nil, fmt.Errorf("unsupported atom %q", expr.text)
	case nodeList:
		switch expr.delim {
		case '[':
			return p.evalList(state, depth, expr, env)
		case '(':
			return p.evalCall(state, depth, expr, env)
		default:
			return nil, fmt.Errorf("cannot evaluate %c-delimited form", expr.delim)
		}
	default:
		return nil, errors.New("unknown Shen expression node")
	}
}

func (p *Program) evalList(state *evalState, depth int, expr node, env map[string]any) (any, error) {
	pipe := -1
	for i, child := range expr.children {
		if child.kind == nodeAtom && child.text == "|" {
			if pipe != -1 {
				return nil, errors.New("list expression has multiple cons separators")
			}
			pipe = i
		}
	}
	end := len(expr.children)
	if pipe >= 0 {
		if pipe+2 != len(expr.children) {
			return nil, errors.New("list cons expression requires exactly one tail")
		}
		end = pipe
	}
	out := make(List, 0, end+4)
	for _, child := range expr.children[:end] {
		value, err := p.eval(state, depth+1, child, env)
		if err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	if pipe >= 0 {
		tailValue, err := p.eval(state, depth+1, expr.children[pipe+1], env)
		if err != nil {
			return nil, err
		}
		tail, ok := asList(tailValue)
		if !ok {
			return nil, fmt.Errorf("cons tail is %T, not a list", tailValue)
		}
		out = append(out, tail...)
	}
	return out, nil
}

func (p *Program) evalCall(state *evalState, depth int, expr node, env map[string]any) (any, error) {
	if len(expr.children) == 0 || expr.children[0].kind != nodeAtom {
		return nil, errors.New("call has no function name")
	}
	name := expr.children[0].text
	args := expr.children[1:]
	switch name {
	case "and":
		if len(args) != 2 {
			return nil, errors.New("and expects two arguments")
		}
		left, err := p.eval(state, depth+1, args[0], env)
		if err != nil {
			return nil, err
		}
		leftBool, ok := left.(bool)
		if !ok {
			return nil, fmt.Errorf("and left operand is %T", left)
		}
		if !leftBool {
			return false, nil
		}
		right, err := p.eval(state, depth+1, args[1], env)
		if err != nil {
			return nil, err
		}
		rightBool, ok := right.(bool)
		if !ok {
			return nil, fmt.Errorf("and right operand is %T", right)
		}
		return rightBool, nil
	case "or":
		if len(args) != 2 {
			return nil, errors.New("or expects two arguments")
		}
		left, err := p.eval(state, depth+1, args[0], env)
		if err != nil {
			return nil, err
		}
		leftBool, ok := left.(bool)
		if !ok {
			return nil, fmt.Errorf("or left operand is %T", left)
		}
		if leftBool {
			return true, nil
		}
		right, err := p.eval(state, depth+1, args[1], env)
		if err != nil {
			return nil, err
		}
		rightBool, ok := right.(bool)
		if !ok {
			return nil, fmt.Errorf("or right operand is %T", right)
		}
		return rightBool, nil
	case "not":
		if len(args) != 1 {
			return nil, errors.New("not expects one argument")
		}
		value, err := p.eval(state, depth+1, args[0], env)
		if err != nil {
			return nil, err
		}
		boolean, ok := value.(bool)
		if !ok {
			return nil, fmt.Errorf("not operand is %T", value)
		}
		return !boolean, nil
	case "if":
		if len(args) != 3 {
			return nil, errors.New("if expects three arguments")
		}
		condition, err := p.eval(state, depth+1, args[0], env)
		if err != nil {
			return nil, err
		}
		boolean, ok := condition.(bool)
		if !ok {
			return nil, fmt.Errorf("if condition is %T", condition)
		}
		if boolean {
			return p.eval(state, depth+1, args[1], env)
		}
		return p.eval(state, depth+1, args[2], env)
	case "=", ">", ">=", "<", "<=", "+", "-":
		return p.evalOperator(state, depth, name, args, env)
	case "head", "tail":
		if len(args) != 1 {
			return nil, fmt.Errorf("%s expects one argument", name)
		}
		value, err := p.eval(state, depth+1, args[0], env)
		if err != nil {
			return nil, err
		}
		list, ok := asList(value)
		if !ok || len(list) == 0 {
			return nil, fmt.Errorf("%s requires a nonempty list", name)
		}
		if name == "head" {
			return list[0], nil
		}
		return append(List(nil), list[1:]...), nil
	default:
		values := make([]any, len(args))
		for i, arg := range args {
			value, err := p.eval(state, depth+1, arg, env)
			if err != nil {
				return nil, err
			}
			values[i] = value
		}
		return p.call(state, depth+1, name, values)
	}
}

func (p *Program) evalOperator(state *evalState, depth int, name string, args []node, env map[string]any) (any, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("%s expects two arguments", name)
	}
	left, err := p.eval(state, depth+1, args[0], env)
	if err != nil {
		return nil, err
	}
	right, err := p.eval(state, depth+1, args[1], env)
	if err != nil {
		return nil, err
	}
	if name == "=" {
		return reflect.DeepEqual(normalizeLists(left), normalizeLists(right)), nil
	}
	leftNumber, leftOK := asUint64(left)
	rightNumber, rightOK := asUint64(right)
	if !leftOK || !rightOK {
		return nil, fmt.Errorf("%s operands must be nonnegative integers, got %T and %T", name, left, right)
	}
	switch name {
	case ">":
		return leftNumber > rightNumber, nil
	case ">=":
		return leftNumber >= rightNumber, nil
	case "<":
		return leftNumber < rightNumber, nil
	case "<=":
		return leftNumber <= rightNumber, nil
	case "+":
		if ^uint64(0)-leftNumber < rightNumber {
			return nil, errors.New("integer addition overflow")
		}
		return leftNumber + rightNumber, nil
	case "-":
		if leftNumber < rightNumber {
			return nil, errors.New("integer subtraction underflow")
		}
		return leftNumber - rightNumber, nil
	default:
		return nil, fmt.Errorf("unknown operator %s", name)
	}
}

func parseFunction(form node) (function, error) {
	if len(form.children) < 4 || form.children[1].kind != nodeAtom {
		return function{}, errors.New("malformed define form")
	}
	fn := function{name: form.children[1].text}
	i := 2
	if i < len(form.children) && form.children[i].kind == nodeList && form.children[i].delim == '{' {
		i++
	}
	for i < len(form.children) {
		start := i
		for i < len(form.children) && !(form.children[i].kind == nodeAtom && form.children[i].text == "->") {
			i++
		}
		if i == len(form.children) {
			return function{}, fmt.Errorf("define %s clause has no ->", fn.name)
		}
		patterns := append([]node(nil), form.children[start:i]...)
		i++
		if i >= len(form.children) {
			return function{}, fmt.Errorf("define %s clause has no body", fn.name)
		}
		body := form.children[i]
		i++
		if len(patterns) == 0 {
			return function{}, fmt.Errorf("define %s has zero-pattern clause", fn.name)
		}
		if fn.arity == 0 {
			fn.arity = len(patterns)
		} else if len(patterns) != fn.arity {
			return function{}, fmt.Errorf("define %s mixes arities %d and %d", fn.name, fn.arity, len(patterns))
		}
		fn.clauses = append(fn.clauses, clause{patterns: patterns, body: body})
	}
	if len(fn.clauses) == 0 {
		return function{}, fmt.Errorf("define %s has no clauses", fn.name)
	}
	return fn, nil
}

func match(pattern node, value any, env map[string]any) bool {
	switch pattern.kind {
	case nodeString:
		stringValue, ok := value.(string)
		return ok && stringValue == pattern.text
	case nodeAtom:
		if pattern.text == "_" {
			return true
		}
		if isVariable(pattern.text) {
			if existing, ok := env[pattern.text]; ok {
				return reflect.DeepEqual(normalizeLists(existing), normalizeLists(value))
			}
			env[pattern.text] = value
			return true
		}
		switch pattern.text {
		case "true":
			boolean, ok := value.(bool)
			return ok && boolean
		case "false":
			boolean, ok := value.(bool)
			return ok && !boolean
		}
		if number, ok := parseNumber(pattern.text); ok {
			other, ok := asUint64(value)
			return ok && number == other
		}
		return false
	case nodeList:
		if pattern.delim != '[' {
			return false
		}
		list, ok := asList(value)
		if !ok {
			return false
		}
		pipe := -1
		for i, child := range pattern.children {
			if child.kind == nodeAtom && child.text == "|" {
				pipe = i
				break
			}
		}
		if pipe < 0 {
			if len(list) != len(pattern.children) {
				return false
			}
			for i, child := range pattern.children {
				if !match(child, list[i], env) {
					return false
				}
			}
			return true
		}
		if pipe+2 != len(pattern.children) || len(list) < pipe {
			return false
		}
		for i := 0; i < pipe; i++ {
			if !match(pattern.children[i], list[i], env) {
				return false
			}
		}
		return match(pattern.children[pipe+1], append(List(nil), list[pipe:]...), env)
	default:
		return false
	}
}

func asList(value any) (List, bool) {
	switch list := value.(type) {
	case List:
		return list, true
	case []any:
		return List(list), true
	default:
		return nil, false
	}
}

func normalizeLists(value any) any {
	list, ok := asList(value)
	if !ok {
		return value
	}
	out := make(List, len(list))
	for i, item := range list {
		out[i] = normalizeLists(item)
	}
	return out
}

func asUint64(value any) (uint64, bool) {
	switch number := value.(type) {
	case uint64:
		return number, true
	case uint32:
		return uint64(number), true
	case uint16:
		return uint64(number), true
	case uint8:
		return uint64(number), true
	case int:
		if number < 0 {
			return 0, false
		}
		return uint64(number), true
	case int64:
		if number < 0 {
			return 0, false
		}
		return uint64(number), true
	default:
		return 0, false
	}
}

func parseNumber(text string) (uint64, bool) {
	if text == "" || strings.HasPrefix(text, "-") {
		return 0, false
	}
	value, err := strconv.ParseUint(text, 10, 64)
	return value, err == nil
}

func isVariable(text string) bool {
	if text == "" || text == "_" {
		return false
	}
	r, _ := utf8.DecodeRuneInString(text)
	return unicode.IsUpper(r)
}

type scanner struct {
	source string
	pos    int
}

func parseAll(source string) ([]node, error) {
	s := &scanner{source: source}
	var forms []node
	for {
		s.skipSpaceAndComments()
		if s.pos >= len(s.source) {
			return forms, nil
		}
		form, err := s.readNode()
		if err != nil {
			return nil, err
		}
		forms = append(forms, form)
	}
}

func (s *scanner) readNode() (node, error) {
	s.skipSpaceAndComments()
	if s.pos >= len(s.source) {
		return node{}, ioError("unexpected end of Shen source")
	}
	ch := s.source[s.pos]
	switch ch {
	case '(', '[', '{':
		s.pos++
		closing := map[byte]byte{'(': ')', '[': ']', '{': '}'}[ch]
		result := node{kind: nodeList, delim: ch}
		for {
			s.skipSpaceAndComments()
			if s.pos >= len(s.source) {
				return node{}, ioError(fmt.Sprintf("unterminated %c form", ch))
			}
			if s.source[s.pos] == closing {
				s.pos++
				return result, nil
			}
			if s.source[s.pos] == ')' || s.source[s.pos] == ']' || s.source[s.pos] == '}' {
				return node{}, ioError(fmt.Sprintf("mismatched closing delimiter %c", s.source[s.pos]))
			}
			child, err := s.readNode()
			if err != nil {
				return node{}, err
			}
			result.children = append(result.children, child)
		}
	case '"':
		return s.readString()
	case ')', ']', '}':
		return node{}, ioError(fmt.Sprintf("unexpected closing delimiter %c", ch))
	default:
		return s.readAtom()
	}
}

func (s *scanner) readString() (node, error) {
	s.pos++
	var out strings.Builder
	for s.pos < len(s.source) {
		ch := s.source[s.pos]
		s.pos++
		if ch == '"' {
			return node{kind: nodeString, text: out.String()}, nil
		}
		if ch != '\\' {
			out.WriteByte(ch)
			continue
		}
		if s.pos >= len(s.source) {
			return node{}, ioError("unterminated string escape")
		}
		escaped := s.source[s.pos]
		s.pos++
		switch escaped {
		case 'n':
			out.WriteByte('\n')
		case 'r':
			out.WriteByte('\r')
		case 't':
			out.WriteByte('\t')
		case '\\', '"':
			out.WriteByte(escaped)
		default:
			out.WriteByte(escaped)
		}
	}
	return node{}, ioError("unterminated Shen string")
}

func (s *scanner) readAtom() (node, error) {
	start := s.pos
	for s.pos < len(s.source) {
		ch := s.source[s.pos]
		if unicode.IsSpace(rune(ch)) || strings.ContainsRune("()[]{}\"", rune(ch)) {
			break
		}
		if ch == '\\' && s.pos+1 < len(s.source) && s.source[s.pos+1] == '\\' {
			break
		}
		s.pos++
	}
	if start == s.pos {
		return node{}, ioError(fmt.Sprintf("unexpected byte %q", s.source[s.pos]))
	}
	return node{kind: nodeAtom, text: s.source[start:s.pos]}, nil
}

func (s *scanner) skipSpaceAndComments() {
	for {
		for s.pos < len(s.source) && unicode.IsSpace(rune(s.source[s.pos])) {
			s.pos++
		}
		if s.pos+1 < len(s.source) && s.source[s.pos] == '\\' && s.source[s.pos+1] == '\\' {
			s.pos += 2
			for s.pos < len(s.source) && s.source[s.pos] != '\n' {
				s.pos++
			}
			continue
		}
		return
	}
}

type parseError string

func (e parseError) Error() string { return string(e) }
func ioError(message string) error { return parseError(message) }
