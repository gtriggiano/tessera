package expr

import (
	"fmt"
	"net/netip"
	"reflect"
	"slices"

	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"

	"github.com/gtriggiano/tessera/internal/sets"
)

// The CEL types that tessera declares. Request, Client and Sets are object
// types whose fields the provider describes; IP and the types of the sets
// are opaque: expressions use them only through the functions of library.go.
var (
	requestType   = types.NewObjectType("tessera.Request")
	clientType    = types.NewObjectType("tessera.Client")
	setsType      = types.NewObjectType("tessera.Sets")
	ipType        = types.NewOpaqueType("tessera.IP")
	cidrSetType   = types.NewOpaqueType("tessera.CIDRSet")
	asnSetType    = types.NewOpaqueType("tessera.ASNSet")
	stringSetType = types.NewOpaqueType("tessera.StringSet")
)

// setTypes are the CEL types of the sets, by the type of a set.
var setTypes = map[sets.Type]*types.Type{
	sets.CIDR:   cidrSetType,
	sets.ASN:    asnSetType,
	sets.String: stringSetType,
}

// ip is the CEL value of an IP address, always unmapped: an IPv4-mapped IPv6
// address is held as the IPv4 address it maps. in is the activation it
// belongs to, which a guarded function that panics reports the rule of.
type ip struct {
	addr netip.Addr
	in   *Input
}

func (v *ip) ConvertToNative(typeDesc reflect.Type) (any, error) {
	switch {
	case typeDesc == reflect.TypeFor[netip.Addr]():
		return v.addr, nil
	case typeDesc.Kind() == reflect.String:
		return v.addr.String(), nil
	}
	return nil, fmt.Errorf("unsupported conversion of %s to %v", ipType, typeDesc)
}

func (v *ip) ConvertToType(typeValue ref.Type) ref.Val {
	switch typeValue {
	case types.StringType:
		return types.String(v.addr.String())
	case ipType:
		return v
	case types.TypeType:
		return ipType
	}
	return types.NewErr("type conversion error from '%s' to '%s'", ipType, typeValue)
}

func (v *ip) Equal(other ref.Val) ref.Val {
	o, ok := other.(*ip)
	return types.Bool(ok && v.addr == o.addr)
}

func (v *ip) Type() ref.Type { return ipType }
func (v *ip) Value() any     { return v.addr }

// field is one field of an object type: its CEL type, whether it is set on a
// value, and how to read it. The functions receive the Go value that the
// activation returned for the variable, a *requestVal, a *clientVal or a
// *setsVal.
type field struct {
	typ   *types.Type
	isSet func(obj any) bool
	get   func(obj any) (any, error)
}

// provider declares tessera's object types to the type checker and to the
// interpreter, and leaves every other type to the standard registry.
//
// Because every field has isSet and get, cel-go plans a selection such as
// request.host as a call to get, and has(client.ip) as a call to isSet,
// instead of a lookup by name at each evaluation.
type provider struct {
	*types.Registry
	objects map[string]map[string]field
}

func newProvider(lib *library) (*provider, error) {
	reg, err := types.NewRegistry()
	if err != nil {
		return nil, err
	}
	if err := reg.RegisterType(ipType, cidrSetType, asnSetType, stringSetType); err != nil {
		return nil, err
	}
	setFields := make(map[string]field, len(lib.sets))
	for i, d := range lib.sets {
		f, err := setField(i, len(lib.sets), d)
		if err != nil {
			return nil, err
		}
		setFields[d.Name] = f
	}
	guard := func(fields map[string]field) map[string]field {
		out := make(map[string]field, len(fields))
		for name, f := range fields {
			f.get = lib.guardGet(f.get)
			out[name] = f
		}
		return out
	}
	return &provider{
		Registry: reg,
		objects: map[string]map[string]field{
			requestType.TypeName(): guard(requestFields),
			clientType.TypeName():  guard(clientFields),
			setsType.TypeName():    guard(setFields),
		},
	}, nil
}

func (p *provider) FindStructType(name string) (*types.Type, bool) {
	if _, ok := p.objects[name]; ok {
		return types.NewTypeTypeWithParam(types.NewObjectType(name)), true
	}
	return p.Registry.FindStructType(name)
}

func (p *provider) FindStructFieldNames(name string) ([]string, bool) {
	if fields, ok := p.objects[name]; ok {
		names := make([]string, 0, len(fields))
		for n := range fields {
			names = append(names, n)
		}
		slices.Sort(names)
		return names, true
	}
	return p.Registry.FindStructFieldNames(name)
}

func (p *provider) FindStructFieldType(name, fieldName string) (*types.FieldType, bool) {
	fields, ok := p.objects[name]
	if !ok {
		return p.Registry.FindStructFieldType(name, fieldName)
	}
	f, ok := fields[fieldName]
	if !ok {
		return nil, false
	}
	return &types.FieldType{Type: f.typ, IsSet: f.isSet, GetFrom: f.get}, true
}

// NewValue refuses to build tessera's objects: expressions only read them.
func (p *provider) NewValue(name string, fields map[string]ref.Val) ref.Val {
	if _, ok := p.objects[name]; ok {
		return types.NewErr("cannot create a value of type %s", name)
	}
	return p.Registry.NewValue(name, fields)
}

// object is embedded in the values of the object types, which expressions
// read only through their fields.
type object struct{ typ *types.Type }

func (o object) ConvertToNative(typeDesc reflect.Type) (any, error) {
	return nil, fmt.Errorf("unsupported conversion of %s to %v", o.typ, typeDesc)
}

func (o object) ConvertToType(typeValue ref.Type) ref.Val {
	if typeValue == types.TypeType {
		return o.typ
	}
	return types.NewErr("type conversion error from '%s' to '%s'", o.typ, typeValue)
}

func (o object) Type() ref.Type { return o.typ }
