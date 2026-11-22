package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// configSnippetInfo is the fence info string of a tessera configuration
// snippet in the documentation.
const configSnippetInfo = "yaml [config.yaml]"

var (
	repoRoot    = filepath.Join("..", "..")
	examplesDir = filepath.Join(repoRoot, "docs", "examples")
)

type configSnippet struct {
	line    int // line of the opening fence, from 1
	content string
}

// extractConfigSnippets returns the fenced code blocks of a Markdown document
// whose info string is exactly configSnippetInfo. A fence is three or more
// backticks or tildes, possibly indented (inside a list item), and is closed by
// a line of the same character at least as long; an unclosed fence runs to the
// end of the document. Content lines lose up to as much indentation as the
// opening fence has. Anything inside a fence, other fences included, is content.
//
// misplaced lists the lines, outside any fence, that mention [config.yaml]
// without opening a snippet (a fence with highlights or other modifiers, a
// fence inside a block quote, or prose), and the YAML fences without a title.
// Such a block would escape validation.
func extractConfigSnippets(markdown string) (snippets []configSnippet, misplaced []int) {
	lines := strings.Split(strings.ReplaceAll(markdown, "\r\n", "\n"), "\n")
	for i := 0; i < len(lines); i++ {
		indent, marker, length, info, ok := openingFence(lines[i])
		language := info
		if end := strings.IndexAny(language, " {:["); end >= 0 {
			language = language[:end]
		}
		untitled := ok && (language == "yaml" || language == "yml") && !strings.Contains(info, "[")
		if untitled || (!ok || info != configSnippetInfo) && strings.Contains(lines[i], "[config.yaml]") {
			misplaced = append(misplaced, i+1)
		}
		if !ok {
			continue
		}
		start := i
		var body []string
		for i++; i < len(lines) && !closingFence(lines[i], marker, length); i++ {
			line := lines[i]
			for n := 0; n < indent && strings.HasPrefix(line, " "); n++ {
				line = line[1:]
			}
			body = append(body, line)
		}
		if info == configSnippetInfo {
			snippets = append(snippets, configSnippet{line: start + 1, content: strings.Join(body, "\n") + "\n"})
		}
	}
	return snippets, misplaced
}

func openingFence(line string) (indent int, marker byte, length int, info string, ok bool) {
	text := strings.TrimLeft(line, " ")
	if text == "" || (text[0] != '`' && text[0] != '~') {
		return 0, 0, 0, "", false
	}
	marker = text[0]
	for length < len(text) && text[length] == marker {
		length++
	}
	info = strings.TrimSpace(text[length:])
	if length < 3 || (marker == '`' && strings.Contains(info, "`")) {
		return 0, 0, 0, "", false
	}
	return len(line) - len(text), marker, length, info, true
}

func closingFence(line string, marker byte, length int) bool {
	text := strings.TrimLeft(line, " ")
	n := 0
	for n < len(text) && text[n] == marker {
		n++
	}
	return n >= length && strings.TrimSpace(text[n:]) == ""
}

// completeConfigSnippet turns a snippet into a whole configuration: it adds
// policy.default_action (merged into an existing policy mapping) when it is
// missing. Everything else is left for Parse to judge.
func completeConfigSnippet(snippet string) ([]byte, error) {
	decoder := yaml.NewDecoder(strings.NewReader(snippet))
	var doc yaml.Node
	if err := decoder.Decode(&doc); err != nil {
		return nil, fmt.Errorf("snippet: invalid YAML: %w", err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("snippet: exactly one YAML document required")
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("snippet: expected a YAML mapping")
	}
	root := doc.Content[0]
	str := func(value string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value} }
	policy := mappingValue(root, "policy")
	if policy == nil {
		policy = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		root.Content = append(root.Content, str("policy"), policy)
	}
	if policy.Kind == yaml.MappingNode && mappingValue(policy, "default_action") == nil {
		policy.Content = append([]*yaml.Node{str("default_action"), str(ActionDeny)}, policy.Content...)
	}
	return yaml.Marshal(&doc)
}

func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

// parseConfigSnippet completes a snippet and parses it, with every
// environment variable unset: a snippet that reads one gives it a default. It
// returns the completed YAML for diagnostics.
func parseConfigSnippet(snippet string) (*Config, []byte, error) {
	data, err := completeConfigSnippet(snippet)
	if err != nil {
		return nil, nil, err
	}
	c, err := Parse(data, nil)
	return c, data, err
}

// docsConfigSnippets returns the configuration snippets of every page under
// docs/, keyed by file and line, and reports the misplaced ones.
func docsConfigSnippets(t *testing.T) map[string]string {
	t.Helper()
	found := map[string]string{}
	err := filepath.WalkDir(filepath.Join(repoRoot, "docs"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case "node_modules", ".vitepress", "public":
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".md" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		name, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return err
		}
		snippets, misplaced := extractConfigSnippets(string(data))
		for _, line := range misplaced {
			t.Errorf("%s:%d: a tessera configuration needs a fence with exactly ```yaml [config.yaml] (not in a blockquote, no line highlights or other modifiers), and other YAML a fence with another title, such as ```yaml [envoy.yaml]", filepath.ToSlash(name), line)
		}
		for _, snippet := range snippets {
			found[fmt.Sprintf("%s:%d", filepath.ToSlash(name), snippet.line)] = snippet.content
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The reference pages contain snippets: finding none means the walk or
	// the extraction is broken, not that there is nothing to check.
	if len(found) == 0 {
		t.Fatal("no configuration snippets found under docs/")
	}
	return found
}

func TestDocsConfigSnippets(t *testing.T) {
	for where, snippet := range docsConfigSnippets(t) {
		_, completed, err := parseConfigSnippet(snippet)
		if err == nil {
			continue
		}
		if completed == nil {
			t.Errorf("%s: %v", where, err)
			continue
		}
		// Parse reports lines of the completed configuration.
		var numbered strings.Builder
		for i, line := range strings.Split(strings.TrimSuffix(string(completed), "\n"), "\n") {
			fmt.Fprintf(&numbered, "%4d  %s\n", i+1, line)
		}
		t.Errorf("%s: %v\ncompleted configuration:\n%s", where, err, numbered.String())
	}
}

// exampleSchemaDirective is the first line of every example: it points an
// editor at the schema of the working tree, and the documentation build
// replaces the path with the address of the published schema.
const exampleSchemaDirective = "# yaml-language-server: $schema=../public/config.schema.json"

// TestRepositoryExamples loads every file in docs/examples/ as a complete
// configuration, with every environment variable unset.
func TestRepositoryExamples(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(examplesDir, "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no file in docs/examples/")
	}
	for _, file := range files {
		if strings.HasPrefix(filepath.Base(file), ".") {
			continue // Such as the .DS_Store of macOS, which Git ignores.
		}
		t.Run(filepath.Base(file), func(t *testing.T) {
			if filepath.Ext(file) != ".yaml" {
				t.Fatal("docs/examples/ holds complete configurations only, as .yaml files")
			}
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if first, _, _ := strings.Cut(string(data), "\n"); first != exampleSchemaDirective {
				t.Errorf("the first line must be %s", exampleSchemaDirective)
			}
			c, err := Load(file, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.Redacted(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestExtractConfigSnippets(t *testing.T) {
	// ^ stands for a backtick, which a Go raw string cannot hold.
	markdown := strings.ReplaceAll(`# Sample

^^^yaml [config.yaml]
log:
  level: debug
^^^

~~~ yaml [config.yaml]
admin: {}
^^^
~~~

^^^^md
^^^yaml [config.yaml]
inside: a longer fence
^^^
^^^^

^^^yaml
plain: yaml
^^^

^^^yaml [envoy.yaml]
static_resources: {}
^^^

1. A step:

   ^^^yaml [config.yaml]
   server:
     listen: ":9001"
   ^^^

^^^^yaml [config.yaml]
policy:
  default_action: allow
^^^
^^^^^
`, "^", "`")
	want := []configSnippet{
		{3, "log:\n  level: debug\n"},
		{8, "admin: {}\n```\n"},
		{29, "server:\n  listen: \":9001\"\n"},
		{34, "policy:\n  default_action: allow\n```\n"},
	}
	got, misplaced := extractConfigSnippets(markdown)
	// The untitled YAML fence at line 19 is reported.
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(misplaced, []int{19}) {
		t.Fatalf("got %#v, misplaced %v\nwant %#v, misplaced [19]", got, misplaced, want)
	}
}

func TestExtractConfigSnippetsNearMisses(t *testing.T) {
	// ^ stands for a backtick. Each near miss would escape validation; the
	// one inside an example fence is content, and the last block is a snippet.
	markdown := strings.ReplaceAll(`^^^yaml{2} [config.yaml]
log: {level: a}
^^^

^^^yaml:line-numbers [config.yaml]
log: {level: b}
^^^

^^^yaml [config.yaml]{1}
log: {level: c}
^^^

> ^^^yaml [config.yaml]
> log: {level: d}
> ^^^

Use a ^yaml [config.yaml]^ fence.

^^^^md
^^^yaml{2} [config.yaml]
log: {level: e}
^^^
^^^^

^^^yaml [config.yaml]
log: {level: f}
^^^

^^^yml
log: {level: g}
^^^

^^^yaml{1}
log: {level: h}
^^^

^^^yaml:line-numbers
log: {level: i}
^^^
`, "^", "`")
	snippets, misplaced := extractConfigSnippets(markdown)
	if want := []configSnippet{{25, "log: {level: f}\n"}}; !reflect.DeepEqual(snippets, want) {
		t.Fatalf("snippets %#v, want %#v", snippets, want)
	}
	if want := []int{1, 5, 9, 13, 17, 29, 33, 37}; !reflect.DeepEqual(misplaced, want) {
		t.Fatalf("misplaced lines %v, want %v", misplaced, want)
	}
}

func TestParseConfigSnippet(t *testing.T) {
	for _, tc := range []struct{ snippet, action, level string }{
		{"log:\n  level: debug\n", "deny", "debug"},
		{"policy:\n  default_action: allow\n", "allow", "info"},
		{"policy: {}\nlog: {level: warn}\n", "deny", "warn"},
		{"log:\n  level: ${LOG_LEVEL:-error}\n", "deny", "error"},
	} {
		c, _, err := parseConfigSnippet(tc.snippet)
		if err != nil {
			t.Fatalf("%q: %v", tc.snippet, err)
		}
		if c.Policy.DefaultAction != tc.action || c.Log.Level != tc.level {
			t.Fatalf("%q: default action %q, level %q", tc.snippet, c.Policy.DefaultAction, c.Log.Level)
		}
	}
	for _, snippet := range []string{"", "- a\n", "log: [\n", "log: {}\n---\nlog: {}\n", "log:\n  level: 1\n", "sets: {}\n", "log:\n  level: ${LOG_LEVEL}\n", "policy: deny\n"} {
		if _, _, err := parseConfigSnippet(snippet); err == nil {
			t.Fatalf("%q: invalid snippet accepted", snippet)
		}
	}
}
