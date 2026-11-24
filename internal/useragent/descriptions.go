package useragent

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	_ "unsafe" // for go:linkname

	agents "github.com/monperrus/crawler-user-agents"
)

// crawlersJSON is the list of crawler-user-agents as its module embeds it:
// the unexported variable crawlersJson of the module, read through
// go:linkname. The module reads the list into agents.Crawlers without the
// description of each crawler, which ua.bot.description gives. Reading the
// bytes that the module embeds gives the descriptions of the version of the
// list in go.mod, with no copy of the list in this repository for an update
// of the module to leave behind.
//
// The linker checks neither that the variable exists nor its type, so the
// build does not fail when a version of the module renames the variable or
// changes its type: renamed, crawlersJSON links to an empty slice without an
// error. tessera then starts and analyzes headers all the same, with every
// description empty, and TestCrawlerDescriptions, which matches the entries
// of this JSON with agents.Crawlers one to one, fails, and so does make
// check: that test is the guard. Then point the directive at the variable
// that now holds the JSON, a []byte, or read the descriptions from
// agents.Crawlers if the module gives them there, and remove the directive;
// if neither is possible, keep the previous version of the module. The
// section on Dependabot of CONTRIBUTING.md says the same.
//
// The bytes belong to the module: tessera reads them, once, and never writes
// or appends to them.
//
//go:linkname crawlersJSON github.com/monperrus/crawler-user-agents.crawlersJson
var crawlersJSON []byte

// maxJSONBytes bounds the length of crawlersJSON that tessera reads. The JSON
// of the list is well under it; a variable of another type can hold anything
// in the place of the length of a slice, and reading that many bytes could
// reach memory that is not mapped, which no recover catches.
const maxJSONBytes = 64 << 20

// crawlerDescriptions returns the description of each pattern of the list of
// crawler-user-agents, by its index in the list, read from crawlersJSON once,
// without the white space around it.
// When crawlersJSON cannot be read, or its entries do not match the crawlers
// of agents.Crawlers one to one, every description is empty.
var crawlerDescriptions = sync.OnceValue(func() []string {
	return descriptions(crawlersJSON, agents.Crawlers)
})

// descriptions returns the descriptions that readDescriptions reads, or,
// when it fails, an empty description for each crawler.
func descriptions(data []byte, crawlers []agents.Crawler) []string {
	out, err := readDescriptions(data, crawlers)
	if err != nil {
		return make([]string, len(crawlers))
	}
	return out
}

// readDescriptions returns the description of each entry of data, the JSON
// of the list of crawler-user-agents, by its index, without the white space
// around it, when the entries match crawlers one to one: as many, with the
// same patterns in the same order.
func readDescriptions(data []byte, crawlers []agents.Crawler) (out []string, err error) {
	if len(data) == 0 {
		return nil, errors.New("the JSON of crawler-user-agents is empty: the module has no variable crawlersJson any more, which crawlersJSON reads through go:linkname")
	}
	if len(data) > maxJSONBytes {
		return nil, errors.New("the JSON of crawler-user-agents is longer than 64 MiB: the variable crawlersJson, which crawlersJSON reads through go:linkname, is no []byte any more")
	}
	// A variable of another type can give a slice whose capacity is not
	// its own, or whose bytes are not JSON: the slice is clipped to its
	// length, and a panic is an error like any other.
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, fmt.Errorf("the JSON of crawler-user-agents cannot be read: %v", r)
		}
	}()
	data = data[:len(data):len(data)]
	var entries []struct {
		Pattern     string `json:"pattern"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("the JSON of crawler-user-agents cannot be read: %w", err)
	}
	if len(entries) != len(crawlers) {
		return nil, fmt.Errorf("the JSON of crawler-user-agents has %d entries, and agents.Crawlers %d", len(entries), len(crawlers))
	}
	out = make([]string, len(entries))
	for i, e := range entries {
		if e.Pattern != crawlers[i].Pattern {
			return nil, fmt.Errorf("entry %d of the JSON of crawler-user-agents has the pattern %q, and agents.Crawlers %q", i, e.Pattern, crawlers[i].Pattern)
		}
		out[i] = strings.TrimSpace(e.Description)
	}
	return out, nil
}
