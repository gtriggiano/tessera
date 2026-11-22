// Package engine decides whether to allow a request. It knows nothing of
// Envoy: it takes a normalized request and returns a decision.
//
// The rules are evaluated in order, and the first whose condition is true
// decides; a request that no rule decides gets the default action. A
// condition that cannot be evaluated counts against allowing: a deny rule
// denies, with the reason error, and an allow rule does not match. A rule in
// audit mode never decides, and a policy in audit mode allows every request
// while it reports the decision it computed.
package engine

import (
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"strings"
	"time"

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

// Policy is what an Engine decides with.
type Policy struct {
	// Audit: every request is allowed; the decision is computed and
	// reported.
	Audit         bool
	DefaultAction Action
	// DenyResponse is the response of a denial by the default action.
	DenyResponse Response
	Rules        []Rule
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
	// Duration is the time of the decision, from the Received of the request:
	// with the Envoy adapter, the normalization of the request, the client
	// address and the rules.
	Duration time.Duration
}

// evaluator is a compiled condition.
type evaluator interface {
	Eval(*expr.Input) (bool, error)
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
}

// New compiles p. An action other than allow or deny, or a condition that
// does not compile, is an error: a valid configuration has neither.
func New(p Policy, o Options) (*Engine, error) {
	env, err := expr.NewEnv(expr.Options{OnPanic: o.OnPanic, Sets: o.Sets.Declarations()})
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
	d := e.decide(r, nil)
	d.Enforced = !(e.audit && d.Action == Deny)
	d.Duration = time.Since(start)
	return d
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
}

// Explain decides r as Decide does, and reports the outcome of each rule.
func (e *Engine) Explain(r *request.Request) Explanation {
	var x Explanation
	start := r.Received
	if start.IsZero() {
		start = time.Now()
	}
	read := map[string]bool{}
	x.Decision = e.decide(r, func(rl *rule, o Outcome) {
		x.Outcomes = append(x.Outcomes, o)
		for _, a := range rl.attributes {
			read[a] = true
		}
	})
	x.Decision.Enforced = !(e.audit && x.Decision.Action == Deny)
	x.Decision.Duration = time.Since(start)
	for a := range read {
		x.Attributes = append(x.Attributes, a)
	}
	slices.Sort(x.Attributes)
	return x
}

// decide decides r, with one snapshot of the sets for every rule. trace,
// when set, receives the outcome of each rule evaluated.
func (e *Engine) decide(r *request.Request, trace func(*rule, Outcome)) Decision {
	var d Decision
	in := expr.NewInput(r, e.sets.Snapshot())
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
				return d
			default:
				d.IndeterminateRules = append(d.IndeterminateRules, rl.name)
			}
		case !matched:
		case rl.audit:
			d.AuditRules = append(d.AuditRules, AuditMatch{Rule: rl.name, Action: rl.action})
		default:
			d.Action, d.Rule, d.Reason, d.Response = rl.action, rl.name, ReasonRule, rl.response
			return d
		}
	}
	d.Action, d.Rule, d.Reason = e.defaultAction, DefaultRule, ReasonDefault
	if d.Action == Deny {
		d.Response = e.denyResponse
	}
	return d
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
