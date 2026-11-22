// Package engine decides whether to allow a request. It knows nothing of
// Envoy: it takes a normalized request and returns a decision.
package engine

import "github.com/gtriggiano/tessera/internal/request"

// Action is what a decision does with a request.
type Action string

// The actions of a decision.
const (
	Allow Action = "allow"
	Deny  Action = "deny"
)

// Decision is the outcome of deciding a request.
type Decision struct {
	Action Action
}

// Engine decides requests. It is immutable and safe for concurrent use.
type Engine struct {
	defaultAction Action
}

// New returns an engine that answers every request with defaultAction, the
// value of policy.default_action. Any value other than allow denies.
func New(defaultAction string) *Engine {
	if Action(defaultAction) == Allow {
		return &Engine{defaultAction: Allow}
	}
	return &Engine{defaultAction: Deny}
}

// Decide returns the decision for r.
func (e *Engine) Decide(*request.Request) Decision {
	return Decision{Action: e.defaultAction}
}
