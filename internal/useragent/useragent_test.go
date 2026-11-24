package useragent

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode"

	uasurfer "github.com/LumenResearch/uasurfer"
	agents "github.com/monperrus/crawler-user-agents"

	"github.com/gtriggiano/tessera/internal/sets"
)

// The user-agent headers of the tests: real browsers and programs.
const (
	chromeWindows   = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.6778.86 Safari/537.36"
	edgeWindows     = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36 Edg/131.0.2903.70"
	edgeLegacy      = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/70.0.3538.102 Safari/537.36 Edge/18.18363"
	edgeAndroid     = "Mozilla/5.0 (Linux; Android 10; K) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Mobile Safari/537.36 EdgA/131.0.0.0"
	edgeIOS         = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) EdgiOS/131.0.2903.68 Version/17.0 Mobile/15E148 Safari/604.1"
	ie11            = "Mozilla/5.0 (Windows NT 6.1; WOW64; Trident/7.0; rv:11.0) like Gecko"
	safariIPhone    = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.6 Mobile/15E148 Safari/604.1"
	safariIPad      = "Mozilla/5.0 (iPad; CPU OS 17_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.6 Mobile/15E148 Safari/604.1"
	safariMac       = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.6 Safari/605.1.15"
	firefoxLinux    = "Mozilla/5.0 (X11; Linux x86_64; rv:133.0) Gecko/20100101 Firefox/133.0"
	chromeTablet    = "Mozilla/5.0 (Linux; Android 6.0.1; SM-T560) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	chromeOS        = "Mozilla/5.0 (X11; CrOS x86_64 14541.0.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	facebookAndroid = "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Version/4.0 Chrome/131.0.6778.81 Mobile Safari/537.36 [FB_IAB/FB4A;FBAV/489.0.0.47.104;]"
	playStation     = "Mozilla/5.0 (PlayStation; PlayStation 5/2.26) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/13.0 Safari/605.1.15"
	tizenTV         = "Mozilla/5.0 (SMART-TV; Linux; Tizen 6.0) AppleWebKit/537.36 (KHTML, like Gecko) 76.0.3809.146/6.0 TV Safari/537.36"
	googlebot       = "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)"
	bingbot         = "Mozilla/5.0 (compatible; bingbot/2.0; +http://www.bing.com/bingbot.htm)"
	claudeBot       = "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; ClaudeBot/1.0; +claudebot@anthropic.com)"
	curl            = "curl/8.7.1"
	sqlmap          = "sqlmap/1.8"
)

func TestAnalyze(t *testing.T) {
	a := New(0)
	for _, tc := range []struct {
		header string
		want   Record
	}{
		{chromeWindows, Record{Browser: Browser{"Chrome", "131.0.6778", 131, 0}, OS: OS{"Windows", "10.0.0", 10, 0}, Device: DeviceDesktop}},
		{edgeWindows, Record{Browser: Browser{"Edge", "131.0.2903", 131, 0}, OS: OS{"Windows", "10.0.0", 10, 0}, Device: DeviceDesktop}},
		{ie11, Record{Browser: Browser{"Internet Explorer", "11.0.0", 11, 0}, OS: OS{"Windows", "6.1.0", 6, 1}, Device: DeviceDesktop}},
		{safariIPhone, Record{Browser: Browser{"Safari", "17.6.0", 17, 6}, OS: OS{"iOS", "17.6.0", 17, 6}, Device: DeviceMobile}},
		{safariIPad, Record{Browser: Browser{"Safari", "17.6.0", 17, 6}, OS: OS{"iPadOS", "17.6.0", 17, 6}, Device: DeviceTablet}},
		{safariMac, Record{Browser: Browser{"Safari", "17.6.0", 17, 6}, OS: OS{"macOS", "10.15.7", 10, 15}, Device: DeviceDesktop}},
		// A version that uasurfer does not know is empty, not 0.0.0, and
		// its parts are 0.
		{firefoxLinux, Record{Browser: Browser{"Firefox", "133.0.0", 133, 0}, OS: OS{Name: "Linux"}, Device: DeviceDesktop}},
		{chromeTablet, Record{Browser: Browser{"Chrome", "131.0.0", 131, 0}, OS: OS{"Android", "6.0.1", 6, 0}, Device: DeviceTablet}},
		{chromeOS, Record{Browser: Browser{"Chrome", "131.0.0", 131, 0}, OS: OS{Name: "ChromeOS"}, Device: DeviceDesktop}},
		{facebookAndroid, Record{Browser: Browser{"Facebook", "489.0.0", 489, 0}, OS: OS{"Android", "14.0.0", 14, 0}, Device: DeviceMobile}},
		{playStation, Record{Browser: Browser{"Safari", "13.0.0", 13, 0}, OS: OS{Name: "PlayStation"}, Device: DeviceConsole}},
		{tizenTV, Record{OS: OS{"Tizen", "6.0.0", 6, 0}, Device: DeviceTV}},
		// A bot has only its kind of device, its name and its description:
		// the name of the first pattern of the list that matches, the text
		// of a literal one or the name of the table, and the description
		// that the list gives to that pattern.
		{googlebot, bot(t, "Googlebot", `Googlebot\/`)},
		// The list names the same crawler as uasurfer: the spelling of
		// uasurfer, with the description of the list.
		{bingbot, bot(t, "Bing Bot", "bingbot")},
		{"Mozilla/5.0 (compatible; Yahoo! Slurp; http://help.yahoo.com/help/us/ysearch/slurp)", bot(t, "Yahoo Slurp", "Slurp")},
		{"facebookexternalhit/1.1 (+http://www.facebook.com/externalhit_uatext.php)", bot(t, "Facebook Crawler", "facebookexternalhit")},
		{"Mozilla/5.0 (compatible; coccocbot-web/1.1; +http://help.coccoc.com/searchengine)", bot(t, "Coccoc Bot", "coccoc")},
		// Another crawler of the same operator keeps its own name, here one
		// of the table for a literal pattern.
		{"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko); compatible; GPTBot/1.1; +https://openai.com/gptbot", bot(t, "OpenAI GPT", "GPTBot")},
		{"Googlebot-Image/1.0", bot(t, "Googlebot Image", "Googlebot-Image")},
		{claudeBot, bot(t, "Claude Bot", "[cC]laude[bB]ot")},
		{curl, bot(t, "curl", "^curl")},
		// A bot that only the list recognizes.
		{sqlmap, bot(t, "sqlmap", "sqlmap")},
		// Bots that only uasurfer recognizes: the name of the table of
		// tessera, without a description, and none for the generic bot.
		{"Mozilla/5.0 (compatible; duckduckbot)", Record{Device: DeviceBot, IsBot: true, Bot: Bot{Name: "DuckDuck Bot"}}},
		{"Mozilla/5.0 (compatible; ahrefsbot)", Record{Device: DeviceBot, IsBot: true, Bot: Bot{Name: "Ahrefs"}}},
		{"Mozilla/5.0 (compatible; googlebot)", Record{Device: DeviceBot, IsBot: true, Bot: Bot{Name: "Googlebot"}}},
		{"Mozilla/5.0 (compatible; ExampleCrawler/1.0)", Record{Device: DeviceBot, IsBot: true}},
		{"Mozilla/5.0", Record{Device: DeviceUnknown}},
		{"x", Record{Device: DeviceUnknown}},
		{"\xff\xfe", Record{Device: DeviceUnknown}},
	} {
		got := a.Analyze(tc.header)
		if got != tc.want {
			t.Errorf("%s:\n got %+v\nwant %+v", tc.header, got, tc.want)
		}
		checkRecord(t, tc.header, got)
	}
}

// bot returns the record of a bot named name by the pattern of the list of
// crawler-user-agents, with the description that the JSON of the list gives
// to that pattern, without the white space around it.
func bot(t *testing.T, name, pattern string) Record {
	t.Helper()
	for _, e := range listEntries(t) {
		if e["pattern"] == pattern {
			description, _ := e["description"].(string)
			return Record{Device: DeviceBot, IsBot: true, Bot: Bot{Name: name, Description: strings.TrimSpace(description)}}
		}
	}
	t.Fatalf("no pattern %s in the list", pattern)
	return Record{}
}

// listEntries returns the entries of the JSON of the list of
// crawler-user-agents, read apart from readDescriptions, but with the same
// care: a slice that crawlersJSON does not link to, or that is no []byte,
// fails the test with the guard's message instead of a panic.
func listEntries(t *testing.T) []map[string]any {
	t.Helper()
	data := crawlersJSON
	switch {
	case len(data) == 0:
		t.Fatal("crawlersJSON is empty: the module github.com/monperrus/crawler-user-agents has no variable crawlersJson any more, and the go:linkname of crawlersJSON links to nothing; see Dependabot in CONTRIBUTING.md")
	case len(data) > maxJSONBytes:
		t.Fatal("crawlersJSON is longer than 64 MiB: the variable crawlersJson of the module github.com/monperrus/crawler-user-agents is no []byte any more; see Dependabot in CONTRIBUTING.md")
	}
	var entries []map[string]any
	if err := json.Unmarshal(data[:len(data):len(data)], &entries); err != nil {
		t.Fatalf("crawlersJSON is not the JSON of the list: %v; see Dependabot in CONTRIBUTING.md", err)
	}
	return entries
}

// The descriptions are those of the JSON that the module embeds, entry by
// entry, without the white space around them: the JSON has one entry for
// each crawler of agents.Crawlers, in the same order, as many, with the same
// pattern. An update of the module that
// renames the variable crawlersJson, which crawlersJSON links to, or changes
// the JSON, fails here.
func TestCrawlerDescriptions(t *testing.T) {
	entries := listEntries(t)
	got, err := readDescriptions(crawlersJSON, agents.Crawlers)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(agents.Crawlers) || len(got) != len(agents.Crawlers) {
		t.Fatalf("%d entries in the JSON, %d descriptions, %d crawlers", len(entries), len(got), len(agents.Crawlers))
	}
	described := 0
	for i, e := range entries {
		if e["pattern"] != agents.Crawlers[i].Pattern {
			t.Fatalf("entry %d: pattern %q in the JSON, %q in agents.Crawlers", i, e["pattern"], agents.Crawlers[i].Pattern)
		}
		raw, _ := e["description"].(string)
		want := strings.TrimSpace(raw)
		if got[i] != want || crawlerDescriptions()[i] != want {
			t.Errorf("entry %d, %q: description %q, want %q", i, agents.Crawlers[i].Pattern, got[i], want)
		}
		if strings.TrimSpace(got[i]) != got[i] {
			t.Errorf("entry %d, %q: description %q with white space around it", i, agents.Crawlers[i].Pattern, got[i])
		}
		if want != "" {
			described++
		}
	}
	if described == 0 {
		t.Error("no entry of the JSON has a description")
	}
	// What the JSON is checked against.
	for _, tc := range []struct {
		name string
		data string
		want string
	}{
		{"empty", "", "the JSON of crawler-user-agents is empty: "},
		{"not JSON", "{", "the JSON of crawler-user-agents cannot be read: "},
		{"fewer entries", `[{"pattern": "Googlebot\\/", "description": "Google"}]`, "the JSON of crawler-user-agents has 1 entries, and agents.Crawlers "},
	} {
		if _, err := readDescriptions([]byte(tc.data), agents.Crawlers); err == nil || !strings.HasPrefix(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q…", tc.name, err, tc.want)
		}
		// The analysis goes on, with every description empty.
		if d := descriptions([]byte(tc.data), agents.Crawlers); len(d) != len(agents.Crawlers) || slices.ContainsFunc(d, func(s string) bool { return s != "" }) {
			t.Errorf("%s: %d descriptions, not all empty", tc.name, len(d))
		}
	}
	// A length that the JSON of the list cannot have, as a variable of
	// another type can give, is not read.
	long := make([]byte, maxJSONBytes+1)
	if _, err := readDescriptions(long, agents.Crawlers); err == nil || !strings.HasPrefix(err.Error(), "the JSON of crawler-user-agents is longer than 64 MiB: ") {
		t.Errorf("a slice of %d bytes: %v", len(long), err)
	}
	if d := descriptions(long, agents.Crawlers); len(d) != len(agents.Crawlers) || slices.ContainsFunc(d, func(s string) bool { return s != "" }) {
		t.Errorf("a slice of %d bytes: %d descriptions, not all empty", len(long), len(d))
	}
	other := slices.Clone(agents.Crawlers)
	other[1].Pattern = "Examplebot"
	if _, err := readDescriptions(crawlersJSON, other); err == nil || err.Error() != `entry 1 of the JSON of crawler-user-agents has the pattern "Googlebot-Mobile", and agents.Crawlers "Examplebot"` {
		t.Errorf("another pattern: %v", err)
	}
}

// uasurfer names Edge as Internet Explorer: tessera says Edge when the header
// names one of its products, in any case.
func TestEdge(t *testing.T) {
	for _, h := range []string{edgeWindows, edgeLegacy, edgeAndroid, edgeIOS, strings.ToUpper(edgeWindows)} {
		if got := New(0).Analyze(h); got.Browser.Name != "Edge" || got.IsBot {
			t.Errorf("%s: %+v", h, got)
		}
	}
	if got := New(0).Analyze(ie11); got.Browser.Name != "Internet Explorer" {
		t.Errorf("Internet Explorer 11: %+v", got)
	}
}

// An absent or empty header gives empty names and the device unknown,
// without an analysis or a lookup in the cache.
func TestEmptyHeader(t *testing.T) {
	a := New(100)
	if got := a.Analyze(""); got != (Record{Device: DeviceUnknown}) {
		t.Errorf("empty header: %+v", got)
	}
	if hits, misses := a.Lookups(); hits != 0 || misses != 0 || a.Entries() != 0 {
		t.Errorf("lookups %d, %d, entries %d", hits, misses, a.Entries())
	}
	var none *Analyzer
	if got := none.Analyze(""); got != (Record{Device: DeviceUnknown}) {
		t.Errorf("nil Analyzer: %+v", got)
	}
}

// tessera analyzes the first MaxBytes bytes of the header only: the record
// depends on them, and two headers with the same first bytes share one entry
// of the cache.
func TestFirstBytes(t *testing.T) {
	padding := strings.Repeat("a", MaxBytes-len("Mozilla/5.0 "))
	head := "Mozilla/5.0 " + padding
	if len(head) != MaxBytes {
		t.Fatalf("head of %d bytes", len(head))
	}
	a := New(100)
	// A bot named after the first bytes is not seen.
	if got := a.Analyze(head + " Googlebot/2.1"); got.IsBot || got.Device != DeviceUnknown {
		t.Errorf("a bot after %d bytes: %+v", MaxBytes, got)
	}
	if got := a.Analyze(head + " curl/8.7.1"); got.IsBot {
		t.Errorf("another header with the same first bytes: %+v", got)
	}
	if hits, misses := a.Lookups(); hits != 1 || misses != 1 || a.Entries() != 1 {
		t.Errorf("lookups %d, %d, entries %d; want one entry for both headers", hits, misses, a.Entries())
	}
	// Within the first bytes, it is seen.
	if got := a.Analyze(head[:MaxBytes-len("Googlebot/2.1")] + "Googlebot/2.1 and more"); !got.IsBot || got.Bot.Name != "Googlebot" {
		t.Errorf("a bot within %d bytes: %+v", MaxBytes, got)
	}
	// The cut is by bytes, also inside a character.
	long := strings.Repeat("é", MaxBytes)
	if a.Analyze(long) != New(0).Analyze(long[:MaxBytes]) {
		t.Error("a header of characters of two bytes")
	}
}

// Every value of uasurfer has a name in the tables of tessera: an update of
// uasurfer with a new browser, operating system or device fails here.
func TestNameTables(t *testing.T) {
	for i := range 1024 {
		b := uasurfer.BrowserName(i)
		if i > 0 && b.String() == "BrowserUnknown" {
			continue
		}
		_, browser := browserNames[b]
		_, crawler := uasurferCrawlers[b]
		bot := strings.HasSuffix(b.String(), "Bot")
		switch {
		case browser && crawler:
			t.Errorf("%s is in both tables", b)
		case bot && !crawler:
			t.Errorf("the bot %s has no name in uasurferCrawlers", b)
		case !bot && !browser:
			t.Errorf("the browser %s has no name in browserNames", b)
		}
		if crawler && uasurferCrawlers[b] == "" && b != uasurfer.BrowserBot {
			t.Errorf("the bot %s has an empty name", b)
		}
		if browser && browserNames[b] == "" && b != uasurfer.BrowserUnknown {
			t.Errorf("the browser %s has an empty name", b)
		}
	}
	for i := range 1024 {
		o := uasurfer.OSName(i)
		if i > 0 && o.String() == "OSUnknown" {
			continue
		}
		name, ok := osNames[o]
		switch {
		case !ok:
			t.Errorf("the operating system %s has no name in osNames", o)
		case name == "" && o != uasurfer.OSUnknown && o != uasurfer.OSBot:
			t.Errorf("the operating system %s has an empty name", o)
		}
	}
	for i := range 1024 {
		d := uasurfer.DeviceType(i)
		if i > 0 && d.String() == "DeviceUnknown" {
			continue
		}
		if (device(d) == DeviceUnknown) != (d == uasurfer.DeviceUnknown) {
			t.Errorf("the device %s gives %s", d, device(d))
		}
	}
}

// The names are listed in the expressions reference.
func TestNamesDocumented(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "reference", "expressions.md"))
	if err != nil {
		t.Fatal(err)
	}
	page := string(data)
	names := append([]string{browserEdge}, Devices...)
	for _, table := range []map[uasurfer.BrowserName]string{browserNames, uasurferCrawlers} {
		for _, name := range table {
			names = append(names, name)
		}
	}
	for _, name := range osNames {
		names = append(names, name)
	}
	for _, name := range names {
		if name != "" && !strings.Contains(page, "`"+name+"`") {
			t.Errorf("%s is not in the expressions reference", name)
		}
	}
}

// Every pattern of the list of crawler-user-agents has a name: a literal one
// its text, and the others a name of crawlerPatternNames, which applies to a
// literal one too. An update of the list with a pattern that is not a literal
// fails here, until it has a name, and so does an entry of
// crawlerPatternNames that the list no longer has.
func TestCrawlerNames(t *testing.T) {
	names := crawlerNames()
	if len(names) != len(agents.Crawlers) {
		t.Fatalf("%d names for %d patterns", len(names), len(agents.Crawlers))
	}
	patterns := map[string]bool{}
	for i, c := range agents.Crawlers {
		patterns[c.Pattern] = true
		hand, written := crawlerPatternNames[c.Pattern]
		text, literal := literalName(c.Pattern)
		switch {
		case !written && !literal:
			t.Errorf("pattern %d, %q, is not a literal: give it a name in crawlerPatternNames, the name of the product or of its operator", i, c.Pattern)
		case written && listName(c.Pattern) != hand, !written && listName(c.Pattern) != text:
			t.Errorf("pattern %d, %q: name %q", i, c.Pattern, listName(c.Pattern))
		}
		if names[i] == "" {
			t.Errorf("pattern %d, %q, has no name", i, c.Pattern)
		}
	}
	for p := range crawlerPatternNames {
		if !patterns[p] {
			t.Errorf("crawlerPatternNames names %q, which is no pattern of the list", p)
		}
	}
	// The text of a literal is the name, even when it is a host name or a
	// URL.
	for pattern, want := range map[string]string{
		`Googlebot\/`:                       "Googlebot",
		`^curl`:                             "curl",
		`^Go-http-client$`:                  "Go-http-client",
		` crawler \/ `:                      "crawler",
		`updown\.io`:                        "updown.io",
		`loc\.gov\/programs\/web-archiving`: "loc.gov/programs/web-archiving",
	} {
		if got, ok := literalName(pattern); !ok || got != want {
			t.Errorf("%s: %q, %v; want %q", pattern, got, ok, want)
		}
	}
	for _, pattern := range []string{`[wW]get`, `a|b`, `^`, `(?i)bot`, `bot+`, `(`} {
		if got, ok := literalName(pattern); ok {
			t.Errorf("%s: a literal, %q", pattern, got)
		}
	}
}

// The names of the list that denote the same crawler as a name of uasurfer
// take its spelling: this table, and no more.
func TestCrawlerAliases(t *testing.T) {
	want := map[string]string{
		"bingbot":             "Bing Bot",
		"msnbot":              "MSN Bot",
		"Slurp":               "Yahoo Slurp",
		"facebookexternalhit": "Facebook Crawler",
		"coccoc":              "Coccoc Bot",
		"Applebot":            "Apple Bot",
		"Baiduspider":         "Baidu Spider",
		"DuckDuckBot":         "DuckDuck Bot",
		"LinkedInBot":         "LinkedIn Bot",
		"Twitterbot":          "Twitter Bot",
		"PerplexityBot":       "Perplexity Bot",
		"yandex.com/bots":     "Yandex Bot",
	}
	if !maps.Equal(crawlerAliases, want) {
		t.Errorf("crawlerAliases %v, want %v", crawlerAliases, want)
	}
	uasurferNames := values(uasurferCrawlers)
	listed := map[string]bool{}
	for i, c := range agents.Crawlers {
		name := listName(c.Pattern)
		listed[name] = true
		alias, aligned := crawlerAliases[name]
		for u := range uasurferNames {
			// A name equal to one of uasurfer but for its case, its spaces,
			// - and _ is aligned.
			if u != "" && name != u && fold(name) == fold(u) && alias != u {
				t.Errorf("pattern %d, %q, gives %q: align it with %q in crawlerAliases", i, c.Pattern, name, u)
			}
		}
		if got := crawlerNames()[i]; aligned && got != alias || !aligned && got != name {
			t.Errorf("pattern %d, %q: name %q", i, c.Pattern, got)
		}
	}
	for name, alias := range crawlerAliases {
		if !listed[name] {
			t.Errorf("crawlerAliases aligns %q, which no pattern of the list gives", name)
		}
		if !uasurferNames[alias] || alias == "" {
			t.Errorf("crawlerAliases aligns %q with %q, which is no name of uasurferCrawlers", name, alias)
		}
	}
	// Different crawlers of the same operators keep names of their own.
	for _, name := range []string{"OpenAI GPT", "OpenAI ChatGPT User", "OpenAI SearchBot", "Claude Bot", "Anthropic AI", "Googlebot Image", "Facebook Facebot", "Meta FacebookBot", "Bing Ads Bot", "Bing Preview", "Yahoo Japan"} {
		if !slices.Contains(crawlerNames(), name) {
			t.Errorf("%s is not a name of the list any more", name)
		}
		if uasurferNames[name] {
			t.Errorf("%s is a name of uasurfer", name)
		}
	}
}

// Two names that ua.bot.name can give, from the list, hand-written, aligned
// or from uasurfer, never differ only in case, spaces, - and _, as Applebot
// and Apple Bot do: an update of the list that brings such a pair fails here,
// to be aligned in crawlerPatternNames or crawlerAliases.
func TestBotNamesDistinct(t *testing.T) {
	spellings := map[string]string{}
	for _, name := range append(crawlerNames(), slices.Collect(maps.Values(uasurferCrawlers))...) {
		if name == "" {
			continue
		}
		key := fold(name)
		if other, ok := spellings[key]; ok && other != name {
			t.Errorf("the bot names %q and %q differ only in case, spaces, - and _: align them", other, name)
			continue
		}
		spellings[key] = name
	}
	// What the comparison ignores.
	for _, pair := range [][2]string{{"Applebot", "Apple Bot"}, {"Meta-ExternalHit", "meta externalhit"}, {"archive.org_bot", "Archive.org Bot"}} {
		if fold(pair[0]) != fold(pair[1]) {
			t.Errorf("%q and %q are different names", pair[0], pair[1])
		}
	}
	if fold("Mail.ru") == fold("Mailru") {
		t.Error("the comparison ignores the dot")
	}
}

// fold returns name in lower case, without its white space, - and _: two
// bot names with the same fold are one name in two spellings.
func fold(name string) string {
	return strings.ToLower(strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || r == '-' || r == '_' {
			return -1
		}
		return r
	}, name))
}

// Bots gives every name that a record can give, once: each name of the list,
// with the description of the first of its patterns that has one, and each
// name of uasurfer that the list does not give, without a description.
func TestBots(t *testing.T) {
	want := map[string]string{}
	for i, name := range crawlerNames() {
		if d, ok := want[name]; !ok || d == "" {
			want[name] = crawlerDescriptions()[i]
		}
	}
	for _, name := range uasurferCrawlers {
		if _, ok := want[name]; !ok && name != "" {
			want[name] = ""
		}
	}
	got := map[string]string{}
	for _, b := range Bots() {
		if _, ok := got[b.Name]; ok {
			t.Errorf("%q twice", b.Name)
		}
		got[b.Name] = b.Description
	}
	if !maps.Equal(got, want) {
		for name, d := range want {
			if g, ok := got[name]; !ok || g != d {
				t.Errorf("%q: description %q, %v; want %q", name, g, ok, d)
			}
		}
		for name := range got {
			if _, ok := want[name]; !ok {
				t.Errorf("%q is no name that a record can give", name)
			}
		}
	}
	// A name of uasurfer that the list gives too has the description of the
	// list.
	if got["Googlebot"] == "" {
		t.Error("Googlebot without a description")
	}
}

// Every name that ua.bot.name can give is a value of a string set, in values
// and in the file of a set, where it is the whole line: a set of bot names can
// hold any of them, for sets.<name>.contains(ua.bot.name). An update of the
// list that brings a name that is not, such as one with a # that starts a
// comment in a file, fails here, until it has a name in crawlerPatternNames.
func TestBotNamesInStringSets(t *testing.T) {
	for _, b := range Bots() {
		fromValues, err := sets.FromValues(sets.String, []string{b.Name}, 1)
		if err != nil {
			t.Errorf("the bot name %q is not a value of a string set: %v", b.Name, err)
			continue
		}
		fromFile, err := sets.Read(sets.String, strings.NewReader(b.Name+"\n"), 1)
		if err != nil {
			t.Errorf("the bot name %q is not a line of the file of a string set: %v", b.Name, err)
			continue
		}
		if !fromValues.ContainsString(b.Name) || !fromFile.ContainsString(b.Name) {
			t.Errorf("the bot name %q is another value in a string set", b.Name)
		}
	}
}

// botNames are the names that Bots gives, and listBots the name and the
// description of each pattern of the list.
var (
	botNames = sync.OnceValue(func() map[string]bool {
		out := map[string]bool{}
		for _, b := range Bots() {
			out[b.Name] = true
		}
		return out
	})
	listBots = sync.OnceValue(func() map[Bot]bool {
		out := map[Bot]bool{}
		for i, name := range crawlerNames() {
			out[Bot{Name: name, Description: crawlerDescriptions()[i]}] = true
		}
		return out
	})
)

// The values of the record hold for any header.
func checkRecord(t *testing.T, header string, r Record) {
	t.Helper()
	known := func(name string, tables ...map[string]bool) bool {
		for _, table := range tables {
			if table[name] {
				return true
			}
		}
		return name == ""
	}
	if !known(r.Device, set(Devices...)) || r.Device == "" {
		t.Fatalf("%q: device %q", header, r.Device)
	}
	if r.IsBot {
		if r.Device != DeviceBot || r.Browser != (Browser{}) || r.OS != (OS{}) {
			t.Fatalf("%q: a bot with %+v", header, r)
		}
		// A bot has a name of Bots, with the description of a pattern of
		// that name or none, or has no name and no description.
		if r.Bot != (Bot{}) && (!botNames()[r.Bot.Name] || r.Bot.Description != "" && !listBots()[r.Bot]) {
			t.Fatalf("%q: bot %+v", header, r.Bot)
		}
		return
	}
	if r.Device == DeviceBot || r.Bot != (Bot{}) {
		t.Fatalf("%q: not a bot, with %+v", header, r)
	}
	if !known(r.Browser.Name, values(browserNames), set(browserEdge)) || !known(r.OS.Name, values(osNames)) {
		t.Fatalf("%q: names %q, %q", header, r.Browser.Name, r.OS.Name)
	}
	// A version is major.minor.patch, its parts are those of the version,
	// and 0 without a version.
	b, o := r.Browser, r.OS
	if b.Version == "" && (b.Major != 0 || b.Minor != 0) || b.Version != "" && !strings.HasPrefix(b.Version, fmt.Sprintf("%d.%d.", b.Major, b.Minor)) {
		t.Fatalf("%q: browser %+v", header, b)
	}
	if o.Version == "" && (o.Major != 0 || o.Minor != 0) || o.Version != "" && !strings.HasPrefix(o.Version, fmt.Sprintf("%d.%d.", o.Major, o.Minor)) {
		t.Fatalf("%q: operating system %+v", header, o)
	}
	for _, v := range []string{b.Version, o.Version} {
		if v != "" && strings.Count(v, ".") != 2 {
			t.Fatalf("%q: version %q", header, v)
		}
	}
}

func set(names ...string) map[string]bool {
	out := map[string]bool{}
	for _, n := range names {
		out[n] = true
	}
	return out
}

func values[K comparable](m map[K]string) map[string]bool {
	out := map[string]bool{}
	for _, v := range m {
		out[v] = true
	}
	return out
}

// FuzzAnalyze analyzes any header, without recovering a panic: a panic of a
// library fails it. The record has the values of its attributes only,
// depends on the first MaxBytes bytes only, and is the same from the cache.
func FuzzAnalyze(f *testing.F) {
	for _, h := range []string{chromeWindows, edgeWindows, edgeLegacy, edgeAndroid, edgeIOS, ie11, safariIPhone, safariIPad, safariMac, firefoxLinux, chromeTablet, chromeOS, facebookAndroid, playStation, tizenTV, googlebot, bingbot, claudeBot, curl, sqlmap, "", "x", "\xff", strings.Repeat("Mozilla/5.0 ", 60)} {
		f.Add(h)
	}
	cached := New(64)
	f.Fuzz(func(t *testing.T, header string) {
		r := New(0).Analyze(header)
		if header == "" {
			if r != (Record{Device: DeviceUnknown}) {
				t.Fatalf("empty header: %+v", r)
			}
			return
		}
		checkRecord(t, header, r)
		if len(header) > MaxBytes {
			if prefix := New(0).Analyze(header[:MaxBytes]); prefix != r {
				t.Fatalf("%q: %+v, and %+v for its first bytes", header, r, prefix)
			}
		}
		for range 2 {
			if got := cached.Analyze(header); got != r {
				t.Fatalf("%q: %+v from the cache, %+v without", header, got, r)
			}
		}
		if cached.Entries() > 64 {
			t.Fatalf("%d entries", cached.Entries())
		}
	})
}
