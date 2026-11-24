// Command botnames writes the bot names reference of the documentation site:
// every bot that ua.bot.name can name, with the description that
// ua.bot.description gives with it, from the tables of tessera and the list of
// crawler-user-agents of go.mod. The documentation build runs it before
// VitePress, in make docs-build and make docs-dev, so that the page follows
// the code; Git ignores the page. It is not part of the tessera binary.
//
// Usage:
//
//	go run ./internal/botnames docs/reference/bot-names.md
package main

import (
	"cmp"
	"fmt"
	"os"
	"slices"
	"strings"
	"unicode"

	"github.com/gtriggiano/tessera/internal/useragent"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: go run ./internal/botnames <page>")
		os.Exit(2)
	}
	if err := write(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "botnames:", err)
		os.Exit(1)
	}
}

// write writes the page of the bots of tessera to path.
func write(path string) error {
	return os.WriteFile(path, []byte(page(useragent.Bots())), 0o644)
}

// header is the page before the table of the bots. The table is in a v-pre
// container, so that VitePress does not read the text of a name or of a
// description as Vue.
const header = `---
description: "Every name that tessera gives to a bot in ua.bot.name, with the description that ua.bot.description gives with it."
llms: optional
editLink: false
---

# Bot names reference

This page lists every name that **tessera** can give to a bot in ` + "`ua.bot.name`" + `, once, with the description that ` + "`ua.bot.description`" + ` gives with it, in the order of the names, whatever their case. How tessera recognizes and names a bot is in the [expressions reference](./expressions.md#ua).

The names and the descriptions depend on the version of the list of crawler-user-agents that tessera is built with: this page lists those of the version of tessera that this site documents.

` + "`ua.bot.description`" + ` is empty when only the analysis of browsers recognizes the bot, and no pattern of the list matches its header, even for a name that has a description here; a name without a description here is one that the list does not describe, such as one that only that analysis gives. A name that several patterns of the list give has here the description of the first of them, and in ` + "`ua.bot.description`" + ` the one of the pattern that matches.

::: v-pre
| Name | Description |
| --- | --- |
`

// page returns the page of bots: a row for each, sorted by name whatever its
// case.
func page(bots []useragent.Bot) string {
	bots = slices.Clone(bots)
	slices.SortFunc(bots, func(a, b useragent.Bot) int {
		return cmp.Or(
			cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)),
			cmp.Compare(a.Name, b.Name),
			cmp.Compare(a.Description, b.Description),
		)
	})
	var out strings.Builder
	out.WriteString(header)
	for _, b := range bots {
		fmt.Fprintf(&out, "| %s | %s |\n", code(b.Name), text(b.Description))
	}
	out.WriteString(":::\n")
	return out.String()
}

// code returns s as a code span in a cell of a table: between more backticks
// than any run of them in s, and with each | escaped, which would end the
// cell.
func code(s string) string {
	s = oneLine(s)
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") {
		s = " " + s + " "
	}
	fence := strings.Repeat("`", longest+1)
	return fence + strings.ReplaceAll(s, "|", `\|`) + fence
}

// markup are the characters that Markdown or HTML can read as markup in the
// text of a cell of a table.
const markup = "\\`*_[]<>|&~"

// entities are the characters that the plugins of the site can read in text:
// attributes in braces, an emoji between colons, and the scheme of a link or
// an email address, which become links. A description is the text of others:
// each of these characters is an HTML entity, which none of them reads.
var entities = map[rune]string{'{': "&#123;", '}': "&#125;", ':': "&#58;", '@': "&#64;"}

// text returns s as the text of a cell of a table: on one line, with a
// backslash before each character of markup, and the characters of entities
// as HTML entities.
func text(s string) string {
	var out strings.Builder
	for _, r := range oneLine(s) {
		if e, ok := entities[r]; ok {
			out.WriteString(e)
			continue
		}
		if strings.ContainsRune(markup, r) {
			out.WriteByte('\\')
		}
		out.WriteRune(r)
	}
	return out.String()
}

// oneLine returns s with each run of white space or control characters
// replaced by a space, and none around it: a line break would end the row.
func oneLine(s string) string {
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}), " ")
}
