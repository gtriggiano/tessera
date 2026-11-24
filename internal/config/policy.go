package config

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"

	"github.com/gtriggiano/tessera/internal/enrichment"
	"github.com/gtriggiano/tessera/internal/expr"
)

// ruleNameRE is the form of a rule name: it is a label value of metrics.
var ruleNameRE = regexp.MustCompile(`^[a-z0-9-]+$`)

// DefaultRule names the decision of policy.default_action, where a rule name
// would be: no rule can take it.
const DefaultRule = "default"

// forbiddenResponseHeaders are the headers that Envoy sets itself on a
// response, or that describe its connection rather than its content.
var forbiddenResponseHeaders = []string{"connection", "content-length", "keep-alive", "proxy-connection", "te", "trailer", "transfer-encoding", "upgrade"}

func (c *Config) validatePolicy() error {
	p := &c.Policy
	switch p.Mode {
	case ModeEnforce, ModeAudit:
	default:
		return c.err("policy.mode", "expected enforce or audit")
	}
	if !c.present("policy.default_action") {
		return c.err("policy.default_action", "required")
	}
	switch p.DefaultAction {
	case ActionAllow, ActionDeny:
	default:
		return c.err("policy.default_action", "expected allow or deny")
	}
	if err := c.validateResponse("policy.deny_response", p.DenyResponse, true, true, true); err != nil {
		return err
	}
	if len(p.Rules) > MaxRules {
		return c.err("policy.rules", "more than "+strconv.Itoa(MaxRules)+" rules")
	}
	env, err := expr.NewEnv(expr.Options{Sets: c.SetDeclarations(), Databases: c.DatabaseKinds(), UserAgent: c.Enrichment.UserAgent != nil, SecretHeaders: c.SecretHeaderNames()})
	if err != nil {
		return errors.New("configuration: cannot prepare the expressions")
	}
	names := map[string]int{}
	u := &uses{sets: map[string]bool{}, databases: map[enrichment.Kind]bool{}}
	for i := range p.Rules {
		if err := c.validateRule(env, i, names, u); err != nil {
			return err
		}
	}
	if err := c.validateUpstream(env, u); err != nil {
		return err
	}
	if err := c.validateDecisionFields(env, u); err != nil {
		return err
	}
	// A set or an enrichment that nothing reads is still loaded and checked.
	for _, k := range c.keys["sets"] {
		if !u.sets[k.name] {
			c.warnings = append(c.warnings, Warning{Path: childPath("sets", k.name), Line: k.at.line, Column: k.at.column, Problem: "the set is not used by any rule, upstream header or decision field"})
		}
	}
	for _, k := range c.DatabaseKinds() {
		if !u.databases[k] {
			p := "enrichment." + string(k)
			at := c.nodes[p]
			c.warnings = append(c.warnings, Warning{Path: p, Line: at.line, Column: at.column, Problem: "the database is not used by any rule, upstream header or decision field"})
		}
	}
	if c.Enrichment.UserAgent != nil && !u.userAgent {
		const p = "enrichment.user_agent"
		at := c.nodes[p]
		c.warnings = append(c.warnings, Warning{Path: p, Line: at.line, Column: at.column, Problem: "the enrichment is not used by any rule, upstream header or decision field"})
	}
	slices.SortStableFunc(c.warnings, func(a, b Warning) int {
		if a.Line != b.Line {
			return a.Line - b.Line
		}
		return a.Column - b.Column
	})
	return nil
}

// uses are the sets, the databases and the analysis of the user-agent header
// that the expressions read.
type uses struct {
	sets      map[string]bool
	databases map[enrichment.Kind]bool
	userAgent bool
}

// add records what e, a compiled expression, reads.
func (u *uses) add(e interface {
	Sets() []string
	Databases() []enrichment.Kind
	ReadsUserAgent() bool
}) {
	for _, name := range e.Sets() {
		u.sets[name] = true
	}
	for _, k := range e.Databases() {
		u.databases[k] = true
	}
	u.userAgent = u.userAgent || e.ReadsUserAgent()
}

// forbiddenUpstreamHeaders are the headers that change the meaning of the
// request, or that hold its credentials, which upstream.headers cannot set;
// nor can it set a header whose name starts with x-envoy-, which belong to
// Envoy.
var forbiddenUpstreamHeaders = []string{"host", "content-length", "transfer-encoding", "connection", "authorization", "proxy-authorization", "cookie", "keep-alive", "proxy-connection", "te", "trailer", "upgrade"}

// validateUpstream checks upstream.headers: their number, their names, in the
// order of the file, and their expressions, whose reads it adds to used.
func (c *Config) validateUpstream(env *expr.Env, used *uses) error {
	const path = "upstream.headers"
	if len(c.Upstream.Headers) > MaxUpstreamHeaders {
		return c.err(path, "more than "+strconv.Itoa(MaxUpstreamHeaders)+" headers")
	}
	seen := map[string]bool{}
	for _, k := range c.keys[path] {
		name := strings.ToLower(k.name)
		switch {
		case !headerName(k.name):
			return c.errAt(path, k.at, "expected a header name")
		case slices.Contains(forbiddenUpstreamHeaders, name) || strings.HasPrefix(name, "x-envoy-"):
			return c.errAt(path, k.at, "this header is not allowed: it changes the meaning of the request, holds its credentials, or belongs to Envoy")
		case seen[name]:
			return c.errAt(path, k.at, "the same header twice, in different cases")
		}
		seen[name] = true
		value, err := env.CompileString(string(c.Upstream.Headers[k.name]))
		if err != nil {
			return c.compileError(path, childPath(path, k.name), string(c.Upstream.Headers[k.name]), err)
		}
		used.add(value)
	}
	return nil
}

// validateDecisionFields checks the expressions of log.decision_fields, in
// the order of the file, and adds their reads to used.
func (c *Config) validateDecisionFields(env *expr.Env, used *uses) error {
	const path = "log.decision_fields"
	for _, k := range c.keys[path] {
		text := string(c.Log.DecisionFields[k.name])
		value, err := env.CompileDecisionField(text)
		if err != nil {
			return c.compileError(path, childPath(path, k.name), text, err)
		}
		used.add(value)
	}
	return nil
}

// compileError returns the error of err, the error of compiling text, the
// expression at path, as an error at report.
func (c *Config) compileError(report, path, text string, err error) error {
	var ce *expr.CompileError
	if !errors.As(err, &ce) || len(ce.Issues) == 0 {
		return c.err(report, "invalid expression")
	}
	return c.expressionErrorAt(report, path, text, ce.Issues[0])
}

// validateRule checks the rule i, and adds the sets and the databases that
// its condition reads to used.
func (c *Config) validateRule(env *expr.Env, i int, names map[string]int, used *uses) error {
	r := &c.Policy.Rules[i]
	path := itemPath("policy.rules", i)
	switch {
	case !c.present(path + ".name"):
		return c.err(path+".name", "required")
	case len(r.Name) > MaxRuleNameLength || !ruleNameRE.MatchString(r.Name):
		return c.err(path+".name", "expected lowercase letters, digits and hyphens, at most "+strconv.Itoa(MaxRuleNameLength)+" characters")
	case r.Name == DefaultRule:
		return c.err(path+".name", "default is reserved for the decision of policy.default_action")
	}
	if first, ok := names[r.Name]; ok {
		return c.err(path+".name", "the name of "+itemPath("policy.rules", first)+" is repeated")
	}
	names[r.Name] = i
	if len(r.Description) > MaxDescriptionBytes {
		return c.err(path+".description", "longer than "+strconv.Itoa(MaxDescriptionBytes)+" bytes")
	}
	if !c.present(path + ".action") {
		return c.err(path+".action", "required")
	}
	switch r.Action {
	case ActionAllow, ActionDeny:
	default:
		return c.err(path+".action", "expected allow or deny")
	}
	switch r.Mode {
	case ModeEnforce, ModeAudit:
	default:
		return c.err(path+".mode", "expected enforce or audit")
	}
	cond, err := env.Compile(string(r.When))
	if err != nil {
		return c.compileError(path+".when", path+".when", string(r.When), err)
	}
	used.add(cond)
	r.written.response = c.present(path + ".response")
	r.written.status = c.present(path + ".response.status")
	r.written.body = c.present(path + ".response.body")
	r.written.headers = c.present(path + ".response.headers")
	if r.written.response && r.Action != ActionDeny {
		return c.err(path+".response", "allowed only when action is deny")
	}
	if err := c.validateResponse(path+".response", r.Response, r.written.status, r.written.body, r.written.headers); err != nil {
		return err
	}
	if r.Action == ActionDeny {
		// The keys that the rule does not write come from
		// policy.deny_response.
		d := c.Policy.DenyResponse
		if !r.written.status {
			r.Response.Status = d.Status
		}
		if !r.written.body {
			r.Response.Body = d.Body
		}
		if !r.written.headers {
			r.Response.Headers = d.Headers
		}
	}
	if cond.Always() && r.Mode == ModeEnforce && i < len(c.Policy.Rules)-1 {
		at := path + ".when"
		if !c.present(at) {
			at = path
		}
		return c.err(at, "the rule matches every request, so the rules after it are never evaluated: give it a condition, or set its mode to audit")
	}
	return nil
}

// validateResponse checks the keys of a response that the file writes.
func (c *Config) validateResponse(path string, r Response, status, body, headers bool) error {
	if status && (r.Status < 400 || r.Status > 599) {
		return c.err(path+".status", "expected an integer from 400 to 599")
	}
	if body && len(r.Body) > MaxBodyBytes {
		return c.err(path+".body", "longer than "+strconv.Itoa(MaxBodyBytes)+" bytes")
	}
	if !headers {
		return nil
	}
	path += ".headers"
	seen := map[string]bool{}
	for _, k := range c.keys[path] {
		name := strings.ToLower(k.name)
		switch {
		case !headerName(k.name):
			return c.errAt(path, k.at, "expected a header name")
		case slices.Contains(forbiddenResponseHeaders, name):
			return c.errAt(path, k.at, "this header is not allowed: Envoy sets it, or it describes the connection")
		case seen[name]:
			return c.errAt(path, k.at, "the same header twice, in different cases")
		case !headerValue(r.Headers[k.name]):
			return c.errAt(path, k.at, "the value of this header has a control character")
		}
		seen[name] = true
	}
	return nil
}

// headerValue reports whether s can be the value of an HTTP field: it has no
// control character but the horizontal tab.
func headerValue(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 && r != '\t' || r == 0x7f })
}

// expressionErrorAt returns the error of an issue of the expression at path,
// as an error at report: path itself, or, for an expression of a mapping,
// such as upstream.headers, the mapping, whose keys are not named. Its
// position is where the issue is in the file when the file holds the text of
// the expression as it is, in one line or in a literal block, and the
// position of the value otherwise, with the position in the expression in
// the message. A message about an expression with a substitution names
// nothing from it.
func (c *Config) expressionErrorAt(report, path, text string, is expr.Issue) error {
	at := c.nodes[path]
	if c.tainted[path] {
		return c.errAt(report, at, "invalid expression")
	}
	lines := strings.Split(text, "\n")
	if is.Line >= 1 && is.Line <= len(lines) {
		want := []rune(lines[is.Line-1])
		style := c.styles[path]
		line, column := 0, -1
		switch {
		case style&yaml.LiteralStyle != 0:
			// The block starts on the line after its indicator.
			line = at.line + is.Line
			if got := c.line(line); len(got) >= len(want) && string(got[len(got)-len(want):]) == string(want) && strings.TrimLeft(string(got[:len(got)-len(want)]), " ") == "" {
				column = len(got) - len(want)
			}
		case len(lines) == 1 && style&(yaml.FoldedStyle|yaml.FlowStyle) == 0:
			line = at.line
			first := at.column - 1
			if style&(yaml.SingleQuotedStyle|yaml.DoubleQuotedStyle) != 0 {
				first++
			}
			if got := c.line(line); first >= 0 && first+len(want) <= len(got) && string(got[first:first+len(want)]) == string(want) {
				column = first
			}
		}
		if column >= 0 {
			return c.errAt(report, location{line, column + is.Column}, is.Message)
		}
	}
	return c.errAt(report, at, fmt.Sprintf("%s (line %d, column %d of the expression)", is.Message, is.Line, is.Column))
}

// line returns the line n of the file, from 1, as characters.
func (c *Config) line(n int) []rune {
	if n < 1 || n > len(c.lines) {
		return nil
	}
	return []rune(strings.TrimSuffix(c.lines[n-1], "\r"))
}
