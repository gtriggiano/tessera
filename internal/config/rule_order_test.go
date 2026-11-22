package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// Every example of a rule writes its keys in one order, the order of the
// fields of Rule: name, description, action, mode, when, response; and the
// keys of response in the order of Response. The order is a convention of
// the examples, not of the file: tessera reads the keys in any order.
var (
	ruleKeyOrder     = keyOrder(reflect.TypeFor[Rule]())
	responseKeyOrder = keyOrder(reflect.TypeFor[Response]())
)

// e2eTestdata holds the configurations of the end-to-end test.
var e2eTestdata = filepath.Join(repoRoot, "internal", "e2e", "testdata")

func keyOrder(t reflect.Type) []string {
	var keys []string
	for i := range t.NumField() {
		if f := t.Field(i); f.IsExported() {
			keys = append(keys, yamlName(f))
		}
	}
	return keys
}

// ruleOrderProblems returns where the rules of the configuration data write
// their keys in another order, as line, from 1, and problem, and the number
// of rules it checked.
func ruleOrderProblems(t *testing.T, data string) (problems []string, rules int) {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(data), &doc); err != nil || len(doc.Content) != 1 {
		t.Fatalf("not one YAML document: %v", err)
	}
	list := mappingValue(doc.Content[0], "policy")
	if list != nil {
		list = mappingValue(list, "rules")
	}
	if list == nil || list.Kind != yaml.SequenceNode {
		return nil, 0
	}
	check := func(n *yaml.Node, order []string, what string) {
		last := -1
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			at := slices.Index(order, k.Value)
			if at < 0 {
				continue // An unknown key: Parse reports it.
			}
			if at < last {
				problems = append(problems, fmt.Sprintf("line %d: the %s key %s comes after %s; write %s", k.Line, what, k.Value, order[last], strings.Join(order, ", ")))
			}
			last = max(last, at)
		}
	}
	for _, rule := range list.Content {
		if rule.Kind != yaml.MappingNode {
			continue
		}
		rules++
		check(rule, ruleKeyOrder, "rule")
		if response := mappingValue(rule, "response"); response != nil && response.Kind == yaml.MappingNode {
			check(response, responseKeyOrder, "response")
		}
	}
	return problems, rules
}

// writtenRules returns the number of items of policy.rules in the
// configuration data, read by decoding it rather than by walking its nodes as
// ruleOrderProblems does: when the two counts differ, ruleOrderProblems
// skipped rules, and would pass without checking them.
func writtenRules(t *testing.T, data string) int {
	t.Helper()
	var doc struct {
		Policy struct {
			Rules []any `yaml:"rules"`
		} `yaml:"policy"`
	}
	if err := yaml.Unmarshal([]byte(data), &doc); err != nil {
		t.Fatalf("cannot decode: %v", err)
	}
	return len(doc.Policy.Rules)
}

// TestRuleKeyOrder checks the order of the keys of every rule in the
// configuration snippets of the documentation, in docs/examples/ and in the
// configurations of the end-to-end test.
func TestRuleKeyOrder(t *testing.T) {
	sources := map[string]string{}
	for where, snippet := range docsConfigSnippets(t) {
		sources[where+" (snippet)"] = snippet
	}
	for _, dir := range []string{examplesDir, e2eTestdata} {
		files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
		if err != nil || len(files) == 0 {
			t.Fatalf("no YAML file in %s: %v", dir, err)
		}
		for _, file := range files {
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			name, _ := filepath.Rel(repoRoot, file)
			sources[filepath.ToSlash(name)] = string(data)
		}
	}
	checked := 0
	for where, data := range sources {
		problems, rules := ruleOrderProblems(t, data)
		for _, problem := range problems {
			t.Errorf("%s, %s", where, problem)
		}
		if written := writtenRules(t, data); rules != written {
			t.Errorf("%s: %d rules checked of the %d written", where, rules, written)
		}
		checked += rules
	}
	if checked == 0 {
		t.Fatal("no rule checked: the documentation, docs/examples/ and the end-to-end configurations have no rule")
	}
}

func TestRuleOrderProblems(t *testing.T) {
	ordered := "policy:\n  default_action: deny\n  rules:\n    - name: a\n      description: d\n      action: deny\n      mode: audit\n      when: \"true\"\n      response: {status: 404, body: \"\", headers: {}}\n    - {name: b, action: allow}\n"
	if p, n := ruleOrderProblems(t, ordered); len(p) != 0 || n != 2 {
		t.Fatalf("ordered rules: %v, %d checked", p, n)
	}
	for yaml, want := range map[string]string{
		"policy:\n  default_action: deny\n  rules:\n    - name: a\n      when: \"true\"\n      action: deny\n":                                  "line 6: the rule key action comes after when; write name, description, action, mode, when, response",
		"policy:\n  default_action: deny\n  rules:\n    - action: deny\n      name: a\n":                                                        "line 5: the rule key name comes after action; write name, description, action, mode, when, response",
		"policy:\n  default_action: deny\n  rules:\n    - name: a\n      action: deny\n      response:\n        body: x\n        status: 404\n": "line 8: the response key status comes after body; write status, body, headers",
	} {
		if p, _ := ruleOrderProblems(t, yaml); len(p) != 1 || p[0] != want {
			t.Errorf("%q: %v, want %q", yaml, p, want)
		}
	}
	// Envoy's configurations have no policy: nothing to check.
	if p, n := ruleOrderProblems(t, "static_resources:\n  listeners: []\n"); len(p) != 0 || n != 0 {
		t.Fatalf("Envoy configuration: %v, %d checked", p, n)
	}
}
