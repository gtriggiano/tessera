package expr

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"

	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"

	"github.com/gtriggiano/tessera/internal/request"
	"github.com/gtriggiano/tessera/internal/useragent"
)

// The user-agent headers of the tests.
const (
	chromeWindows = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.6778.86 Safari/537.36"
	safariIPhone  = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.6 Mobile/15E148 Safari/604.1"
	googlebot     = "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)"
)

// withUserAgent returns a request whose user-agent header is header, or that
// has none when header is absent.
func withUserAgent(header string, present bool) *request.Request {
	r := sampleRequest("192.0.2.7")
	delete(r.Headers, "user-agent")
	if present {
		r.Headers["user-agent"] = header
	}
	return r
}

func evalUA(t *testing.T, env *Env, ua *useragent.Analyzer, text string, r *request.Request) (bool, error) {
	t.Helper()
	return mustCompile(t, env, text).Eval(NewInput(r, nil, nil, ua))
}

// expectedBot returns the bot that the analysis finds in header, with its
// name, which the tests of internal/useragent fix, and the description of the
// list of crawler-user-agents, both nonempty: a new name or a reworded
// description does not fail this test.
func expectedBot(t *testing.T, header string) useragent.Bot {
	t.Helper()
	b := useragent.New(0).Analyze(header).Bot
	if b.Name == "" || b.Description == "" {
		t.Fatalf("%s: bot %+v, without a name or a description: see Dependabot in CONTRIBUTING.md", header, b)
	}
	return b
}

func TestUserAgent(t *testing.T) {
	env := newEnv(t, Options{UserAgent: true})
	ua := useragent.New(100)
	googlebotBot, curlBot := expectedBot(t, googlebot), expectedBot(t, "curl/8.7.1")
	for _, tc := range []struct {
		header string
		exprs  []string
	}{
		{chromeWindows, []string{
			`ua.browser.name == "Chrome" && ua.browser.version == "131.0.6778" && ua.browser.major == 131 && ua.browser.minor == 0`,
			`ua.os.name == "Windows" && ua.os.version == "10.0.0" && ua.os.major == 10 && ua.os.minor == 0 && ua.device == "desktop"`,
			`!ua.is_bot && ua.bot.name == "" && ua.bot.description == "" && ua.browser.major >= 120`,
			`ua == ua && has(ua.browser) && has(ua.browser.name) && has(ua.os.major) && has(ua.is_bot) && has(ua.bot.description)`,
		}},
		{safariIPhone, []string{
			`ua.browser.name == "Safari" && ua.os.name in ["iOS", "iPadOS"] && ua.device == "mobile"`,
			`ua.os.version.startsWith("17.") && ua.os.major == 17 && ua.os.minor == 6 && ua.browser.major == 17 && ua.browser.minor == 6`,
		}},
		{googlebot, []string{
			"ua.is_bot && ua.bot.name == " + strconv.Quote(googlebotBot.Name) + ` && ua.device == "bot"`,
			"ua.bot.description == " + strconv.Quote(googlebotBot.Description),
			`ua.browser.name == "" && ua.browser.version == "" && ua.browser.major == 0 && ua.browser.minor == 0`,
			`ua.os.name == "" && ua.os.version == "" && ua.os.major == 0 && ua.os.minor == 0`,
		}},
		{"curl/8.7.1", []string{
			"ua.bot.name == " + strconv.Quote(curlBot.Name) + " && ua.bot.description == " + strconv.Quote(curlBot.Description),
		}},
	} {
		for _, text := range tc.exprs {
			if got, err := evalUA(t, env, ua, text, withUserAgent(tc.header, true)); !got || err != nil {
				t.Errorf("%s: %s = %v, %v; want true", tc.header, text, got, err)
			}
		}
	}
	// Without an Analyzer, the same analysis, without a cache.
	if got, err := evalUA(t, env, nil, `ua.browser.name == "Chrome"`, withUserAgent(chromeWindows, true)); !got || err != nil {
		t.Errorf("without an Analyzer: %v, %v", got, err)
	}
	// ua does not read the client address.
	r := withUserAgent(googlebot, true)
	r.ClientIP = sampleRequest("").ClientIP
	if got, err := evalUA(t, env, ua, `ua.is_bot`, r); !got || err != nil {
		t.Errorf("without a client address: %v, %v", got, err)
	}
}

// A request without the header, or with an empty one, reads empty strings,
// the device unknown, no bot and versions 0: not an error, and no analysis.
func TestUserAgentWithoutHeader(t *testing.T) {
	env := newEnv(t, Options{UserAgent: true})
	ua := useragent.New(100)
	const empty = `ua.browser.name == "" && ua.browser.version == "" && ua.browser.major == 0 && ua.browser.minor == 0 && ua.os.name == "" && ua.os.version == "" && ua.os.major == 0 && ua.os.minor == 0 && ua.device == "unknown" && !ua.is_bot && ua.bot.name == "" && ua.bot.description == ""`
	for _, r := range []*request.Request{withUserAgent("", false), withUserAgent("", true)} {
		if got, err := evalUA(t, env, ua, empty, r); !got || err != nil {
			t.Errorf("headers %v: %v, %v", r.Headers, got, err)
		}
	}
	if hits, misses := ua.Lookups(); hits != 0 || misses != 0 {
		t.Errorf("lookups %d, %d; want none", hits, misses)
	}
}

// A condition that does not read ua, or decides before reading it, analyzes
// nothing; the expressions of one Input that read ua share one analysis.
func TestUserAgentOncePerDecision(t *testing.T) {
	env := newEnv(t, Options{UserAgent: true})
	ua := useragent.New(100)
	in := NewInput(withUserAgent(chromeWindows, true), nil, nil, ua)
	for _, text := range []string{
		`request.host == "api.example.com"`,
		`request.host == "api.example.com" || ua.is_bot`,
		`!(request.method == "POST" && ua.device == "mobile")`,
	} {
		if got, err := mustCompile(t, env, text).Eval(in); !got || err != nil {
			t.Fatalf("%s: %v, %v", text, got, err)
		}
	}
	if hits, misses := ua.Lookups(); hits+misses != 0 {
		t.Fatalf("conditions that do not read ua analyzed the header: %d, %d", hits, misses)
	}
	for _, text := range []string{`ua.browser.name == "Chrome"`, `ua.device == "desktop" && !ua.is_bot`, `ua.browser.major > 100`, `ua.browser == ua.browser`} {
		if got, err := mustCompile(t, env, text).Eval(in); !got || err != nil {
			t.Fatalf("%s: %v, %v", text, got, err)
		}
	}
	for _, text := range []string{`ua.browser.name + "/" + ua.os.name`, `ua.device`, `ua.bot.description`} {
		if _, err := mustCompileString(t, env, text).Eval(in); err != nil {
			t.Fatalf("%s: %v", text, err)
		}
	}
	if r, err := in.UserAgent(); err != nil || r.Browser.Name != "Chrome" {
		t.Errorf("UserAgent: %+v, %v", r, err)
	}
	if hits, misses := ua.Lookups(); hits != 0 || misses != 1 {
		t.Errorf("lookups %d, %d; want one analysis for the decision", hits, misses)
	}
	// The next decision with the same header finds it in the cache.
	if got, err := evalUA(t, env, ua, `ua.browser.name == "Chrome"`, withUserAgent(chromeWindows, true)); !got || err != nil {
		t.Fatalf("next decision: %v, %v", got, err)
	}
	if hits, misses := ua.Lookups(); hits != 1 || misses != 1 {
		t.Errorf("lookups %d, %d; want a hit", hits, misses)
	}
}

// panicking is an analysis that panics.
type panicking struct{}

func (panicking) Analyze(string) useragent.Record { panic("analysis") }

// A panic of the analysis is recovered and reported once, with its rule, and
// fails every read of ua in the decision with the cause panic, has()
// included.
func TestUserAgentPanic(t *testing.T) {
	var mu sync.Mutex
	var reported []*Panic
	env := newEnv(t, Options{UserAgent: true, OnPanic: func(p *Panic) {
		mu.Lock()
		defer mu.Unlock()
		reported = append(reported, p)
	}})
	in := NewInput(withUserAgent(chromeWindows, true), nil, nil, nil)
	in.userAgent = panicking{}
	in.SetRule("bots")
	for _, text := range []string{`ua.is_bot`, `!ua.is_bot`, `has(ua.browser)`, `has(ua.bot.name)`, `ua == ua`, `ua.browser == ua.browser`, `ua.browser.major == 0`} {
		if got, err := mustCompile(t, env, text).Eval(in); got || cause(err) != CausePanic {
			t.Errorf("%s: %v, %v; want cause panic", text, got, err)
		}
	}
	if _, err := mustCompileString(t, env, `ua.device`).Eval(in); cause(err) != CausePanic {
		t.Errorf("string expression: %v", err)
	}
	// && and || absorb it when the other side decides.
	if got, err := mustCompile(t, env, `request.host == "api.example.com" || ua.is_bot`).Eval(in); !got || err != nil {
		t.Errorf("absorbed: %v, %v", got, err)
	}
	var p *Panic
	if _, err := in.UserAgent(); !errors.As(err, &p) {
		t.Errorf("UserAgent: %v, want a *Panic", err)
	}
	if len(reported) != 1 || reported[0].Rule != "bots" || reported[0].Stack == nil || !strings.Contains(fmt.Sprint(reported[0].Value), "analysis") {
		t.Errorf("reported %+v, want one panic of the rule bots, with its stack", reported)
	}
}

func TestUserAgentCompileErrors(t *testing.T) {
	env := newEnv(t, Options{UserAgent: true})
	for _, tc := range []struct {
		expr         string
		line, column int
		prefix       string
	}{
		{`ua.browsr == "Chrome"`, 1, 3, "undefined field 'browsr'"},
		{`ua.raw == "curl"`, 1, 3, "undefined field 'raw'"},
		{`ua.found`, 1, 3, "undefined field 'found'"},
		{`ua.browser_major == 131`, 1, 3, "undefined field 'browser_major'"},
		{`ua.bot_name == "curl"`, 1, 3, "undefined field 'bot_name'"},
		{`ua.os.nam == "iOS"`, 1, 6, "undefined field 'nam'"},
		{`ua.os.minor == "6"`, 1, 13, "found no matching overload for '_==_' applied to '(int, string)'"},
		{`ua.browser.major == "131"`, 1, 18, "found no matching overload for '_==_' applied to '(int, string)'"},
		{`ua.is_bot == "true"`, 1, 11, "found no matching overload for '_==_' applied to '(bool, string)'"},
		{`ua == "curl"`, 1, 4, "found no matching overload for '_==_' applied to '(tessera.UserAgent, string)'"},
		{`ua.browser == "Chrome"`, 1, 12, "found no matching overload for '_==_' applied to '(tessera.UserAgentBrowser, string)'"},
		{`ua.os == ua.browser`, 1, 7, "found no matching overload for '_==_' applied to '(tessera.UserAgentOS, tessera.UserAgentBrowser)'"},
		{`ua.bot in ["curl"]`, 1, 8, "found no matching overload for '@in' applied to '(tessera.UserAgentBot, list(string))'"},
		{`ua.device`, 1, 1, "the condition must be a bool, not string"},
		{`ua`, 1, 1, "the condition must be a bool, not tessera.UserAgent"},
		{`ua.browser`, 1, 1, "the condition must be a bool, not tessera.UserAgentBrowser"},
	} {
		is := compileIssue(t, env, tc.expr)
		if is.Line != tc.line || is.Column != tc.column || !strings.HasPrefix(is.Message, tc.prefix) {
			t.Errorf("%q: line %d, column %d: %q; want line %d, column %d: %q…", tc.expr, is.Line, is.Column, is.Message, tc.line, tc.column, tc.prefix)
		}
	}
	// Without the analysis, ua is not available, at the position of the
	// variable.
	for _, tc := range []struct {
		expr         string
		line, column int
	}{
		{`ua.is_bot`, 1, 1},
		{"request.host == \"x\" &&\n  ua.device == \"bot\"", 2, 3},
		{`has(ua.browser.name)`, 1, 5},
	} {
		is := compileIssue(t, newEnv(t, Options{}), tc.expr)
		if is.Line != tc.line || is.Column != tc.column || is.Message != "ua is not available: the configuration has no enrichment.user_agent" {
			t.Errorf("%q: line %d, column %d: %q", tc.expr, is.Line, is.Column, is.Message)
		}
	}
	if _, err := newEnv(t, Options{}).CompileString(`ua.device`); err == nil || !strings.Contains(err.Error(), "ua is not available") {
		t.Errorf("string expression without the analysis: %v", err)
	}
	// An iteration variable named ua is not the variable ua.
	if _, err := newEnv(t, Options{}).Compile(`["a"].exists(ua, ua == "a")`); err != nil {
		t.Errorf("iteration variable: %v", err)
	}
}

func TestUserAgentRead(t *testing.T) {
	env := newEnv(t, Options{UserAgent: true})
	for text, want := range map[string]struct {
		attributes string
		reads      bool
	}{
		`ua.is_bot && ua.bot.name == "Googlebot"`:        {`ua.bot.name ua.is_bot`, true},
		`ua.browser.major < 15 || ua.os.name in ["iOS"]`: {`ua.browser.major ua.os.name`, true},
		`has(ua.device)`:                                  {`ua.device`, true},
		`has(ua.browser) && has(ua.os.major)`:             {`ua.browser ua.os.major`, true},
		`ua.browser == ua.browser && ua.bot != ua.bot`:    {`ua.bot ua.browser`, true},
		`ua.os in [ua.os] && ua.os.version != ""`:         {`ua.os ua.os.version`, true},
		`[ua.bot].exists(b, b.description.contains("x"))`: {`ua.bot`, true},
		`ua == ua`: {``, true},
		`request.headers["user-agent"].contains("curl")`:    {`request.headers["user-agent"]`, false},
		`["a"].exists(ua, ua == "a") && request.host != ""`: {`request.host`, false},
	} {
		c := mustCompile(t, env, text)
		if got := strings.Join(c.Attributes(), " "); got != want.attributes {
			t.Errorf("%s: attributes %q, want %q", text, got, want.attributes)
		}
		if c.ReadsUserAgent() != want.reads {
			t.Errorf("%s: ReadsUserAgent %v", text, c.ReadsUserAgent())
		}
	}
	// A decision field reads ua as any other expression.
	f, err := env.CompileDecisionField(`ua.is_bot ? ua.bot.name : ua.browser.name`)
	if err != nil || !f.ReadsUserAgent() || f.ReadsSecrets() {
		t.Errorf("decision field: %v", err)
	}
}

// The objects of ua compare by their fields, as those of geo: two values
// with the same fields are equal, whatever the decision they come from; ua
// itself compares by identity.
func TestUserAgentEquality(t *testing.T) {
	env := newEnv(t, Options{UserAgent: true})
	ua := useragent.New(100)
	for header, texts := range map[string][]string{
		chromeWindows: {
			`ua.browser == ua.browser && ua.os == ua.os && ua.bot == ua.bot`,
			`ua.browser in [ua.browser] && [ua.os] == [ua.os] && !(ua.bot != ua.bot)`,
			`ua == ua && dyn(ua.browser) != dyn(ua.os) && dyn(ua.bot) != dyn(ua.browser)`,
		},
		googlebot: {`ua.bot == ua.bot && ua.browser == ua.browser`},
	} {
		for _, text := range texts {
			if got, err := evalUA(t, env, ua, text, withUserAgent(header, true)); !got || err != nil {
				t.Errorf("%s: %s = %v, %v; want true", header, text, got, err)
			}
		}
	}

	// The values of two decisions: the objects equal by their fields, ua
	// does not.
	read := func(header string) *userAgentVal {
		v, _ := NewInput(withUserAgent(header, true), nil, nil, ua).ResolveName("ua")
		u, ok := v.(*userAgentVal)
		if !ok {
			t.Fatalf("ua for %q: %v", header, v)
		}
		return u
	}
	field := func(u *userAgentVal, name string) ref.Val {
		v, err := userAgentFields[name].get(u)
		if err != nil {
			t.Fatal(err)
		}
		return v.(ref.Val)
	}
	equal := func(a, b ref.Val) bool { return a.Equal(b) == types.True }
	chrome, chrome2, safari, bot, curl := read(chromeWindows), read(chromeWindows), read(safariIPhone), read(googlebot), read("curl/8.7.1")
	for _, name := range []string{"browser", "os", "bot"} {
		if field(chrome, name) == field(chrome2, name) {
			t.Fatalf("%s: one value for two decisions", name)
		}
		if !equal(field(chrome, name), field(chrome2, name)) {
			t.Errorf("%s of the same header in two decisions: not equal", name)
		}
	}
	for _, tc := range []struct {
		a, b  *userAgentVal
		name  string
		equal bool
	}{
		{chrome, safari, "browser", false},
		{chrome, safari, "os", false},
		// Two clients that are not bots have the same empty bot, and two
		// bots the same empty browser and operating system.
		{chrome, safari, "bot", true},
		{bot, curl, "bot", false},
		{bot, curl, "browser", true},
		{bot, curl, "os", true},
	} {
		if got := equal(field(tc.a, tc.name), field(tc.b, tc.name)); got != tc.equal {
			t.Errorf("%s of %+v and %+v: equal %v, want %v", tc.name, tc.a.rec, tc.b.rec, got, tc.equal)
		}
	}
	if equal(chrome, chrome2) {
		t.Error("ua of two decisions: equal")
	}
	if equal(field(chrome, "browser"), types.String("Chrome")) || equal(field(chrome, "browser"), field(chrome, "os")) {
		t.Error("a browser equals a string or an operating system")
	}
}

// Decisions that read ua at once share the cache, and each agrees with an
// analysis without it. Run with -race.
func TestUserAgentConcurrent(t *testing.T) {
	env := newEnv(t, Options{UserAgent: true})
	ua := useragent.New(16)
	c := mustCompile(t, env, `ua.is_bot == (ua.device == "bot")`)
	headers := []string{chromeWindows, safariIPhone, googlebot, "curl/8.7.1", ""}
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Go(func() {
			for i := range 500 {
				r := withUserAgent(headers[(i+w)%len(headers)], true)
				if got, err := c.Eval(NewInput(r, nil, nil, ua)); !got || err != nil {
					t.Errorf("%v, %v", got, err)
					return
				}
			}
		})
	}
	wg.Wait()
}
