package expr

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types/ref"

	"github.com/gtriggiano/tessera/internal/request"
)

func newEnv(t testing.TB, opts Options) *Env {
	t.Helper()
	env, err := NewEnv(opts)
	if err != nil {
		t.Fatalf("NewEnv: %v", err)
	}
	return env
}

func mustCompile(t testing.TB, env *Env, text string) *Condition {
	t.Helper()
	c, err := env.Compile(text)
	if err != nil {
		t.Fatalf("Compile(%q): %v", text, err)
	}
	return c
}

// compileIssue compiles text and returns its first issue.
func compileIssue(t *testing.T, env *Env, text string) Issue {
	t.Helper()
	_, err := env.Compile(text)
	var ce *CompileError
	if !errors.As(err, &ce) || len(ce.Issues) == 0 {
		t.Fatalf("Compile(%q): got %v, want a *CompileError", text, err)
	}
	return ce.Issues[0]
}

func sampleRequest(clientIP string) *request.Request {
	r := &request.Request{
		ID:     "4f1c2a",
		Scheme: "https",
		Method: "GET",
		Host:   "api.example.com",
		Path:   "/v1/orders/42",
		Query:  "page=2",
		Headers: map[string]string{
			"user-agent": "curl/8.7.1",
			"accept":     "application/json",
			"x-api-key":  "k-123",
		},
	}
	if clientIP != "" {
		r.ClientIP = netip.MustParseAddr(clientIP)
	}
	return r
}

// eval compiles and evaluates text for r.
func eval(t *testing.T, env *Env, text string, r *request.Request) (bool, error) {
	t.Helper()
	return mustCompile(t, env, text).Eval(NewInput(r, nil, nil))
}

func cause(err error) Cause {
	var e *EvalError
	if errors.As(err, &e) {
		return e.Cause
	}
	return ""
}

func TestRequestAttributes(t *testing.T) {
	env := newEnv(t, Options{})
	for _, tc := range []struct {
		expr string
		want bool
	}{
		{`request.host == "api.example.com"`, true},
		{`request.method == "GET" && request.scheme == "https"`, true},
		{`request.path == "/v1/orders/42" && request.query == "page=2"`, true},
		{`request.id == "4f1c2a"`, true},
		{`"x-api-key" in request.headers`, true},
		{`"authorization" in request.headers`, false},
		{`request.headers["accept"] == "application/json"`, true},
		{`request.method in ["GET", "HEAD"]`, true},
		{`request.method in ["POST", "PUT"]`, false},
		{`request.path.startsWith("/v1/") && request.host.endsWith(".example.com")`, true},
		{`request.path.contains("orders")`, true},
		{`request.path.matches("^/v[0-9]+/orders/[0-9]+$")`, true},
		{`request.headers["user-agent"].matches("(?i)^CURL/")`, true},
		{`size(request.headers) == 3 && size(request.path) == 13`, true},
		{`(request.method == "GET" ? request.path : "/") == "/v1/orders/42"`, true},
		{`request.headers.exists(k, k.startsWith("x-"))`, true},
		{`has(request.host)`, true},
	} {
		got, err := eval(t, env, tc.expr, sampleRequest(""))
		if err != nil || got != tc.want {
			t.Errorf("%s = %v, %v; want %v", tc.expr, got, err, tc.want)
		}
	}
	// A missing header is an error, not an empty string.
	_, err := eval(t, env, `request.headers["authorization"] == ""`, sampleRequest(""))
	if cause(err) != CauseExpression {
		t.Errorf("missing header: %v, want an error with cause expression", err)
	}
}

func TestClientIP(t *testing.T) {
	env := newEnv(t, Options{})
	for _, tc := range []struct {
		client, expr string
		want         bool
	}{
		{"192.0.2.3", `client.ip.inCidr("192.0.2.0/24")`, true},
		{"192.0.2.3", `client.ip.inCidr("198.51.100.0/24")`, false},
		{"192.0.2.3", `client.ip.family() == 4`, true},
		{"192.0.2.3", `string(client.ip) == "192.0.2.3"`, true},
		{"192.0.2.10", `client.ip.inCidr("0.0.0.0/0")`, true},
		{"192.0.2.10", `client.ip.inCidr("::/0")`, false},
		{"::ffff:192.0.2.10", `client.ip.inCidr("192.0.2.0/24") && client.ip.family() == 4 && string(client.ip) == "192.0.2.10"`, true},
		{"::ffff:192.0.2.10", `client.ip.inCidr("::/0")`, false},
		{"2001:db8::1", `client.ip.inCidr("2001:db8::/32") && client.ip.family() == 6`, true},
		{"2001:db8::1", `client.ip.inCidr("2001:db8::1/128") && string(client.ip) == "2001:db8::1"`, true},
		{"2001:db8::1", `client.ip.inCidr("192.0.2.0/24")`, false},
		{"2001:db8::1", `client.ip == client.ip`, true},
		{"203.0.113.7", `has(client.ip)`, true},
		{"", `has(client.ip)`, false},
		{"", `!has(client.ip)`, true},
		{"", `has(client.ip) && client.ip.inCidr("192.0.2.0/24")`, false},
	} {
		got, err := eval(t, env, tc.expr, sampleRequest(tc.client))
		if err != nil || got != tc.want {
			t.Errorf("client %q: %s = %v, %v; want %v", tc.client, tc.expr, got, err, tc.want)
		}
	}
	// Without an address, every read of client.ip is an error, never a zero
	// value.
	for _, text := range []string{
		`client.ip.inCidr("192.0.2.0/24")`,
		`client.ip.family() == 4`,
		`string(client.ip) == ""`,
		`client.ip == client.ip`,
		`!client.ip.inCidr("192.0.2.0/24")`,
	} {
		_, err := eval(t, env, text, sampleRequest(""))
		if cause(err) != CauseClientIPMissing || !errors.Is(err, ErrClientIPMissing) {
			t.Errorf("%s without an address: %v, want cause client_ip_missing", text, err)
		}
	}
}

// && and || absorb an error when their other operand decides, whatever its
// side; nothing else does.
func TestErrorAbsorption(t *testing.T) {
	env := newEnv(t, Options{})
	r := sampleRequest("")
	r.Headers["x-count"] = "not-a-number"
	const missing = `client.ip.inCidr("192.0.2.0/24")`
	const conversion = `int(request.headers["x-count"]) > 5`
	const overflow = `size(request.path) + 9223372036854775807 > 0`
	const division = `100 / (size(request.host) - size(request.host)) > 1`
	for _, tc := range []struct {
		expr  string
		value bool
		cause Cause
	}{
		{`true || ` + missing, true, ""},
		{missing + ` || true`, true, ""},
		{`false && ` + missing, false, ""},
		{missing + ` && false`, false, ""},
		{missing + ` || false`, false, CauseClientIPMissing},
		{missing + ` && true`, false, CauseClientIPMissing},
		{`!` + missing, false, CauseClientIPMissing},
		{missing + ` ? true : false`, false, CauseClientIPMissing},
		{`(` + missing + ` || request.host == "api.example.com") && request.method == "GET"`, true, ""},
		{conversion, false, CauseExpression},
		{conversion + ` || true`, true, ""},
		{`false && ` + conversion, false, ""},
		{`!(` + conversion + `)`, false, CauseExpression},
		{overflow, false, CauseExpression},
		{overflow + ` && false`, false, ""},
		{division, false, CauseExpression},
		{division + ` || true`, true, ""},
	} {
		got, err := eval(t, env, tc.expr, r)
		if cause(err) != tc.cause || (err == nil) != (tc.cause == "") || got != tc.value {
			t.Errorf("%s = %v, %v; want %v with cause %q", tc.expr, got, err, tc.value, tc.cause)
		}
	}
}

func TestCompileErrors(t *testing.T) {
	env := newEnv(t, Options{})
	for _, tc := range []struct {
		expr         string
		line, column int
		prefix       string
	}{
		{`request.hots == "x"`, 1, 8, "undefined field 'hots'"},
		{`request.host == 1`, 1, 14, "found no matching overload for '_==_' applied to '(string, int)'"},
		{`request.host`, 1, 1, "the condition must be a bool, not string"},
		{`size(request.path)`, 1, 1, "the condition must be a bool, not int"},
		{`  int(request.headers["x"]) + 1`, 1, 3, "the condition must be a bool, not int"},
		{`requests.host == "x"`, 1, 1, "undeclared reference to 'requests'"},
		{`geo.country.code == "IT"`, 1, 1, "geo is not available: the configuration has no enrichment.geo"},
		{`request.host == "x" || asn.number == 64496`, 1, 24, "asn is not available: the configuration has no enrichment.asn"},
		{`georgia == "x"`, 1, 1, "undeclared reference to 'georgia'"},
		{`request.method in ["GET", 1]`, 1, 27, "expected type 'string' but found 'int'"},
		{"request.method == \"GET\" &&\n    request.hots == \"x\"", 2, 12, "undefined field 'hots'"},
		// Columns count characters: é and 🙂 are one column each.
		{`request.host == "é🙂" && request.hots == "x"`, 1, 32, "undefined field 'hots'"},
		{`request.host == "x" &&`, 1, 23, "syntax error in the expression"},
		{`request.path.matches("[a-")`, 1, 22, "invalid regular expression: missing closing ]"},
		{`matches(request.path, "(a")`, 1, 23, "invalid regular expression: missing closing )"},
		{`client.ip == "192.0.2.10"`, 1, 11, "found no matching overload for '_==_' applied to '(tessera.IP, string)'"},
		{`client.ip.startsWith("192.")`, 1, 21, "found no matching overload for 'startsWith'"},
		{`client.ipp.inCidr("192.0.2.0/24")`, 1, 7, "undefined field 'ipp'"},
		{`client.ip.inCidr(request.host)`, 1, 18, "the argument of inCidr must be a string literal"},
		{`client.ip.inCidr("192.0.2." + "0/24")`, 1, 18, "the argument of inCidr must be a string literal"},
		{`client.ip.inCidr("192.0.2.1/24")`, 1, 18, "invalid network: the network has bits set"},
		{`client.ip.inCidr("192.0.2.0")`, 1, 18, "invalid network: the network must be"},
		{`client.ip.inCidr("192.0.2.0/33")`, 1, 18, "invalid network: the network must be"},
		{`client.ip.inCidr("::ffff:192.0.2.0/120")`, 1, 18, "invalid network: write an IPv4 network"},
		{`client.ip.inCidr(8)`, 1, 17, "found no matching overload for 'inCidr'"},
		{`request.headers.all(k, request.headers.all(j, k == j))`, 1, 43, "comprehension exceeds nesting limit"},
		{``, 1, 1, "the expression is empty"},
		{" \n ", 1, 1, "the expression is empty"},
		{strings.Repeat(" ", MaxLength) + "true", 1, 1, "the expression is longer than 8192 characters"},
	} {
		is := compileIssue(t, env, tc.expr)
		if is.Line != tc.line || is.Column != tc.column || !strings.HasPrefix(is.Message, tc.prefix) {
			t.Errorf("%q: line %d, column %d: %q; want line %d, column %d: %q…", tc.expr, is.Line, is.Column, is.Message, tc.line, tc.column, tc.prefix)
		}
	}
	// A message never repeats a literal of the expression: the parser's
	// quote the text where they stopped, so a syntax error has its position
	// only; a message of the checker that holds a literal is replaced.
	for _, text := range []string{
		`request.host == "s3cr3t" &&`,
		`request.headers["x-api-key"] == "s3cr3t-value`,
		`request.host == "s3cr3t-token" "other-s3cr3t"`,
		`request.host == 's3cr3t' 's3cr3t'`,
		`request.host == b"s3cr3t" + `,
		`has("s3cr3t")`,
		`request.path.matches("s3cr3t[")`,
		`s3cr3t == "s3cr3t"`,
		`request.hosts3cr3t == "s3cr3t"`,
		`{"s3cr3t": 1} == 1`,
		`request.host == "s3cr3t" ? 1 : "s3cr3t"`,
	} {
		_, err := env.Compile(text)
		if err == nil || strings.Contains(err.Error(), "s3cr3t") {
			t.Errorf("%s: %v", text, err)
		}
	}
	if mustCompile(t, env, strings.Repeat(" ", MaxLength-4)+"true") == nil {
		t.Error("an expression of the largest length does not compile")
	}
	if _, err := env.Compile(`request.headers.exists(k, k.startsWith("x-debug-"))`); err != nil {
		t.Errorf("one comprehension: %v", err)
	}
}

func TestAlways(t *testing.T) {
	env := newEnv(t, Options{})
	for text, want := range map[string]bool{
		`true`:                              true,
		` (true) `:                          true,
		`false`:                             false,
		`true || false`:                     false,
		`request.host == "x"`:               false,
		`!false`:                            false,
		`has(client.ip) || !has(client.ip)`: false,
	} {
		if got := mustCompile(t, env, text).Always(); got != want {
			t.Errorf("Always(%s) = %v, want %v", text, got, want)
		}
	}
}

// largeRequest is the largest request that Envoy lets through with its
// default limits: 100 headers, one of them filling most of the 60 KiB.
func largeRequest() *request.Request {
	r := sampleRequest("192.0.2.10")
	for i := len(r.Headers); i < 99; i++ {
		r.Headers[fmt.Sprintf("x-filler-%02d", i)] = "v"
	}
	r.Headers["x-big"] = strings.Repeat("a", 58*1024)
	return r
}

func TestCostLimit(t *testing.T) {
	env := newEnv(t, Options{})
	for _, tc := range []struct {
		expr  string
		limit bool
	}{
		{`"user-agent" in request.headers && request.headers["user-agent"].matches("(?i)(sqlmap|nikto)")`, false},
		{`request.headers.exists(k, k.startsWith("x-debug-"))`, false},
		{`request.headers["x-big"].contains("needle")`, false},
		{`request.headers["x-big"].matches("^(a|b)*c$")`, true},
		// The limit stops the evaluation: || does not absorb it.
		{`request.headers["x-big"].matches("^(a|b)*c$") || true`, true},
		{`true || request.headers["x-big"].matches("^(a|b)*c$")`, false},
	} {
		_, err := eval(t, env, tc.expr, largeRequest())
		if errors.Is(err, ErrCostLimit) != tc.limit || tc.limit && cause(err) != CauseExpression {
			t.Errorf("%s: %v; want the cost limit: %v", tc.expr, err, tc.limit)
		}
	}
}

// withPanics extends env with two functions of an address that panic:
// boomRaw, bare, as cel-go sees a function that is not tessera's, and boom,
// guarded as tessera's functions are.
func withPanics(t *testing.T, env *Env) *Env {
	t.Helper()
	boom := func(ref.Val) ref.Val { panic("boom") }
	ext, err := env.cel.Extend(
		cel.Function("boomRaw", cel.MemberOverload("test_boom_raw", []*cel.Type{ipType}, cel.BoolType, cel.UnaryBinding(boom))),
		cel.Function("boom", cel.MemberOverload("test_boom", []*cel.Type{ipType}, cel.BoolType, cel.UnaryBinding(env.lib.unary(boom)))),
	)
	if err != nil {
		t.Fatal(err)
	}
	cp := *env
	cp.cel = ext
	return &cp
}

func TestPanics(t *testing.T) {
	var mu sync.Mutex
	var reported []*Panic
	env := withPanics(t, newEnv(t, Options{OnPanic: func(p *Panic) {
		mu.Lock()
		defer mu.Unlock()
		reported = append(reported, p)
	}}))
	evalAs := func(rule, text string, r *request.Request) (bool, error) {
		t.Helper()
		in := NewInput(r, nil, nil)
		in.SetRule(rule)
		return mustCompile(t, env, text).Eval(in)
	}
	for _, tc := range []struct {
		expr      string
		value     bool
		cause     Cause
		withStack bool
	}{
		// Guarded: an error value with the stack, reported when recovered.
		{`client.ip.boom()`, false, CausePanic, true},
		{`client.ip.boom() && true`, false, CausePanic, true},
		// As an error value, a guarded panic follows CEL's rules: || true
		// absorbs it, and it is reported all the same, with its rule.
		{`client.ip.boom() || true`, true, "", true},
		// Unguarded: cel-go recovers it and aborts the evaluation, without a
		// stack; Eval reports it.
		{`client.ip.boomRaw()`, false, CausePanic, false},
		{`client.ip.boomRaw() || true`, false, CausePanic, false},
	} {
		reported = nil
		got, err := evalAs("office", tc.expr, sampleRequest("192.0.2.10"))
		if got != tc.value || cause(err) != tc.cause || (err == nil) != (tc.cause == "") {
			t.Errorf("%s = %v, %v; want %v with cause %q", tc.expr, got, err, tc.value, tc.cause)
		}
		if len(reported) != 1 || reported[0].Rule != "office" || (reported[0].Stack != nil) != tc.withStack || !strings.Contains(fmt.Sprint(reported[0].Value), "boom") {
			t.Errorf("%s: reported %+v, want one panic of rule office, with a stack: %v", tc.expr, reported, tc.withStack)
		} else if tc.withStack && !strings.Contains(string(reported[0].Stack), "expr_test.go") {
			t.Errorf("%s: the stack does not show the function that panicked", tc.expr)
		}
	}
	// A field read that panics, here on a request that is not there, is
	// guarded too, and reported with its rule.
	reported = nil
	in := NewInput(&request.Request{}, nil, nil)
	in.SetRule("admin")
	in.request.req = nil
	_, err := mustCompile(t, env, `request.host == "x"`).Eval(in)
	if cause(err) != CausePanic || len(reported) != 1 || reported[0].Stack == nil || reported[0].Rule != "admin" {
		t.Errorf("panicking field: %v, reported %+v", err, reported)
	}
}

// The network of inCidr is parsed when the program is built, never during an
// evaluation.
func TestInCidrNetworkParsedOnce(t *testing.T) {
	env := newEnv(t, Options{})
	parses := 0
	env.lib.parseNetwork = func(s string) (netip.Prefix, error) {
		parses++
		return parseNetwork(s)
	}
	c := mustCompile(t, env, `client.ip.inCidr("198.51.100.0/24") || client.ip.inCidr("203.0.113.0/24") || client.ip.inCidr("2001:db8::/32")`)
	compiled := parses
	for range 100 {
		if ok, err := c.Eval(NewInput(sampleRequest("2001:db8::7"), nil, nil)); !ok || err != nil {
			t.Fatalf("Eval = %v, %v", ok, err)
		}
	}
	if compiled == 0 || parses != compiled {
		t.Errorf("%d parses when compiled, %d after 100 evaluations", compiled, parses)
	}
}

// One Input serves the rules of one decision; concurrent decisions share no
// mutable state (run with -race).
func TestConcurrentEvaluations(t *testing.T) {
	env := newEnv(t, Options{})
	conditions := []*Condition{
		mustCompile(t, env, `request.host == "api.example.com" && client.ip.inCidr("192.0.2.0/24")`),
		mustCompile(t, env, `"x-api-key" in request.headers && request.path.startsWith("/v1/")`),
	}
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Go(func() {
			for i := range 500 {
				in := NewInput(sampleRequest(fmt.Sprintf("192.0.2.%d", (w*500+i)%250+1)), nil, nil)
				for _, c := range conditions {
					if ok, err := c.Eval(in); !ok || err != nil {
						t.Errorf("Eval = %v, %v", ok, err)
						return
					}
				}
			}
		})
	}
	wg.Wait()
}

// The standard functions, macros and operators that the expression language
// reference lists, as tessera's environment has them.
func TestStandardDefinitions(t *testing.T) {
	env := newEnv(t, Options{})
	r := sampleRequest("192.0.2.10")
	r.Headers["x-count"] = "7"
	r.Headers["x-tag"] = "a,b"
	r.Path = "/café"
	for _, text := range []string{
		`size(request.path) == 5`,
		`size(request.headers) == 5 && size(["a", "b"]) == 2`,
		`int(request.headers["x-count"]) == 7 && uint(request.headers["x-count"]) == 7u && double(request.headers["x-count"]) == 7.0`,
		`bool("true") && bytes("a") == b"a" && string(7) == "7"`,
		`request.headers["x-tag"] == "a,b"`,
		`has(request.headers.accept) && !has(request.headers.authorization)`,
		`request.headers.exists(k, k.startsWith("x-")) && request.headers.all(k, k != "")`,
		`request.headers.exists_one(k, k == "accept")`,
		`["GET", "HEAD"].map(m, m + "!") == ["GET!", "HEAD!"] && ["GET", "HEAD"].filter(m, m.startsWith("G")) == ["GET"]`,
		`request.host.matches("example") && !request.host.matches("^example$") && request.host.matches("(?i)^API\\.")`,
		`!(int(request.headers["x-count"]) > 7)`,
		`1 + 2 * 3 == 7 && true || false && false ? true : false`,
		`-int(request.headers["x-count"]) < 0 && 7 % 4 == 3`,
	} {
		got, err := eval(t, env, text, r)
		if err != nil || !got {
			t.Errorf("%s = %v, %v; want true", text, got, err)
		}
	}
	// Not available: the functions of CEL's extension libraries.
	for _, text := range []string{
		`request.host.lowerAscii() == "x"`,
		`request.path.split("/").size() > 1`,
		`request.host.trim() == "x"`,
		`cel.bind(h, request.host, h == "x")`,
	} {
		if _, err := env.Compile(text); err == nil {
			t.Errorf("%s compiles", text)
		}
	}
	// A macro cannot iterate over the result of another.
	if is := compileIssue(t, env, `["GET"].map(m, m + "!").filter(m, m != "")`); !strings.Contains(is.Message, "comprehension exceeds nesting limit") {
		t.Errorf("chained macros: %v", is)
	}
	// ! binds tighter than a comparison.
	is := compileIssue(t, env, `!int(request.headers["x-count"]) > 5`)
	if !strings.Contains(is.Message, "found no matching overload for '!_'") {
		t.Errorf("!int(x) > 5: %v", is)
	}
}

func TestAttributes(t *testing.T) {
	env := newEnv(t, Options{})
	for text, want := range map[string]string{
		`request.host == "x" && client.ip.inCidr("192.0.2.0/24")`:                         `client.ip request.host`,
		`has(client.ip) && request.method in ["GET"] && request.path.startsWith("/a")`:    `client.ip request.method request.path`,
		`request.headers["user-agent"].contains("bot") || "x-api-key" in request.headers`: `request.headers["user-agent"] request.headers["x-api-key"]`,
		`has(request.headers.accept) && request.query == "" && request.scheme == "https"`: `request.headers["accept"] request.query request.scheme`,
		`request.headers.exists(k, k.startsWith("x-")) && request.id != ""`:               `request.headers request.id`,
		`size(request.headers) > 3`:             `request.headers`,
		`request.headers[request.host] == "x"`:  `request.headers request.host`,
		`["a"].exists(request, request == "a")`: ``,
		`true`:                                  ``,
		`{"k": request.host}["k"] == "x" && [string(client.ip)].size() == 1`: `client.ip request.host`,
	} {
		if got := strings.Join(mustCompile(t, env, text).Attributes(), " "); got != want {
			t.Errorf("%s: attributes %q, want %q", text, got, want)
		}
	}
}
