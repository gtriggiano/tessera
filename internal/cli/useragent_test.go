package cli

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/gtriggiano/tessera/internal/useragent"
)

// userAgentConfig reads ua in its rules and in an upstream header.
const userAgentConfig = `enrichment:
  user_agent: {}
policy:
  default_action: allow
  rules:
    - name: bots
      action: deny
      when: ua.is_bot && ua.bot.name != "Googlebot"
    - name: old-ios
      action: deny
      when: ua.os.name == "iOS" && ua.browser.major < 15
upstream:
  headers:
    x-client-device: ua.device
`

// eval shows the attributes of ua that the rules read, for the user-agent
// header of --header: strings in quotes, the bool and the integer without.
func TestEvalUserAgent(t *testing.T) {
	path := writeConfig(t, userAgentConfig)
	curl := expectedBot(t, "curl/8.7.1")
	for _, tc := range []struct {
		name   string
		args   []string
		output string
	}{
		{"a browser", []string{"--header", "user-agent: Mozilla/5.0 (iPhone; CPU iPhone OS 14_8 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/14.1.2 Mobile/15E148 Safari/604.1"}, `ua.bot.name       ""
ua.browser.major  14
ua.is_bot         false
ua.os.name        "iOS"

bots     false
old-ios  true -> deny

decision: deny (rule old-ios, reason rule)
response: status 403
`},
		{"a bot", []string{"--header", "User-Agent: curl/8.7.1"}, `ua.bot.name  ` + strconv.Quote(curl.Name) + `
ua.is_bot    true

bots  true -> deny

decision: deny (rule bots, reason rule)
response: status 403
`},
		{"no header", nil, `ua.bot.name       ""
ua.browser.major  0
ua.is_bot         false
ua.os.name        ""

bots     false
old-ios  false

decision: allow (rule default, reason default)

x-client-device  "unknown"
`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errOut := execute(append([]string{"eval", "--config", path}, tc.args...))
			if code != 0 || errOut != "" || out != tc.output {
				t.Fatalf("code=%d stderr=%q stdout:\n%s\nwant:\n%s", code, errOut, out, tc.output)
			}
		})
	}
	code, out, errOut := execute([]string{"eval", "--config", path, "--header", "user-agent: curl/8.7.1", "--output", "json"})
	name, err := json.Marshal(curl.Name)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"attributes":[{"name":"ua.bot.name","value":` + string(name) + `},{"name":"ua.is_bot","value":"true"}],"rules":[{"name":"bots","action":"deny","mode":"enforce","result":"true"}],"decision":{"action":"deny","enforced":true,"rule":"bots","reason":"rule","response":{"status":403,"body":"","headers":{}}}}` + "\n"
	if code != 0 || errOut != "" || out != want {
		t.Fatalf("json: code=%d stderr=%q stdout:\n%s\nwant:\n%s", code, errOut, out, want)
	}
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

// eval shows an object of ua that a rule reads whole as a CEL literal, its
// strings in quotes and its integers without, and a field of an object
// under its own name; with --output json, the literal is the value.
func TestEvalUserAgentObjects(t *testing.T) {
	path := writeConfig(t, `enrichment:
  user_agent: {}
policy:
  default_action: allow
  rules:
    - name: objects
      action: deny
      when: ua.browser == ua.browser && ua.os == ua.os && ua.bot == ua.bot && ua.is_bot
    - name: fields
      action: deny
      when: ua.browser.minor == 0 && ua.os.major == 10 && ua.os.minor == 0 && ua.bot.description == "" && ua.browser.version != ""
`)
	googlebot, curlBot := expectedBot(t, "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)"), expectedBot(t, "curl/8.7.1")
	for _, tc := range []struct {
		name   string
		agent  string
		output string
	}{
		{"a browser", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.6778.86 Safari/537.36", `ua.bot              {name: "", description: ""}
ua.bot.description  ""
ua.browser          {name: "Chrome", version: "131.0.6778", major: 131, minor: 0}
ua.browser.minor    0
ua.browser.version  "131.0.6778"
ua.is_bot           false
ua.os               {name: "Windows", version: "10.0.0", major: 10, minor: 0}
ua.os.major         10
ua.os.minor         0

objects  false
fields   true -> deny

decision: deny (rule fields, reason rule)
response: status 403
`},
		{"a bot", "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)", `ua.bot      {name: ` + strconv.Quote(googlebot.Name) + `, description: ` + strconv.Quote(googlebot.Description) + `}
ua.browser  {name: "", version: "", major: 0, minor: 0}
ua.is_bot   true
ua.os       {name: "", version: "", major: 0, minor: 0}

objects  true -> deny

decision: deny (rule objects, reason rule)
response: status 403
`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errOut := execute([]string{"eval", "--config", path, "--header", "user-agent: " + tc.agent})
			if code != 0 || errOut != "" || out != tc.output {
				t.Fatalf("code=%d stderr=%q stdout:\n%s\nwant:\n%s", code, errOut, out, tc.output)
			}
		})
	}
	code, out, errOut := execute([]string{"eval", "--config", path, "--header", "user-agent: curl/8.7.1", "--output", "json"})
	curl, err := json.Marshal(`{name: ` + strconv.Quote(curlBot.Name) + `, description: ` + strconv.Quote(curlBot.Description) + `}`)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"attributes":[{"name":"ua.bot","value":` + string(curl) + `},{"name":"ua.browser","value":"{name: \"\", version: \"\", major: 0, minor: 0}"},{"name":"ua.is_bot","value":"true"},{"name":"ua.os","value":"{name: \"\", version: \"\", major: 0, minor: 0}"}],"rules":[{"name":"objects","action":"deny","mode":"enforce","result":"true"}],"decision":{"action":"deny","enforced":true,"rule":"objects","reason":"rule","response":{"status":403,"body":"","headers":{}}}}` + "\n"
	if code != 0 || errOut != "" || out != want {
		t.Fatalf("json: code=%d stderr=%q stdout:\n%s\nwant:\n%s", code, errOut, out, want)
	}
}

// config validate warns about the analysis when nothing reads ua, and stops
// on ua without it.
func TestConfigValidateUserAgent(t *testing.T) {
	code, out, errOut := execute([]string{"config", "validate", "--config", writeConfig(t, "enrichment:\n  user_agent: {}\npolicy:\n  default_action: deny\n")})
	if code != 0 || out != "configuration valid\n" || errOut != "tessera: warning: enrichment.user_agent (line 2, column 3): the enrichment is not used by any rule, upstream header or decision field\n" {
		t.Fatalf("unused: code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	code, out, errOut = execute([]string{"config", "validate", "--config", writeConfig(t, userAgentConfig)})
	if code != 0 || out != "configuration valid\n" || errOut != "" {
		t.Fatalf("used: code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	code, out, errOut = execute([]string{"config", "validate", "--config", writeConfig(t, strings.Replace(userAgentConfig, "enrichment:\n  user_agent: {}\n", "", 1))})
	if code != 2 || out != "" || errOut != "tessera: policy.rules[0].when (line 6, column 13): ua is not available: the configuration has no enrichment.user_agent\n" {
		t.Fatalf("without the analysis: code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	code, out, errOut = execute([]string{"config", "print", "--config", writeConfig(t, userAgentConfig)})
	if code != 0 || errOut != "" || !strings.Contains(out, "enrichment:\n  user_agent:\n    cache_entries: 10000\n") {
		t.Fatalf("config print: code=%d stdout=%q stderr=%q", code, out, errOut)
	}
}
