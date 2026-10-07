package main

import (
	"errors"
	"fmt"
	"log"
	"math/rand"
	"strconv"
	"strings"
	"time"

	playwright "github.com/mxschmitt/playwright-go"

	"wipos/internal/browser"
	"wipos/internal/queue"
	"wipos/internal/site"
)

const (
	// resultsPaneSel wraps the position line, the pager arrows and the grid, so
	// the cheap marker checks do not have to pull the text of a page that
	// carries a thousand trademarks. If it ever disappears, paneText falls
	// back to the whole body.
	resultsPaneSel = "#results_container"

	// gridLoadingSel is jqGrid's "Loading..." overlay. It carries the
	// ui-state-active class exactly while the grid is being (re)loaded, which
	// is the signal that the DOM underneath it is still the previous page.
	gridLoadingSel = "#load_gridForsearch_pane"

	// gridBusyClass is the class jqGrid toggles on the overlay while loading.
	gridBusyClass = "ui-state-active"

	// gridRowsSel counts the rows the grid is showing. It is the structural
	// proof that a page really holds results and not a shell that merely looks
	// like the monitor.
	gridRowsSel = site.GridSel + " tbody tr"

	// A step is "late" once it has burned a tenth of its own budget. Short
	// enough that a merely slow proxy is noticed before it costs an attempt,
	// long enough that an ordinary page render is not mistaken for one.
	lateFraction = 10

	// pollInterval is the cadence of the wait loops. Short enough that a fast
	// page is picked up almost immediately, long enough not to hammer the
	// page through the proxy.
	pollInterval = 250 * time.Millisecond
)

// errNoResults marks the zero-hits results page. It is a terminal outcome, not
// a failure: the query is done and its count is 0.
var errNoResults = errors.New("query has no trademarks")

// errNotResults marks a page that is neither a result page nor a known block
// page — a redirect, a maintenance screen, something we do not recognise.
var errNotResults = errors.New("not a results page")

// pageErr is a page that is not what the run asked for: a wall, a gateway error
// in front of WIPO, or the site being down. It is a separate type from the
// sentinel errors above because the worker reacts to it differently: a wall is
// cooled down, a gateway error charges the endpoint a full failure, and a site
// outage charges nobody.
//
// This is the check that keeps a proxy's own error page — or a bot wall that
// says "no documents match" — from being written into results.txt as an answer.
type pageErr struct {
	kind    site.Kind
	marker  string
	where   string // which wait loop saw it
	snippet string
}

func (e *pageErr) Error() string {
	if e.snippet == "" {
		return fmt.Sprintf("%s: %s page (%s)", e.where, e.kind, e.marker)
	}
	return fmt.Sprintf("%s: %s page (%s): %s", e.where, e.kind, e.marker, e.snippet)
}

// snapshot reads the three things the classifier needs from the page. It reads
// textContent rather than innerText: innerText is layout-dependent and came back
// empty on a monitor page whose body was still being laid out, which reads
// exactly like a block page and is not one.
func (w *worker) snapshot() site.PageSnapshot {
	snap := site.PageSnapshot{}
	snap.URL = w.page.URL()
	snap.Title, _ = w.page.Title()
	if txt, err := w.page.InnerText(resultsPaneSel); err == nil && strings.TrimSpace(txt) != "" {
		snap.Text = txt
	} else if txt, err := w.page.InnerText("body"); err == nil {
		snap.Text = txt
	} else if v, err := w.page.Evaluate(`() => document.body ? document.body.textContent : ""`); err == nil {
		snap.Text, _ = v.(string)
	}
	if n, err := w.page.Locator(gridRowsSel).Count(); err == nil {
		snap.GridRows = n
	}
	if txt, err := w.page.Locator(site.PagerSel).First().TextContent(playwright.LocatorTextContentOptions{
		Timeout: playwright.Float(2000),
	}); err == nil {
		snap.Pager = txt
	}
	return snap
}

// classifyPage returns the page's kind plus the reason, or a nil error while the
// page has not decided itself yet.
func (w *worker) classifyPage(where string) (site.Kind, string, *pageErr) {
	snap := w.snapshot()
	kind, marker := site.Classify(snap)
	switch {
	case kind.Faulty():
		return kind, marker, &pageErr{
			kind:    kind,
			marker:  marker,
			where:   where,
			snippet: site.TrimSnippet(snap.Text, 200),
		}
	default:
		return kind, marker, nil
	}
}

// markerErr carries a matched rules action out of a wait loop. A block page
// never turns into a results page, so the loops below check the markers on every
// poll: without it a block page is only noticed when the results timeout
// expires, which is minutes of a browser staring at a page it will never use.
type markerErr struct {
	action string
	marker string
}

func (e *markerErr) Error() string {
	return fmt.Sprintf("marker %q (%s)", e.marker, e.action)
}

// checkMarkers returns a markerErr as soon as the current page matches a rule.
func (w *worker) checkMarkers() error {
	if w.deps.Rules == nil {
		return nil
	}
	if action, marker := w.deps.Rules.Match(w.paneText()); action != "" {
		return &markerErr{action: action, marker: marker}
	}
	return nil
}

// openHome loads the monitor front page and waits until the search box is
// really on it. Every attempt starts here: a fresh navigation also drops
// whatever the previous query left in the session.
func (w *worker) openHome() error {
	w.at("open home")
	start := time.Now()
	// "commit" and not "domcontentloaded": the monitor's <head> carries a dozen
	// synchronous scripts, and DOMContentLoaded does not fire until every one of
	// them has arrived — measured at over two minutes through a working proxy,
	// which is longer than any navigation budget we would want to set. The
	// search box appearing is the real readiness signal, and that is what the
	// loop below waits for.
	navBudget := w.navTimeout()
	if _, err := w.page.Goto(site.HomeURL, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateCommit,
		Timeout:   playwright.Float(float64(navBudget.Milliseconds())),
	}); err != nil {
		w.noteStall("open home", start, navBudget, err)
		return fmt.Errorf("home: %w", err)
	}

	deadline := time.Now().Add(w.resultsTimeout())
	for {
		if n, err := w.page.Locator(site.SearchInputSel).Count(); err == nil && n > 0 {
			break
		}
		// A wall or a gateway error never grows a search box, so there is no
		// point in waiting out the whole budget to find out.
		if _, _, perr := w.classifyPage("open home"); perr != nil {
			return perr
		}
		if time.Now().After(deadline) {
			w.noteStall("open home", start, w.resultsTimeout(), nil)
			return fmt.Errorf("home: search box never appeared (%s)", w.excerpt())
		}
		w.page.WaitForTimeout(float64(pollInterval.Milliseconds()))
	}
	// The box being in the DOM is not enough: the page also has to be WIPO's
	// own. Without this a proxy that answers with a page of its own would be
	// treated as a monitor and every query would come back "no trademarks".
	if _, _, perr := w.classifyPage("open home"); perr != nil {
		return perr
	}
	log.Printf("home loaded in %s", time.Since(start).Round(time.Millisecond))
	// The one honest latency sample in the run: a page load is a single round
	// trip through the proxy, with none of the site's own thinking in it. Every
	// later budget for this endpoint is sized from it.
	w.deps.Proxy.NoteLatency(w.proxy, time.Since(start))

	w.at("home warm-up")
	browser.HumanMouse(w.page, w.fp.ViewportW, w.fp.ViewportH)
	return nil
}

// submitSearch types the query into the search box and presses Enter, the way
// the site expects it. The term goes in lowercase: the monitor is
// case-insensitive, and a lowercase-only search is also the less unusual
// fingerprint.
func (w *worker) submitSearch(t *queue.Task) error {
	w.at("submit search")
	if err := w.revealSearchBox(); err != nil {
		return err
	}
	w.page.WaitForTimeout(400 + rand.Float64()*1200) // a beat before touching the box
	if err := browser.HumanType(w.page, site.SearchInputSel, strings.ToLower(t.Name)); err != nil {
		return fmt.Errorf("type %q: %w", t.Name, err)
	}
	w.page.WaitForTimeout(250 + rand.Float64()*700) // and a beat before sending it

	// The gap is enforced per proxy: WIPO scores the source IP, and with one
	// browser per proxy the fleet still submits N searches per window.
	w.deps.Gate.Wait(w.proxy)

	// Dismiss the suggestion menu first. Typing the term character by character
	// — which is what a human does and what makes this look like one — makes the
	// site open its autocomplete, and Enter then picks a suggestion instead of
	// searching. The page does not look broken when that happens: the home page
	// ships a hidden results grid, so the run sits on "Loading..." until it times
	// out and reports a page that was never searched. Escape closes the menu and
	// leaves the term in the box.
	if err := w.page.Keyboard().Press("Escape"); err != nil {
		return fmt.Errorf("dismiss suggestions: %w", err)
	}
	w.page.WaitForTimeout(200 + rand.Float64()*400)

	// Press the key on the element, not on the page. Anything between typing and
	// submitting may have moved focus off the box, and a page-level Enter then
	// goes nowhere: the value is in the input, the site never hears about it, and
	// the run waits out its whole budget on the home page. Pressing through the
	// locator focuses the box first, which is what the site expects.
	box := w.page.Locator(site.SearchInputSel).First()
	if err := box.Press("Enter", playwright.LocatorPressOptions{
		Timeout: playwright.Float(15000),
	}); err != nil {
		return fmt.Errorf("press enter: %w", err)
	}
	return nil
}

// revealSearchBox makes the box typeable on a page the previous query left
// behind. select.jsp ships it collapsed — #search_input_container carries
// display:none — and still holding the previous term. A user expands the box;
// here it is put back the way the home page ships it, and the value is emptied
// with the native setter, which fires no events: the site's own handlers
// restore the old query on any synthetic input event, and a keyboard
// select-all does not win against them either. When the box is still not
// visible afterwards there is nothing to type into, and a home reload always
// ships one.
func (w *worker) revealSearchBox() error {
	js := fmt.Sprintf(`() => {
		const c = document.getElementById('search_input_container');
		if (c) c.style.display = 'block';
		const el = document.getElementById(%q);
		if (el) {
			const d = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value');
			if (d && d.set) d.set.call(el, '');
		}
	}`, strings.TrimPrefix(site.SearchInputSel, "#"))
	if _, err := w.page.Evaluate(js); err != nil {
		return fmt.Errorf("reveal search box: %w", err)
	}
	vis, err := w.page.Locator(site.SearchInputSel).First().IsVisible()
	if err == nil && vis {
		return nil
	}
	return w.openHome()
}

// outcome is what the results page turned out to be.
type outcome struct {
	pager string // the "1 - 100 / 955" position line, empty when there are no hits
	none  bool   // the zero-hits page
}

// waitOutcome polls until the page shows either the position line (hits) or the
// no-results marker. It replaces the old blind two-second sleep: a fast proxy
// is picked up in a fraction of a second, and a slow one is still waited out
// instead of being read half-rendered.
func (w *worker) waitOutcome() (outcome, error) {
	w.at("wait results")
	deadline := time.Now().Add(w.resultsTimeout())
	for {
		snap := w.snapshot()
		kind, marker := site.Classify(snap)
		switch {
		case kind.Terminal():
			if kind == site.KindNoResults {
				return outcome{none: true}, nil
			}
			if p := strings.TrimSpace(snap.Pager); p != "" {
				return outcome{pager: p}, nil
			}
		case kind.Faulty():
			return outcome{}, &pageErr{
				kind:    kind,
				marker:  marker,
				where:   "wait results",
				snippet: site.TrimSnippet(snap.Text, 200),
			}
		default:
			// Not a results page and not a wall: the classic "still loading".
			if err := w.checkMarkers(); err != nil {
				return outcome{}, err
			}
			if time.Now().After(deadline) {
				return outcome{}, fmt.Errorf("results never appeared (%s): %w", w.excerpt(), errNotResults)
			}
		}
		w.page.WaitForTimeout(float64(pollInterval.Milliseconds()))
	}
}

// readPager reads the position line and parses it.
func (w *worker) readPager() (site.Pager, error) {
	// The pane can turn into the application's error page while the grid
	// reloads (the rows-per-page switch just before this call). Classifying
	// first keeps that from surfacing as a bare "pager: not found".
	if _, _, perr := w.classifyPage("read pager"); perr != nil {
		return site.Pager{}, perr
	}
	txt, err := w.page.Locator(site.PagerSel).First().TextContent(playwright.LocatorTextContentOptions{
		Timeout: playwright.Float(5000),
	})
	if err != nil {
		return site.Pager{}, fmt.Errorf("pager: %w", err)
	}
	return site.ParsePager(txt)
}

// setRowsPerPage switches the grid to the configured page size. The site offers
// only 10/30/60/100; the default 100 is the difference between one page load
// and ten for a large result set.
func (w *worker) setRowsPerPage() error {
	w.at("set rows per page")
	want := strconv.Itoa(w.deps.Cfg.RowsPerPage)
	cur, err := w.page.Locator(site.RowCountSel).First().InputValue(playwright.LocatorInputValueOptions{
		Timeout: playwright.Float(10000),
	})
	if err == nil && strings.TrimSpace(cur) == want {
		return nil // already the size we want, no reload needed
	}
	if _, err := w.page.Locator(site.RowCountSel).First().SelectOption(playwright.SelectOptionValues{
		Values: playwright.StringSlice(want),
	}, playwright.LocatorSelectOptionOptions{Timeout: playwright.Float(10000)}); err != nil {
		return fmt.Errorf("select rows per page: %w", err)
	}
	return w.waitGridIdle(w.resultsTimeout())
}

// waitGridIdle waits for jqGrid's loading overlay to clear. Reading the grid
// while it is still loading returns the previous page's rows, which is how a
// long result set silently loses its second page.
func (w *worker) waitGridIdle(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		// A reload that never finishes often means the pane was replaced by
		// the application's error page — classify before blaming the budget.
		if _, _, perr := w.classifyPage("grid reload"); perr != nil {
			return perr
		}
		cls, err := w.page.Locator(gridLoadingSel).GetAttribute("class", playwright.LocatorGetAttributeOptions{
			Timeout: playwright.Float(1000),
		})
		// No overlay at all means nothing is loading.
		if err != nil || !strings.Contains(cls, gridBusyClass) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("grid still loading after %s", timeout)
		}
		w.page.WaitForTimeout(float64(pollInterval.Milliseconds()))
	}
}

// nextPage clicks the pager arrow and waits for the position line to move. The
// arrow stays in the DOM on the last page (marked ui-state-disabled), so the
// caller must check Pager.Last() first; here the changed text is what proves
// the click actually paged.
//
// The click is a plain locator click on purpose. browser.HumanClick aims up to
// 40% off the centre of the element, which on an icon-only button that is mostly
// its own padding lands beside the icon — and the monitor's arrows also carry
// qtip tooltips that pop up on the hover the helper performs. Either way the
// click lands somewhere that is not the control, jqGrid never hears about it,
// and the run reports "page did not advance" while the arrow sits there looking
// perfectly clickable.
func (w *worker) nextPage(prev string) (string, error) {
	w.at("next page")
	next := w.page.Locator(site.NextPageSel).First()
	if err := next.Click(playwright.LocatorClickOptions{
		Timeout: playwright.Float(15000),
	}); err != nil {
		return "", fmt.Errorf("click next page: %w", err)
	}
	deadline := time.Now().Add(w.resultsTimeout())
	for {
		// The application can replace the pane with its own error page at any
		// point mid-walk. The position line then never moves, so without this
		// check the loop would sit here until the results timeout expires and
		// the page would be graded as a late proxy instead of requeued as WIPO's
		// own failure.
		if _, _, perr := w.classifyPage("next page"); perr != nil {
			return "", perr
		}
		txt, err := w.page.Locator(site.PagerSel).First().TextContent(playwright.LocatorTextContentOptions{
			Timeout: playwright.Float(2000),
		})
		if err == nil {
			if p := strings.TrimSpace(txt); p != "" && p != prev {
				return p, nil
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("page did not advance past %q (last read %q: %v)", prev, strings.TrimSpace(txt), err)
		}
		w.page.WaitForTimeout(float64(pollInterval.Milliseconds()))
	}
}

// gotoPage types a page number into the pager's "Go to page" box and waits for
// the grid to move there. This is the entry point for a sharded crawl: a process
// that owns pages 4000-7999 still has to run the search, which always lands on
// page 1, and walking 4000 arrow clicks to get into position would cost more
// than the shard itself.
//
// The wait is on the position line, not on a sleep: the box accepts the number
// immediately and jqGrid fetches the page behind it, so a fixed pause would
// return the old grid. A page number past the end is clamped by the site, and
// the caller gets the line it actually landed on.
func (w *worker) gotoPage(target int, prev string) (string, error) {
	w.at(fmt.Sprintf("jump to page %d", target))
	box := w.page.Locator(site.SkipInputSel).First()
	if err := box.Fill(strconv.Itoa(target), playwright.LocatorFillOptions{
		Timeout: playwright.Float(15000),
	}); err != nil {
		return "", fmt.Errorf("fill go-to-page box: %w", err)
	}
	if err := box.Press("Enter", playwright.LocatorPressOptions{
		Timeout: playwright.Float(15000),
	}); err != nil {
		return "", fmt.Errorf("submit go-to-page box: %w", err)
	}
	deadline := time.Now().Add(w.resultsTimeout())
	for {
		// Same mid-walk error page as in nextPage: a jump that never lands has
		// its own reason if the pane was replaced, and it is not the proxy's.
		if _, _, perr := w.classifyPage("goto page"); perr != nil {
			return "", perr
		}
		txt, err := w.page.Locator(site.PagerSel).First().TextContent(playwright.LocatorTextContentOptions{
			Timeout: playwright.Float(2000),
		})
		if err == nil {
			if p := strings.TrimSpace(txt); p != "" && p != prev {
				return p, nil
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("page %d never loaded (still %q)", target, strings.TrimSpace(txt))
		}
		w.page.WaitForTimeout(float64(pollInterval.Milliseconds()))
	}
}

// paneText returns the text of the results pane, falling back to the whole page
// when the pane selector is gone. It is empty when there is no page at all —
// replay mode has an HTTP session, not a browser.
func (w *worker) paneText() string {
	if w.page == nil {
		return ""
	}
	if txt, err := w.page.InnerText(resultsPaneSel); err == nil && strings.TrimSpace(txt) != "" {
		return txt
	}
	txt, err := w.page.InnerText("body")
	if err != nil {
		return ""
	}
	return txt
}

// excerpt renders a short body snippet for the log. When an attempt fails we
// need to know what the page actually said — a block page, a maintenance
// screen or a cookie banner all look identical from the outside.
func (w *worker) excerpt() string {
	title, _ := w.page.Title()
	body := strings.Join(strings.Fields(w.paneText()), " ")
	if len(body) > 240 {
		body = body[:240] + "…"
	}
	return fmt.Sprintf("title=%q body=%q", title, body)
}
