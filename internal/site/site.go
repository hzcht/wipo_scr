// Package site is the WIPO-specific half of the collector: URLs, selectors and
// the parsing of the results page. Everything above this package is generic
// crawling machinery, so a new target means swapping this one out.
package site

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	// HomeURL is the Madrid Monitor entry point. The search is a single box on
	// that page, so every attempt starts here: a fresh navigation also drops
	// whatever the previous query left in the session.
	HomeURL = "https://www3.wipo.int/madrid/monitor/en/"

	// SearchInputSel is the simple-search box of the current site build
	// (romarin 16117). It is created by the client's own JavaScript, not by the
	// server: the server ships an empty #simple_search_container, and the bundles
	// that fill it in (branddb.*, imageEditor/*, romarin) must all load or the
	// box never appears at all. Readiness has to be "the input exists and the
	// page is WIPO's", never "the markup was in the response".
	SearchInputSel = "#AUTO_input"

	// PagerSel carries the "1 - 100 / 955" position line. It is the marker
	// that the results grid is live, and its text is where the total count
	// comes from.
	//
	// Every one of these results selectors is scoped to the *visible* match,
	// and that is not cosmetic. The monitor ships one pager, one page-size
	// dropdown and one grid per search mode — four .results_pager blocks on a
	// single results page (verified on romarin 16117) — and only the active
	// mode's is on screen. An unscoped selector resolves to the first mode's
	// copy, so clicking the next arrow that the user can see advances a
	// different grid than the one being read, and the position line never moves.
	PagerSel = "div.pagerPos:visible"

	// RowCountSel is the page-size dropdown. The site only offers 10/30/60/100.
	RowCountSel = "#rowCount1:visible"

	// GridSel is the results grid. Its inner HTML is what we save and parse.
	GridSel = "#gridForsearch_pane:visible"

	// NextPageSel is the pager arrow. WIPO leaves it in the DOM and marks it
	// ui-state-disabled on the last page, so its mere presence says nothing —
	// the caller compares the pager text before and after the click.
	NextPageSel = `a[aria-label="next page"]:visible`

	// SkipInputSel is the "Go to page" box in the pager: <div class="skipWindow">
	// <label>Go to page</label><input id="skipValue1">. It is how a process
	// jumps to an arbitrary page of a huge result set instead of walking there
	// one arrow click at a time — the only way to split one crawl across several
	// browsers, since every process has to start its search from page 1.
	//
	// The id is not matched: the site numbers its two pagers (skipValue1,
	// skipValue2) and which one is first is not guaranteed, while every call
	// goes through .First() anyway. Both boxes drive the same grid.
	SkipInputSel = ".results_pager:visible .skipWindow input"

	// NoResultsText marks a results page with zero hits. There is no pager on
	// such a page, which is why the count for it is 0 and not a parse failure.
	NoResultsText = "No documents match your query"
)

// ariaPrefix is the jqGrid column prefix in a cell's aria-describedby, e.g.
// "gridForsearch_pane_BRAND" or "gridForsearch_pane_real_id". Matching on the
// full value is what keeps "id" from swallowing "real_id" — a prefix
// substring test is where the old standalone parser picked up the wrong cells.
const ariaPrefix = "gridForsearch_pane_"

// Columns is the results grid, in the order its columns are written to the
// rows file. Each entry maps a jqGrid column id to the field name used in the
// output layout:
//
//	date time \t query \t brand \t holder \t status \t origin \t reg date \t
//	reg number \t nice classes \t vienna classes \t record id \t record status \t
//	result number
//
// The first eleven are the trademark itself; the last three are the monitor's
// internal bookkeeping, kept because they identify the record in the site's
// own detail view.
var Columns = []struct {
	ID   string
	Name string
}{
	{"BRAND", "brand"},
	{"HOL", "holder"},
	{"STATUS", "status"},
	{"OO", "origin"},
	{"RD", "registration date"},
	{"IRN", "registration number"},
	{"NC", "nice classes"},
	{"VCS", "vienna classes"},
	{"real_id", "record id"},
	{"real_status", "record status"},
	{"result_number", "result number"},
}

// CountDateLayout / StampLayout are the two date formats on output. The count
// file carries the day, the rows file carries the second — the rows file is
// what gets appended to while a long query paginates.
const (
	CountDateLayout = "2006-01-02"
	StampLayout     = "2006-01-02 15:04:05"
)

// nonAlphanumeric keeps letters, digits, spaces and dashes; every other run
// collapses into a single dash. A search term is usually a domain or a brand,
// and both bring characters Windows does not want in a file name. Replacing
// rather than deleting keeps "example.com" readable as "example-com" instead of
// gluing it into "examplecom".
var nonAlphanumeric = regexp.MustCompile(`[^\p{L}\p{N} -]+`)
var repeatedDashes = regexp.MustCompile(`-{2,}`)

// wildcardTerms names the two characters Windows forbids in a file name, so
// that a term like "*" — the monitor's "everything" query, 1.5 million records —
// still gets its own set of saved pages instead of every wildcard collapsing on
// the fallback "query_p1.raw". The replacement carries its own dashes so
// "a*?" cannot be mistaken for the literal word "astarqmark".
var wildcardTerms = strings.NewReplacer("*", "-star-", "?", "-qmark-")

// SanitizeQuery turns a search term into a filesystem-safe lowercase base.
func SanitizeQuery(q string) string {
	s := strings.ToLower(nonAlphanumeric.ReplaceAllString(wildcardTerms.Replace(strings.TrimSpace(q)), "-"))
	s = strings.Trim(repeatedDashes.ReplaceAllString(s, "-"), "-")
	if s == "" {
		s = "query"
	}
	if len(s) > 60 {
		s = strings.Trim(s[:60], "-")
	}
	return s
}

// GridFile names the saved grid of one result page: <query>_p1.raw, ...
func GridFile(query string, page int) string {
	return fmt.Sprintf("%s_p%d.raw", SanitizeQuery(query), page)
}

// FullFile names the saved whole results page of one query.
func FullFile(query string) string {
	return SanitizeQuery(query) + "_full.html"
}

// PDFFile names the printed first results page of one query.
func PDFFile(query string) string {
	return SanitizeQuery(query) + ".pdf"
}

// Stamped renders the collection timestamp for the rows file.
func Stamped(at time.Time) string { return at.Format(StampLayout) }

// Day renders the collection date for the count file.
func Day(at time.Time) string { return at.Format(CountDateLayout) }
