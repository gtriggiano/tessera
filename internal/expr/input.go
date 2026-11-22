package expr

import (
	"errors"
	"fmt"
	"reflect"

	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/interpreter"

	"github.com/gtriggiano/tessera/internal/request"
	"github.com/gtriggiano/tessera/internal/sets"
)

// Input is the activation of one request: it answers the variables request,
// client and sets. It builds the CEL value of an attribute when a condition
// first reads it, and keeps it for the other conditions evaluated with it, so
// one Input serves every rule of one decision. It is not safe for concurrent
// use.
type Input struct {
	request requestVal
	client  clientVal
	sets    setsVal
	// rule is the rule being evaluated, which a recovered panic reports.
	rule string
}

// NewInput returns the activation of r, whose ClientIP is the client
// address, or the zero Addr when it is missing, and of snap, the state of
// the sets for the whole decision: a Snapshot of the Store whose
// declarations the Env was built with, or nil, in which every set is never
// loaded. r must not change while the Input is in use.
func NewInput(r *request.Request, snap *sets.Snapshot) *Input {
	in := &Input{}
	in.request = requestVal{object: object{requestType}, req: r, in: in}
	in.client = clientVal{object: object{clientType}, in: in}
	if r.ClientIP.IsValid() {
		in.client.ip = ip{addr: r.ClientIP.Unmap(), in: in}
		in.client.hasIP = true
	}
	in.sets = setsVal{object: object{setsType}, in: in, snap: snap}
	return in
}

// SetRule names the rule whose condition is evaluated next with in: a panic
// recovered meanwhile is reported with it.
func (in *Input) SetRule(name string) { in.rule = name }

// ResolveName implements interpreter.Activation.
func (in *Input) ResolveName(name string) (any, bool) {
	switch name {
	case "request":
		return &in.request, true
	case "client":
		return &in.client, true
	case "sets":
		return &in.sets, true
	}
	return nil, false
}

// Parent implements interpreter.Activation.
func (in *Input) Parent() interpreter.Activation { return nil }

// ErrClientIPMissing is the error of a read of client.ip when the request has
// no client address.
var ErrClientIPMissing = errors.New("client.ip is not available")

// requestVal is the value of the variable request.
type requestVal struct {
	object
	in                                    *Input
	req                                   *request.Request
	host, method, path, query, scheme, id ref.Val
	headers                               ref.Val
}

func (r *requestVal) Equal(other ref.Val) ref.Val { return types.Bool(r == other) }
func (r *requestVal) Value() any                  { return r }

func always(any) bool { return true }

// requestString declares a string field of request: pick returns where its
// CEL value is kept and the Go string it is built from.
func requestString(pick func(r *requestVal) (*ref.Val, string)) field {
	return field{
		typ:   types.StringType,
		isSet: always,
		get: func(obj any) (any, error) {
			kept, s := pick(obj.(*requestVal))
			if *kept == nil {
				*kept = types.String(s)
			}
			return *kept, nil
		},
	}
}

var requestFields = map[string]field{
	"host":   requestString(func(r *requestVal) (*ref.Val, string) { return &r.host, r.req.Host }),
	"method": requestString(func(r *requestVal) (*ref.Val, string) { return &r.method, r.req.Method }),
	"path":   requestString(func(r *requestVal) (*ref.Val, string) { return &r.path, r.req.Path }),
	"query":  requestString(func(r *requestVal) (*ref.Val, string) { return &r.query, r.req.Query }),
	"scheme": requestString(func(r *requestVal) (*ref.Val, string) { return &r.scheme, r.req.Scheme }),
	"id":     requestString(func(r *requestVal) (*ref.Val, string) { return &r.id, r.req.ID }),
	"headers": {
		typ:   types.NewMapType(types.StringType, types.StringType),
		isSet: always,
		get: func(obj any) (any, error) {
			r := obj.(*requestVal)
			if r.headers == nil {
				r.headers = types.NewStringStringMap(types.DefaultTypeAdapter, r.req.Headers)
			}
			return r.headers, nil
		},
	},
}

// clientVal is the value of the variable client. client.ip is set when the
// request has a client address.
type clientVal struct {
	object
	in    *Input
	ip    ip
	hasIP bool
}

func (c *clientVal) Equal(other ref.Val) ref.Val { return types.Bool(c == other) }
func (c *clientVal) Value() any                  { return c }

var clientFields = map[string]field{
	"ip": {
		typ:   ipType,
		isSet: func(obj any) bool { return obj.(*clientVal).hasIP },
		get: func(obj any) (any, error) {
			c := obj.(*clientVal)
			if !c.hasIP {
				return nil, ErrClientIPMissing
			}
			return &c.ip, nil
		},
	},
}

// setsVal is the value of the variable sets: one field per declared set,
// read from the snapshot of the decision. It builds the value of a set when a
// condition first reads it, and keeps it for the rest of the decision.
type setsVal struct {
	object
	in   *Input
	snap *sets.Snapshot
	vals []setVal
}

func (s *setsVal) Equal(other ref.Val) ref.Val { return types.Bool(s == other) }
func (s *setsVal) Value() any                  { return s }

// setField declares sets.<name> for d, the declaration of index i among n.
func setField(i, n int, d sets.Declaration) (field, error) {
	typ, ok := setTypes[d.Type]
	if !ok {
		return field{}, fmt.Errorf("expr: set %d: unknown set type", i)
	}
	switch d.OnUnavailable {
	case sets.Fail, sets.Match, sets.NoMatch:
	default:
		return field{}, fmt.Errorf("expr: set %d: unknown answer for an unavailable set", i)
	}
	return field{
		typ:   typ,
		isSet: always,
		get: func(obj any) (any, error) {
			s := obj.(*setsVal)
			if s.vals == nil {
				s.vals = make([]setVal, n)
			}
			v := &s.vals[i]
			if v.typ == nil {
				*v = setVal{typ: typ, entry: s.snap.Entry(i), on: d.OnUnavailable, in: s.in}
			}
			return v, nil
		},
	}, nil
}

// setVal is the value of one set in one decision: its copy in the snapshot,
// its availability, and what contains answers when it is unavailable.
type setVal struct {
	typ   *types.Type
	entry sets.Entry
	on    sets.OnUnavailable
	in    *Input
}

func (v *setVal) ConvertToNative(typeDesc reflect.Type) (any, error) {
	return nil, fmt.Errorf("unsupported conversion of %s to %v", v.typ, typeDesc)
}

func (v *setVal) ConvertToType(typeValue ref.Type) ref.Val {
	if typeValue == types.TypeType {
		return v.typ
	}
	return types.NewErr("type conversion error from '%s' to '%s'", v.typ, typeValue)
}

func (v *setVal) Equal(other ref.Val) ref.Val { return types.Bool(v == other) }
func (v *setVal) Type() ref.Type              { return v.typ }
func (v *setVal) Value() any                  { return v }

// ErrSetUnavailable is the error of contains on an unavailable set whose
// on_unavailable is fail.
var ErrSetUnavailable = errors.New("the set is not available")

// inUse returns the copy of the set to look a value up in, or, when the set
// is unavailable, nil and what on_unavailable makes contains answer.
func (v *setVal) inUse() (*sets.Set, ref.Val) {
	if v.entry.Available && v.entry.Set != nil {
		return v.entry.Set, nil
	}
	switch v.on {
	case sets.Match:
		return nil, types.True
	case sets.NoMatch:
		return nil, types.False
	}
	return nil, types.WrapErr(ErrSetUnavailable)
}
