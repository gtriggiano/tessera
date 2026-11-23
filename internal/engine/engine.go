// Package engine decides whether to allow a request. It knows nothing of
// Envoy: it takes a normalized request and returns a decision.
//
// The rules are evaluated in order, and the first whose condition is true
// decides; a request that no rule decides gets the default action. A
// condition that cannot be evaluated counts against allowing: a deny rule
// denies, with the reason error, and an allow rule does not match. A rule in
// audit mode never decides, and a policy in audit mode allows every request
// while it reports the decision it computed.
//
// A decision that lets the request through computes the upstream headers of
// the policy, after the rules and with what they read; a failure removes a
// header and never changes the decision. The decision fields are computed
// on demand, for a decision event, with what the rules read too.
package engine

import (
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gtriggiano/tessera/internal/enrichment"
	"github.com/gtriggiano/tessera/internal/expr"
	"github.com/gtriggiano/tessera/internal/request"
	"github.com/gtriggiano/tessera/internal/sets"
)

// Action is what a decision does with a request.
type Action string

// The actions of a decision and of a rule.
const (
	Allow Action = "allow"
	Deny  Action = "deny"
)

// Reason says what decided a request.
type Reason string

// The reasons of a decision.
const (
	// ReasonRule: a rule whose condition was true.
	ReasonRule Reason = "rule"
	// ReasonDefault: no rule decided, and the default action applied.
	ReasonDefault Reason = "default"
	// ReasonError: a deny rule whose condition could not be evaluated.
	ReasonError Reason = "error"
)

// DefaultRule is the rule of a decision that the default action made.
const DefaultRule = "default"

// Header is one header of a response.
type Header struct {
	Name, Value string
}

// Response is what Envoy answers the client when a request is denied.
type Response struct {
	Status int
	Body   string
	// Headers have their names in lowercase, sorted.
	Headers []Header
}

// Rule is a rule of a Policy.
type Rule struct {
	Name string
	// When is the condition, an expression; "true" matches every request.
	When   string
	Action Action
	// Audit: the rule never decides; a match is reported.
	Audit bool
	// Response is the response of a deny rule.
	Response Response
}

// NamedExpression is an expression of a Policy whose result is a string,
// with its name: an upstream header or a decision field.
type NamedExpression struct {
	Name, Expression string
}

// Policy is what an Engine decides with.
type Policy struct {
	// Audit: every request is allowed; the decision is computed and
	// reported.
	Audit         bool
	DefaultAction Action
	// DenyResponse is the response of a denial by the default action.
	DenyResponse Response
	Rules        []Rule
	// UpstreamHeaders are the headers that a request that Envoy sends on
	// gets, by name in any case, each with the expression of its value.
	UpstreamHeaders []NamedExpression
	// DecisionFields are the fields of a decision event, each with the
	// expression of its value.
	DecisionFields []NamedExpression
	// SecretHeaders are the secret headers of the configuration, in any
	// case, in addition to expr.AlwaysSecretHeaders: a decision field cannot
	// read them.
	SecretHeaders []string
}

// MaxUpstreamHeaderBytes is the longest value of an upstream header: a
// longer value removes the header.
const MaxUpstreamHeaderBytes = 4096

// UpstreamHeader is a header of the policy for a request that Envoy sends
// on: the value to set, or its removal.
type UpstreamHeader struct {
	// Name is in lowercase.
	Name string
	// Value is the value to set, which replaces the one of the client. It is
	// empty when the header is removed: its expression gave an empty string,
	// failed, or gave a value longer than MaxUpstreamHeaderBytes, with a
	// control character other than the tab, or not in UTF-8.
	Value string
	// Cause, for a header removed because its expression failed or gave an
	// invalid value, is why: CauseExpression for an invalid value. It is
	// empty for a value set, and for an empty string, a deliberate removal.
	Cause expr.Cause
}

// Field is a decision field and its value.
type Field struct {
	Name, Value string
}

// Options are the hooks and the data of an Engine.
type Options struct {
	// OnPanic, when set, is called with each panic recovered while a
	// condition is evaluated, with its stack when it is known.
	OnPanic func(*expr.Panic)
	// Sets holds the sets that the conditions read: its declarations are
	// the fields of the variable sets, and each decision reads one
	// snapshot of it. Nil declares no set.
	Sets *sets.Store
	// Databases holds the databases that geo and asn read: its kinds are
	// the variables that the conditions can read, and each decision reads
	// one snapshot of it. Nil configures no database.
	Databases *enrichment.Store
}

// AuditMatch is a rule in audit mode whose condition was true.
type AuditMatch struct {
	Rule   string
	Action Action
}

// RuleError is a condition that could not be evaluated.
type RuleError struct {
	Rule  string
	Cause expr.Cause
}

// Decision is the outcome of deciding a request.
type Decision struct {
	// Action is what the policy decided.
	Action Action
	// Enforced is false when the policy is in audit mode and Action is deny:
	// Envoy is told to allow the request.
	Enforced bool
	// Rule is the rule that decided, or DefaultRule.
	Rule   string
	Reason Reason
	// Cause, with ReasonError, is why the condition of Rule could not be
	// evaluated.
	Cause expr.Cause
	// AuditRules are the rules in audit mode whose condition was true, in
	// order.
	AuditRules []AuditMatch
	// IndeterminateRules are the allow rules that did not match because
	// their condition could not be evaluated, in order.
	IndeterminateRules []string
	// Errors are all the conditions that could not be evaluated, in order:
	// those of IndeterminateRules, of rules in audit mode, and of a deny
	// rule that decided with ReasonError.
	Errors []RuleError
	// Response, when Action is deny, is the response to send. It is shared:
	// do not modify it.
	Response *Response
	// Upstream, when Envoy sends the request on, that is when Action is
	// allow or Enforced is false, are the upstream headers of the policy,
	// in the order of their names; nil otherwise.
	Upstream []UpstreamHeader
	// Duration is the time of the decision, from the Received of the request:
	// with the Envoy adapter, the normalization of the request, the client
	// address, the rules and the upstream headers.
	Duration time.Duration

	// input is the activation of the decision, which the decision fields
	// read; nil for a decision of no Engine.
	input *expr.Input
}

// sent reports whether Envoy sends the request on: the policy allows it, or
// denies it in audit mode.
func (d *Decision) sent() bool {
	return d.Action == Allow || !d.Enforced
}

// evaluator is a compiled condition.
type evaluator interface {
	Eval(*expr.Input) (bool, error)
}

// stringEvaluator is a compiled expression whose result is a string.
type stringEvaluator interface {
	Eval(*expr.Input) (string, error)
}

// named is a compiled upstream header or decision field. secrets: its value
// can hold the value of a secret header.
type named struct {
	name    string
	value   stringEvaluator
	secrets bool
}

type rule struct {
	name       string
	when       evaluator
	attributes []string
	action     Action
	audit      bool
	response   *Response
}

// Engine decides requests. It is immutable and safe for concurrent use.
type Engine struct {
	rules         []rule
	audit         bool
	defaultAction Action
	denyResponse  *Response
	onPanic       func(*expr.Panic)
	sets          *sets.Store
	databases     *enrichment.Store
	// headers are the upstream headers, by name in lowercase, and fields
	// the decision fields, by name, both sorted.
	headers []named
	fields  []named
}

// New compiles p. An action other than allow or deny, or an expression that
// does not compile, is an error: a valid configuration has neither.
func New(p Policy, o Options) (*Engine, error) {
	env, err := expr.NewEnv(expr.Options{OnPanic: o.OnPanic, Sets: o.Sets.Declarations(), Databases: o.Databases.Kinds(), SecretHeaders: p.SecretHeaders})
	if err != nil {
		return nil, err
	}
	if p.DefaultAction != Allow && p.DefaultAction != Deny {
		return nil, errors.New("policy: the default action is neither allow nor deny")
	}
	e := &Engine{
		audit:         p.Audit,
		defaultAction: p.DefaultAction,
		denyResponse:  normalized(p.DenyResponse),
		onPanic:       o.OnPanic,
		sets:          o.Sets,
		databases:     o.Databases,
	}
	for i, r := range p.Rules {
		if r.Action != Allow && r.Action != Deny {
			return nil, fmt.Errorf("policy: rule %d: the action is neither allow nor deny", i)
		}
		when, err := env.Compile(r.When)
		if err != nil {
			return nil, fmt.Errorf("policy: rule %d: %w", i, err)
		}
		compiled := rule{name: r.Name, when: when, attributes: when.Attributes(), action: r.Action, audit: r.Audit}
		if r.Action == Deny {
			compiled.response = normalized(r.Response)
		}
		e.rules = append(e.rules, compiled)
	}
	for i, h := range p.UpstreamHeaders {
		value, err := env.CompileString(h.Expression)
		if err != nil {
			return nil, fmt.Errorf("policy: upstream header %d: %w", i, err)
		}
		e.headers = append(e.headers, named{name: strings.ToLower(h.Name), value: value, secrets: value.ReadsSecrets()})
	}
	for i, f := range p.DecisionFields {
		value, err := env.CompileDecisionField(f.Expression)
		if err != nil {
			return nil, fmt.Errorf("policy: decision field %d: %w", i, err)
		}
		e.fields = append(e.fields, named{name: f.Name, value: value})
	}
	byName := func(a, b named) int { return strings.Compare(a.name, b.name) }
	slices.SortFunc(e.headers, byName)
	slices.SortFunc(e.fields, byName)
	return e, nil
}

// normalized returns r with its header names in lowercase, sorted.
func normalized(r Response) *Response {
	out := &Response{Status: r.Status, Body: r.Body}
	for _, h := range r.Headers {
		out.Headers = append(out.Headers, Header{Name: strings.ToLower(h.Name), Value: h.Value})
	}
	slices.SortFunc(out.Headers, func(a, b Header) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// Decide returns the decision for r. Its duration counts from r.Received,
// or from the call when r.Received is zero.
func (e *Engine) Decide(r *request.Request) Decision {
	start := r.Received
	if start.IsZero() {
		start = time.Now()
	}
	d, in := e.decide(r, nil)
	e.finish(&d, in)
	d.Duration = time.Since(start)
	return d
}

// finish completes d, decided with the activation in: whether it is
// enforced, and the upstream headers when Envoy sends the request on.
func (e *Engine) finish(d *Decision, in *expr.Input) {
	d.Enforced = !(e.audit && d.Action == Deny)
	d.input = in
	if !d.sent() || len(e.headers) == 0 {
		return
	}
	d.Upstream = make([]UpstreamHeader, len(e.headers))
	for i := range e.headers {
		d.Upstream[i] = e.header(&e.headers[i], in)
	}
}

// header computes the upstream header h with in. A failure, or a value that
// cannot be sent, removes it with its cause.
func (e *Engine) header(h *named, in *expr.Input) UpstreamHeader {
	out := UpstreamHeader{Name: h.name}
	value, cause := e.evalString(h, in)
	switch {
	case cause != "":
		out.Cause = cause
	case !validHeaderValue(value):
		out.Cause = expr.CauseExpression
	default:
		out.Value = value
	}
	return out
}

// validHeaderValue reports whether v can be sent as the value of a header:
// at most MaxUpstreamHeaderBytes of UTF-8, without a control character other
// than the horizontal tab. The empty string is valid: it removes the header.
func validHeaderValue(v string) bool {
	return len(v) <= MaxUpstreamHeaderBytes && utf8.ValidString(v) &&
		!strings.ContainsFunc(v, func(r rune) bool { return r < 0x20 && r != '\t' || r == 0x7f })
}

// HeaderReadsSecrets reports whether the value of the upstream header name,
// in lowercase, can hold the value of a secret header of the request, as
// expr.StringExpr.ReadsSecrets decides when the policy is compiled; false
// for a header that the policy does not have.
func (e *Engine) HeaderReadsSecrets(name string) bool {
	for i := range e.headers {
		if e.headers[i].name == name {
			return e.headers[i].secrets
		}
	}
	return false
}

// DecisionFields computes the decision fields of the policy for d, a
// decision of this Engine, with what its rules read: the same snapshots of
// the sets and the databases, and the records of geo and asn already looked
// up. A field whose expression fails is empty. They are in the order of
// their names; nil without fields, or for a decision of no Engine. Call it on
// the goroutine of the decision, as the decision events do.
func (e *Engine) DecisionFields(d Decision) []Field {
	if len(e.fields) == 0 || d.input == nil {
		return nil
	}
	out := make([]Field, len(e.fields))
	for i := range e.fields {
		f := &e.fields[i]
		value, _ := e.evalString(f, d.input)
		out[i] = Field{Name: f.name, Value: value}
	}
	return out
}

// evalString evaluates the expression of n, an upstream header or a decision
// field, outside the condition of any rule: a panic is reported without a
// rule. The cause is empty when the expression could be evaluated.
func (e *Engine) evalString(n *named, in *expr.Input) (value string, cause expr.Cause) {
	in.SetRule("")
	defer func() {
		if r := recover(); r != nil {
			if e.onPanic != nil {
				e.onPanic(&expr.Panic{Value: r, Stack: debug.Stack()})
			}
			value, cause = "", expr.CausePanic
		}
	}()
	value, err := n.value.Eval(in)
	if err != nil {
		var ee *expr.EvalError
		if errors.As(err, &ee) {
			return "", ee.Cause
		}
		return "", expr.CauseExpression
	}
	return value, ""
}

// Result is the outcome of the condition of a rule.
type Result string

// The outcomes of a condition.
const (
	ResultTrue          Result = "true"
	ResultFalse         Result = "false"
	ResultIndeterminate Result = "indeterminate"
)

// Outcome is the outcome of the condition of one rule.
type Outcome struct {
	Rule   string
	Result Result
	// Cause, with ResultIndeterminate, is why.
	Cause expr.Cause
}

// Explanation is a decision with the outcome of each rule evaluated.
type Explanation struct {
	Decision Decision
	// Outcomes are those of the rules evaluated, in order, up to the one
	// that decided.
	Outcomes []Outcome
	// Attributes are those that the conditions of these rules read, sorted,
	// as expr.Condition.Attributes names them.
	Attributes []string
	// Input is the activation of the decision: its Geo and ASN read the
	// databases as the conditions did, from the snapshot of the decision.
	Input *expr.Input
}

// Explain decides r as Decide does, and reports the outcome of each rule.
func (e *Engine) Explain(r *request.Request) Explanation {
	var x Explanation
	start := r.Received
	if start.IsZero() {
		start = time.Now()
	}
	read := map[string]bool{}
	x.Decision, x.Input = e.decide(r, func(rl *rule, o Outcome) {
		x.Outcomes = append(x.Outcomes, o)
		for _, a := range rl.attributes {
			read[a] = true
		}
	})
	e.finish(&x.Decision, x.Input)
	x.Decision.Duration = time.Since(start)
	for a := range read {
		x.Attributes = append(x.Attributes, a)
	}
	slices.Sort(x.Attributes)
	return x
}

// decide decides r, with one snapshot of the sets and one of the databases
// for every rule, and returns the activation of the decision, which an
// expression evaluated after it reads in the same state. trace, when set,
// receives the outcome of each rule evaluated.
func (e *Engine) decide(r *request.Request, trace func(*rule, Outcome)) (Decision, *expr.Input) {
	var d Decision
	in := expr.NewInput(r, e.sets.Snapshot(), e.databases.Snapshot())
	for i := range e.rules {
		rl := &e.rules[i]
		in.SetRule(rl.name)
		matched, cause := e.eval(rl, in)
		if trace != nil {
			o := Outcome{Rule: rl.name, Result: ResultFalse, Cause: cause}
			switch {
			case cause != "":
				o.Result = ResultIndeterminate
			case matched:
				o.Result = ResultTrue
			}
			trace(rl, o)
		}
		switch {
		case cause != "":
			d.Errors = append(d.Errors, RuleError{Rule: rl.name, Cause: cause})
			switch {
			case rl.audit:
			case rl.action == Deny:
				d.Action, d.Rule, d.Reason, d.Cause, d.Response = Deny, rl.name, ReasonError, cause, rl.response
				return d, in
			default:
				d.IndeterminateRules = append(d.IndeterminateRules, rl.name)
			}
		case !matched:
		case rl.audit:
			d.AuditRules = append(d.AuditRules, AuditMatch{Rule: rl.name, Action: rl.action})
		default:
			d.Action, d.Rule, d.Reason, d.Response = rl.action, rl.name, ReasonRule, rl.response
			return d, in
		}
	}
	d.Action, d.Rule, d.Reason = e.defaultAction, DefaultRule, ReasonDefault
	if d.Action == Deny {
		d.Response = e.denyResponse
	}
	return d, in
}

// eval evaluates the condition of rl. The cause is empty when the condition
// could be evaluated. A panic outside the evaluation of the expression is
// recovered and reported here.
func (e *Engine) eval(rl *rule, in *expr.Input) (matched bool, cause expr.Cause) {
	defer func() {
		if r := recover(); r != nil {
			if e.onPanic != nil {
				e.onPanic(&expr.Panic{Rule: rl.name, Value: r, Stack: debug.Stack()})
			}
			matched, cause = false, expr.CausePanic
		}
	}()
	ok, err := rl.when.Eval(in)
	if err != nil {
		var ee *expr.EvalError
		if errors.As(err, &ee) {
			return false, ee.Cause
		}
		return false, expr.CauseExpression
	}
	return ok, ""
}
