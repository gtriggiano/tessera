package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestSecretHeaders(t *testing.T) {
	c := mustParse(t, "policy: {default_action: deny}\nsecret_headers:\n  - X-Api-Key\n  - x-session-token\n", nil)
	if want := []string{"X-Api-Key", "x-session-token"}; !reflect.DeepEqual(c.SecretHeaders, want) {
		t.Fatalf("secret_headers %q", c.SecretHeaders)
	}
	if want := []string{"x-api-key", "x-session-token"}; !reflect.DeepEqual(c.SecretHeaderNames(), want) {
		t.Errorf("names %q", c.SecretHeaderNames())
	}
	if names := mustParse(t, "policy: {default_action: deny}\n", nil).SecretHeaderNames(); names == nil || len(names) != 0 {
		t.Errorf("default %q", names)
	}
	// The largest number of headers.
	items := make([]string, MaxSecretHeaders)
	for i := range items {
		items[i] = "x-h" + strings.Repeat("a", i)
	}
	if c := mustParse(t, "policy: {default_action: deny}\nsecret_headers: ["+strings.Join(items, ", ")+"]\n", nil); len(c.SecretHeaders) != MaxSecretHeaders {
		t.Errorf("%d headers", len(c.SecretHeaders))
	}

	// Printed as written, a substitution redacted, it reads back the same.
	c = mustParse(t, "policy: {default_action: deny}\nsecret_headers: [X-Api-Key, \"${TOKEN_HEADER}\"]\n", map[string]string{"TOKEN_HEADER": "x-" + secret})
	got, err := c.Redacted()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "\nsecret_headers:\n  - X-Api-Key\n  - '[REDACTED]'\nenrichment: {}\n") || strings.Contains(string(got), secret) {
		t.Fatalf("printed:\n%s", got)
	}
	again := mustParse(t, strings.Replace(string(got), "'[REDACTED]'", "x-"+secret, 1), nil)
	if !reflect.DeepEqual(again.SecretHeaders, c.SecretHeaders) {
		t.Errorf("reads back as %q", again.SecretHeaders)
	}
}

func TestInvalidSecretHeaders(t *testing.T) {
	const head = "policy: {default_action: deny}\nsecret_headers:\n"
	many := make([]string, MaxSecretHeaders+1)
	for i := range many {
		many[i] = "  - x-h" + strings.Repeat("a", i) + "\n"
	}
	for _, tc := range []struct{ name, yaml, want string }{
		{"not a list", "policy: {default_action: deny}\nsecret_headers: x-api-key\n", "secret_headers (line 2, column 17): expected a list"},
		{"item not a string", head + "  - x-api-key\n  - 1\n", "secret_headers[1] (line 4, column 5): expected a string"},
		{"null item", head + "  - x-api-key\n  -\n", "secret_headers[1] (line 4, column 4): null is not allowed; omit the key to use its default"},
		{"name", head + "  - \"x " + secret + "\"\n", "secret_headers[0] (line 3, column 5): expected a header name"},
		{"empty name", head + "  - \"\"\n", "secret_headers[0] (line 3, column 5): expected a header name"},
		{"pseudo-header", head + "  - \":authority\"\n", "secret_headers[0] (line 3, column 5): expected a header name"},
		{"authorization", head + "  - x-api-key\n  - Authorization\n", "secret_headers[1] (line 4, column 5): this header is always secret"},
		{"proxy-authorization", head + "  - proxy-authorization\n", "secret_headers[0] (line 3, column 5): this header is always secret"},
		{"cookie", head + "  - COOKIE\n", "secret_headers[0] (line 3, column 5): this header is always secret"},
		{"twice", head + "  - x-" + secret + "\n  - x-" + secret + "\n", "secret_headers[1] (line 4, column 5): the same header twice"},
		{"twice in different cases", head + "  - x-api-key\n  - X-API-KEY\n", "secret_headers[1] (line 4, column 5): the same header twice, in different cases"},
		{"too many", head + strings.Join(many, ""), "secret_headers[64] (line 67, column 5): more than 64 headers"},
		{"substituted name", "policy: {default_action: deny}\nsecret_headers: [\"${H}\"]\n", "secret_headers[0] (line 2, column 18): expected a header name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse(t, tc.yaml, map[string]string{"H": "x " + secret})
			if err == nil {
				t.Fatal("Parse succeeded")
			}
			if err.Error() != tc.want {
				t.Fatalf("error %q\nwant  %q", err, tc.want)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatal("the error repeats a configured value")
			}
		})
	}
}

// A decision field cannot read a secret header, built in or configured, in
// any spelling; an upstream header can.
func TestDecisionFieldsAndSecretHeaders(t *testing.T) {
	const head = "policy: {default_action: deny}\nsecret_headers: [X-Api-Key, token]\nlog:\n  decision_fields:\n"
	const message = "a decision field cannot read a secret header"
	for _, tc := range []struct{ field, want string }{
		{`    a: request.headers["x-api-key"]`, "log.decision_fields (line 5, column 8): " + message},
		{`    a: request.headers["X-API-KEY"]`, "log.decision_fields (line 5, column 8): " + message},
		{`    a: request.headers.token`, "log.decision_fields (line 5, column 8): " + message},
		{`    a: request.headers.Token`, "log.decision_fields (line 5, column 8): " + message},
		{`    a: '"x-api-key" in request.headers ? "y" : "n"'`, "log.decision_fields (line 5, column 24): " + message},
		{`    a: request.headers["proxy-authorization"]`, "log.decision_fields (line 5, column 8): " + message},
		{`    a: request.host + request.headers["Proxy-Authorization"]`, "log.decision_fields (line 5, column 23): " + message},
		{`    a: request.headers["authorization"]`, "log.decision_fields (line 5, column 8): " + message},
		{`    a: request.headers.cookie`, "log.decision_fields (line 5, column 8): " + message},
	} {
		_, err := parse(t, head+tc.field+"\n", nil)
		if err == nil || err.Error() != tc.want {
			t.Errorf("%s: error %v\nwant %s", tc.field, err, tc.want)
		}
	}
	// Another header, and the same header in an upstream header.
	mustParse(t, head+"    a: request.headers[\"x-api-key-id\"]\n", nil)
	mustParse(t, "policy: {default_action: deny}\nsecret_headers: [X-Api-Key]\nupstream:\n  headers:\n    x-key: request.headers[\"x-api-key\"]\n", nil)
	// A name of secret_headers can be an upstream header; proxy-authorization
	// cannot (see TestInvalidUpstreamHeaders).
	mustParse(t, "policy: {default_action: deny}\nsecret_headers: [X-Api-Key]\nupstream:\n  headers:\n    x-api-key: '\"k-1\"'\n", nil)
}
