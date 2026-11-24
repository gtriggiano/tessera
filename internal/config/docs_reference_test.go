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
	"time"

	"go.yaml.in/yaml/v3"
)

// The configuration reference documents every key under a heading that is
// exactly its path in backticks, followed by a "**Type:**" line, a
// "**Default:**" line and, when validation narrows the type, an "**Allowed:**"
// line. The default is a YAML literal in backticks, the word required when
// omitting the key is an error at its path, the word none for an optional
// block, which is absent unless the file writes it, or "required when `<key>`
// is `<value>`" when omitting it is an error at its path once the other key
// has that value. Such a key is checked with the other key set to its value. A
// key that takes the value of another when omitted has the default
// "inherited from `<key>`", checked with the other key set to a value that is
// not its default. One of two keys that are each "required unless `<key>` is
// set" must be in the file: each is checked with the other one present, then
// with neither, which must be an error at the key or at its block.
//
// The keys of the items of a list are written with [] after the list, as in
// policy.rules[].name. They are checked in a list of one item, the minimal
// item of referenceItems. The keys of the blocks of a mapping of names are
// written with <name> for the name, as in sets.<name>.type. They are checked
// in a block named referenceName, the minimal block of referenceItems. The
// keys of an optional block are checked with its minimal block of
// referenceBlocks in the file.
//
// An allowed line that starts with one of these forms is checked against
// Parse; any text may follow a form after "; ":
//
//	`a`, `b` or `c`           exactly these values load
//	`1` to `3`                the bounds load, their neighbors do not
//	positive                  zero is rejected
//	zero or positive          zero loads
//	zero or longer than `<key>`
//	                          zero loads, a negative value does not; with
//	                          <key> at its default and at twice its default,
//	                          a duration equal to it does not load, one
//	                          second more does
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
	keyHeadingRE     = regexp.MustCompile("^#{2,6} `([a-z0-9_]+(?:\\[\\])?(?:\\.(?:[a-z0-9_]+|<name>)(?:\\[\\])?)*)`$")
	typeLineRE       = regexp.MustCompile(`^\*\*Type:\*\* (.+)$`)
	defaultLiteralRE = regexp.MustCompile("^\\*\\*Default:\\*\\* `([^`]+)`$")
	defaultWhenRE    = regexp.MustCompile("^\\*\\*Default:\\*\\* required when `([a-z0-9_.]+)` is `([^`]+)`$")
	inheritedRE      = regexp.MustCompile("^\\*\\*Default:\\*\\* inherited from `([a-z0-9_.]+)`$")
	unlessRE         = regexp.MustCompile("^\\*\\*Default:\\*\\* required unless `([a-z0-9_.<>]+)` is set$")
	itemIndexRE      = regexp.MustCompile(`\[[0-9]+\]`)
	allowedLineRE    = regexp.MustCompile(`^\*\*Allowed:\*\* (\S.*)$`)
	allowedEnumRE    = regexp.MustCompile("^`[^`]+`(?:(?:, | or )`[^`]+`)+$")
	allowedRangeRE   = regexp.MustCompile("^`(-?[0-9]+)` to `(-?[0-9]+)`$")
	allowedLongerRE  = regexp.MustCompile("^zero or longer than `([a-z0-9_.<>]+)`$")
	codeRE           = regexp.MustCompile("`([^`]+)`")
	anchorRE         = regexp.MustCompile(`[^a-z0-9]+`)
)

const (
	tableHeader = "| Key | Type | Default | Allowed |"
	// referenceBase is the smallest valid configuration: each key is checked
	// by adding it to this one, or by removing it.
	referenceBase = "policy:\n  default_action: deny\n"
)

// referenceItems is the smallest valid item of each list, which the keys of
// its items are checked in, and the smallest valid block of each mapping of
// names, named referenceName.
var referenceItems = map[string]string{
	"policy.rules": "{name: reference-rule, action: deny}",
	"sets":         "{type: string, file: reference.txt}",
}

// referenceName is the name of the block of a mapping of names that its keys
// are checked in.
const referenceName = "reference_name"

// referenceBlocks is the smallest valid content of each optional block,
// which its keys are checked in.
var referenceBlocks = map[string]string{
	"enrichment.geo": "{database: reference.mmdb}",
	"enrichment.asn": "{database: reference.mmdb}",
	// No key is required: the block is empty.
	"enrichment.user_agent": "{}",
}

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
	absent   bool      // an optional block, absent by default
	when     condition // the value of another key that makes it required
	from     string    // the key that it is inherited from
	unless   string    // the key whose presence makes it optional
	block    string    // the optional block that holds the key
}

// base is the configuration that the key is checked in: referenceBase, with
// the minimal content of the optional block that holds the key, and the key
// that makes it required set to its value.
func (e referenceEntry) base(t *testing.T) []byte {
	t.Helper()
	base := []byte(referenceBase)
	if e.block != "" {
		base = referenceVariant(t, base, e.block, referenceBlock(t, e.block))
	}
	if e.when == (condition{}) {
		return base
	}
	return referenceVariant(t, base, e.when.path, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: e.when.value})
}

// referenceBlock returns the minimal content of the optional block path.
func referenceBlock(t *testing.T, path string) *yaml.Node {
	t.Helper()
	block, err := yamlValue(referenceBlocks[path])
	if err != nil {
		t.Fatalf("%s: no minimal block in referenceBlocks: %v", path, err)
	}
	block.Style = 0
	return block
}

// blockOf returns the optional block that holds the key path, or "".
func blockOf(path string) string {
	block := ""
	for b := range referenceBlocks {
		if strings.HasPrefix(path, b+".") && len(b) > len(block) {
			block = b
		}
	}
	return block
}

func TestDocsConfigReference(t *testing.T) {
	data, err := os.ReadFile(referencePage)
	if err != nil {
		t.Fatal(err)
	}
	documented := parseReferencePage(t, string(data))
	for path, entry := range documented {
		entry.block = blockOf(path)
		documented[path] = entry
	}

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
		if entry.when != jsonSchemaNotes[key.path].requiredWhen {
			t.Errorf("%s:%d: %s: documented as required when %v, but the JSON Schema (jsonSchemaNotes) has %v", referencePage, entry.line, key.path, entry.when, jsonSchemaNotes[key.path].requiredWhen)
		}
		if entry.from != jsonSchemaNotes[key.path].inherited {
			t.Errorf("%s:%d: %s: documented as inherited from %q, but the JSON Schema (jsonSchemaNotes) has %q", referencePage, entry.line, key.path, entry.from, jsonSchemaNotes[key.path].inherited)
		}
		if entry.unless != jsonSchemaNotes[key.path].requiredUnless {
			t.Errorf("%s:%d: %s: documented as required unless %q is set, but the JSON Schema (jsonSchemaNotes) has %q", referencePage, entry.line, key.path, entry.unless, jsonSchemaNotes[key.path].requiredUnless)
		}
		switch {
		case entry.required || entry.when != (condition{}):
			checkRequired(t, key.path, entry)
		case entry.absent:
			checkAbsent(t, key, entry)
		case entry.unless != "":
			checkRequiredUnless(t, key, entry, documented[entry.unless])
		case entry.from != "":
			checkInherited(t, key, entry, documented[entry.from])
		default:
			checkDefault(t, key.path, entry)
		}
		checkAllowed(t, key, entry, documented)
	}
	// The page lists the keys in the order of the configuration types,
	// which config print, the JSON Schema and the examples follow.
	previous := 0
	for _, key := range keys {
		entry, ok := documented[key.path]
		if !ok {
			continue
		}
		if entry.line < previous {
			t.Errorf("%s:%d: %s comes before a key that the configuration types list before it", referencePage, entry.line, key.path)
		}
		previous = entry.line
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
		switch f.Type.Kind() {
		case reflect.Slice:
			collectSchemaKeys(f.Type.Elem(), path+"[]", keys)
		case reflect.Map:
			collectSchemaKeys(f.Type.Elem(), path+".<name>", keys)
		case reflect.Pointer:
			collectSchemaKeys(f.Type.Elem(), path, keys)
		default:
			collectSchemaKeys(f.Type, path, keys)
		}
	}
}

// typeName is the page's name for a Go type. A type without a name here is
// reported as its Go type, so a new kind of key needs a deliberate name.
func typeName(t reflect.Type) string {
	switch t {
	case durationType:
		return "duration"
	case expressionType:
		return "expression"
	}
	switch t.Kind() {
	case reflect.Slice:
		switch {
		case t.Elem().Kind() == reflect.Struct:
			return "list"
		case t.Elem() == setValueType:
			return "list of values"
		case t.Elem().Kind() == reflect.String:
			return "list of strings"
		}
	case reflect.Map:
		switch {
		case t.Key().Kind() == reflect.String && t.Elem() == expressionType:
			return "mapping of expressions"
		case t.Key().Kind() == reflect.String && t.Elem().Kind() == reflect.String:
			return "mapping of strings"
		case t.Key().Kind() == reflect.String && t.Elem().Kind() == reflect.Struct:
			return "mapping of mappings"
		}
	case reflect.String:
		return "string"
	case reflect.Int:
		return "integer"
	case reflect.Struct:
		return "mapping"
	case reflect.Pointer:
		if t.Elem().Kind() == reflect.Struct {
			return "mapping"
		}
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
		case body[1] == "**Default:** none":
			entry.absent, entry.summary = true, "none"
		case defaultWhenRE.MatchString(body[1]):
			m := defaultWhenRE.FindStringSubmatch(body[1])
			entry.when, entry.summary = condition{m[1], m[2]}, strings.TrimPrefix(body[1], "**Default:** ")
		case inheritedRE.MatchString(body[1]):
			entry.from, entry.summary = inheritedRE.FindStringSubmatch(body[1])[1], strings.TrimPrefix(body[1], "**Default:** ")
		case unlessRE.MatchString(body[1]):
			entry.unless, entry.summary = unlessRE.FindStringSubmatch(body[1])[1], strings.TrimPrefix(body[1], "**Default:** ")
		default:
			t.Errorf("%s:%d: %s: the line after **Type:** must be **Default:** followed by `<yaml literal>`, required, none, required when `<key>` is `<value>`, required unless `<key>` is set, or inherited from `<key>`", referencePage, line, path)
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
	omitted, err := Parse(referenceVariant(t, entry.base(t), path, nil), nil)
	if err != nil {
		t.Errorf("%s:%d: %s has a default, but omitting it is an error: %v", referencePage, entry.line, path, err)
		return
	}
	explicit, err := Parse(referenceVariant(t, entry.base(t), path, literal), nil)
	if err != nil {
		t.Errorf("%s:%d: %s: documented default `%s` is rejected: %v", referencePage, entry.line, path, entry.literal, err)
		return
	}
	got, want := valueAt(omitted, path), valueAt(explicit, path)
	if !reflect.DeepEqual(got.Interface(), want.Interface()) {
		t.Errorf("%s:%d: %s: documented default `%s` loads as %v, but omitting the key gives %v", referencePage, entry.line, path, entry.literal, want, got)
	}
}

// checkAbsent checks an optional block: omitting it leaves it absent, and
// its minimal block loads.
func checkAbsent(t *testing.T, key schemaKey, entry referenceEntry) {
	t.Helper()
	at := fmt.Sprintf("%s:%d: %s", referencePage, entry.line, key.path)
	if key.typ.Kind() != reflect.Pointer || referenceBlocks[key.path] == "" {
		t.Errorf("%s: none is the default of an optional block, with a minimal block in referenceBlocks", at)
		return
	}
	omitted, err := Parse(referenceVariant(t, entry.base(t), key.path, nil), nil)
	if err != nil || !valueAt(omitted, key.path).IsNil() {
		t.Errorf("%s: documented as absent by default, but omitting it gives %v, %v", at, err, err == nil && !valueAt(omitted, key.path).IsNil())
	}
	present, err := Parse(referenceVariant(t, entry.base(t), key.path, referenceBlock(t, key.path)), nil)
	if err != nil || valueAt(present, key.path).IsNil() {
		t.Errorf("%s: its minimal block does not load: %v", at, err)
	}
}

// checkRequired checks that omitting a key documented as required is an error
// at its path, or at a key inside it.
func checkRequired(t *testing.T, path string, entry referenceEntry) {
	t.Helper()
	_, err := Parse(referenceVariant(t, entry.base(t), path, nil), nil)
	var invalid *Error
	if !errors.As(err, &invalid) || listPath(invalid.Path) != path && !strings.HasPrefix(listPath(invalid.Path), path+".") {
		t.Errorf("%s:%d: %s is documented as required, but omitting it does not fail at its path (error: %v)", referencePage, entry.line, path, err)
	}
}

// checkRequiredUnless checks a key that one other key makes optional: the
// configuration loads without it when the other is present, and fails at
// the key or at its block when both are missing.
func checkRequiredUnless(t *testing.T, key schemaKey, entry, other referenceEntry) {
	t.Helper()
	at := fmt.Sprintf("%s:%d: %s", referencePage, entry.line, key.path)
	if other.line == 0 {
		t.Errorf("%s: required unless %s is set, which has no heading", at, entry.unless)
		return
	}
	base := []byte(referenceBase)
	// The other key, with a value of its type when the minimal block lacks
	// it.
	segments := strings.Split(entry.unless, ".")
	if len(segments) < 3 || segments[len(segments)-2] != "<name>" {
		t.Errorf("%s: the test checks required unless only for the keys of a mapping of names", at)
		return
	}
	block, err := yamlValue(referenceItems[strings.Join(segments[:len(segments)-2], ".")])
	if err != nil {
		t.Fatal(err)
	}
	if mappingValue(block, segments[len(segments)-1]) == nil {
		literal := map[string]string{"list of values": "[]", "string": "tessera-unless"}[other.typ]
		value, err := yamlValue(literal)
		if err != nil {
			t.Errorf("%s: the test cannot set the %s %s", at, other.typ, entry.unless)
			return
		}
		base = referenceVariant(t, base, entry.unless, value)
	}
	if _, err := Parse(referenceVariant(t, base, key.path, nil), nil); err != nil {
		t.Errorf("%s: required unless %s is set, but omitting it with %s set fails: %v", at, entry.unless, entry.unless, err)
	}
	_, err = Parse(referenceVariant(t, referenceVariant(t, base, entry.unless, nil), key.path, nil), nil)
	parent := key.path[:strings.LastIndexByte(key.path, '.')]
	var invalid *Error
	if !errors.As(err, &invalid) || listPath(invalid.Path) != key.path && listPath(invalid.Path) != parent {
		t.Errorf("%s: required unless %s is set, but omitting both does not fail at %s or %s (error: %v)", at, entry.unless, key.path, parent, err)
	}
}

// listPath writes the path of an error as the page writes it, with [] for
// the index of an item and <name> for the name of the reference block.
func listPath(path string) string {
	path = strings.ReplaceAll(path, "."+referenceName, ".<name>")
	return itemIndexRE.ReplaceAllString(path, "[]")
}

// checkInherited checks that a key omitted from the file takes the value of
// the key from, set to a value that is not its default: the upper bound of
// its range, or a value of its type.
func checkInherited(t *testing.T, key schemaKey, entry, from referenceEntry) {
	t.Helper()
	var literal string
	switch typeName(key.typ) {
	case "integer":
		m := allowedRangeRE.FindStringSubmatch(strings.Split(from.allowed, "; ")[0])
		if m == nil {
			t.Errorf("%s:%d: %s: inherited from %s, which has no range of values to check it with", referencePage, entry.line, key.path, entry.from)
			return
		}
		literal = m[2]
	case "string":
		literal = "tessera-inherited"
	case "mapping of strings":
		literal = "{x-tessera-inherited: value}"
	default:
		t.Errorf("%s:%d: %s: the test cannot check an inherited %s", referencePage, entry.line, key.path, typeName(key.typ))
		return
	}
	value, err := yamlValue(literal)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Parse(referenceVariant(t, referenceVariant(t, []byte(referenceBase), entry.from, value), key.path, nil), nil)
	if err != nil {
		t.Errorf("%s:%d: %s: %s set to %s, the key omitted: %v", referencePage, entry.line, key.path, entry.from, literal, err)
		return
	}
	if got, want := valueAt(c, key.path), valueAt(c, entry.from); !reflect.DeepEqual(got.Interface(), want.Interface()) {
		t.Errorf("%s:%d: %s: documented as inherited from %s, but it loads as %v when %s is %v", referencePage, entry.line, key.path, entry.from, got, entry.from, want)
	}
}

// loads reports whether base loads with the key set to the YAML literal. The
// value loads too when the only error is the absence of a key that this
// value makes required.
func loads(t *testing.T, base []byte, path, literal string) bool {
	t.Helper()
	value, err := yamlValue(literal)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	_, err = Parse(referenceVariant(t, base, path, value), nil)
	var invalid *Error
	if errors.As(err, &invalid) && jsonSchemaNotes[listPath(invalid.Path)].requiredWhen == (condition{path, value.Value}) {
		return true
	}
	return err == nil
}

// checkAllowed checks the **Allowed:** line of a key against Parse. Numbers
// always have one: every integer and duration is constrained.
func checkAllowed(t *testing.T, key schemaKey, entry referenceEntry, documented map[string]referenceEntry) {
	t.Helper()
	path, typ, base := key.path, typeName(key.typ), entry.base(t)
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
			if !loads(t, base, path, m[1]) {
				t.Errorf("%s: the allowed value `%s` is rejected", at, m[1])
			}
		}
		if loads(t, base, path, "tessera-other-value") {
			t.Errorf("%s: the allowed line lists every value, but `tessera-other-value` loads too", at)
		}
	case allowedRangeRE.MatchString(form):
		m := allowedRangeRE.FindStringSubmatch(form)
		low, _ := strconv.Atoi(m[1])
		high, _ := strconv.Atoi(m[2])
		for _, n := range []int{low, high} {
			if !loads(t, base, path, strconv.Itoa(n)) {
				t.Errorf("%s: the allowed bound %d is rejected", at, n)
			}
		}
		for _, n := range []int{low - 1, high + 1} {
			if loads(t, base, path, strconv.Itoa(n)) {
				t.Errorf("%s: %d is outside the allowed range, but it loads", at, n)
			}
		}
	case form == "positive" || form == "nonempty":
		if zero == "" {
			t.Errorf("%s: %q does not apply to a %s", at, form, typ)
		} else if loads(t, base, path, zero) {
			t.Errorf("%s: documented as %s, but `%s` loads", at, form, zero)
		}
		if one := signedLiteral(key.typ, 1); form == "positive" && (one == "" || !loads(t, base, path, one)) {
			t.Errorf("%s: documented as positive, but `%s` is rejected", at, one)
		}
	case form == "zero or positive":
		one, minusOne := signedLiteral(key.typ, 1), signedLiteral(key.typ, -1)
		if zero == "" || one == "" || !loads(t, base, path, zero) || !loads(t, base, path, one) {
			t.Errorf("%s: documented as zero or positive, but `%s` or `%s` is rejected", at, zero, one)
		} else if loads(t, base, path, minusOne) {
			t.Errorf("%s: documented as zero or positive, but `%s` loads", at, minusOne)
		}
	case allowedLongerRE.MatchString(form):
		other := allowedLongerRE.FindStringSubmatch(form)[1]
		checkLonger(t, at, key, base, other, documented[other])
	default:
		if typ == "integer" || typ == "duration" {
			t.Errorf("%s: a %s needs an **Allowed:** line that starts with positive, zero or positive, a range (`1` to `3`) or a list of values", at, typ)
		}
		if entry.allowed == "" && typ == "string" && !loads(t, base, path, zero) {
			t.Errorf("%s: `%s` is rejected, so the key needs an **Allowed:** line", at, zero)
		}
	}
}

// checkLonger checks a duration documented as zero or longer than the key
// other: zero loads and -1s does not; with other at its default, then at
// twice its default, a value equal to other does not load, and one second
// more does.
func checkLonger(t *testing.T, at string, key schemaKey, base []byte, other string, entry referenceEntry) {
	t.Helper()
	def, err := time.ParseDuration(entry.literal)
	if key.typ != durationType || err != nil || def <= 0 {
		t.Errorf("%s: zero or longer than %s needs a duration, and a key with a positive default duration", at, other)
		return
	}
	if !loads(t, base, key.path, "0s") || loads(t, base, key.path, "-1s") {
		t.Errorf("%s: documented as zero or longer than %s, but `0s` is rejected or `-1s` loads", at, other)
	}
	for _, v := range []time.Duration{def, 2 * def} {
		variant := base
		if v != def {
			variant = referenceVariant(t, base, other, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v.String()})
		}
		if loads(t, variant, key.path, v.String()) {
			t.Errorf("%s: documented as longer than %s, but `%s` loads with %s at %s", at, other, v, other, v)
		}
		if longer := v + time.Second; !loads(t, variant, key.path, longer.String()) {
			t.Errorf("%s: documented as longer than %s, but `%s` is rejected with %s at %s", at, other, longer, other, v)
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

// referenceVariant returns base with the key at path removed (nil value) or
// set to value.
func referenceVariant(t *testing.T, base []byte, path string, value *yaml.Node) []byte {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal(base, &doc); err != nil {
		t.Fatal(err)
	}
	segments := strings.Split(path, ".")
	node := doc.Content[0]
	for i, segment := range segments {
		last := i == len(segments)-1
		if segment == "<name>" && !last {
			// The reference block, created as the minimal block.
			k := -1
			for j := 0; j < len(node.Content); j += 2 {
				if node.Content[j].Value == referenceName {
					k = j
					break
				}
			}
			if k < 0 {
				item, err := yamlValue(referenceItems[strings.Join(segments[:i], ".")])
				if err != nil {
					t.Fatalf("%s: no block in referenceItems: %v", path, err)
				}
				item.Style = 0
				node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: referenceName}, item)
				k = len(node.Content) - 2
			}
			node = node.Content[k+1]
			continue
		}
		if list, ok := strings.CutSuffix(segment, "[]"); ok && !last {
			// The first item of the list, created as its minimal item.
			k := -1
			for j := 0; j < len(node.Content); j += 2 {
				if node.Content[j].Value == list {
					k = j
					break
				}
			}
			if k < 0 {
				item, err := yamlValue(referenceItems[strings.Join(append(segments[:i:i], list), ".")])
				if err != nil {
					t.Fatalf("%s: no item in referenceItems: %v", path, err)
				}
				item.Style = 0
				node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: list}, &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{item}})
				k = len(node.Content) - 2
			}
			node = node.Content[k+1].Content[0]
			continue
		}
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
		case value == nil && segments[i+1] != "<name>":
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

// valueAt returns the loaded value at path. An optional block on the way is
// followed when it is present.
func valueAt(c *Config, path string) reflect.Value {
	v := reflect.ValueOf(c).Elem()
	for segment := range strings.SplitSeq(path, ".") {
		if v.Kind() == reflect.Pointer && !v.IsNil() {
			v = v.Elem()
		}
		if segment == "<name>" {
			v = v.MapIndex(reflect.ValueOf(referenceName))
			continue
		}
		name, item := strings.CutSuffix(segment, "[]")
		for i := range v.NumField() {
			if f := v.Type().Field(i); f.IsExported() && yamlName(f) == name {
				v = v.Field(i)
				break
			}
		}
		if item {
			v = v.Index(0)
		}
	}
	return v
}
