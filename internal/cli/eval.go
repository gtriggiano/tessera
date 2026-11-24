package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gtriggiano/tessera/internal/app"
	"github.com/gtriggiano/tessera/internal/config"
	"github.com/gtriggiano/tessera/internal/engine"
	"github.com/gtriggiano/tessera/internal/expr"
	"github.com/gtriggiano/tessera/internal/request"
	"github.com/gtriggiano/tessera/internal/sets"
)

// redacted replaces the values that eval never prints: those of the secret
// headers, which the log events never write either.
const redacted = "[REDACTED]"

type evalFlags struct {
	config, ip, host, method, path, query, scheme, output string
	headers, sets                                         []string
}

func evalCommand(lookup config.LookupEnv) *cobra.Command {
	var f evalFlags
	cmd := &cobra.Command{
		Use:   "eval",
		Short: "Show how the policy decides a request",
		Long: "Decide a request described by the flags with the policy of the configuration\n" +
			"file, and show the attributes that the rules read, the outcome of each rule up\n" +
			"to the one that decides, the decision, and the upstream headers of a request\n" +
			"that Envoy sends on. The request goes through the same normalization as a\n" +
			"request from Envoy; --ip sets client.ip, which is missing without it, and\n" +
			"ua reads the user-agent header of --header. The sets are loaded as serve\n" +
			"loads them, their files included, except those that --set replaces, and so\n" +
			"are the databases of geo and asn. Nothing is logged and no socket is opened.\n\n" +
			"Exit status: 0 whatever the decision; 2 for usage and configuration errors.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if f.output != "text" && f.output != "json" {
				return errors.New("invalid --output; want text or json")
			}
			r, err := f.request()
			if err != nil {
				return err
			}
			given, err := f.setValues()
			if err != nil {
				return err
			}
			_, cfg, err := loadConfig(cmd, lookup, f.config)
			if err != nil {
				return err
			}
			replace, err := replacements(cfg, given)
			if err != nil {
				return err
			}
			loaded, err := app.LoadSets(cfg, replace)
			if err != nil {
				return err
			}
			databases, err := app.LoadDatabases(cfg)
			if err != nil {
				return err
			}
			e, err := app.NewEngine(cfg, loaded.Store, databases.Store, nil)
			if err != nil {
				return runtimeError{errors.New("cannot compile the policy")}
			}
			secretHeaders := append(slices.Clone(expr.AlwaysSecretHeaders), cfg.SecretHeaderNames()...)
			out := explain(cfg.Policy, r, e.Explain(r), secretHeaders, e.HeaderReadsSecrets)
			var data []byte
			if f.output == "json" {
				if data, err = json.Marshal(out); err != nil {
					return runtimeError{errors.New("cannot render the evaluation")}
				}
				data = append(data, '\n')
			} else {
				data = out.text()
			}
			return write(cmd.OutOrStdout(), data)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&f.config, "config", "", configFlagUsage)
	flags.StringVar(&f.ip, "ip", "", "the client address, client.ip, with or without a port, an IPv6 address in brackets with a port; missing when absent")
	flags.StringVar(&f.host, "host", "", "the host of the request")
	flags.StringVar(&f.method, "method", "GET", "the method of the request")
	flags.StringVar(&f.path, "path", "/", "the path of the request, without ? and the query")
	flags.StringVar(&f.query, "query", "", "the query of the request, without ?")
	flags.StringVar(&f.scheme, "scheme", "https", "the scheme of the request")
	flags.StringArrayVar(&f.headers, "header", nil, "a header, as 'name: value'; repeat it for more")
	flags.StringArrayVar(&f.sets, "set", nil, "replace the content of a set, as 'name=value,value', read as its values; 'name=' for an empty set, the only form for an area set; its file is not read; once per set, repeat it for more sets")
	flags.StringVar(&f.output, "output", "text", "output format: text or json")
	return cmd
}

// request builds the request of the flags, normalized as a request from
// Envoy. The values of invalid flags are not repeated: they could be
// credentials.
func (f *evalFlags) request() (*request.Request, error) {
	if strings.Contains(f.path, "?") {
		return nil, errors.New("invalid --path; write the query with --query")
	}
	raw := request.Raw{Method: f.method, Host: f.host, Path: f.path, Scheme: f.scheme, Headers: map[string]string{}}
	if f.query != "" {
		raw.Path += "?" + f.query
	}
	for _, h := range f.headers {
		name, value, ok := strings.Cut(h, ":")
		name = strings.TrimSpace(name)
		if !ok || !token(name) {
			return nil, errors.New("invalid --header; want 'name: value'")
		}
		// As Envoy does with a repeated header.
		name = strings.ToLower(name)
		value = strings.Trim(value, " \t")
		if previous, ok := raw.Headers[name]; ok {
			value = previous + "," + value
		}
		raw.Headers[name] = value
	}
	r := request.Normalize(raw)
	if f.ip != "" {
		if r.ClientIP = request.ParseAddress(f.ip); !r.ClientIP.IsValid() {
			return nil, errors.New("invalid --ip; want an IP address")
		}
	}
	return r, nil
}

// setFlag is a --set flag: the name of a set and its values.
type setFlag struct {
	name   string
	values []string
}

// setValues splits the --set flags. Their text is not repeated in an error:
// it could be anything.
func (f *evalFlags) setValues() ([]setFlag, error) {
	out := make([]setFlag, 0, len(f.sets))
	for _, s := range f.sets {
		name, list, ok := strings.Cut(s, "=")
		if !ok {
			return nil, errors.New("invalid --set; want name=value,value")
		}
		flag := setFlag{name: name}
		if list != "" {
			flag.values = strings.Split(list, ",")
		}
		out = append(out, flag)
	}
	return out, nil
}

// replacements builds the copy of each set that --set replaces, from the
// values read as the values of its type; an area set takes no values, and
// becomes empty. An unknown set and an invalid value are usage errors that
// repeat no value; a declared name can be named.
func replacements(cfg *config.Config, given []setFlag) (map[string]*sets.Set, error) {
	out := map[string]*sets.Set{}
	for _, g := range given {
		s, ok := cfg.Sets[g.name]
		if !ok {
			return nil, errors.New("invalid --set: the configuration declares no set of this name")
		}
		if _, twice := out[g.name]; twice {
			return nil, fmt.Errorf("invalid --set: the set %s is given twice", g.name)
		}
		if s.Type == config.SetTypeArea && len(g.values) > 0 {
			return nil, fmt.Errorf("invalid --set for the set %s: an area set takes no values; --set %s= makes it empty", g.name, g.name)
		}
		set, err := sets.FromValues(sets.Type(s.Type), g.values, s.MaxEntries)
		if err != nil {
			return nil, fmt.Errorf("invalid --set for the set %s: %w", g.name, err)
		}
		out[g.name] = set
	}
	return out, nil
}

// token reports whether s is an HTTP field name.
func token(s string) bool {
	return s != "" && !strings.ContainsFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", r))
	})
}

// evalOutput is the result of eval. Its JSON form is a public interface.
type evalOutput struct {
	Attributes []evalAttribute `json:"attributes"`
	Rules      []evalRule      `json:"rules"`
	Decision   evalDecision    `json:"decision"`
	// Upstream, for a request that Envoy sends on and a policy with
	// upstream headers, holds them.
	Upstream *evalUpstream `json:"upstream,omitempty"`
}

// evalUpstream holds the upstream headers: those set, by name, and the names
// of those removed.
type evalUpstream struct {
	Set    map[string]string `json:"set"`
	Remove []string          `json:"remove"`
	// headers are all of them, in the order of their names, for the text.
	headers []evalHeader
}

// evalHeader is an upstream header in the text: its value, or removed; a
// value that can hold the value of a secret header is redacted.
type evalHeader struct {
	name, value       string
	removed, redacted bool
}

type evalAttribute struct {
	Name    string  `json:"name"`
	Value   *string `json:"value,omitempty"`
	Missing bool    `json:"missing,omitempty"`
	// bare: the text output writes the value without quotes, as for an
	// address, a bool or a number.
	bare bool
}

type evalRule struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Action      string `json:"action"`
	Mode        string `json:"mode"`
	Result      string `json:"result"`
	Cause       string `json:"cause,omitempty"`
}

type evalDecision struct {
	Action             string        `json:"action"`
	Enforced           bool          `json:"enforced"`
	Rule               string        `json:"rule"`
	Reason             string        `json:"reason"`
	Error              string        `json:"error,omitempty"`
	AuditRules         []string      `json:"audit_rules,omitempty"`
	IndeterminateRules []string      `json:"indeterminate_rules,omitempty"`
	Response           *evalResponse `json:"response,omitempty"`
}

type evalResponse struct {
	Status  int               `json:"status"`
	Body    string            `json:"body"`
	Headers map[string]string `json:"headers"`
}

// explain renders x, the explanation of r with the policy p. The values of
// secretHeaders, in lowercase, are redacted, and so are the upstream headers
// for which secrets reports that their values can hold one of them.
func explain(p config.Policy, r *request.Request, x engine.Explanation, secretHeaders []string, secrets func(header string) bool) evalOutput {
	out := evalOutput{Attributes: []evalAttribute{}, Rules: []evalRule{}}
	for _, name := range x.Attributes {
		out.Attributes = append(out.Attributes, attributes(name, r, x.Input, secretHeaders)...)
	}
	slices.SortFunc(out.Attributes, func(a, b evalAttribute) int { return strings.Compare(a.Name, b.Name) })
	out.Attributes = slices.CompactFunc(out.Attributes, func(a, b evalAttribute) bool { return a.Name == b.Name })
	rules := map[string]config.Rule{}
	for _, rule := range p.Rules {
		rules[rule.Name] = rule
	}
	for _, o := range x.Outcomes {
		rule := rules[o.Rule]
		out.Rules = append(out.Rules, evalRule{
			Name:        o.Rule,
			Description: strings.Join(strings.Fields(rule.Description), " "),
			Action:      rule.Action,
			Mode:        rule.Mode,
			Result:      string(o.Result),
			Cause:       string(o.Cause),
		})
	}
	d := x.Decision
	out.Decision = evalDecision{
		Action:             string(d.Action),
		Enforced:           d.Enforced,
		Rule:               d.Rule,
		Reason:             string(d.Reason),
		IndeterminateRules: d.IndeterminateRules,
	}
	if d.Reason == engine.ReasonError {
		out.Decision.Error = string(d.Cause)
	}
	for _, m := range d.AuditRules {
		out.Decision.AuditRules = append(out.Decision.AuditRules, m.Rule)
	}
	if d.Response != nil {
		resp := &evalResponse{Status: d.Response.Status, Body: d.Response.Body, Headers: map[string]string{}}
		for _, h := range d.Response.Headers {
			resp.Headers[h.Name] = h.Value
		}
		out.Decision.Response = resp
	}
	if len(d.Upstream) > 0 {
		up := &evalUpstream{Set: map[string]string{}, Remove: []string{}}
		for _, h := range d.Upstream {
			shown := evalHeader{name: h.Name, value: h.Value}
			switch {
			case h.Value == "":
				shown.removed = true
				up.Remove = append(up.Remove, h.Name)
			case secrets(h.Name):
				shown.value, shown.redacted = redacted, true
				up.Set[h.Name] = redacted
			default:
				up.Set[h.Name] = h.Value
			}
			up.headers = append(up.headers, shown)
		}
		out.Upstream = up
	}
	return out
}

// attributes returns the value of the attribute name, as an expression
// names it, for r: every header when name is request.headers, those of
// secretHeaders redacted. in reads geo, asn and ua as the decision did.
func attributes(name string, r *request.Request, in *expr.Input, secretHeaders []string) []evalAttribute {
	str := func(name, s string) evalAttribute { return evalAttribute{Name: name, Value: &s} }
	if a, ok := databaseAttribute(name, in); ok {
		return []evalAttribute{a}
	}
	if a, ok := userAgentAttribute(name, in); ok {
		return []evalAttribute{a}
	}
	header := func(key string) evalAttribute {
		name := fmt.Sprintf("request.headers[%q]", key)
		v, ok := r.Headers[key]
		switch {
		case !ok:
			return evalAttribute{Name: name, Missing: true}
		case slices.Contains(secretHeaders, key):
			return str(name, redacted)
		}
		return str(name, v)
	}
	switch name {
	case "client.ip":
		if !r.ClientIP.IsValid() {
			return []evalAttribute{{Name: name, Missing: true}}
		}
		a := str(name, r.ClientIP.String())
		a.bare = true
		return []evalAttribute{a}
	case "request.host":
		return []evalAttribute{str(name, r.Host)}
	case "request.method":
		return []evalAttribute{str(name, r.Method)}
	case "request.path":
		return []evalAttribute{str(name, r.Path)}
	case "request.query":
		return []evalAttribute{str(name, r.Query)}
	case "request.scheme":
		return []evalAttribute{str(name, r.Scheme)}
	case "request.id":
		return []evalAttribute{str(name, r.ID)}
	case "request.headers":
		var out []evalAttribute
		for key := range r.Headers {
			out = append(out, header(key))
		}
		return out
	}
	if key, ok := strings.CutPrefix(name, "request.headers["); ok {
		if key, err := strconv.Unquote(strings.TrimSuffix(key, "]")); err == nil {
			return []evalAttribute{header(key)}
		}
	}
	return nil
}

// databaseAttribute returns the value of name when it is an attribute of
// geo or asn: missing when the request has no client address, when the
// lookup failed, and for the location of a record without one. An object or
// a list read as a whole is written as a CEL literal, such as
// {code: "IT", name: "Italy", in_eu: true}, and a double always has a decimal
// point, as a CEL double literal: 52.0, not 52.
func databaseAttribute(name string, in *expr.Input) (evalAttribute, bool) {
	value := func(s string, bare bool) (evalAttribute, bool) {
		return evalAttribute{Name: name, Value: &s, bare: bare}, true
	}
	missing := evalAttribute{Name: name, Missing: true}
	number := func(f float64) string {
		s := strconv.FormatFloat(f, 'f', -1, 64)
		if !math.IsInf(f, 0) && !math.IsNaN(f) && !strings.Contains(s, ".") {
			s += ".0"
		}
		return s
	}
	if field, ok := strings.CutPrefix(name, "geo."); ok {
		g, err := in.Geo()
		if err != nil {
			return missing, true
		}
		if !g.HasLocation && strings.HasPrefix(field, "location") {
			return missing, true
		}
		switch field {
		case "found":
			return value(strconv.FormatBool(g.Found), true)
		case "country":
			return value(literal("code", strconv.Quote(g.Country.Code), "name", strconv.Quote(g.Country.Name), "in_eu", strconv.FormatBool(g.Country.InEU)), true)
		case "country.code":
			return value(g.Country.Code, false)
		case "country.name":
			return value(g.Country.Name, false)
		case "country.in_eu":
			return value(strconv.FormatBool(g.Country.InEU), true)
		case "continent":
			return value(literal("code", strconv.Quote(g.Continent.Code), "name", strconv.Quote(g.Continent.Name)), true)
		case "continent.code":
			return value(g.Continent.Code, false)
		case "continent.name":
			return value(g.Continent.Name, false)
		case "subdivisions":
			items := make([]string, len(g.Subdivisions))
			for i, s := range g.Subdivisions {
				items[i] = literal("code", strconv.Quote(s.Code), "name", strconv.Quote(s.Name))
			}
			return value("["+strings.Join(items, ", ")+"]", true)
		case "city":
			return value(g.City, false)
		case "postal_code":
			return value(g.PostalCode, false)
		case "timezone":
			return value(g.TimeZone, false)
		case "location":
			return value(literal("lat", number(g.Latitude), "lon", number(g.Longitude)), true)
		case "location.lat":
			return value(number(g.Latitude), true)
		case "location.lon":
			return value(number(g.Longitude), true)
		}
		return missing, true
	}
	if field, ok := strings.CutPrefix(name, "asn."); ok {
		a, err := in.ASN()
		if err != nil {
			return missing, true
		}
		switch field {
		case "found":
			return value(strconv.FormatBool(a.Found), true)
		case "number":
			return value(strconv.FormatInt(a.Number, 10), true)
		case "organization":
			return value(a.Organization, false)
		}
		return missing, true
	}
	return evalAttribute{}, false
}

// userAgentAttribute returns the value of name when it is an attribute of ua,
// as the decision analyzed the user-agent header: missing when the analysis
// failed. An object read as a whole is written as a CEL literal, as an
// object of geo is: {name: "Chrome", version: "131.0.6778", major: 131,
// minor: 0}.
func userAgentAttribute(name string, in *expr.Input) (evalAttribute, bool) {
	field, ok := strings.CutPrefix(name, "ua.")
	if !ok {
		return evalAttribute{}, false
	}
	value := func(s string, bare bool) (evalAttribute, bool) {
		return evalAttribute{Name: name, Value: &s, bare: bare}, true
	}
	integer := func(i int64) string { return strconv.FormatInt(i, 10) }
	u, err := in.UserAgent()
	if err != nil {
		return evalAttribute{Name: name, Missing: true}, true
	}
	b, o := u.Browser, u.OS
	switch field {
	case "browser":
		return value(literal("name", strconv.Quote(b.Name), "version", strconv.Quote(b.Version), "major", integer(b.Major), "minor", integer(b.Minor)), true)
	case "browser.name":
		return value(b.Name, false)
	case "browser.version":
		return value(b.Version, false)
	case "browser.major":
		return value(integer(b.Major), true)
	case "browser.minor":
		return value(integer(b.Minor), true)
	case "os":
		return value(literal("name", strconv.Quote(o.Name), "version", strconv.Quote(o.Version), "major", integer(o.Major), "minor", integer(o.Minor)), true)
	case "os.name":
		return value(o.Name, false)
	case "os.version":
		return value(o.Version, false)
	case "os.major":
		return value(integer(o.Major), true)
	case "os.minor":
		return value(integer(o.Minor), true)
	case "device":
		return value(u.Device, false)
	case "is_bot":
		return value(strconv.FormatBool(u.IsBot), true)
	case "bot":
		return value(literal("name", strconv.Quote(u.Bot.Name), "description", strconv.Quote(u.Bot.Description)), true)
	case "bot.name":
		return value(u.Bot.Name, false)
	case "bot.description":
		return value(u.Bot.Description, false)
	}
	return evalAttribute{Name: name, Missing: true}, true
}

// literal writes an object as a CEL literal, from the names and the values
// of its fields, in pairs: {name: value, name: value}.
func literal(fields ...string) string {
	var b strings.Builder
	b.WriteString("{")
	for i := 0; i+1 < len(fields); i += 2 {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(fields[i] + ": " + fields[i+1])
	}
	b.WriteString("}")
	return b.String()
}

// text renders the result as three blocks: the attributes, the rules and the
// decision, and a fourth for the upstream headers, when there are some.
func (o evalOutput) text() []byte {
	var b strings.Builder
	width := 0
	for _, a := range o.Attributes {
		width = max(width, len(a.Name))
	}
	for _, a := range o.Attributes {
		value := "missing"
		switch {
		case a.Missing:
		case a.bare || *a.Value == redacted:
			value = *a.Value
		default:
			value = strconv.Quote(*a.Value)
		}
		fmt.Fprintf(&b, "%-*s  %s\n", width, a.Name, value)
	}
	if len(o.Attributes) > 0 {
		b.WriteString("\n")
	}
	outcomes := make([]string, len(o.Rules))
	nameWidth, outcomeWidth := 0, 0
	for i, r := range o.Rules {
		outcome := r.Result
		if r.Cause != "" {
			outcome += ": " + r.Cause
		}
		if r.Mode == config.ModeAudit {
			outcome += " (audit)"
		} else if r.Name == o.Decision.Rule {
			outcome += " -> " + r.Action
		}
		outcomes[i] = outcome
		nameWidth, outcomeWidth = max(nameWidth, len(r.Name)), max(outcomeWidth, len(outcome))
	}
	for i, r := range o.Rules {
		line := fmt.Sprintf("%-*s  %-*s  %s", nameWidth, r.Name, outcomeWidth, outcomes[i], r.Description)
		b.WriteString(strings.TrimRight(line, " ") + "\n")
	}
	if len(o.Rules) > 0 {
		b.WriteString("\n")
	}
	d := o.Decision
	reason := d.Reason
	if d.Error != "" {
		reason += ": " + d.Error
	}
	fmt.Fprintf(&b, "decision: %s (rule %s, reason %s)", d.Action, d.Rule, reason)
	if !d.Enforced {
		b.WriteString(", not enforced: policy.mode is audit")
	}
	b.WriteString("\n")
	if d.Response != nil {
		fmt.Fprintf(&b, "response: status %d\n", d.Response.Status)
	}
	if o.Upstream != nil {
		b.WriteString("\n")
		width = 0
		for _, h := range o.Upstream.headers {
			width = max(width, len(h.name))
		}
		for _, h := range o.Upstream.headers {
			value := "removed"
			switch {
			case h.removed:
			case h.redacted:
				value = h.value
			default:
				value = strconv.Quote(h.value)
			}
			fmt.Fprintf(&b, "%-*s  %s\n", width, h.name, value)
		}
	}
	return []byte(b.String())
}
