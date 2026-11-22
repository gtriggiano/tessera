package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gtriggiano/tessera/internal/app"
	"github.com/gtriggiano/tessera/internal/config"
	"github.com/gtriggiano/tessera/internal/engine"
	"github.com/gtriggiano/tessera/internal/request"
	"github.com/gtriggiano/tessera/internal/sets"
)

// redactedHeaders are the headers whose values eval never prints, as the
// log events never write them.
var redactedHeaders = []string{"authorization", "cookie"}

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
			"to the one that decides, and the decision. The request goes through the same\n" +
			"normalization as a request from Envoy; --ip sets client.ip, which is missing\n" +
			"without it. The sets are loaded as serve loads them, their files included,\n" +
			"except those that --set replaces. Nothing is logged and no socket is opened.\n\n" +
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
			e, err := app.NewEngine(cfg.Policy, loaded.Store, nil)
			if err != nil {
				return runtimeError{errors.New("cannot compile the policy")}
			}
			out := explain(cfg.Policy, r, e.Explain(r))
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
	flags.StringArrayVar(&f.sets, "set", nil, "replace the content of a set, as 'name=value,value', read as its values; 'name=' for an empty set; its file is not read; once per set, repeat it for more sets")
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
// values read as the values of its type. An unknown set and an invalid value
// are usage errors that repeat no value; a declared name can be named.
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
}

type evalAttribute struct {
	Name    string  `json:"name"`
	Value   *string `json:"value,omitempty"`
	Missing bool    `json:"missing,omitempty"`
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

func explain(p config.Policy, r *request.Request, x engine.Explanation) evalOutput {
	out := evalOutput{Attributes: []evalAttribute{}, Rules: []evalRule{}}
	for _, name := range x.Attributes {
		out.Attributes = append(out.Attributes, attributes(name, r)...)
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
	return out
}

// attributes returns the value of the attribute name, as an expression
// names it, for r: every header when name is request.headers.
func attributes(name string, r *request.Request) []evalAttribute {
	str := func(name, s string) evalAttribute { return evalAttribute{Name: name, Value: &s} }
	header := func(key string) evalAttribute {
		name := fmt.Sprintf("request.headers[%q]", key)
		v, ok := r.Headers[key]
		switch {
		case !ok:
			return evalAttribute{Name: name, Missing: true}
		case slices.Contains(redactedHeaders, key):
			return str(name, redacted)
		}
		return str(name, v)
	}
	switch name {
	case "client.ip":
		if !r.ClientIP.IsValid() {
			return []evalAttribute{{Name: name, Missing: true}}
		}
		return []evalAttribute{str(name, r.ClientIP.String())}
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

// text renders the result as three blocks: the attributes, the rules and the
// decision.
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
		case a.Name == "client.ip" || *a.Value == redacted:
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
	return []byte(b.String())
}
