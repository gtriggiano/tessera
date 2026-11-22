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
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.yaml.in/yaml/v3"
)

// MaxFileBytes is the largest configuration file.
const MaxFileBytes = 1 << 20

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

// err returns an Error for path, at the position of path or, when path is
// not in the file, of its nearest parent that is.
func (c *Config) err(path, problem string) error {
	l, p := c.nodes[path], path
	for l.line == 0 && strings.Contains(p, ".") {
		p = p[:strings.LastIndex(p, ".")]
		l = c.nodes[p]
	}
	if path == "" {
		path = "configuration"
	}
	return &Error{Path: path, Line: l.line, Column: l.column, Problem: problem}
}

// Load reads and checks the configuration file at path. It reads nothing but
// that file, and makes the relative paths in it relative to its directory.
func Load(path string, lookup LookupEnv) (*Config, error) {
	data, err := readFile(path)
	if err != nil {
		return nil, err
	}
	c, err := Parse(data, lookup)
	if err != nil {
		return nil, err
	}
	if socket, ok := strings.CutPrefix(c.Server.Listen, UnixPrefix); ok && !filepath.IsAbs(socket) {
		dir, err := filepath.Abs(filepath.Dir(path))
		if err != nil {
			return nil, errors.New("configuration file: cannot resolve its directory")
		}
		c.Server.Listen = UnixPrefix + filepath.Join(dir, socket)
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
	durationType = reflect.TypeFor[time.Duration]()
	integerRE    = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)
	envNameRE    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
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

// decode stores the YAML node n, at the key path, into v.
func (c *Config) decode(n *yaml.Node, v reflect.Value, path string, lookup LookupEnv) error {
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
	switch v.Kind() {
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
	case reflect.String:
		if n.Kind != yaml.ScalarNode || n.Tag != "!!str" {
			return c.err(path, "expected a string")
		}
		s, err := c.expand(n.Value, path, lookup)
		if err != nil {
			return err
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

// expand replaces each ${NAME} and ${NAME:-default} in s, the value at path,
// and $${ with a literal ${. Any other $ is kept.
func (c *Config) expand(s, path string, lookup LookupEnv) (string, error) {
	var out strings.Builder
	for i := 0; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], "$${"):
			out.WriteString("${")
			i += 3
		case strings.HasPrefix(s[i:], "${"):
			end := strings.IndexByte(s[i+2:], '}')
			if end < 0 {
				return "", c.err(path, "unterminated ${ substitution")
			}
			name, fallback, hasFallback := strings.Cut(s[i+2:i+2+end], ":-")
			if !envNameRE.MatchString(name) {
				return "", c.err(path, "invalid environment variable name in a ${ substitution")
			}
			value, ok := lookup(name)
			if hasFallback && value == "" {
				value, ok = fallback, true
			}
			if !ok {
				return "", c.err(path, "environment variable "+name+" is not set")
			}
			c.tainted[path] = true
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
		if n.Kind == yaml.MappingNode {
			for i := 0; i < len(n.Content); i += 2 {
				redact(n.Content[i+1], childPath(path, n.Content[i].Value))
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
