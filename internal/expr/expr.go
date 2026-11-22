// Package expr compiles and evaluates the conditions of tessera's rules,
// written in CEL: the variables request and client, their types, tessera's
// functions on IP addresses, and the limits of an expression.
//
// A condition is compiled once, at startup, and type-checked: an unknown
// attribute, a wrong type or a result that is not a bool is a CompileError
// with a position. Evaluating it reads memory only. An evaluation that fails
// returns an *EvalError, whose Cause says why; && and || absorb an error when
// their other operand decides, as CEL defines.
package expr

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common"
	"cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/interpreter"
)

// CostLimit is the largest cost of one evaluation of a condition, in the
// units of CEL's cost model. A typical condition costs 2 to 30.
const CostLimit = 10_000

// MaxLength is the largest number of characters of an expression.
const MaxLength = 8192

// Cause says why a condition could not be evaluated. Its values are part of
// the public interface.
type Cause string

// The causes of an indeterminate condition.
const (
	// CauseClientIPMissing: the condition read client.ip, and the request
	// has no client address.
	CauseClientIPMissing Cause = "client_ip_missing"
	// CauseExpression: the evaluation failed, as a conversion, an overflow
	// or a missing map key do, or exceeded the cost limit.
	CauseExpression Cause = "expression"
	// CausePanic: tessera recovered a panic.
	CausePanic Cause = "panic"
)

// EvalError is the error of a condition that could not be evaluated. Its
// message is the cause only: the error it wraps can hold request data.
type EvalError struct {
	Cause Cause
	err   error
}

func (e *EvalError) Error() string { return "indeterminate condition: " + string(e.Cause) }
func (e *EvalError) Unwrap() error { return e.err }

// ErrCostLimit is wrapped in the EvalError of an evaluation stopped by the
// cost limit.
var ErrCostLimit = errors.New("the condition exceeded its cost limit")

// Options configures an Env.
type Options struct {
	// OnPanic, when set, is called with each panic that tessera recovers in
	// an evaluation, when it recovers it: a panic that && or || then absorb
	// is reported too.
	OnPanic func(*Panic)
}

// Env compiles conditions. It is immutable and safe for concurrent use.
type Env struct {
	cel      *cel.Env
	lib      *library
	progOpts []cel.ProgramOption
}

// NewEnv returns an Env.
func NewEnv(opts Options) (*Env, error) {
	lib := &library{onPanic: opts.OnPanic, parseNetwork: parseNetwork}
	p, err := newProvider(lib)
	if err != nil {
		return nil, err
	}
	envOpts := []cel.EnvOption{
		cel.CustomTypeProvider(p),
		cel.CustomTypeAdapter(p),
		cel.Variable("request", requestType),
		cel.Variable("client", clientType),
		cel.ParserExpressionSizeLimit(MaxLength),
	}
	env, err := cel.NewEnv(append(envOpts, lib.envOptions()...)...)
	if err != nil {
		return nil, err
	}
	return &Env{
		cel: env,
		lib: lib,
		progOpts: []cel.ProgramOption{
			// Computes the constant parts, and compiles each constant
			// regular expression, once, when the program is built.
			cel.EvalOptions(cel.OptOptimize),
			cel.CustomDecoratorV2(lib.prepareInCidr),
			cel.CostLimit(CostLimit),
		},
	}, nil
}

// validatorMessages start the messages of the validators of the
// environment, which hold no text of the expression.
var validatorMessages = []string{"invalid network: ", errNetworkLiteral.Error(), "invalid regular expression: ", "comprehension exceeds nesting limit"}

// SyntaxError is the message of an issue of the parser.
const SyntaxError = "syntax error in the expression"

// stringLiterals returns the string and bytes literals of e that are long
// enough to be recognized in a message.
func stringLiterals(e ast.Expr) []string {
	var out []string
	ast.PreOrderVisit(e, ast.NewExprVisitor(func(d ast.Expr) {
		if d.Kind() != ast.LiteralKind {
			return
		}
		var s string
		switch v := d.AsLiteral().(type) {
		case types.String:
			s = string(v)
		case types.Bytes:
			s = string(v)
		}
		if len(s) >= 3 {
			out = append(out, s)
		}
	}))
	return out
}

// Issue is one error in an expression, at a position in its text.
type Issue struct {
	// Line is the line, from 1, and Column the column, from 1, counted in
	// characters (Unicode code points).
	Line, Column int
	Message      string
}

// CompileError lists the issues of an expression that does not compile. Its
// messages never repeat the expression, but can name its identifiers and
// types.
type CompileError struct {
	Issues []Issue
}

func (e *CompileError) Error() string {
	var b strings.Builder
	for i, is := range e.Issues {
		if i > 0 {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "line %d, column %d: %s", is.Line, is.Column, is.Message)
	}
	return b.String()
}

// Condition is a compiled condition. It is immutable and safe for concurrent
// use.
type Condition struct {
	prg        cel.Program
	always     bool
	attributes []string
	lib        *library
}

// Compile parses, type-checks and validates text, a condition, and builds
// its program. A condition is an expression whose result is a bool.
func (e *Env) Compile(text string) (*Condition, error) {
	if utf8.RuneCountInString(text) > MaxLength {
		return nil, &CompileError{Issues: []Issue{{Line: 1, Column: 1, Message: fmt.Sprintf("the expression is longer than %d characters", MaxLength)}}}
	}
	if strings.TrimSpace(text) == "" {
		return nil, &CompileError{Issues: []Issue{{Line: 1, Column: 1, Message: "the expression is empty"}}}
	}
	// Issues.Err and Issues.String quote the expression: the messages are
	// built from the errors alone. Those of the parser quote the text where
	// it stopped, a string literal included: a syntax error has its position
	// only.
	parsed, iss := e.cel.Parse(text)
	if iss != nil && len(iss.Errors()) > 0 {
		ce := &CompileError{}
		for _, err := range iss.Errors() {
			ce.Issues = append(ce.Issues, issue(err.Location, SyntaxError))
		}
		return nil, ce
	}
	a, iss := e.cel.Check(parsed)
	if iss != nil && len(iss.Errors()) > 0 {
		// The checker names identifiers and types; a message of its own
		// that still holds a literal of the expression is replaced. The
		// messages of tessera's validators are fixed texts.
		literals := stringLiterals(parsed.NativeRep().Expr())
		ce := &CompileError{}
		for _, err := range iss.Errors() {
			message := err.Message
			own := slices.ContainsFunc(validatorMessages, func(prefix string) bool { return strings.HasPrefix(message, prefix) })
			if !own && slices.ContainsFunc(literals, func(l string) bool { return strings.Contains(message, l) }) {
				message = "invalid expression"
			}
			ce.Issues = append(ce.Issues, issue(err.Location, message))
		}
		return nil, ce
	}
	native := a.NativeRep()
	if t := a.OutputType(); !t.IsExactType(types.BoolType) {
		loc := native.SourceInfo().GetLocationByOffset(start(native.Expr(), native.SourceInfo()))
		return nil, &CompileError{Issues: []Issue{issue(loc, fmt.Sprintf("the condition must be a bool, not %s", t))}}
	}
	prg, err := e.cel.Program(a, e.progOpts...)
	if err != nil {
		return nil, &CompileError{Issues: []Issue{{Line: 1, Column: 1, Message: "the expression cannot be prepared for evaluation"}}}
	}
	root := native.Expr()
	always := root.Kind() == ast.LiteralKind && root.AsLiteral() == types.True
	return &Condition{prg: prg, always: always, attributes: attributes(root, native.ReferenceMap()), lib: e.lib}, nil
}

func issue(loc common.Location, message string) Issue {
	// tessera declares no container: the mention of the empty one is noise.
	message = strings.TrimSuffix(message, " (in container '')")
	line, column := 1, 1
	if loc != nil && loc.Line() > 0 {
		line, column = loc.Line(), loc.Column()+1
	}
	return Issue{Line: line, Column: column, Message: message}
}

// Always reports whether the condition is the literal true, the spelling of
// a condition that matches every request.
func (c *Condition) Always() bool { return c.always }

// Eval evaluates the condition for one request. The error, if any, is an
// *EvalError.
func (c *Condition) Eval(in *Input) (bool, error) {
	out, _, err := c.prg.Eval(in)
	if err != nil {
		return false, c.classify(err, in)
	}
	b, ok := out.(types.Bool)
	if !ok {
		return false, &EvalError{Cause: CauseExpression, err: errors.New("the condition did not return a bool")}
	}
	return bool(b), nil
}

// classify sorts the errors of an evaluation:
//   - a *Panic: a guarded function or field of tessera panicked, and was
//     reported when recovered;
//   - interpreter.EvalCancelledError: the cost limit;
//   - ErrClientIPMissing: the condition read client.ip without an address;
//   - any other *types.Err: an ordinary evaluation error;
//   - anything else: cel-go recovered a panic outside tessera's guards and
//     returned "internal error", without a stack; it is reported here.
func (c *Condition) classify(err error, in *Input) *EvalError {
	var p *Panic
	if errors.As(err, &p) {
		return &EvalError{Cause: CausePanic, err: err}
	}
	var cancelled interpreter.EvalCancelledError
	if errors.As(err, &cancelled) {
		if cancelled.Cause == interpreter.CostLimitExceeded {
			return &EvalError{Cause: CauseExpression, err: ErrCostLimit}
		}
		return &EvalError{Cause: CauseExpression, err: err}
	}
	if errors.Is(err, ErrClientIPMissing) {
		return &EvalError{Cause: CauseClientIPMissing, err: err}
	}
	var celErr *types.Err
	if errors.As(err, &celErr) {
		return &EvalError{Cause: CauseExpression, err: err}
	}
	p = &Panic{Rule: in.rule, Value: err.Error()}
	if c.lib.onPanic != nil {
		c.lib.onPanic(p)
	}
	return &EvalError{Cause: CausePanic, err: p}
}

// Attributes returns the attributes that the condition reads, sorted:
// request.host, client.ip and the others by name, and the headers as
// request.headers["name"] when the condition names them with a literal key,
// or as request.headers when it reads the map otherwise.
func (c *Condition) Attributes() []string {
	return c.attributes
}

// attributes lists the attributes that e reads. refs tells the variables
// request and client from iteration variables of the same name.
func attributes(e ast.Expr, refs map[int64]*ast.ReferenceInfo) []string {
	found := map[string]bool{}
	variable := func(e ast.Expr, name string) bool {
		r, ok := refs[e.ID()]
		return e.Kind() == ast.IdentKind && ok && r.Name == name
	}
	// headers reports whether e is request.headers.
	headers := func(e ast.Expr) bool {
		return e.Kind() == ast.SelectKind && e.AsSelect().FieldName() == "headers" && variable(e.AsSelect().Operand(), "request")
	}
	literal := func(e ast.Expr) (string, bool) {
		if e.Kind() != ast.LiteralKind {
			return "", false
		}
		s, ok := e.AsLiteral().(types.String)
		return string(s), ok
	}
	header := func(name string) { found[fmt.Sprintf("request.headers[%q]", name)] = true }
	var walk func(e ast.Expr)
	walk = func(e ast.Expr) {
		switch e.Kind() {
		case ast.SelectKind:
			sel := e.AsSelect()
			switch op := sel.Operand(); {
			case variable(op, "request") || variable(op, "client"):
				found[op.AsIdent()+"."+sel.FieldName()] = true
			case headers(op):
				header(sel.FieldName())
			default:
				walk(op)
			}
		case ast.CallKind:
			call := e.AsCall()
			args := call.Args()
			if len(args) == 2 {
				if key, ok := literal(args[1]); ok && call.FunctionName() == "_[_]" && headers(args[0]) {
					header(key)
					return
				}
				if key, ok := literal(args[0]); ok && call.FunctionName() == "@in" && headers(args[1]) {
					header(key)
					return
				}
			}
			if call.IsMemberFunction() {
				walk(call.Target())
			}
			for _, a := range args {
				walk(a)
			}
		case ast.ComprehensionKind:
			comp := e.AsComprehension()
			for _, part := range []ast.Expr{comp.IterRange(), comp.AccuInit(), comp.LoopCondition(), comp.LoopStep(), comp.Result()} {
				walk(part)
			}
		case ast.ListKind:
			for _, item := range e.AsList().Elements() {
				walk(item)
			}
		case ast.MapKind:
			for _, entry := range e.AsMap().Entries() {
				walk(entry.AsMapEntry().Key())
				walk(entry.AsMapEntry().Value())
			}
		case ast.StructKind:
			for _, field := range e.AsStruct().Fields() {
				walk(field.AsStructField().Value())
			}
		}
	}
	walk(e)
	out := make([]string, 0, len(found))
	for name := range found {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}
