package expr

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"regexp/syntax"
	"runtime/debug"
	"unicode"
	"unicode/utf8"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/functions"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/interpreter"

	"github.com/gtriggiano/tessera/internal/sets"
)

// The overload IDs of tessera's functions. The decorator finds the calls of
// inCidr by its ID.
const (
	overloadIPInCidr          = "tessera_ip_in_cidr_string"
	overloadIPFamily          = "tessera_ip_family"
	overloadIPToString        = "tessera_ip_to_string"
	overloadCIDRSetContains   = "tessera_cidr_set_contains_ip"
	overloadASNSetContains    = "tessera_asn_set_contains_int"
	overloadStringSetContains = "tessera_string_set_contains_string"
)

// setContainsOverloads are the overloads of contains on a set. Each costs
// one unit, whatever the size of the set: a lookup in a copy does not grow
// with it the way a scan does.
var setContainsOverloads = []string{overloadCIDRSetContains, overloadASNSetContains, overloadStringSetContains}

// Panic is a panic that tessera recovered while it evaluated a condition.
type Panic struct {
	// Rule is the rule whose condition was evaluated, as Input.SetRule
	// named it.
	Rule string
	// Value is the value passed to panic.
	Value any
	// Stack is the stack of the goroutine where the panic was recovered, or
	// nil when cel-go recovered it first.
	Stack []byte
}

func (p *Panic) Error() string { return fmt.Sprintf("panic: %v", p.Value) }

// library holds tessera's functions, the validators of their arguments, the
// decorator that parses the network of inCidr in advance, and the guards
// that turn a panic into an error. It holds no mutable state.
type library struct {
	onPanic      func(*Panic)
	parseNetwork func(string) (netip.Prefix, error)
	// sets are the declared sets: the fields of the variable sets.
	sets []sets.Declaration
}

// The errors of an invalid network. They never repeat the configured value.
var (
	errNetworkSyntax   = errors.New("the network must be an address and a prefix length, such as 192.0.2.0/24 or 2001:db8::/32")
	errNetworkHostBits = errors.New("the network has bits set after the prefix length: write its first address, such as 192.0.2.0/24")
	errNetworkMapped   = errors.New("write an IPv4 network in IPv4 notation, not as an IPv4-mapped IPv6 network")
	errNetworkLiteral  = errors.New("the argument of inCidr must be a string literal")
)

// parseNetwork parses the network argument of inCidr.
func parseNetwork(s string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return netip.Prefix{}, errNetworkSyntax
	}
	if p.Addr().Is4In6() {
		return netip.Prefix{}, errNetworkMapped
	}
	if p != p.Masked() {
		return netip.Prefix{}, errNetworkHostBits
	}
	return p, nil
}

func (l *library) envOptions() []cel.EnvOption {
	return []cel.EnvOption{
		cel.Function("inCidr",
			cel.MemberOverload(overloadIPInCidr, []*cel.Type{ipType, cel.StringType}, cel.BoolType,
				cel.BinaryBinding(l.binary(l.ipInCidrUnprepared)))),
		cel.Function("family",
			cel.MemberOverload(overloadIPFamily, []*cel.Type{ipType}, cel.IntType,
				cel.UnaryBinding(l.unary(ipFamily)))),
		cel.Function("string",
			cel.Overload(overloadIPToString, []*cel.Type{ipType}, cel.StringType,
				cel.UnaryBinding(l.unary(ipToString)))),
		// Merged with contains on strings, from the standard library.
		cel.Function("contains",
			cel.MemberOverload(overloadCIDRSetContains, []*cel.Type{cidrSetType, ipType}, cel.BoolType,
				cel.BinaryBinding(l.binary(cidrSetContains))),
			cel.MemberOverload(overloadASNSetContains, []*cel.Type{asnSetType, cel.IntType}, cel.BoolType,
				cel.BinaryBinding(l.binary(asnSetContains))),
			cel.MemberOverload(overloadStringSetContains, []*cel.Type{stringSetType, cel.StringType}, cel.BoolType,
				cel.BinaryBinding(l.binary(stringSetContains)))),
		cel.ASTValidators(
			setsValidator{},
			networkValidator{lib: l},
			regexValidator{},
			cel.ValidateHomogeneousAggregateLiterals(),
			cel.ValidateComprehensionNestingLimit(1),
		),
	}
}

var (
	four = types.Int(4)
	six  = types.Int(6)
)

func ipFamily(v ref.Val) ref.Val {
	if v.(*ip).addr.Is4() {
		return four
	}
	return six
}

func ipToString(v ref.Val) ref.Val {
	return types.String(v.(*ip).addr.String())
}

// The bindings of contains on a set: the copy answers when the set is
// available, on_unavailable otherwise, whatever the argument.
func cidrSetContains(set, addr ref.Val) ref.Val {
	s, answer := set.(*setVal).inUse()
	if s == nil {
		return answer
	}
	return types.Bool(s.ContainsAddr(addr.(*ip).addr))
}

func asnSetContains(set, number ref.Val) ref.Val {
	s, answer := set.(*setVal).inUse()
	if s == nil {
		return answer
	}
	return types.Bool(s.ContainsASN(int64(number.(types.Int))))
}

func stringSetContains(set, value ref.Val) ref.Val {
	s, answer := set.(*setVal).inUse()
	if s == nil {
		return answer
	}
	return types.Bool(s.ContainsString(string(value.(types.String))))
}

// oneUnit is the cost of contains on a set.
func oneUnit([]ref.Val, ref.Val) *uint64 {
	one := uint64(1)
	return &one
}

// costOptions give contains on a set its constant cost.
func costOptions() []interpreter.CostTrackerOption {
	var out []interpreter.CostTrackerOption
	for _, id := range setContainsOverloads {
		out = append(out, interpreter.OverloadCostTracker(id, oneUnit))
	}
	return out
}

// errSetUse is the message of a set used otherwise than as the target of
// contains.
var errSetUse = errors.New("a set can be used only as the target of contains, as in sets.<name>.contains(value)")

// setsValidator checks that the variable sets is used only as
// sets.<name>.contains(value): a set is not a value that an expression can
// compare, test with has() or keep in a list. It reports each other use at
// the position of sets.
type setsValidator struct{}

func (setsValidator) Name() string { return "tessera.validator.sets" }

func (setsValidator) Validate(_ *cel.Env, _ cel.ValidatorConfig, a *ast.AST, iss *cel.Issues) {
	isSets := func(e ast.Expr) bool {
		return e.Kind() == ast.IdentKind && e.AsIdent() == "sets" && a.GetType(e.ID()).IsExactType(setsType)
	}
	for _, e := range ast.MatchDescendants(ast.NavigateAST(a), func(e ast.NavigableExpr) bool { return isSets(e) }) {
		sel, ok := e.Parent()
		if ok && sel.Kind() == ast.SelectKind && !sel.AsSelect().IsTestOnly() {
			call, ok := sel.Parent()
			if ok && call.Kind() == ast.CallKind && call.AsCall().FunctionName() == "contains" &&
				call.AsCall().IsMemberFunction() && call.AsCall().Target().ID() == sel.ID() {
				continue
			}
		}
		iss.ReportErrorAtID(e.ID(), "%s", errSetUse)
	}
}

// ipInCidrUnprepared is the binding of inCidr when its network was not
// parsed in advance. The validator and the decorator make it unreachable; it
// parses the network so that it stays correct if that ever changes.
func (l *library) ipInCidrUnprepared(addr, network ref.Val) ref.Val {
	p, err := l.parseNetwork(string(network.(types.String)))
	if err != nil {
		return types.WrapErr(err)
	}
	return types.Bool(p.Contains(addr.(*ip).addr))
}

// networkValidator checks the argument of every inCidr call when the
// expression is compiled: it must be a string literal that holds a valid
// network. It reports each error at the position of the argument.
type networkValidator struct{ lib *library }

func (networkValidator) Name() string { return "tessera.validator.inCidr" }

func (v networkValidator) Validate(_ *cel.Env, _ cel.ValidatorConfig, a *ast.AST, iss *cel.Issues) {
	for _, call := range ast.MatchDescendants(ast.NavigateAST(a), ast.FunctionMatcher("inCidr")) {
		args := call.AsCall().Args()
		if len(args) != 1 {
			continue // the type checker has rejected it
		}
		arg := args[0]
		if arg.Kind() != ast.LiteralKind {
			iss.ReportErrorAtID(leftmost(arg, a.SourceInfo()), "%s", errNetworkLiteral)
			continue
		}
		s, ok := arg.AsLiteral().(types.String)
		if !ok {
			continue // the type checker has rejected it
		}
		if _, err := v.lib.parseNetwork(string(s)); err != nil {
			iss.ReportErrorAtID(arg.ID(), "invalid network: %s", err)
		}
	}
}

// regexValidator checks the regular expressions written as literals, the
// patterns of matches, when the expression is compiled. Its message names
// the problem, never the pattern.
type regexValidator struct{}

func (regexValidator) Name() string { return "tessera.validator.matches" }

func (regexValidator) Validate(_ *cel.Env, _ cel.ValidatorConfig, a *ast.AST, iss *cel.Issues) {
	for _, call := range ast.MatchDescendants(ast.NavigateAST(a), ast.FunctionMatcher("matches")) {
		args := call.AsCall().Args()
		pattern := args[len(args)-1]
		if pattern.Kind() != ast.LiteralKind {
			continue
		}
		s, ok := pattern.AsLiteral().(types.String)
		if !ok {
			continue // the type checker has rejected it
		}
		if _, err := regexp.Compile(string(s)); err != nil {
			problem := "the pattern is not valid"
			var serr *syntax.Error
			if errors.As(err, &serr) {
				problem = string(serr.Code)
			}
			iss.ReportErrorAtID(pattern.ID(), "invalid regular expression: %s", problem)
		}
	}
}

// leftmost returns the ID of the sub-expression of e that starts first in the
// text, where an issue about e is reported.
func leftmost(e ast.Expr, info *ast.SourceInfo) int64 {
	best, bestStart := e.ID(), int32(-1)
	ast.PreOrderVisit(e, ast.NewExprVisitor(func(d ast.Expr) {
		r, ok := info.GetOffsetRange(d.ID())
		if ok && (bestStart < 0 || r.Start < bestStart) {
			best, bestStart = d.ID(), r.Start
		}
	}))
	return best
}

// start returns the offset, in characters, where e starts in the text. cel-go
// places a call at its operator or parenthesis and a selection at its dot: e
// starts at its leftmost sub-expression, or at the name of a function, such
// as size, called without a target.
func start(e ast.Expr, info *ast.SourceInfo) int32 {
	first := int32(-1)
	ast.PreOrderVisit(e, ast.NewExprVisitor(func(d ast.Expr) {
		r, ok := info.GetOffsetRange(d.ID())
		if !ok {
			return
		}
		s := r.Start
		if d.Kind() == ast.CallKind && !d.AsCall().IsMemberFunction() {
			if name := d.AsCall().FunctionName(); name != "" && unicode.IsLetter(rune(name[0])) {
				s -= int32(utf8.RuneCountInString(name))
			}
		}
		if first < 0 || s < first {
			first = s
		}
	}))
	return max(first, 0)
}

// prepareInCidr is an interpreter decorator: it replaces each inCidr call
// with one that holds its network already parsed, so that an evaluation never
// parses a network.
func (l *library) prepareInCidr(i interpreter.InterpretableV2) (interpreter.InterpretableV2, error) {
	call, ok := i.(interpreter.InterpretableCall)
	if !ok || call.OverloadID() != overloadIPInCidr {
		return i, nil
	}
	args := call.Args()
	c, ok := args[1].(interpreter.InterpretableConst)
	if !ok {
		return nil, errNetworkLiteral
	}
	s, ok := c.Value().(types.String)
	if !ok {
		return nil, errNetworkLiteral
	}
	p, err := l.parseNetwork(string(s))
	if err != nil {
		return nil, err
	}
	return &inCidrCall{InterpretableCall: call, ip: args[0], network: p, lib: l}, nil
}

// inCidrCall evaluates ip.inCidr(network) with network parsed in advance. It
// keeps the original call for Function, OverloadID and Args, which the cost
// tracker reads.
type inCidrCall struct {
	interpreter.InterpretableCall
	ip      interpreter.InterpretableV2
	network netip.Prefix
	lib     *library
}

func (c *inCidrCall) Exec(frame *interpreter.ExecutionFrame) ref.Val {
	v := c.ip.Exec(frame)
	addr, ok := v.(*ip)
	if !ok {
		if types.IsUnknownOrError(v) {
			return v // inCidr is strict: an error receiver is the result
		}
		return types.MaybeNoSuchOverloadErr(v)
	}
	return c.contains(addr)
}

func (c *inCidrCall) contains(addr *ip) (out ref.Val) {
	defer c.lib.recoverPanic(&out, addr.in)
	return types.Bool(c.network.Contains(addr.addr))
}

func (c *inCidrCall) Eval(act interpreter.Activation) ref.Val {
	return c.Exec(interpreter.AsFrame(act))
}

// unary and binary guard a function binding: a panic becomes an error value
// that wraps a *Panic with its stack, reported to onPanic, instead of an
// "internal error" of cel-go for the whole evaluation.
// Every function of tessera takes a tessera.IP or a set, which leads to the
// activation, and so to the rule being evaluated.
func (l *library) unary(fn functions.UnaryOp) functions.UnaryOp {
	return func(v ref.Val) (out ref.Val) {
		defer l.recoverPanic(&out, inputOf(v))
		return fn(v)
	}
}

func (l *library) binary(fn functions.BinaryOp) functions.BinaryOp {
	return func(lhs, rhs ref.Val) (out ref.Val) {
		defer l.recoverPanic(&out, inputOf(lhs, rhs))
		return fn(lhs, rhs)
	}
}

// inputOf returns the activation of the first tessera.IP or set among
// values, or nil.
func inputOf(values ...ref.Val) *Input {
	for _, v := range values {
		switch v := v.(type) {
		case *ip:
			return v.in
		case *setVal:
			return v.in
		}
	}
	return nil
}

// guardGet guards the reading of a field in the same way: the field belongs
// to the value of request, client or sets, which leads to the activation.
func (l *library) guardGet(get func(any) (any, error)) func(any) (any, error) {
	return func(obj any) (out any, err error) {
		defer func() {
			if r := recover(); r != nil {
				var in *Input
				switch v := obj.(type) {
				case *requestVal:
					in = v.in
				case *clientVal:
					in = v.in
				case *setsVal:
					in = v.in
				}
				out, err = nil, l.recovered(r, in)
			}
		}()
		return get(obj)
	}
}

func (l *library) recoverPanic(out *ref.Val, in *Input) {
	if r := recover(); r != nil {
		*out = types.WrapErr(l.recovered(r, in))
	}
}

// recovered reports a recovered panic, with the rule that in evaluates, and
// returns it as an error. The panic that cel-go uses to stop an evaluation
// at the cost limit goes on.
func (l *library) recovered(r any, in *Input) *Panic {
	if c, ok := r.(interpreter.EvalCancelledError); ok {
		panic(c)
	}
	p := &Panic{Value: r, Stack: debug.Stack()}
	if in != nil {
		p.Rule = in.rule
	}
	if l.onPanic != nil {
		l.onPanic(p)
	}
	return p
}
