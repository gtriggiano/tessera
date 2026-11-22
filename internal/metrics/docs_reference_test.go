package metrics

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// metricsReference is the metrics reference that this test keeps equal to
// the registry.
var metricsReference = filepath.Join("..", "..", "docs", "reference", "metrics.md")

// family is the type and the sorted label names of one tessera_ family.
type family struct {
	typ    string
	labels []string
}

// exportedFamilies returns the tessera_ families of the registry that serve
// builds. A gather leaves out a family without series, such as a vector
// without children: the test fails on a registered family that it does not
// see, so that no family escapes the comparison.
func exportedFamilies(t *testing.T) map[string]family {
	t.Helper()
	m := New(testBuild, testHash)
	got := map[string]family{}
	for name, f := range gather(t, m) {
		if !strings.HasPrefix(name, namespace+"_") {
			continue
		}
		e := family{typ: strings.ToLower(f.GetType().String())}
		if f.GetType() == dto.MetricType_HISTOGRAM {
			t.Errorf("%s is a histogram: teach this test to compare its buckets with a **Buckets:** line", name)
		}
		for _, l := range f.GetMetric()[0].GetLabel() {
			e.labels = append(e.labels, l.GetName())
		}
		slices.Sort(e.labels)
		got[name] = e
	}
	descs := make(chan *prometheus.Desc)
	go func() {
		m.registry.Describe(descs)
		close(descs)
	}()
	for d := range descs {
		match := fqName.FindStringSubmatch(d.String())
		if match == nil {
			t.Fatalf("cannot read the name of %s", d)
		}
		if name := match[1]; strings.HasPrefix(name, namespace+"_") {
			if _, ok := got[name]; !ok {
				t.Errorf("%s is registered but has no series here: make an observation in this test", name)
			}
		}
	}
	return got
}

var (
	fqName        = regexp.MustCompile(`fqName: "([^"]+)"`)
	familyHeading = regexp.MustCompile("^### `(" + namespace + "_[a-z0-9_]+)`$")
	summaryRow    = regexp.MustCompile("^\\| \\[`(" + namespace + "_[a-z0-9_]+)`\\]\\(#([a-z0-9-]+)\\) \\| ([a-z]+) \\| (.+) \\|$")
	codeSpan      = regexp.MustCompile("`([^`]+)`")
)

// labelList parses "none" or a comma-separated list of label names in
// backticks.
func labelList(t *testing.T, line int, name, text string) []string {
	t.Helper()
	var labels []string
	if text != "none" {
		for _, m := range codeSpan.FindAllStringSubmatch(text, -1) {
			labels = append(labels, m[1])
		}
		if len(labels) == 0 {
			t.Errorf("%s:%d: %s: labels must be \"none\" or names in backticks", metricsReference, line, name)
		}
	}
	slices.Sort(labels)
	return labels
}

// documentedFamilies reads the metrics reference. Each family has a heading
// "### `tessera_<name>`" followed by the lines "**Type:** <type>" and
// "**Labels:** none" or "**Labels:** `a`, `b`". The table at the top of the
// page has a row "| [`tessera_<name>`](#anchor) | <type> | <labels> |" per
// family; it is returned apart.
func documentedFamilies(t *testing.T) (sections, summary map[string]family) {
	t.Helper()
	f, err := os.Open(metricsReference)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sections, summary = map[string]family{}, map[string]family{}
	seenType, seenLabels := map[string]bool{}, map[string]bool{}
	current, fenced := "", false
	scanner := bufio.NewScanner(f)
	for line := 1; scanner.Scan(); line++ {
		text := scanner.Text()
		if strings.HasPrefix(strings.TrimSpace(text), "```") {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		if strings.HasPrefix(text, "#") {
			current = ""
			if m := familyHeading.FindStringSubmatch(text); m != nil {
				current = m[1]
				if _, dup := sections[current]; dup {
					t.Errorf("%s:%d: %s is documented twice", metricsReference, line, current)
				}
				sections[current] = family{}
			} else if strings.Contains(text, "`"+namespace+"_") {
				t.Errorf("%s:%d: heading %q is not \"### \" and one family name in backticks", metricsReference, line, text)
			}
			continue
		}
		if m := summaryRow.FindStringSubmatch(text); m != nil {
			name := m[1]
			if _, dup := summary[name]; dup {
				t.Errorf("%s:%d: %s has two rows in the table", metricsReference, line, name)
			}
			if want := strings.ReplaceAll(name, "_", "-"); m[2] != want {
				t.Errorf("%s:%d: %s links to #%s, want #%s", metricsReference, line, name, m[2], want)
			}
			summary[name] = family{typ: m[3], labels: labelList(t, line, name, m[4])}
			continue
		}
		if current == "" {
			continue
		}
		doc := sections[current]
		value := strings.TrimSuffix(text, `\`)
		switch {
		case strings.HasPrefix(value, "**Type:** "):
			doc.typ = strings.TrimPrefix(value, "**Type:** ")
			seenType[current] = true
		case strings.HasPrefix(value, "**Labels:** "):
			doc.labels = labelList(t, line, current, strings.TrimPrefix(value, "**Labels:** "))
			seenLabels[current] = true
		}
		sections[current] = doc
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if fenced {
		t.Errorf("%s: a code fence is not closed", metricsReference)
	}
	for name := range sections {
		if !seenType[name] || !seenLabels[name] {
			t.Errorf("%s: %s has no **Type:** or no **Labels:** line", metricsReference, name)
		}
	}
	return sections, summary
}

// TestMetricsReference keeps docs/reference/metrics.md and the registry
// equal: every exported tessera_ family has a heading with its type and its
// labels, and a row in the table at the top of the page; every documented
// family is exported.
func TestMetricsReference(t *testing.T) {
	exported := exportedFamilies(t)
	documented, summary := documentedFamilies(t)
	if len(exported) == 0 || len(documented) == 0 {
		t.Fatalf("found %d exported and %d documented families", len(exported), len(documented))
	}
	for name, e := range exported {
		d, ok := documented[name]
		if !ok {
			t.Errorf("%s is exported but has no heading in %s", name, metricsReference)
			continue
		}
		if d.typ != e.typ {
			t.Errorf("%s: documented type %q, exported %q", name, d.typ, e.typ)
		}
		if !slices.Equal(d.labels, e.labels) {
			t.Errorf("%s: documented labels %v, exported %v", name, d.labels, e.labels)
		}
	}
	for name, d := range documented {
		if _, ok := exported[name]; !ok {
			t.Errorf("%s is documented in %s but not exported", name, metricsReference)
		}
		row, ok := summary[name]
		switch {
		case !ok:
			t.Errorf("%s has no row in the table of %s", name, metricsReference)
		case row.typ != d.typ || !slices.Equal(row.labels, d.labels):
			t.Errorf("%s: its row says %s %v, its section %s %v", name, row.typ, row.labels, d.typ, d.labels)
		}
	}
	for name := range summary {
		if _, ok := documented[name]; !ok {
			t.Errorf("%s has a row but no heading in %s", name, metricsReference)
		}
	}
}
