package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.yaml.in/yaml/v3"
)

// MaxFileBytes is the largest configuration file.
const MaxFileBytes = 1 << 20

// MaxSocketPathBytes is the longest path of a Unix socket that the operating
// system accepts: 107 bytes on Linux, 103 on macOS.
const MaxSocketPathBytes = len(syscall.RawSockaddrUnix{}.Path) - 1

type location struct{ line, column int }

// Error is an invalid configuration. It names the key and, when the key is in
// the file, its position. It never repeats a configured value.
type Error struct {
	Path         string
	Line, Column int
	Problem      string
}

func (e *Error) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("%s (line %d, column %d): %s", e.Path, e.Line, e.Column, e.Problem)
	}
	return e.Path + ": " + e.Problem
}

// Warning is a problem of a valid configuration: tessera runs with it. It
// names the key and its position, and never repeats a configured value.
type Warning struct {
	Path         string
	Line, Column int
	Problem      string
}

func (w Warning) String() string {
	return (&Error{Path: w.Path, Line: w.Line, Column: w.Column, Problem: w.Problem}).Error()
}

// Warnings returns the warnings of the configuration, in the order of the
// file.
func (c *Config) Warnings() []Warning { return c.warnings }

// ErrorAt returns the *Error of problem for the key at path, such as
// sets.partners.file, with its position in the file.
func (c *Config) ErrorAt(path, problem string) error { return c.err(path, problem) }

// err returns an Error for path, at the position of path or, when path is
// not in the file, of its nearest parent that is.
func (c *Config) err(path, problem string) error {
	l, p := c.nodes[path], path
	for l.line == 0 && strings.ContainsAny(p, ".[") {
		p = p[:strings.LastIndexAny(p, ".[")]
		l = c.nodes[p]
	}
	return c.errAt(path, l, problem)
}

// errAt returns an Error for path at the position l.
func (c *Config) errAt(path string, l location, problem string) error {
	if path == "" {
		path = "configuration"
	}
	return &Error{Path: path, Line: l.line, Column: l.column, Problem: problem}
}

// Load reads and checks the configuration file at path. It reads nothing but
// that file, not even the files of the sets or the databases, and makes the
// relative paths in it relative to its directory.
func Load(path string, lookup LookupEnv) (*Config, error) {
	data, err := readFile(path)
	if err != nil {
		return nil, err
	}
	c, err := Parse(data, lookup)
	if err != nil {
		return nil, err
	}
	absolute := func(p string) (string, error) {
		if filepath.IsAbs(p) {
			return p, nil
		}
		dir, err := filepath.Abs(filepath.Dir(path))
		if err != nil {
			return "", errors.New("configuration file: cannot resolve its directory")
		}
		return filepath.Join(dir, p), nil
	}
	for name, s := range c.Sets {
		if s.File != "" {
			if s.File, err = absolute(s.File); err != nil {
				return nil, err
			}
			c.Sets[name] = s
		}
	}
	for _, k := range c.DatabaseKinds() {
		db := c.Enrichment.Database(k)
		if db.Database, err = absolute(db.Database); err != nil {
			return nil, err
		}
	}
	if socket, ok := strings.CutPrefix(c.Server.Listen, UnixPrefix); ok {
		if socket, err = absolute(socket); err != nil {
			return nil, err
		}
		c.Server.Listen = UnixPrefix + socket
		if len(socket) > MaxSocketPathBytes {
			return nil, c.err("server.listen", fmt.Sprintf("the socket path, made absolute, is longer than %d bytes", MaxSocketPathBytes))
		}
	}
	return c, nil
}

// readFile reads a regular file. It opens it without blocking and refuses
// anything else, such as a FIFO, whose reads could wait for ever.
func readFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, errors.New("configuration file: not found")
	case errors.Is(err, fs.ErrPermission):
		return nil, errors.New("configuration file: permission denied")
	case err != nil:
		return nil, errors.New("configuration file: cannot open")
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("configuration file: not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return nil, errors.New("configuration file: cannot read")
	}
	return data, nil
}

// Parse checks a configuration and applies its defaults. A nil lookup reports
// every environment variable as unset. Relative paths stay as written.
func Parse(data []byte, lookup LookupEnv) (*Config, error) {
	if len(data) > MaxFileBytes {
		return nil, errors.New("configuration: larger than 1 MiB")
	}
	if lookup == nil {
		lookup = func(string) (string, bool) { return "", false }
	}
	d := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := d.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("configuration: the file is empty")
		}
		return nil, syntaxError(err)
	}
	var extra yaml.Node
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, syntaxError(err)
		}
		return nil, errors.New("configuration: more than one YAML document")
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("configuration: the root is not a mapping")
	}
	c := defaults()
	c.lines = strings.Split(string(data), "\n")
	if err := c.decode(doc.Content[0], reflect.ValueOf(c).Elem(), "", lookup); err != nil {
		return nil, err
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

var lineRE = regexp.MustCompile(`line ([0-9]+)`)

// syntaxError keeps only the line of a YAML parser error, whose message can
// quote the file.
func syntaxError(err error) error {
	if m := lineRE.FindStringSubmatch(err.Error()); m != nil {
		n, _ := strconv.Atoi(m[1])
		return &Error{Path: "configuration", Line: n, Column: 1, Problem: "invalid YAML syntax"}
	}
	return errors.New("configuration: invalid YAML syntax")
}

var (
	durationType   = reflect.TypeFor[time.Duration]()
	expressionType = reflect.TypeFor[Expression]()
	setValueType   = reflect.TypeFor[SetValue]()
	integerRE      = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)
	envNameRE      = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// keyRE matches the keys that an error can name: a key that does not
	// match is arbitrary text, and could be a misplaced value.
	keyRE = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
)

func childPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func itemPath(path string, i int) string {
	return path + "[" + strconv.Itoa(i) + "]"
}

// defaulter is a list item whose keys have defaults.
type defaulter interface{ setDefaults() }

// decode stores the YAML node n, at the key path, into v.
func (c *Config) decode(n *yaml.Node, v reflect.Value, path string, lookup LookupEnv) error {
	keyAt := c.nodes[path]
	c.nodes[path] = location{n.Line, n.Column}
	if n.Kind == yaml.AliasNode {
		return c.err(path, "YAML aliases are not allowed")
	}
	switch n.Tag {
	case "!!null":
		return c.err(path, "null is not allowed; omit the key to use its default")
	case "!!map", "!!seq", "!!str", "!!int", "!!bool", "!!float", "!!timestamp", "!!binary":
	default:
		return c.err(path, "custom YAML tags are not allowed")
	}
	if v.Type() == durationType {
		if n.Kind != yaml.ScalarNode || n.Tag != "!!str" {
			return c.err(path, "expected a duration with units, such as 5s")
		}
		s, err := c.expand(n.Value, path, lookup)
		if err != nil {
			return err
		}
		d, err := time.ParseDuration(s)
		if err != nil || strings.Trim(s, "+-") == "0" {
			return c.err(path, "expected a duration with units, such as 5s")
		}
		v.SetInt(int64(d))
		return nil
	}
	if v.Type() == setValueType {
		// An integer is kept as the file writes it, for an asn set to
		// check; validate rejects it in a set of another type.
		if n.Kind == yaml.ScalarNode && n.Tag == "!!int" {
			c.integers[path] = true
			v.SetString(n.Value)
			return nil
		}
		if n.Kind != yaml.ScalarNode || n.Tag != "!!str" {
			return c.err(path, "expected a string, or, in an asn set, a decimal integer without leading zeros, such as 64496")
		}
		s, err := c.expand(n.Value, path, lookup)
		if err != nil {
			return err
		}
		v.SetString(s)
		return nil
	}
	switch v.Kind() {
	case reflect.Pointer:
		// An optional block: absent unless the file writes it, with the
		// defaults of its keys when it does.
		if v.Type().Elem().Kind() != reflect.Struct {
			return c.err(path, "unsupported configuration type")
		}
		block := reflect.New(v.Type().Elem())
		if d, ok := block.Interface().(defaulter); ok {
			d.setDefaults()
		}
		if err := c.decode(n, block.Elem(), path, lookup); err != nil {
			return err
		}
		v.Set(block)
		// The block is where its key is.
		if keyAt.line > 0 {
			c.nodes[path] = keyAt
		}
	case reflect.Struct:
		if n.Kind != yaml.MappingNode || n.Tag != "!!map" {
			return c.err(path, "expected a mapping")
		}
		fields := map[string]int{}
		for i := range v.NumField() {
			if f := v.Type().Field(i); f.IsExported() {
				fields[strings.Split(f.Tag.Get("yaml"), ",")[0]] = i
			}
		}
		seen := map[string]bool{}
		for i := 0; i < len(n.Content); i += 2 {
			k, value := n.Content[i], n.Content[i+1]
			if k.Kind != yaml.ScalarNode || k.Tag != "!!str" || k.Value == "<<" {
				c.nodes[path] = location{k.Line, k.Column}
				return c.err(path, "keys must be plain strings; merge keys are not allowed")
			}
			p := childPath(path, k.Value)
			if !keyRE.MatchString(k.Value) {
				p = childPath(path, "<unknown>")
			}
			c.nodes[p] = location{k.Line, k.Column}
			if seen[k.Value] {
				return c.err(p, "duplicate key")
			}
			seen[k.Value] = true
			fi, ok := fields[k.Value]
			if !ok {
				return c.err(p, "unknown key")
			}
			if err := c.decode(value, v.Field(fi), p, lookup); err != nil {
				return err
			}
		}
	case reflect.Slice:
		if n.Kind != yaml.SequenceNode || n.Tag != "!!seq" {
			return c.err(path, "expected a list")
		}
		items := reflect.MakeSlice(v.Type(), len(n.Content), len(n.Content))
		for i, item := range n.Content {
			elem := items.Index(i)
			if d, ok := elem.Addr().Interface().(defaulter); ok {
				d.setDefaults()
			}
			if err := c.decode(item, elem, itemPath(path, i), lookup); err != nil {
				return err
			}
		}
		v.Set(items)
	case reflect.Map:
		if v.Type().Key().Kind() == reflect.String && v.Type().Elem().Kind() == reflect.Struct {
			return c.decodeNamed(n, v, path, lookup)
		}
		if v.Type().Key().Kind() != reflect.String || v.Type().Elem().Kind() != reflect.String {
			return c.err(path, "unsupported configuration type")
		}
		if n.Kind != yaml.MappingNode || n.Tag != "!!map" {
			return c.err(path, "expected a mapping")
		}
		// The keys are values, not names: an error about one names the
		// mapping, at the position of the key.
		m := reflect.MakeMapWithSize(v.Type(), len(n.Content)/2)
		seen := map[string]bool{}
		for i := 0; i < len(n.Content); i += 2 {
			k, value := n.Content[i], n.Content[i+1]
			at, valueAt := location{k.Line, k.Column}, location{value.Line, value.Column}
			switch {
			case k.Kind != yaml.ScalarNode || k.Tag != "!!str" || k.Value == "<<":
				return c.errAt(path, at, "keys must be plain strings; merge keys are not allowed")
			case seen[k.Value]:
				return c.errAt(path, at, "duplicate key")
			case value.Kind == yaml.AliasNode:
				return c.errAt(path, valueAt, "YAML aliases are not allowed")
			case value.Tag == "!!null":
				return c.errAt(path, valueAt, "null is not allowed")
			case value.Kind != yaml.ScalarNode || value.Tag != "!!str":
				return c.errAt(path, valueAt, "expected a string")
			}
			seen[k.Value] = true
			s, err := c.expandAt(value.Value, path, valueAt, childPath(path, k.Value), lookup)
			if err != nil {
				return err
			}
			c.keys[path] = append(c.keys[path], mapKey{name: k.Value, at: at})
			if v.Type().Elem() == expressionType {
				// Where the file writes the expression, which places an
				// error in it.
				c.nodes[childPath(path, k.Value)] = valueAt
				c.styles[childPath(path, k.Value)] = value.Style
			}
			m.SetMapIndex(reflect.ValueOf(k.Value), reflect.ValueOf(s).Convert(v.Type().Elem()))
		}
		v.Set(m)
	case reflect.String:
		if n.Kind != yaml.ScalarNode || n.Tag != "!!str" {
			return c.err(path, "expected a string")
		}
		s, err := c.expand(n.Value, path, lookup)
		if err != nil {
			return err
		}
		if v.Type() == expressionType {
			c.styles[path] = n.Style
		}
		v.SetString(s)
	case reflect.Int:
		if n.Kind != yaml.ScalarNode || n.Tag != "!!int" || !integerRE.MatchString(n.Value) {
			return c.err(path, "expected a decimal integer")
		}
		x, err := strconv.ParseInt(n.Value, 10, 64)
		if err != nil || v.OverflowInt(x) {
			return c.err(path, "integer out of range")
		}
		v.SetInt(x)
	default:
		return c.err(path, "unsupported configuration type")
	}
	return nil
}

// setNameRE is the form of a set name: it is a field in expressions and a
// label value of metrics.
var setNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// celReserved are the words that the expression language reserves: a set of
// such a name could not be read as sets.<name>.
var celReserved = []string{"false", "in", "null", "true", "as", "break", "const", "continue", "else", "for", "function", "if", "import", "let", "loop", "package", "namespace", "return", "var", "void", "while"}

// decodeNamed stores n, a mapping from names to blocks, such as sets, into
// the map v. Each name is a key of the paths below it; a name that is not
// valid is reported at its position, under the path of the mapping, since
// it could be any text.
func (c *Config) decodeNamed(n *yaml.Node, v reflect.Value, path string, lookup LookupEnv) error {
	if n.Kind != yaml.MappingNode || n.Tag != "!!map" {
		return c.err(path, "expected a mapping")
	}
	m := reflect.MakeMapWithSize(v.Type(), len(n.Content)/2)
	seen := map[string]bool{}
	for i := 0; i < len(n.Content); i += 2 {
		k, value := n.Content[i], n.Content[i+1]
		at := location{k.Line, k.Column}
		switch {
		case k.Kind != yaml.ScalarNode || k.Tag != "!!str" || k.Value == "<<":
			return c.errAt(path, at, "keys must be plain strings; merge keys are not allowed")
		case len(k.Value) > MaxSetNameLength || !setNameRE.MatchString(k.Value) || slices.Contains(celReserved, k.Value):
			return c.errAt(path, at, "expected a name of lowercase letters, digits and underscores that starts with a letter, at most "+strconv.Itoa(MaxSetNameLength)+" characters and not a reserved word of the expression language")
		}
		p := childPath(path, k.Value)
		if seen[k.Value] {
			return c.errAt(p, at, "duplicate key")
		}
		seen[k.Value] = true
		item := reflect.New(v.Type().Elem()).Elem()
		if d, ok := item.Addr().Interface().(defaulter); ok {
			d.setDefaults()
		}
		if err := c.decode(value, item, p, lookup); err != nil {
			return err
		}
		// The block is where its name is.
		c.nodes[p] = at
		c.keys[path] = append(c.keys[path], mapKey{name: k.Value, at: at})
		m.SetMapIndex(reflect.ValueOf(k.Value), item)
	}
	v.Set(m)
	return nil
}

// expand replaces each ${NAME} and ${NAME:-default} in s, the value at path,
// and $${ with a literal ${. Any other $ is kept.
func (c *Config) expand(s, path string, lookup LookupEnv) (string, error) {
	return c.expandAt(s, path, c.nodes[path], path, lookup)
}

// expandAt is expand for a value whose errors are reported at path and at,
// and whose substitutions mark taint as tainted.
func (c *Config) expandAt(s, path string, at location, taint string, lookup LookupEnv) (string, error) {
	var out strings.Builder
	for i := 0; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], "$${"):
			out.WriteString("${")
			i += 3
		case strings.HasPrefix(s[i:], "${"):
			end := strings.IndexByte(s[i+2:], '}')
			if end < 0 {
				return "", c.errAt(path, at, "unterminated ${ substitution")
			}
			name, fallback, hasFallback := strings.Cut(s[i+2:i+2+end], ":-")
			if !envNameRE.MatchString(name) {
				return "", c.errAt(path, at, "invalid environment variable name in a ${ substitution")
			}
			value, ok := lookup(name)
			if hasFallback && value == "" {
				value, ok = fallback, true
			}
			if !ok {
				return "", c.errAt(path, at, "environment variable "+name+" is not set")
			}
			c.tainted[taint] = true
			out.WriteString(value)
			i += end + 3
		default:
			out.WriteByte(s[i])
			i++
		}
	}
	return out.String(), nil
}

func (c *Config) present(path string) bool {
	_, ok := c.nodes[path]
	return ok
}

// Redacted is the configuration as YAML, with every value that contains a
// ${...} substitution replaced by [REDACTED], and every other ${ written as
// $${, so that the output reads back to the same values. Durations are
// printed in the form 1h0m0s.
func (c *Config) Redacted() ([]byte, error) {
	var root yaml.Node
	if err := root.Encode(c); err != nil {
		return nil, errors.New("configuration: cannot encode")
	}
	var redact func(n *yaml.Node, path string)
	redact = func(n *yaml.Node, path string) {
		if c.tainted[path] {
			*n = yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "[REDACTED]"}
			return
		}
		if n.Kind == yaml.ScalarNode && n.Tag == "!!str" {
			n.Value = strings.ReplaceAll(n.Value, "${", "$${")
		}
		switch n.Kind {
		case yaml.MappingNode:
			for i := 0; i < len(n.Content); i += 2 {
				redact(n.Content[i+1], childPath(path, n.Content[i].Value))
			}
		case yaml.SequenceNode:
			for i, item := range n.Content {
				redact(item, itemPath(path, i))
			}
		}
	}
	redact(&root, "")
	var out bytes.Buffer
	e := yaml.NewEncoder(&out)
	e.SetIndent(2)
	if err := e.Encode(&root); err != nil {
		return nil, errors.New("configuration: cannot encode")
	}
	if err := e.Close(); err != nil {
		return nil, errors.New("configuration: cannot encode")
	}
	return out.Bytes(), nil
}
