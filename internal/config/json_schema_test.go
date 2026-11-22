package config

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

// docs/public/config.schema.json is the JSON Schema of the configuration file,
// published with the documentation for editors and other tools. This file
// generates it from the configuration types, the defaults and the notes
// below, and TestJSONSchema fails when the published file differs: a new key
// needs a note here, then
//
//	go test ./internal/config -run TestJSONSchema -update
//
// rewrites the file.
//
// The schema states what a document can state on its own: key names, types,
// enumerations, bounds and the form of some strings. It leaves out every rule
// that involves two keys or the environment, so it never rejects a file that
// tessera accepts: TestJSONSchemaAcceptsValidConfigurations keeps it so.
var updateJSONSchema = flag.Bool("update", false, "rewrite docs/public/config.schema.json")

var jsonSchemaFile = filepath.Join("..", "..", "docs", "public", "config.schema.json")

const (
	jsonSchemaDialect = "https://json-schema.org/draft/2020-12/schema"
	jsonSchemaID      = "https://gtriggiano.github.io/tessera/config.schema.json"

	// A string value can take all or part of its text from the environment.
	jsonSchemaEnvPattern      = `\$\{[A-Za-z_][A-Za-z0-9_]*(:-[^}]*)?\}`
	jsonSchemaDurationPattern = `^[+-]?(([0-9]+(\.[0-9]*)?|\.[0-9]+)(ns|us|µs|μs|ms|s|m|h))+$`
	jsonSchemaPortPattern     = `^.*:[1-9][0-9]{0,4}$`
	jsonSchemaListenPattern   = `^(unix:.+|.*:[1-9][0-9]{0,4})$`
	jsonSchemaHeaderPattern   = "^[!#$%&'*+.^_`|~0-9A-Za-z-]+$"
	jsonSchemaRuleNamePattern = "^[a-z0-9-]{1,63}$"
)

// jsonSchemaNote is what the types and defaults do not say about a key.
type jsonSchemaNote struct {
	doc      string
	required bool // in its mapping
	// requiredWhen is the value of another key that makes this key
	// required. The schema states neither this rule, which involves two
	// keys, nor a default.
	requiredWhen condition
	enum         []string
	min, max     int  // of an integer; 0 leaves the bound out
	nonNegative  bool // of an integer: a minimum of 0, which min cannot state
	maxItems     int  // of a list; 0 leaves the bound out
	pattern      string
	// inherited is the key whose value this key takes when the file does not
	// write it. The schema states no default for it.
	inherited string
}

// condition is a key that has a value.
type condition struct{ path, value string }

var jsonSchemaNotes = map[string]jsonSchemaNote{
	"server":                        {doc: "The gRPC listener for Envoy."},
	"server.listen":                 {doc: "The address of the gRPC listener: host:port, or unix: followed by the path of a socket, relative to the configuration file when it is not absolute.", pattern: jsonSchemaListenPattern},
	"server.max_connections":        {doc: "The largest number of open connections from Envoy to the gRPC listener; a connection beyond it is closed at once.", min: 1, max: MaxConnectionsLimit},
	"server.max_concurrent_streams": {doc: "The largest number of concurrent calls on one connection from Envoy.", min: 1, max: 4294967295},
	"server.max_connection_age":     {doc: "The age at which a connection from Envoy is closed gracefully, so that the load spreads across replicas. 0s sets no limit."},
	"server.drain_delay":            {doc: "At shutdown, the time between reporting not ready and closing the connections."},
	"server.shutdown_timeout":       {doc: "At shutdown, the longest time to finish the calls in progress. Positive."},

	"admin":        {doc: "The listener of the health checks and the metrics."},
	"admin.listen": {doc: "The address of the admin listener, as host:port. It must not share its address and port with server.listen.", pattern: jsonSchemaPortPattern},

	"log":           {doc: "The log events, written to standard output."},
	"log.level":     {doc: "The lowest level of the events that are written.", enum: []string{LevelDebug, LevelInfo, LevelWarn, LevelError}},
	"log.format":    {doc: "The format of the events: one line of key=value pairs, or one JSON object per line.", enum: []string{FormatLogfmt, FormatJSON}},
	"log.decisions": {doc: "The decisions that a decision event reports: none; deny, the denials, and the allows in which an audit rule with action deny matched; or all.", enum: []string{DecisionsNone, DecisionsDeny, DecisionsAll}},

	"client_ip":              {doc: "Where tessera reads the client address of a request: from one source, without falling back on another."},
	"client_ip.from":         {doc: "The source of the client address: peer, the address of the connection to Envoy; header, a header that holds one address; x_forwarded_for, the X-Forwarded-For header, read from the right.", enum: []string{ClientIPPeer, ClientIPHeader, ClientIPXForwardedFor}},
	"client_ip.header":       {doc: "The header that holds the client address, in any case. Required, and allowed only, when from is header.", requiredWhen: condition{"client_ip.from", ClientIPHeader}, pattern: jsonSchemaHeaderPattern},
	"client_ip.trusted_hops": {doc: "The number of entries at the right end of X-Forwarded-For that trusted proxies wrote after the client address. Required, and allowed only, when from is x_forwarded_for.", requiredWhen: condition{"client_ip.from", ClientIPXForwardedFor}, nonNegative: true},

	"policy":                          {doc: "How tessera decides.", required: true},
	"policy.mode":                     {doc: "enforce applies the decisions; audit allows every request, and reports the denials it did not enforce with enforced=false, in the decision event and in tessera_decisions_total.", enum: []string{ModeEnforce, ModeAudit}},
	"policy.default_action":           {doc: "The decision for a request that no rule decides. It has no default, so that every configuration states whether such requests are allowed or denied.", required: true, enum: []string{ActionAllow, ActionDeny}},
	"policy.deny_response":            {doc: "What Envoy answers the client when a request is denied, unless the rule that denies it states otherwise."},
	"policy.deny_response.status":     {doc: "The HTTP status of a denial.", min: 400, max: 599},
	"policy.deny_response.body":       {doc: "The body of a denial, at most 8 KiB. Envoy adds content-type: text/plain to a body when the headers have no content-type."},
	"policy.deny_response.headers":    {doc: "The headers of a denial, by name. A header that Envoy sets itself or that describes the connection, such as content-length, is not allowed."},
	"policy.rules":                    {doc: "The rules, evaluated in order: the first whose condition is true decides. At most 1000.", maxItems: MaxRules},
	"policy.rules[].name":             {doc: "The name of the rule: lowercase letters, digits and hyphens, at most 63 characters, unique. default is reserved.", required: true, pattern: jsonSchemaRuleNamePattern},
	"policy.rules[].description":      {doc: "A description of the rule, at most 1024 bytes."},
	"policy.rules[].when":             {doc: "The condition of the rule, a CEL expression whose result is a bool. Without it, the rule matches every request."},
	"policy.rules[].action":           {doc: "What the rule does with the requests it matches.", required: true, enum: []string{ActionAllow, ActionDeny}},
	"policy.rules[].mode":             {doc: "enforce lets the rule decide; audit never lets it decide: a true condition shows in audit_rules of the decision event and in tessera_audit_matches_total, and tessera goes on to the next rule.", enum: []string{ModeEnforce, ModeAudit}},
	"policy.rules[].response":         {doc: "The response of a deny rule: each key it does not write comes from policy.deny_response. Not allowed on an allow rule."},
	"policy.rules[].response.status":  {doc: "The HTTP status of the denial. By default, policy.deny_response.status.", min: 400, max: 599, inherited: "policy.deny_response.status"},
	"policy.rules[].response.body":    {doc: "The body of the denial, at most 8 KiB. By default, policy.deny_response.body.", inherited: "policy.deny_response.body"},
	"policy.rules[].response.headers": {doc: "The headers of the denial, which replace policy.deny_response.headers as a whole. By default, policy.deny_response.headers.", inherited: "policy.deny_response.headers"},
}

// jsonObject is a JSON object that keeps the order of its members.
type jsonObject []jsonMember
type jsonMember struct {
	key   string
	value any
}

func (o jsonObject) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, m := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		key, err := json.Marshal(m.key)
		if err != nil {
			return nil, err
		}
		value, err := jsonSchemaMarshal(m.value)
		if err != nil {
			return nil, err
		}
		b.Write(key)
		b.WriteByte(':')
		b.Write(value)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// jsonSchemaMarshal keeps <, > and & as they are, for a file people read.
func jsonSchemaMarshal(v any) ([]byte, error) {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

type jsonSchemaBuilder struct {
	t    *testing.T
	used map[string]bool
}

func buildJSONSchema(t *testing.T) []byte {
	t.Helper()
	b := &jsonSchemaBuilder{t: t, used: map[string]bool{}}
	root := jsonObject{
		{"$schema", jsonSchemaDialect},
		{"$id", jsonSchemaID},
		{"title", "tessera configuration"},
		{"description", "The configuration file of tessera, an external authorization service for Envoy. " +
			"This schema checks key names, types, allowed values, integer bounds and the form of some strings. " +
			"It does not check the rules that involve two keys (such as two listeners on the same port), the syntax of hosts and socket paths, or environment variables: " +
			"a string value can take text from the environment with ${NAME} or ${NAME:-default}, and such a value is checked only after substitution. " +
			"`tessera config validate` is the authority on whether a file is valid."},
	}
	root = append(root, b.mapping(reflect.TypeFor[Config](), "", reflect.ValueOf(defaults()).Elem())...)
	root = append(root, jsonMember{"$defs", jsonObject{
		{"duration", jsonObject{
			{"description", "A duration with units, such as 500ms, 30s, 5m or 1h30m. Write 0s for zero: a number without a unit is an error."},
			{"type", "string"},
			{"anyOf", []any{jsonObject{{"pattern", jsonSchemaDurationPattern}}, jsonObject{{"$ref", "#/$defs/environment"}}}},
		}},
		{"environment", jsonObject{
			{"description", "A string that takes all or part of its value from the environment, with ${NAME} or ${NAME:-default}. The value is checked after substitution."},
			{"type", "string"},
			{"pattern", jsonSchemaEnvPattern},
		}},
	}})
	for path := range jsonSchemaNotes {
		if !b.used[path] {
			t.Errorf("jsonSchemaNotes has %s, which is not a configuration key", path)
		}
	}
	raw, err := jsonSchemaMarshal(root)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := json.Indent(&out, raw, "", "  "); err != nil {
		t.Fatal(err)
	}
	out.WriteByte('\n')
	return out.Bytes()
}

func (b *jsonSchemaBuilder) note(path string) jsonSchemaNote {
	note, ok := jsonSchemaNotes[path]
	if !ok || note.doc == "" {
		b.t.Errorf("configuration key %s has no description in jsonSchemaNotes", path)
	}
	b.used[path] = true
	return note
}

func yamlName(f reflect.StructField) string { return strings.Split(f.Tag.Get("yaml"), ",")[0] }

// mapping returns the keywords of a struct, a closed set of keys, whose
// defaults are in v.
func (b *jsonSchemaBuilder) mapping(t reflect.Type, prefix string, v reflect.Value) jsonObject {
	properties := jsonObject{}
	required := []string{}
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		path := childPath(prefix, yamlName(f))
		note := b.note(path)
		if note.required {
			required = append(required, yamlName(f))
		}
		properties = append(properties, jsonMember{yamlName(f), b.field(f.Type, path, note, v.Field(i))})
	}
	out := jsonObject{{"type", "object"}, {"additionalProperties", false}}
	if len(required) > 0 {
		out = append(out, jsonMember{"required", required})
	}
	return append(out, jsonMember{"properties", properties})
}

// listItem returns a new item of a list of t, with its defaults.
func listItem(t reflect.Type) reflect.Value {
	item := reflect.New(t)
	if d, ok := item.Interface().(defaulter); ok {
		d.setDefaults()
	}
	return item.Elem()
}

// field returns the schema of one key, whose default is v.
func (b *jsonSchemaBuilder) field(t reflect.Type, path string, note jsonSchemaNote, v reflect.Value) jsonObject {
	out := jsonObject{{"description", note.doc}}
	switch {
	case t == durationType:
		out = append(out, jsonMember{"$ref", "#/$defs/duration"})
	case t.Kind() == reflect.Struct:
		return append(out, b.mapping(t, path, v)...)
	case t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Struct:
		out = append(out, jsonMember{"type", "array"})
		if note.maxItems > 0 {
			out = append(out, jsonMember{"maxItems", note.maxItems})
		}
		out = append(out, jsonMember{"items", b.mapping(t.Elem(), path+"[]", listItem(t.Elem()))})
	case t.Kind() == reflect.Map && t.Elem().Kind() == reflect.String:
		out = append(out, jsonMember{"type", "object"}, jsonMember{"additionalProperties", jsonObject{{"type", "string"}}})
	case t.Kind() == reflect.String:
		out = append(out, jsonSchemaString(note.enum, note.pattern)...)
	case t.Kind() == reflect.Int:
		out = append(out, jsonMember{"type", "integer"})
		if note.nonNegative {
			out = append(out, jsonMember{"minimum", 0})
		} else if note.min != 0 {
			out = append(out, jsonMember{"minimum", note.min})
		}
		if note.max != 0 {
			out = append(out, jsonMember{"maximum", note.max})
		}
	default:
		b.t.Fatalf("%s: no JSON Schema for the Go type %s", path, t)
	}
	if note.required || note.requiredWhen != (condition{}) || note.inherited != "" {
		return out
	}
	return append(out, jsonMember{"default", jsonSchemaLiteral(b.t, path, v)})
}

// jsonSchemaString returns the keywords of a string. A pattern or an
// enumeration applies to the text of the file only when no part of it comes
// from the environment.
func jsonSchemaString(enum []string, pattern string) jsonObject {
	out := jsonObject{{"type", "string"}}
	var literal jsonObject
	switch {
	case len(enum) > 0:
		literal = jsonObject{{"enum", enum}}
	case pattern != "":
		literal = jsonObject{{"pattern", pattern}}
	default:
		return out
	}
	return append(out, jsonMember{"anyOf", []any{literal, jsonObject{{"$ref", "#/$defs/environment"}}}})
}

// jsonSchemaLiteral writes a default as the configuration file writes it.
func jsonSchemaLiteral(t *testing.T, path string, v reflect.Value) any {
	t.Helper()
	switch x := v.Interface().(type) {
	case time.Duration:
		for _, unit := range []struct {
			d time.Duration
			s string
		}{{time.Hour, "h"}, {time.Minute, "m"}, {time.Second, "s"}, {time.Millisecond, "ms"}} {
			if x != 0 && x%unit.d == 0 {
				return fmt.Sprintf("%d%s", x/unit.d, unit.s)
			}
		}
		if x == 0 {
			return "0s"
		}
		return x.String()
	case string, int:
		return x
	case Expression:
		return string(x)
	}
	switch v.Kind() {
	case reflect.Slice:
		if v.Len() == 0 {
			return []any{}
		}
	case reflect.Map:
		if v.Len() == 0 {
			return jsonObject{}
		}
	}
	t.Fatalf("%s: no JSON literal for a default of type %s", path, v.Type())
	return nil
}

func TestJSONSchema(t *testing.T) {
	want := buildJSONSchema(t)
	if t.Failed() {
		return
	}
	if *updateJSONSchema {
		if err := os.WriteFile(jsonSchemaFile, want, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(jsonSchemaFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the configuration types, defaults and jsonSchemaNotes: regenerate it with\n\tgo test ./internal/config -run TestJSONSchema -update", filepath.ToSlash(jsonSchemaFile))
	}
}

// The published file, not the generated one, is what editors read.
func loadJSONSchema(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile(jsonSchemaFile)
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	return schema
}

// jsonSchemaDocument reads a YAML document as the values JSON Schema sees.
func jsonSchemaDocument(t *testing.T, data []byte) any {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil || len(doc.Content) != 1 {
		t.Fatalf("not one YAML document: %v", err)
	}
	var convert func(n *yaml.Node) any
	convert = func(n *yaml.Node) any {
		switch n.Kind {
		case yaml.MappingNode:
			out := map[string]any{}
			for i := 0; i < len(n.Content); i += 2 {
				out[n.Content[i].Value] = convert(n.Content[i+1])
			}
			return out
		case yaml.SequenceNode:
			out := []any{}
			for _, item := range n.Content {
				out = append(out, convert(item))
			}
			return out
		case yaml.ScalarNode:
			var value any
			if err := n.Decode(&value); err != nil {
				t.Fatal(err)
			}
			return value
		}
		t.Fatalf("line %d: unsupported YAML node", n.Line)
		return nil
	}
	return convert(doc.Content[0])
}

// jsonSchemaProblems validates a value with the keywords the schema uses, and
// fails on any other keyword, so that the schema cannot grow a rule that no
// test exercises.
func jsonSchemaProblems(t *testing.T, root, schema map[string]any, value any, path string) []string {
	t.Helper()
	var problems []string
	report := func(format string, args ...any) {
		problems = append(problems, path+": "+fmt.Sprintf(format, args...))
	}
	keys := make([]string, 0, len(schema))
	for key := range schema {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		rule := schema[key]
		switch key {
		case "$schema", "$id", "$defs", "title", "description", "default":
		case "$ref":
			name := strings.TrimPrefix(rule.(string), "#/$defs/")
			problems = append(problems, jsonSchemaProblems(t, root, root["$defs"].(map[string]any)[name].(map[string]any), value, path)...)
		case "type":
			ok := false
			switch rule {
			case "object":
				_, ok = value.(map[string]any)
			case "string":
				_, ok = value.(string)
			case "integer":
				_, ok = value.(int)
			case "array":
				_, ok = value.([]any)
			default:
				t.Fatalf("the test validator does not know the type %v", rule)
			}
			if !ok {
				report("expected %v", rule)
			}
		case "enum":
			if !slices.Contains(rule.([]any), value) {
				report("not one of %v", rule)
			}
		case "anyOf":
			matched := false
			for _, branch := range rule.([]any) {
				matched = matched || len(jsonSchemaProblems(t, root, branch.(map[string]any), value, path)) == 0
			}
			if !matched {
				report("matches no alternative")
			}
		case "pattern":
			if s, ok := value.(string); ok && !regexp.MustCompile(rule.(string)).MatchString(s) {
				report("does not match %v", rule)
			}
		case "minimum":
			if n, ok := value.(int); ok && float64(n) < rule.(float64) {
				report("less than %v", rule)
			}
		case "maximum":
			if n, ok := value.(int); ok && float64(n) > rule.(float64) {
				report("greater than %v", rule)
			}
		case "required":
			if m, ok := value.(map[string]any); ok {
				for _, name := range rule.([]any) {
					if _, present := m[name.(string)]; !present {
						report("%v is required", name)
					}
				}
			}
		case "properties":
			if m, ok := value.(map[string]any); ok {
				for name, sub := range rule.(map[string]any) {
					if item, present := m[name]; present {
						problems = append(problems, jsonSchemaProblems(t, root, sub.(map[string]any), item, childPath(path, name))...)
					}
				}
			}
		case "additionalProperties":
			if m, ok := value.(map[string]any); ok {
				known, _ := schema["properties"].(map[string]any)
				for name, item := range m {
					if _, declared := known[name]; declared {
						continue
					}
					switch rule := rule.(type) {
					case bool:
						if !rule {
							report("unknown key %s", name)
						}
					case map[string]any:
						problems = append(problems, jsonSchemaProblems(t, root, rule, item, childPath(path, name))...)
					default:
						t.Fatalf("the test validator does not know additionalProperties: %v", rule)
					}
				}
			}
		case "maxItems":
			if items, ok := value.([]any); ok && float64(len(items)) > rule.(float64) {
				report("more than %v items", rule)
			}
		case "items":
			if items, ok := value.([]any); ok {
				for i, item := range items {
					problems = append(problems, jsonSchemaProblems(t, root, rule.(map[string]any), item, itemPath(path, i))...)
				}
			}
		default:
			t.Fatalf("the test validator does not know the keyword %q", key)
		}
	}
	return problems
}

func jsonSchemaCheck(t *testing.T, schema map[string]any, data []byte) []string {
	t.Helper()
	return jsonSchemaProblems(t, schema, schema, jsonSchemaDocument(t, data), "configuration")
}

// The schema never rejects a file that tessera accepts: the examples, the
// configuration snippets of the documentation, and the cases below.
func TestJSONSchemaAcceptsValidConfigurations(t *testing.T) {
	schema := loadJSONSchema(t)
	examples, err := filepath.Glob(filepath.Join(examplesDir, "*.yaml"))
	if err != nil || len(examples) == 0 {
		t.Fatalf("no examples found: %v", err)
	}
	for _, path := range examples {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Parse(data, nil); err != nil {
			continue // Reported by TestRepositoryExamples.
		}
		for _, problem := range jsonSchemaCheck(t, schema, data) {
			t.Errorf("%s: tessera accepts it, the schema does not: %s", filepath.Base(path), problem)
		}
	}
	for where, snippet := range docsConfigSnippets(t) {
		_, completed, err := parseConfigSnippet(snippet)
		if err != nil {
			continue // Reported by TestDocsConfigSnippets.
		}
		for _, problem := range jsonSchemaCheck(t, schema, completed) {
			t.Errorf("%s: tessera accepts it, the schema does not: %s", where, problem)
		}
	}
	environment := map[string]string{"LISTEN": ":9101", "ADMIN": "127.0.0.1:9102", "AGE": "30m", "LEVEL": "warn", "ACTION": "allow", "SOURCE": "header", "HEADER": "x-client-address"}
	for name, text := range map[string]string{
		"required keys only": "policy:\n  default_action: deny\n",
		"every key": `
server:
  listen: 127.0.0.1:9101
  max_concurrent_streams: 4294967295
  max_connection_age: 30m
  drain_delay: 0s
  shutdown_timeout: 1m30s
admin:
  listen: "[::1]:9102"
log:
  level: debug
  format: json
client_ip:
  from: x_forwarded_for
  trusted_hops: 0
policy:
  mode: audit
  default_action: allow
  deny_response:
    status: 451
    body: Not available.
    headers:
      Content-Type: text/plain; charset=utf-8
  rules:
    - name: admin
      description: The admin pages, from the office only.
      action: deny
      mode: enforce
      when: request.path.startsWith("/admin") && !client.ip.inCidr("192.0.2.0/24")
      response:
        status: 404
        body: ""
        headers: {}
    - name: everyone
      action: allow
      mode: audit
`,
		"client address from a header": `
client_ip:
  from: header
  header: X-Envoy-External-Address
policy:
  default_action: deny
`,
		"values from the environment": `
server:
  listen: ${LISTEN}
  max_connection_age: ${AGE}
  drain_delay: ${DELAY:-2s}
admin:
  listen: ${ADMIN}
log:
  level: ${LEVEL}
  format: ${FORMAT:-json}
client_ip:
  from: ${SOURCE}
  header: ${HEADER}
policy:
  default_action: ${ACTION}
`,
		"spellings tessera accepts": `
server:
  listen: unix:run/grpc.sock
  max_connection_age: +10m
  drain_delay: .5s
  shutdown_timeout: 1.5s
admin:
  listen: localhost:9090
policy:
  default_action: deny
`,
		"literal ${": "server:\n  listen: unix:/run/$${x}.sock\npolicy:\n  default_action: deny\n",
	} {
		data := []byte(text)
		if _, err := Parse(data, func(k string) (string, bool) { v, ok := environment[k]; return v, ok }); err != nil {
			t.Errorf("%s: tessera rejects a case meant to be valid: %v", name, err)
			continue
		}
		for _, problem := range jsonSchemaCheck(t, schema, data) {
			t.Errorf("%s: tessera accepts it, the schema does not: %s", name, problem)
		}
	}
}

// Each kind of rule in the schema rejects something, and tessera rejects it
// too.
func TestJSONSchemaRejectsInvalidConfigurations(t *testing.T) {
	schema := loadJSONSchema(t)
	for name, text := range map[string]string{
		"missing policy":            "log:\n  level: info\n",
		"missing default action":    "policy: {}\n",
		"unknown top-level key":     "policy: {default_action: deny}\nsets: {}\n",
		"unknown key":               "policy: {default_action: deny}\nserver:\n  lisen: :9001\n",
		"unknown value":             "policy: {default_action: maybe}\n",
		"unknown log level":         "policy: {default_action: deny}\nlog: {level: trace}\n",
		"number for a duration":     "policy: {default_action: deny}\nserver: {drain_delay: 5}\n",
		"duration without unit":     "policy: {default_action: deny}\nserver: {drain_delay: \"5\"}\n",
		"duration in days":          "policy: {default_action: deny}\nserver: {drain_delay: 1d}\n",
		"string for an integer":     "policy: {default_action: deny}\nserver: {max_concurrent_streams: \"16\"}\n",
		"integer below bound":       "policy: {default_action: deny}\nserver: {max_concurrent_streams: 0}\n",
		"integer above bound":       "policy: {default_action: deny}\nserver: {max_concurrent_streams: 4294967296}\n",
		"listen without port":       "policy: {default_action: deny}\nserver: {listen: localhost}\n",
		"admin on a socket":         "policy: {default_action: deny}\nadmin: {listen: \"unix:/run/admin.sock\"}\n",
		"scalar for a mapping":      "policy: deny\n",
		"unknown source":            "policy: {default_action: deny}\nclient_ip: {from: forwarded}\n",
		"header name with space":    "policy: {default_action: deny}\nclient_ip: {from: header, header: \"x client\"}\n",
		"negative trusted hops":     "policy: {default_action: deny}\nclient_ip: {from: x_forwarded_for, trusted_hops: -1}\n",
		"unknown policy mode":       "policy: {default_action: deny, mode: observe}\n",
		"status out of range":       "policy: {default_action: deny, deny_response: {status: 302}}\n",
		"header value not a string": "policy: {default_action: deny, deny_response: {headers: {x-a: 1}}}\n",
		"rules not a list":          "policy: {default_action: deny, rules: {}}\n",
		"rule without a name":       "policy: {default_action: deny, rules: [{action: deny}]}\n",
		"rule name":                 "policy: {default_action: deny, rules: [{name: Office, action: deny}]}\n",
		"unknown rule key":          "policy: {default_action: deny, rules: [{name: r, action: deny, then: x}]}\n",
		"rule action":               "policy: {default_action: deny, rules: [{name: r, action: block}]}\n",
		"rule status":               "policy: {default_action: deny, rules: [{name: r, action: deny, response: {status: 200}}]}\n",
		"too many rules":            "policy: {default_action: deny, rules: [" + strings.Repeat("{name: r, action: deny, when: \"false\"},", MaxRules+1) + "]}\n",
	} {
		t.Run(name, func(t *testing.T) {
			data := []byte(text)
			if _, err := Parse(data, nil); err == nil {
				t.Fatal("tessera accepts a case meant to be invalid")
			}
			if len(jsonSchemaCheck(t, schema, data)) == 0 {
				t.Error("the schema accepts it")
			}
		})
	}
}
