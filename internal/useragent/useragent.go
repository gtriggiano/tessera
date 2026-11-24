// Package useragent analyzes the user-agent header of a request, which the
// attribute ua reads: the browser, the operating system and the kind of
// device that it names, and whether it is a bot, with the name and the
// description of the bot.
//
// The analysis reads the first MaxBytes bytes of the header only, with two
// libraries: uasurfer for the browser, the operating system and the device,
// and the list of crawler-user-agents, with uasurfer, for the bots. tessera
// names what they find with its own tables, so that a value of an attribute
// never changes with the spelling of a library. The analysis reads memory
// only, and never fails: a header that the libraries do not recognize gives
// empty names and the device unknown.
//
// An Analyzer keeps the records of the headers it analyzed in a cache with a
// fixed number of entries, shared by every decision of the process and safe
// for concurrent use: a header already in the cache is not analyzed again. A
// decision never waits to put a record in the cache.
package useragent

import (
	"strconv"
	"strings"

	uasurfer "github.com/LumenResearch/uasurfer"
	agents "github.com/monperrus/crawler-user-agents"
)

// MaxBytes is the number of bytes of the header that tessera analyzes, from
// its start: the record of a header depends on them only, and they are the key
// of the cache.
const MaxBytes = 512

// The kinds of a device.
const (
	DeviceDesktop = "desktop"
	DeviceMobile  = "mobile"
	DeviceTablet  = "tablet"
	DeviceTV      = "tv"
	DeviceConsole = "console"
	DeviceBot     = "bot"
	DeviceUnknown = "unknown"
)

// Devices are the kinds of a device, in the order of the expressions
// reference.
var Devices = []string{DeviceDesktop, DeviceMobile, DeviceTablet, DeviceTV, DeviceConsole, DeviceBot, DeviceUnknown}

// Record is what tessera reads of a user-agent header. For a bot, only
// Device, IsBot and Bot are set; for a client that is not a bot, Bot is
// empty.
type Record struct {
	// Browser is the browser that the header names.
	Browser Browser
	// OS is the operating system that the header names.
	OS OS
	// Device is one of Devices.
	Device string
	// IsBot reports whether uasurfer or the list of crawler-user-agents
	// recognizes a bot, and Bot is what tessera knows of it.
	IsBot bool
	Bot   Bot
}

// Browser is the browser of a header.
type Browser struct {
	// Name is the name of the browser, from the table of tessera, or empty
	// when it is unknown.
	Name string
	// Version is the version of the browser as major.minor.patch, or empty
	// when it is unknown, and Major and Minor its first two parts, or 0.
	Version      string
	Major, Minor int64
}

// OS is the operating system of a header.
type OS struct {
	// Name is the name of the operating system, from the table of tessera,
	// or empty when it is unknown.
	Name string
	// Version is the version of the operating system as major.minor.patch,
	// or empty when it is unknown, and Major and Minor its first two parts,
	// or 0.
	Version      string
	Major, Minor int64
}

// Bot is the bot of a header.
type Bot struct {
	// Name is the name of the bot: the name of the first pattern of the list
	// of crawler-user-agents that matches the header, or else the name of
	// the crawler of uasurfer, or empty.
	Name string
	// Description is the description that the list of crawler-user-agents
	// gives to the pattern that named the bot, without the white space
	// around it; empty when the name comes from uasurfer or is empty.
	Description string
}

// empty is the record of an absent or empty header.
var empty = Record{Device: DeviceUnknown}

// Analyzer analyzes user-agent headers, with a cache of at most the number of
// entries that New received. A nil Analyzer analyzes without a cache. It is
// safe for concurrent use.
type Analyzer struct {
	cache *cache
}

// New returns an Analyzer whose cache holds at most cacheEntries records; 0,
// or less, makes an Analyzer without a cache. It prepares the names and the
// descriptions of the bots of the list of crawler-user-agents, so that no
// analysis does.
func New(cacheEntries int) *Analyzer {
	crawlerNames()
	crawlerDescriptions()
	a := &Analyzer{}
	if cacheEntries > 0 {
		a.cache = newCache(cacheEntries)
	}
	return a
}

// Analyze returns the record of header, the value of the user-agent header of
// a request: the record of its first MaxBytes bytes, from the cache when it is
// there. Otherwise Analyze analyzes the header and keeps the record in the
// cache, unless another decision holds that part of the cache at that moment:
// it never waits for it. An empty header is neither analyzed nor looked up:
// its record has empty names and the device unknown.
func (a *Analyzer) Analyze(header string) Record {
	if header == "" {
		return empty
	}
	key := header[:min(len(header), MaxBytes)]
	if a == nil || a.cache == nil {
		return analyze(key)
	}
	if r, ok := a.cache.get(key); ok {
		return r
	}
	r := analyze(key)
	// A copy, so that the cache does not hold the request.
	a.cache.add(strings.Clone(key), r)
	return r
}

// CacheEntries returns the largest number of entries of the cache, or 0 for
// an Analyzer without a cache.
func (a *Analyzer) CacheEntries() int {
	if a == nil || a.cache == nil {
		return 0
	}
	return a.cache.capacity
}

// Entries returns the number of records in the cache.
func (a *Analyzer) Entries() int {
	if a == nil || a.cache == nil {
		return 0
	}
	return a.cache.len()
}

// Lookups returns the number of lookups in the cache that found the record of
// the header, and of those that did not, since New.
func (a *Analyzer) Lookups() (hits, misses uint64) {
	if a == nil || a.cache == nil {
		return 0, 0
	}
	return a.cache.lookups()
}

// analyze analyzes s, a nonempty header of at most MaxBytes bytes.
func analyze(s string) Record {
	var u uasurfer.UserAgent
	uasurfer.ParseUserAgent(s, &u)
	crawler := firstCrawler(agents.MatchingCrawlers(s))
	if crawler >= 0 || u.IsBot() {
		r := Record{Device: DeviceBot, IsBot: true}
		if crawler >= 0 {
			r.Bot = Bot{Name: crawlerNames()[crawler], Description: crawlerDescriptions()[crawler]}
		}
		if r.Bot.Name == "" {
			r.Bot = Bot{Name: uasurferCrawlers[u.Browser.Name]}
		}
		return r
	}
	r := Record{
		Browser: Browser{Name: browserName(u.Browser.Name, s)},
		OS:      OS{Name: osNames[u.OS.Name]},
		Device:  device(u.DeviceType),
	}
	if v := u.Browser.Version; v != (uasurfer.Version{}) {
		r.Browser.Version = version(v)
		r.Browser.Major = int64(v.Major)
		r.Browser.Minor = int64(v.Minor)
	}
	if v := u.OS.Version; v != (uasurfer.Version{}) {
		r.OS.Version = version(v)
		r.OS.Major = int64(v.Major)
		r.OS.Minor = int64(v.Minor)
	}
	return r
}

// firstCrawler returns the first of the indices of the list of
// crawler-user-agents that matches holds, in the order of the list, or -1.
func firstCrawler(matches []int) int {
	first := -1
	for _, i := range matches {
		if first < 0 || i < first {
			first = i
		}
	}
	return first
}

// version writes v as major.minor.patch.
func version(v uasurfer.Version) string {
	return strconv.Itoa(v.Major) + "." + strconv.Itoa(v.Minor) + "." + strconv.Itoa(v.Patch)
}

// device returns the kind of the device d, of a header that is not a bot.
func device(d uasurfer.DeviceType) string {
	switch d {
	case uasurfer.DeviceComputer:
		return DeviceDesktop
	case uasurfer.DevicePhone, uasurfer.DeviceWearable:
		return DeviceMobile
	case uasurfer.DeviceTablet:
		return DeviceTablet
	case uasurfer.DeviceTV:
		return DeviceTV
	case uasurfer.DeviceConsole:
		return DeviceConsole
	}
	return DeviceUnknown
}

// edgeTokens are the products of Edge, which uasurfer names as Internet
// Explorer, in lowercase.
var edgeTokens = []string{"edg/", "edga/", "edgios/", "edge/"}

// browserName returns the name of the browser b, of a header that is not a
// bot: Edge for an Internet Explorer whose header s names an Edge product.
func browserName(b uasurfer.BrowserName, s string) string {
	if b == uasurfer.BrowserIE {
		lower := strings.ToLower(s)
		for _, t := range edgeTokens {
			if strings.Contains(lower, t) {
				return browserEdge
			}
		}
	}
	return browserNames[b]
}
