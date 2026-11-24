// Package expr compiles and evaluates the expressions of tessera, written in
// CEL: the conditions of the rules, and the string expressions of the
// upstream headers and of the decision fields. It declares the variables
// request, client, sets, geo, asn and ua, their types, tessera's functions on
// IP addresses and sets, and the limits of an expression.
//
// An expression is compiled once, at startup, and type-checked: an unknown
// attribute, a wrong type or a result of another type than the one expected
// is a CompileError with a position. A decision field cannot read
// request.query, a secret header, nor request.headers otherwise than by a
// literal name. Evaluating an expression reads memory
// only. An evaluation that fails returns an *EvalError, whose Cause says why;
// && and || absorb an error when their other operand decides, as CEL
// defines.
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
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/interpreter"

	"github.com/gtriggiano/tessera/internal/enrichment"
	"github.com/gtriggiano/tessera/internal/sets"
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
	// CauseClientIPMissing: the condition read client.ip, geo or asn, and
	// the request has no client address.
	CauseClientIPMissing Cause = "client_ip_missing"
	// CauseExpression: the evaluation failed, as a conversion, an overflow
	// or a missing map key do, or exceeded the cost limit.
	CauseExpression Cause = "expression"
	// CausePanic: tessera recovered a panic.
	CausePanic Cause = "panic"
	// CauseSetUnavailable: the condition read a set that was not
	// available, and whose on_unavailable is fail.
	CauseSetUnavailable Cause = "set_unavailable"
	// CauseDatabaseError: the condition read geo or asn, and the lookup of
	// the client address in the database failed.
	CauseDatabaseError Cause = "database_error"
)

// Causes are the causes of an indeterminate condition, in the order of the
// metrics reference.
var Causes = []Cause{CauseClientIPMissing, CauseExpression, CausePanic, CauseSetUnavailable, CauseDatabaseError}

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
	// Sets declares the sets: each is a field of the variable sets, in the
	// order of the entries of the Snapshot that NewInput receives.
	Sets []sets.Declaration
	// Databases are the kinds of the configured databases: an expression
	// can read geo, or asn, only when its kind is among them.
	Databases []enrichment.Kind
	// UserAgent: the analysis of the user-agent header is configured, and
	// an expression can read ua.
	UserAgent bool
	// SecretHeaders are the names of the secret headers, in any case, in
	// addition to AlwaysSecretHeaders: a decision field cannot read them.
	SecretHeaders []string
}

// AlwaysSecretHeaders are the headers that are secret in every
// configuration: they hold credentials.
var AlwaysSecretHeaders = []string{"authorization", "proxy-authorization", "cookie"}

// Env compiles conditions. It is immutable and safe for concurrent use.
type Env struct {
	cel      *cel.Env
	lib      *library
	progOpts []cel.ProgramOption
}

// NewEnv returns an Env.
func NewEnv(opts Options) (*Env, error) {
	lib := &library{onPanic: opts.OnPanic, parseNetwork: parseNetwork, sets: slices.Clone(opts.Sets), databases: slices.Clone(opts.Databases), userAgent: opts.UserAgent, secrets: slices.Clone(AlwaysSecretHeaders)}
	for _, name := range opts.SecretHeaders {
		lib.secrets = append(lib.secrets, strings.ToLower(name))
	}
	p, err := newProvider(lib)
	if err != nil {
		return nil, err
	}
	envOpts := []cel.EnvOption{
		cel.CustomTypeProvider(p),
		cel.CustomTypeAdapter(p),
		cel.Variable("request", requestType),
		cel.Variable("client", clientType),
		cel.Variable("sets", setsType),
		cel.Variable("geo", geoType),
		cel.Variable("asn", asnType),
		cel.Variable("ua", userAgentType),
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
			cel.CustomDecoratorV2(lib.prepareAreaContains),
			cel.CostLimit(CostLimit),
			cel.CostTrackerOptions(costOptions()...),
		},
	}, nil
}

// validatorMessages start the messages of the validators of the
// environment, which hold no text of the expression.
var validatorMessages = []string{"invalid network: ", errNetworkLiteral.Error(), "invalid regular expression: ", "comprehension exceeds nesting limit", errSetUse.Error(), "geo is not available: ", "asn is not available: ", errUserAgentMissing.Error(), "a decision field "}

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

// program is a compiled expression, and what it reads.
type program struct {
	prg        cel.Program
	attributes []string
	sets       []string
	databases  []enrichment.Kind
	userAgent  bool
	lib        *library
}

// Condition is a compiled condition. It is immutable and safe for concurrent
// use.
type Condition struct {
	program
	always bool
}

// StringExpr is a compiled expression whose result is a string: the value of
// an upstream header or of a decision field. It is immutable and safe for
// concurrent use.
type StringExpr struct {
	program
	secrets bool
}

// ReadsSecrets reports whether the value of the expression can hold the
// value of a secret header, of AlwaysSecretHeaders or of
// Options.SecretHeaders: it reads one of them, in any spelling, or reads
// request.headers otherwise than by a literal name. It is decided when the
// expression is compiled, from its text. A decision field never does.
func (s *StringExpr) ReadsSecrets() bool { return s.secrets }

// Compile parses, type-checks and validates text, a condition, and builds
// its program. A condition is an expression whose result is a bool.
func (e *Env) Compile(text string) (*Condition, error) {
	a, p, err := e.compile(text, types.BoolType, "the condition must be a bool, not %s", false)
	if err != nil {
		return nil, err
	}
	root := a.Expr()
	always := root.Kind() == ast.LiteralKind && root.AsLiteral() == types.True
	return &Condition{program: p, always: always}, nil
}

// CompileString compiles text, an expression whose result is a string, as
// Compile compiles a condition: the value of an upstream header.
func (e *Env) CompileString(text string) (*StringExpr, error) {
	a, p, err := e.compile(text, types.StringType, "the expression must be a string, not %s", false)
	if err != nil {
		return nil, err
	}
	return &StringExpr{program: p, secrets: readsSecrets(a, e.lib.secrets)}, nil
}

// CompileDecisionField compiles text, the expression of a decision field,
// as CompileString does. A decision field is written in the log events, so
// it cannot read what tessera never logs: request.query, a secret header, in
// any spelling, nor request.headers otherwise than by a literal name, as in request.headers["user-agent"],
// request.headers.accept or "x-tag" in request.headers. Each such read is an
// issue at the position of request.
func (e *Env) CompileDecisionField(text string) (*StringExpr, error) {
	_, p, err := e.compile(text, types.StringType, "the expression must be a string, not %s", true)
	if err != nil {
		return nil, err
	}
	return &StringExpr{program: p}, nil
}

// compile parses, type-checks and validates text, whose result must be of
// the type want, with the message wrong otherwise, and, for a decision
// field, checks what it reads. It returns its checked form and its program.
func (e *Env) compile(text string, want *types.Type, wrong string, field bool) (*ast.AST, program, error) {
	if utf8.RuneCountInString(text) > MaxLength {
		return nil, program{}, &CompileError{Issues: []Issue{{Line: 1, Column: 1, Message: fmt.Sprintf("the expression is longer than %d characters", MaxLength)}}}
	}
	if strings.TrimSpace(text) == "" {
		return nil, program{}, &CompileError{Issues: []Issue{{Line: 1, Column: 1, Message: "the expression is empty"}}}
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
		return nil, program{}, ce
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
		return nil, program{}, ce
	}
	native := a.NativeRep()
	if t := a.OutputType(); !t.IsExactType(want) {
		loc := native.SourceInfo().GetLocationByOffset(start(native.Expr(), native.SourceInfo()))
		return nil, program{}, &CompileError{Issues: []Issue{issue(loc, fmt.Sprintf(wrong, t))}}
	}
	if field {
		if issues := fieldIssues(native, e.lib.secrets); len(issues) > 0 {
			return nil, program{}, &CompileError{Issues: issues}
		}
	}
	prg, err := e.cel.Program(a, e.progOpts...)
	if err != nil {
		return nil, program{}, &CompileError{Issues: []Issue{{Line: 1, Column: 1, Message: "the expression cannot be prepared for evaluation"}}}
	}
	return native, program{prg: prg, attributes: attributes(native.Expr(), native.ReferenceMap()), sets: setsRead(native), databases: databasesRead(native), userAgent: userAgentRead(native), lib: e.lib}, nil
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
	out, err := c.eval(in)
	if err != nil {
		return false, err
	}
	b, ok := out.(types.Bool)
	if !ok {
		return false, &EvalError{Cause: CauseExpression, err: errors.New("the condition did not return a bool")}
	}
	return bool(b), nil
}

// Eval evaluates the expression for one request. The error, if any, is an
// *EvalError.
func (s *StringExpr) Eval(in *Input) (string, error) {
	out, err := s.eval(in)
	if err != nil {
		return "", err
	}
	str, ok := out.(types.String)
	if !ok {
		return "", &EvalError{Cause: CauseExpression, err: errors.New("the expression did not return a string")}
	}
	return string(str), nil
}

// eval evaluates the program with in. The error, if any, is an *EvalError.
func (p *program) eval(in *Input) (ref.Val, error) {
	// The expressions of one Input come from one Env: its library reports a
	// panic of a lookup.
	in.lib = p.lib
	out, _, err := p.prg.Eval(in)
	if err != nil {
		return nil, p.classify(err, in)
	}
	return out, nil
}

// classify sorts the errors of an evaluation:
//   - a *Panic: a guarded function or field of tessera panicked, and was
//     reported when recovered;
//   - interpreter.EvalCancelledError: the cost limit;
//   - ErrClientIPMissing: the condition read client.ip without an address;
//   - ErrSetUnavailable: contains on an unavailable set, whose
//     on_unavailable is fail;
//   - ErrDatabase: a read of geo or asn whose lookup failed;
//   - any other *types.Err: an ordinary evaluation error;
//   - anything else: cel-go recovered a panic outside tessera's guards and
//     returned "internal error", without a stack; it is reported here.
func (p *program) classify(err error, in *Input) *EvalError {
	var pn *Panic
	if errors.As(err, &pn) {
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
	if errors.Is(err, ErrSetUnavailable) {
		return &EvalError{Cause: CauseSetUnavailable, err: err}
	}
	if errors.Is(err, ErrDatabase) {
		return &EvalError{Cause: CauseDatabaseError, err: err}
	}
	var celErr *types.Err
	if errors.As(err, &celErr) {
		return &EvalError{Cause: CauseExpression, err: err}
	}
	pn = &Panic{Rule: in.rule, Value: err.Error()}
	if p.lib.onPanic != nil {
		p.lib.onPanic(pn)
	}
	return &EvalError{Cause: CausePanic, err: pn}
}

// Attributes returns the attributes that the expression reads, sorted:
// request.host, client.ip, geo.city, ua.device and the others by name, the
// headers as request.headers["name"] when the condition names them with a
// literal key, or as request.headers when it reads the map otherwise; a
// field of an object of geo, its country, continent or location, or of ua,
// its browser, operating system or bot, as geo.country.code,
// geo.location.lat or ua.browser.name, or the object as geo.country or
// ua.browser when the expression reads it as a whole, as has(geo.location)
// and ua.browser == ua.browser do; and geo.subdivisions for any read of the
// list or of its items, such as geo.subdivisions[0].code or a macro over it.
func (p *program) Attributes() []string {
	return p.attributes
}

// Sets returns the names of the sets that the expression reads, sorted.
func (p *program) Sets() []string {
	return p.sets
}

// Databases returns the kinds of the databases that the expression reads,
// through geo or asn, sorted.
func (p *program) Databases() []enrichment.Kind {
	return p.databases
}

// ReadsUserAgent reports whether the expression reads ua.
func (p *program) ReadsUserAgent() bool {
	return p.userAgent
}

// userAgentRead reports whether a reads the variable ua, not an iteration
// variable of the same name.
func userAgentRead(a *ast.AST) bool {
	return len(ast.MatchDescendants(ast.NavigateAST(a), func(e ast.NavigableExpr) bool { return isUserAgent(a, e) })) > 0
}

// databasesRead lists the databases that a reads: the variables geo and
// asn, not iteration variables of the same name.
func databasesRead(a *ast.AST) []enrichment.Kind {
	var out []enrichment.Kind
	for _, d := range databaseVariables {
		if len(ast.MatchDescendants(ast.NavigateAST(a), func(e ast.NavigableExpr) bool {
			return e.Kind() == ast.IdentKind && e.AsIdent() == string(d.kind) && a.GetType(e.ID()).IsExactType(d.typ)
		})) > 0 {
			out = append(out, d.kind)
		}
	}
	slices.Sort(out)
	return out
}

// setsRead lists the sets that a reads: the fields of the variable sets, the
// only selections of it that the validator lets through.
func setsRead(a *ast.AST) []string {
	found := map[string]bool{}
	for _, e := range ast.MatchDescendants(ast.NavigateAST(a), ast.KindMatcher(ast.SelectKind)) {
		op := e.AsSelect().Operand()
		if op.Kind() == ast.IdentKind && op.AsIdent() == "sets" && a.GetType(op.ID()).IsExactType(setsType) {
			found[e.AsSelect().FieldName()] = true
		}
	}
	out := make([]string, 0, len(found))
	for name := range found {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// objectFields are the fields of geo and ua whose values are objects, by
// the name of the variable.
var objectFields = map[string][]string{
	"geo": {"continent", "country", "location"},
	"ua":  {"bot", "browser", "os"},
}

// attributes lists the attributes that e reads. refs tells the variables
// request, client, geo, asn and ua from iteration variables of the same name.
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
	// objectOf returns the variable, geo or ua, whose object e reads,
	// without testing it, or an empty string.
	objectOf := func(e ast.Expr) string {
		if e.Kind() != ast.SelectKind || e.AsSelect().IsTestOnly() {
			return ""
		}
		for name, fields := range objectFields {
			if slices.Contains(fields, e.AsSelect().FieldName()) && variable(e.AsSelect().Operand(), name) {
				return name
			}
		}
		return ""
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
			case variable(op, "request") || variable(op, "client") || variable(op, "geo") || variable(op, "asn") || variable(op, "ua"):
				found[op.AsIdent()+"."+sel.FieldName()] = true
			case objectOf(op) != "":
				found[objectOf(op)+"."+op.AsSelect().FieldName()+"."+sel.FieldName()] = true
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
