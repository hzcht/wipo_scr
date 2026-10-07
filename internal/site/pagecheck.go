package site

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Kind is what a loaded page actually turned out to be. The collector has to
// know this before it reads anything out of the page: a proxy error page, a bot
// wall and a real results page all arrive with HTTP 200, and a collector that
// only checks the status code will happily parse a gateway error as "no
// trademarks".
type Kind int

const (
	// KindUnknown is a page we cannot place. It is never treated as an answer.
	KindUnknown Kind = iota
	// KindMonitor is the monitor front page with its search form.
	KindMonitor
	// KindResults is a results page whose grid and position line are readable.
	KindResults
	// KindNoResults is a real answer: the page loaded and matched nothing.
	KindNoResults
	// KindBlock is a bot wall, a challenge or a rate limit.
	KindBlock
	// KindProxyError is a page produced by the proxy or a gateway on the way,
	// not by WIPO. The endpoint is finished, so this is a proxy problem.
	KindProxyError
	// KindMaintenance is WIPO telling us it is down. Nobody's fault, and no
	// amount of rotating endpoints will help.
	KindMaintenance
	// KindAppError is WIPO's own "An error occurred processing your request"
	// page — the application failed while serving this request. Nobody's fault
	// either, but unlike maintenance it is usually transient: requeue the query
	// and carry on from where the walk stopped.
	KindAppError
)

func (k Kind) String() string {
	switch k {
	case KindMonitor:
		return "monitor"
	case KindResults:
		return "results"
	case KindNoResults:
		return "no results"
	case KindBlock:
		return "block"
	case KindProxyError:
		return "proxy error"
	case KindMaintenance:
		return "maintenance"
	case KindAppError:
		return "application error"
	default:
		return "unknown"
	}
}

// Terminal reports whether the kind is a final answer for the attempt. Only a
// real results page and a real empty answer are.
func (k Kind) Terminal() bool {
	return k == KindResults || k == KindNoResults
}

// Faulty reports whether the page is something the run should not try to parse:
// a wall, a gateway error, a site that is down, or the application's own
// error page.
func (k Kind) Faulty() bool {
	return k == KindBlock || k == KindProxyError || k == KindMaintenance || k == KindAppError
}

// ErrAppError marks WIPO's own "An error occurred processing your request"
// answer, wherever it shows up — as the results page after a search, as the
// home page, or as a JSON error envelope from select.jsp. It is the site's
// fault, not the endpoint's: the worker requeues the query without charging
// the proxy, without cooling it down, and without adding to the block streak.
// The checkpoint is kept, so the retry resumes where the walk stopped.
var ErrAppError = errors.New("the monitor reported an application error")

// appErrorMarkers identify that page. Only the failure sentence is used: the
// "Contact Madrid" link exists on perfectly healthy pages, so it can never be
// a marker on its own.
var appErrorMarkers = []string{
	"an error occurred processing your request",
	"please report the error",
}

// IsAppError reports whether a raw body (HTML page or JSON envelope) carries
// the application's error sentence. Used by the replay client, which never
// builds a PageSnapshot and has only the bytes to go on.
func IsAppError(text string) bool {
	_, marker := firstMatch(strings.ToLower(normalize(text)), appErrorMarkers)
	return marker != ""
}

// PageSnapshot is everything the classifier is allowed to look at. Keeping it a
// plain struct is what makes the classifier testable without a browser.
type PageSnapshot struct {
	URL   string
	Title string
	Text  string // the visible text of the page or results pane
	// GridRows is the number of rows the results grid currently holds, and
	// Pager the position line above it. Both are read by the caller because
	// they are the only proof that a grid really is a grid.
	GridRows int
	Pager    string
}

// controlMarkers are strings that only WIPO's own monitor renders. The form
// labels are used as the core control: a proxy error page, a gateway stub and a
// bot wall all lack "International Registration Number" next to "Mark name".
//
// The previous collector compared the page against a list of four help-link
// words ("Trademark search", "Trademark image search", ...). Three of the four
// are gone from the current site build, so that gate passed nothing at all.
var controlMarkers = []string{
	"International Registration Number",
	"Mark name",
	"WIPO reference",
	"Keep up to date with your international registrations",
}

// controlCore is how many control markers a page must carry to count as the
// real monitor. Two, because one of them ("Mark name") is short enough to
// appear in unrelated prose.
const controlCore = 2

// titleMarkers identify the monitor in the document title. The site separates
// the words with a non-breaking space ("WIPO Madrid Monitor"), so
// matching is done on normalised text.
var titleMarkers = []string{"madrid monitor", "wipo"}

// blockMarkers are bot walls, challenges and rate limits. They are checked
// before the control markers: a wall served under WIPO's own domain is still a
// wall, and reading it as "no results" is the one mistake that quietly poisons
// the output files.
var blockMarkers = []string{
	"attention required",
	"checking your browser",
	"verify you are human",
	"verifying you are human",
	"are you a robot",
	"unusual traffic",
	"you have been blocked",
	"access denied",
	"request blocked",
	"request rejected",
	"too many requests",
	"error 1010",
	"error 1020",
	"cloudflare",
	"ray id",
	"captcha",
	"ddos protection",
}

// proxyMarkers are pages a proxy or a gateway put in front of WIPO. They mean
// the endpoint is not usable, not that the site refused us.
var proxyMarkers = []string{
	"407 proxy authentication required",
	"proxy authentication required",
	"err_proxy",
	"err_tunnel",
	"err_socks",
	"err_name_not_resolved",
	"err_connection_refused",
	"err_connection_reset",
	"502 bad gateway",
	"503 service unavailable",
	"504 gateway",
	"gateway time-out",
	"gateway timeout",
	"bad gateway",
	"this site can't be reached",
	"the connection was reset",
	"unable to connect",
	"tunnel connection failed",
}

// maintenanceMarkers are WIPO's own downtime notices.
var maintenanceMarkers = []string{
	"temporarily unavailable",
	"temporarily out of service",
	"under maintenance",
	"scheduled maintenance",
	"service interruption",
	"we are sorry",
	"please try again later",
}

// Classify decides what a page is and reports the substring that decided it, so
// a failure in the log says why the page was thrown away.
//
// The order matters: walls and gateway errors are looked for first, the site's
// own downtime second, and the control markers last. Anything that reaches the
// end without a control marker is KindUnknown, never a result.
func Classify(p PageSnapshot) (Kind, string) {
	text := strings.ToLower(normalize(p.Text))
	title := strings.ToLower(normalize(p.Title))

	if _, marker := firstMatch(text, appErrorMarkers); marker != "" {
		return KindAppError, marker
	}
	if kind, marker := firstMatch(text, maintenanceMarkers); kind {
		_ = kind
		return KindMaintenance, marker
	}
	if _, marker := firstMatch(text, proxyMarkers); marker != "" {
		return KindProxyError, marker
	}
	if _, marker := firstMatch(text, blockMarkers); marker != "" {
		return KindBlock, marker
	}
	// A wall can carry a short generic phrase; the browser's own error pages
	// and most proxy stubs also name the code in the title.
	if _, marker := firstMatch(title, proxyMarkers); marker != "" {
		return KindProxyError, "title: " + marker
	}

	// A readable grid plus a parsable position line is the strongest possible
	// proof, and it does not depend on any of the wording below.
	if p.GridRows > 0 {
		if _, err := ParsePager(p.Pager); err == nil {
			return KindResults, fmt.Sprintf("%d rows, pager %q", p.GridRows, strings.TrimSpace(p.Pager))
		}
	}
	if strings.Contains(text, strings.ToLower(NoResultsText)) {
		return KindNoResults, NoResultsText
	}

	if hasControl(text) || (hasTitleMarker(title) && isMonitorURL(p.URL)) {
		return KindMonitor, controlMatch(text)
	}
	return KindUnknown, ""
}

// hasControl counts the site's own strings on the page. Requiring several of
// them is what keeps a lookalike page from passing as the monitor.
func hasControl(text string) bool {
	n := 0
	for _, m := range controlMarkers {
		if strings.Contains(text, strings.ToLower(m)) {
			n++
		}
	}
	return n >= controlCore
}

// hasTitleMarker accepts the monitor's title, but only together with a URL in
// WIPO's own space: a hijacking or mistyping proxy can serve any title it
// likes, and a bare "WIPO" string proves nothing on its own.
func hasTitleMarker(title string) bool {
	for _, m := range titleMarkers {
		if strings.Contains(title, m) {
			return true
		}
	}
	return false
}

// isMonitorURL reports whether the page came from WIPO itself rather than from
// something in the middle. The host is checked first and separately: a
// hijacking proxy can put any path it likes on a lookalike hostname, and a path
// check alone would wave that through.
func isMonitorURL(raw string) bool {
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	host = strings.TrimSuffix(host, ".")
	if host != "wipo.int" && !strings.HasSuffix(host, ".wipo.int") &&
		host != "wipo.org" && !strings.HasSuffix(host, ".wipo.org") {
		return false
	}
	return strings.Contains(strings.ToLower(u.Path), "madrid/monitor")
}

// controlMatch names what proved the page is the monitor, for the log line.
func controlMatch(text string) string {
	for _, m := range controlMarkers {
		if strings.Contains(text, strings.ToLower(m)) {
			return m
		}
	}
	return "title and url"
}

// firstMatch is a small helper: it reports whether any marker hit and which.
func firstMatch(text string, markers []string) (bool, string) {
	for _, m := range markers {
		if strings.Contains(text, m) {
			return true, m
		}
	}
	return false, ""
}

// spaceRunes are the Unicode spaces WIPO's pages use inside words and between
// numbers. strings.Fields splits on them, but strings.Contains does not, so
// every substring comparison in this file runs on normalised text first.
var spaceRunes = map[rune]rune{
	'\u00a0': ' ', // no-break space
	'\u2007': ' ', // figure space
	'\u2009': ' ', // thin space
	'\u202f': ' ', // narrow no-break space
	'\u200b': ' ', // zero width space
	'\u2002': ' ',
	'\u2003': ' ',
	'\u2008': ' ',
	'\u200a': ' ',
	'\ufeff': ' ', // byte order mark that survived a copy
}

// normalize folds the exotic spaces down to plain ones and squeezes runs of
// whitespace, so marker matching cannot be defeated by a non-breaking space.
func normalize(s string) string {
	return strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if u, ok := spaceRunes[r]; ok {
			return u
		}
		return r
	}, s)), " ")
}

// TrimSnippet shortens a page snippet for a log line, normalised so it stays on
// one line.
func TrimSnippet(s string, n int) string {
	f := normalize(s)
	if len(f) <= n {
		return f
	}
	if n <= 3 {
		return f[:n]
	}
	return f[:n-3] + "..."
}
