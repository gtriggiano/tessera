package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// The configuration reference documents every key under a heading that is
// exactly its path in backticks, followed by a "**Type:**" line, a
// "**Default:**" line and, when validation narrows the type, an "**Allowed:**"
// line. The default is a YAML literal in backticks, or the word required when
// omitting the key is an error at its path.
//
// An allowed line that starts with one of these forms is checked against
// Parse; any text may follow a form after "; ":
//
//	`a`, `b` or `c`           exactly these values load
//	`1` to `3`                the bounds load, their neighbors do not
//	positive                  zero is rejected
//	zero or positive          zero loads
//	nonempty                  "" is rejected
//
// A list of values is complete: it equals the enumeration of the key in the
// JSON Schema (jsonSchemaNotes), which TestJSONSchema keeps equal to the
// published file. A string that rejects its empty value has an allowed line.
// The text after "; ", an allowed line in free text and the prose are not
// checked.
//
// Each top-level section with keys below it has a table of them, whose cells
// repeat these lines.
var referencePage = filepath.Join("..", "..", "docs", "reference", "configuration.md")

var (
	keyHeadingRE     = regexp.MustCompile("^#{2,6} `([a-z0-9_]+(?:\\.[a-z0-9_]+)*)`$")
	typeLineRE       = regexp.MustCompile(`^\*\*Type:\*\* (.+)$`)
	defaultLiteralRE = regexp.MustCompile("^\\*\\*Default:\\*\\* `([^`]+)`$")
	allowedLineRE    = regexp.MustCompile(`^\*\*Allowed:\*\* (\S.*)$`)
	allowedEnumRE    = regexp.MustCompile("^`[^`]+`(?:(?:, | or )`[^`]+`)+$")
	allowedRangeRE   = regexp.MustCompile("^`(-?[0-9]+)` to `(-?[0-9]+)`$")
	codeRE           = regexp.MustCompile("`([^`]+)`")
	anchorRE         = regexp.MustCompile(`[^a-z0-9]+`)
)

const (
	tableHeader = "| Key | Type | Default | Allowed |"
	// referenceBase is the smallest valid configuration: each key is checked
	// by adding it to this one, or by removing it.
	referenceBase = "policy:\n  default_action: deny\n"
)

type schemaKey struct {
	path string
	typ  reflect.Type
}

type referenceEntry struct {
	line     int
	typ      string // the documented type
	literal  string // the documented default, empty when it is required
	summary  string // the default as the section's table repeats it
	allowed  string // the text of the **Allowed:** line, or empty
	required bool
}

func TestDocsConfigReference(t *testing.T) {
	data, err := os.ReadFile(referencePage)
	if err != nil {
		t.Fatal(err)
	}
	documented := parseReferencePage(t, string(data))

	var keys []schemaKey
	collectSchemaKeys(reflect.TypeFor[Config](), "", &keys)
	known := map[string]bool{}
	for _, key := range keys {
		known[key.path] = true
		if _, ok := documented[key.path]; !ok {
			t.Errorf("%s: key %s has no heading", referencePage, key.path)
		}
	}
	for path, entry := range documented {
		if !known[path] {
			t.Errorf("%s:%d: %s is not a configuration key", referencePage, entry.line, path)
		}
	}
	for _, key := range keys {
		entry, ok := documented[key.path]
		if !ok {
			continue
		}
		if want := typeName(key.typ); entry.typ != want {
			t.Errorf("%s:%d: %s: documented type %q, want %q", referencePage, entry.line, key.path, entry.typ, want)
		}
		if entry.required != jsonSchemaNotes[key.path].required {
			t.Errorf("%s:%d: %s: documented as required %t, but the JSON Schema (jsonSchemaNotes) has %t", referencePage, entry.line, key.path, entry.required, jsonSchemaNotes[key.path].required)
		}
		if entry.required {
			checkRequired(t, key.path, entry)
		} else {
			checkDefault(t, key.path, entry)
		}
		checkAllowed(t, key, entry)
	}
	checkSummaryTables(t, string(data), documented)
}

// collectSchemaKeys appends every key reachable from t, blocks included, in
// the order of the types.
func collectSchemaKeys(t reflect.Type, prefix string, keys *[]schemaKey) {
	if t.Kind() != reflect.Struct || t == durationType {
		return
	}
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		path := childPath(prefix, yamlName(f))
		*keys = append(*keys, schemaKey{path, f.Type})
		collectSchemaKeys(f.Type, path, keys)
	}
}

// typeName is the page's name for a Go type. A type without a name here is
// reported as its Go type, so a new kind of key needs a deliberate name.
func typeName(t reflect.Type) string {
	if t == durationType {
		return "duration"
	}
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Int:
		return "integer"
	case reflect.Struct:
		return "mapping"
	}
	return t.String()
}

// zeroLiteral is the YAML spelling of the zero value of a scalar key, or ""
// for a block.
func zeroLiteral(t reflect.Type) string {
	if t == durationType {
		return "0s"
	}
	switch t.Kind() {
	case reflect.String:
		return `""`
	case reflect.Int:
		return "0"
	}
	return ""
}

// signedLiteral is the YAML spelling of n for an integer or a duration, in
// seconds, or "" for another type.
func signedLiteral(t reflect.Type, n int) string {
	switch {
	case t == durationType:
		return strconv.Itoa(n) + "s"
	case t.Kind() == reflect.Int:
		return strconv.Itoa(n)
	}
	return ""
}

// parseReferencePage returns the key headings of the page, outside code
// fences, with their documented types and defaults.
func parseReferencePage(t *testing.T, page string) map[string]referenceEntry {
	t.Helper()
	lines := strings.Split(page, "\n")
	documented := map[string]referenceEntry{}
	anchors := map[string]int{} // heading anchor → line
	for i := 0; i < len(lines); i++ {
		if _, marker, length, _, ok := openingFence(lines[i]); ok {
			for i++; i < len(lines) && !closingFence(lines[i], marker, length); i++ {
			}
			continue
		}
		if !strings.HasPrefix(lines[i], "#") {
			continue
		}
		// Like the site's anchors: the link to a key must not move to another
		// heading with the same text.
		anchor := strings.Trim(anchorRE.ReplaceAllString(strings.ToLower(strings.TrimLeft(lines[i], "# ")), "-"), "-")
		if previous, ok := anchors[anchor]; ok {
			t.Errorf("%s:%d: this heading has the same anchor #%s as line %d", referencePage, i+1, anchor, previous)
		}
		anchors[anchor] = i + 1
		m := keyHeadingRE.FindStringSubmatch(lines[i])
		if m == nil {
			if strings.Contains(lines[i], "`") {
				t.Errorf("%s:%d: a heading with code must be exactly one key path in backticks", referencePage, i+1)
			}
			continue
		}
		path, line := m[1], i+1
		if previous, ok := documented[path]; ok {
			t.Errorf("%s:%d: %s is already documented at line %d", referencePage, line, path, previous.line)
			continue
		}
		// The first paragraph of the section: **Type:**, **Default:** and an
		// optional **Allowed:**, joined by hard line breaks.
		j := i + 1
		for j < len(lines) && strings.TrimSpace(lines[j]) == "" {
			j++
		}
		var body []string
		for ; j < len(lines) && strings.TrimSpace(lines[j]) != ""; j++ {
			body = append(body, lines[j])
		}
		for k := 0; k < len(body)-1; k++ {
			if !strings.HasSuffix(body[k], `\`) {
				t.Errorf(`%s:%d: %s: every line of the first paragraph but the last must end with \`, referencePage, line, path)
			}
			body[k] = strings.TrimSuffix(body[k], `\`)
		}
		var typ []string
		if len(body) == 2 || len(body) == 3 {
			typ = typeLineRE.FindStringSubmatch(body[0])
		}
		if typ == nil {
			t.Errorf("%s:%d: %s: the section must start with a **Type:** line, a **Default:** line and an optional **Allowed:** line", referencePage, line, path)
			continue
		}
		entry := referenceEntry{line: line, typ: typ[1]}
		switch m := defaultLiteralRE.FindStringSubmatch(body[1]); {
		case m != nil:
			entry.literal, entry.summary = m[1], "`"+m[1]+"`"
		case body[1] == "**Default:** required":
			entry.required, entry.summary = true, "required"
		default:
			t.Errorf("%s:%d: %s: the line after **Type:** must be **Default:** followed by `<yaml literal>` or required", referencePage, line, path)
			continue
		}
		if len(body) == 3 {
			m := allowedLineRE.FindStringSubmatch(body[2])
			if m == nil {
				t.Errorf("%s:%d: %s: the line after **Default:** must be **Allowed:** <constraint>", referencePage, line, path)
				continue
			}
			entry.allowed = m[1]
		}
		documented[path] = entry
	}
	return documented
}

// checkDefault parses the base configuration without the key and with the key
// set to the documented literal: both must give the same value.
func checkDefault(t *testing.T, path string, entry referenceEntry) {
	t.Helper()
	literal, err := yamlValue(entry.literal)
	if err != nil {
		t.Errorf("%s:%d: %s: default `%s` is not a YAML value", referencePage, entry.line, path, entry.literal)
		return
	}
	omitted, err := Parse(referenceVariant(t, path, nil), nil)
	if err != nil {
		t.Errorf("%s:%d: %s has a default, but omitting it is an error: %v", referencePage, entry.line, path, err)
		return
	}
	explicit, err := Parse(referenceVariant(t, path, literal), nil)
	if err != nil {
		t.Errorf("%s:%d: %s: documented default `%s` is rejected: %v", referencePage, entry.line, path, entry.literal, err)
		return
	}
	got, want := valueAt(omitted, path), valueAt(explicit, path)
	if !reflect.DeepEqual(got.Interface(), want.Interface()) {
		t.Errorf("%s:%d: %s: documented default `%s` loads as %v, but omitting the key gives %v", referencePage, entry.line, path, entry.literal, want, got)
	}
}

// checkRequired checks that omitting a key documented as required is an error
// at its path, or at a key inside it.
func checkRequired(t *testing.T, path string, entry referenceEntry) {
	t.Helper()
	_, err := Parse(referenceVariant(t, path, nil), nil)
	var invalid *Error
	if !errors.As(err, &invalid) || invalid.Path != path && !strings.HasPrefix(invalid.Path, path+".") {
		t.Errorf("%s:%d: %s is documented as required, but omitting it does not fail at its path (error: %v)", referencePage, entry.line, path, err)
	}
}

// loads reports whether the base configuration loads with the key set to the
// YAML literal.
func loads(t *testing.T, path, literal string) bool {
	t.Helper()
	value, err := yamlValue(literal)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	_, err = Parse(referenceVariant(t, path, value), nil)
	return err == nil
}

// checkAllowed checks the **Allowed:** line of a key against Parse. Numbers
// always have one: every integer and duration is constrained.
func checkAllowed(t *testing.T, key schemaKey, entry referenceEntry) {
	t.Helper()
	path, typ := key.path, typeName(key.typ)
	at := fmt.Sprintf("%s:%d: %s", referencePage, entry.line, path)
	form, _, _ := strings.Cut(entry.allowed, "; ")
	zero := zeroLiteral(key.typ)
	// A list of values on the page and the enumeration of the schema are the
	// same list.
	enum := jsonSchemaNotes[path].enum
	if listed := allowedEnumRE.MatchString(form); listed || len(enum) > 0 {
		var values []string
		if listed {
			for _, m := range codeRE.FindAllStringSubmatch(form, -1) {
				values = append(values, m[1])
			}
		}
		if !slices.Equal(values, enum) {
			t.Errorf("%s: the allowed line lists the values [%s], but the JSON Schema (jsonSchemaNotes) has [%s]: list the same values, in the same order", at, strings.Join(values, ", "), strings.Join(enum, ", "))
		}
	}
	switch {
	case allowedEnumRE.MatchString(form):
		for _, m := range codeRE.FindAllStringSubmatch(form, -1) {
			if !loads(t, path, m[1]) {
				t.Errorf("%s: the allowed value `%s` is rejected", at, m[1])
			}
		}
		if loads(t, path, "tessera-other-value") {
			t.Errorf("%s: the allowed line lists every value, but `tessera-other-value` loads too", at)
		}
	case allowedRangeRE.MatchString(form):
		m := allowedRangeRE.FindStringSubmatch(form)
		low, _ := strconv.Atoi(m[1])
		high, _ := strconv.Atoi(m[2])
		for _, n := range []int{low, high} {
			if !loads(t, path, strconv.Itoa(n)) {
				t.Errorf("%s: the allowed bound %d is rejected", at, n)
			}
		}
		for _, n := range []int{low - 1, high + 1} {
			if loads(t, path, strconv.Itoa(n)) {
				t.Errorf("%s: %d is outside the allowed range, but it loads", at, n)
			}
		}
	case form == "positive" || form == "nonempty":
		if zero == "" {
			t.Errorf("%s: %q does not apply to a %s", at, form, typ)
		} else if loads(t, path, zero) {
			t.Errorf("%s: documented as %s, but `%s` loads", at, form, zero)
		}
		if one := signedLiteral(key.typ, 1); form == "positive" && (one == "" || !loads(t, path, one)) {
			t.Errorf("%s: documented as positive, but `%s` is rejected", at, one)
		}
	case form == "zero or positive":
		one, minusOne := signedLiteral(key.typ, 1), signedLiteral(key.typ, -1)
		if zero == "" || one == "" || !loads(t, path, zero) || !loads(t, path, one) {
			t.Errorf("%s: documented as zero or positive, but `%s` or `%s` is rejected", at, zero, one)
		} else if loads(t, path, minusOne) {
			t.Errorf("%s: documented as zero or positive, but `%s` loads", at, minusOne)
		}
	default:
		if typ == "integer" || typ == "duration" {
			t.Errorf("%s: a %s needs an **Allowed:** line that starts with positive, zero or positive, a range (`1` to `3`) or a list of values", at, typ)
		}
		if entry.allowed == "" && typ == "string" && !loads(t, path, zero) {
			t.Errorf("%s: `%s` is rejected, so the key needs an **Allowed:** line", at, zero)
		}
	}
}

// checkSummaryTables checks that each top-level section with keys below it
// has one table that lists them in order, and that each row repeats the type,
// the default and the allowed line of its key.
func checkSummaryTables(t *testing.T, page string, documented map[string]referenceEntry) {
	t.Helper()
	var section string     // the top-level key of the current section
	var want, got []string // the rows of its table
	finish := func() {
		if section == "" {
			return
		}
		if len(want) > 0 && len(got) == 0 {
			t.Errorf("%s: section %s needs a table that starts with %s", referencePage, section, tableHeader)
		}
		for i := 0; i < len(want) || i < len(got); i++ {
			switch {
			case i >= len(got):
				t.Errorf("%s: the table of %s lacks the row %s", referencePage, section, want[i])
			case i >= len(want):
				t.Errorf("%s: the table of %s has a row without a key below it: %s", referencePage, section, got[i])
			case got[i] != want[i]:
				t.Errorf("%s: the table of %s has the row\n\t%s\nwant\n\t%s", referencePage, section, got[i], want[i])
			}
		}
	}
	lines := strings.Split(page, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if _, marker, length, _, ok := openingFence(line); ok {
			for i++; i < len(lines) && !closingFence(lines[i], marker, length); i++ {
			}
			continue
		}
		if strings.HasPrefix(line, "## ") {
			finish()
			section, want, got = "", nil, nil
		}
		if line == tableHeader && section != "" {
			for i += 2; i < len(lines) && strings.HasPrefix(lines[i], "|"); i++ {
				got = append(got, lines[i])
			}
			i--
			continue
		}
		m := keyHeadingRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		path := m[1]
		if strings.HasPrefix(line, "## ") {
			section = path
			continue
		}
		entry, ok := documented[path]
		if !ok || section == "" {
			continue
		}
		anchor := strings.Trim(anchorRE.ReplaceAllString(path, "-"), "-")
		want = append(want, fmt.Sprintf("| [`%s`](#%s) | %s | %s | %s |", path, anchor, entry.typ, entry.summary, entry.allowed))
	}
	finish()
}

func yamlValue(literal string) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(literal), &doc); err != nil || len(doc.Content) != 1 {
		return nil, fmt.Errorf("not a YAML value: %s", literal)
	}
	return doc.Content[0], nil
}

// referenceVariant returns referenceBase with the key at path removed (nil
// value) or set to value.
func referenceVariant(t *testing.T, path string, value *yaml.Node) []byte {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(referenceBase), &doc); err != nil {
		t.Fatal(err)
	}
	segments := strings.Split(path, ".")
	node := doc.Content[0]
	for i, segment := range segments {
		last := i == len(segments)-1
		k := -1
		for j := 0; j < len(node.Content); j += 2 {
			if node.Content[j].Value == segment {
				k = j
				break
			}
		}
		switch {
		case last && value == nil:
			if k >= 0 {
				node.Content = append(node.Content[:k], node.Content[k+2:]...)
			}
		case last && k >= 0:
			node.Content[k+1] = value
		case last:
			node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: segment}, value)
		case k >= 0:
			node = node.Content[k+1]
		case value == nil:
			return mustMarshal(t, &doc) // nothing to remove
		default:
			child := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: segment}, child)
			node = child
		}
	}
	return mustMarshal(t, &doc)
}

func mustMarshal(t *testing.T, doc *yaml.Node) []byte {
	t.Helper()
	data, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// valueAt returns the loaded value at path.
func valueAt(c *Config, path string) reflect.Value {
	v := reflect.ValueOf(c).Elem()
	for segment := range strings.SplitSeq(path, ".") {
		for i := range v.NumField() {
			if f := v.Type().Field(i); f.IsExported() && yamlName(f) == segment {
				v = v.Field(i)
				break
			}
		}
	}
	return v
}
