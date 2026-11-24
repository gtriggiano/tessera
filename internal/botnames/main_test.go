package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gtriggiano/tessera/internal/useragent"
)

// The page has a row for each name that ua.bot.name can give, once, in the
// order of the names whatever their case, after the header and inside the
// v-pre container.
func TestPage(t *testing.T) {
	bots := useragent.Bots()
	got := page(bots)
	if !strings.HasPrefix(got, header) || !strings.HasSuffix(got, "\n:::\n") {
		t.Fatalf("the page does not start with the header or end with the container:\n%.300s", got)
	}
	rows := strings.Split(strings.TrimSuffix(strings.TrimPrefix(got, header), ":::\n"), "\n")
	rows = rows[:len(rows)-1]
	if len(rows) != len(bots) {
		t.Fatalf("%d rows for %d bots", len(rows), len(bots))
	}
	want := map[string]bool{}
	for _, b := range bots {
		want["| "+code(b.Name)+" | "+text(b.Description)+" |"] = true
	}
	var previous string
	for i, row := range rows {
		if !want[row] {
			t.Errorf("row %d, %q, is no bot", i+1, row)
		}
		name := strings.ToLower(strings.Trim(strings.SplitN(row, " | ", 2)[0], "| `"))
		if name <= previous {
			t.Errorf("row %d, %q, after %q", i+1, row, previous)
		}
		previous = name
	}
	// The name of a bot that a pattern of the list names, such as Googlebot,
	// has one row, with the description of the list; so has the name of a
	// bot that only the analysis of browsers recognizes. The names are those
	// of the analysis, which the tests of internal/useragent fix.
	listed := useragent.New(0).Analyze("Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)").Bot
	if listed.Name == "" || listed.Description == "" || strings.Count(got, "| "+code(listed.Name)+" | "+text(listed.Description)+" |\n") != 1 {
		t.Errorf("Googlebot: %+v, not one row with its description", listed)
	}
	fallback := useragent.New(0).Analyze("Mozilla/5.0 (compatible; googlebot)").Bot
	if fallback.Name == "" || strings.Count(got, "| "+code(fallback.Name)+" |") != 1 {
		t.Errorf("googlebot, which only the analysis of browsers recognizes: %+v, not one row", fallback)
	}
}

// The rows are sorted by name whatever its case; what Markdown, HTML, a line
// break or a plugin of the site would read in a name or a description is
// escaped: no attribute, emoji or link comes from a description.
func TestRows(t *testing.T) {
	got := strings.TrimPrefix(page([]useragent.Bot{
		{Name: "beta"},
		{Name: "Alpha", Description: "a"},
		{Name: "alpha2"},
		{Name: "a|b", Description: "x <script> & *y* _z_ [l](u) \\ | ~ `\nnext\tline"},
		{Name: "`tick`", Description: " around "},
		{Name: "a``b", Description: "{{ x }} {.red} :smile:"},
		{Name: "links", Description: "see https://example.com/bot, mailto:bot@example.com and bot@example.com"},
	}), header)
	want := strings.Join([]string{
		"| `` `tick` `` | around |",
		"| ```a``b``` | &#123;&#123; x &#125;&#125; &#123;.red&#125; &#58;smile&#58; |",
		"| `Alpha` | a |",
		"| `alpha2` |  |",
		"| `a\\|b` | x \\<script\\> \\& \\*y\\* \\_z\\_ \\[l\\](u) \\\\ \\| \\~ \\` next line |",
		"| `beta` |  |",
		"| `links` | see https&#58;//example.com/bot, mailto&#58;bot&#64;example.com and bot&#64;example.com |",
		":::",
		"",
	}, "\n")
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// The program writes the page to the file it is given.
func TestWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot-names.md")
	if err := write(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != page(useragent.Bots()) {
		t.Error("the file is not the page")
	}
	if err := write(filepath.Join(t.TempDir(), "missing", "bot-names.md")); err == nil {
		t.Error("no error for a file in a missing directory")
	}
}
