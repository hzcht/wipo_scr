package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	playwright "github.com/mxschmitt/playwright-go"

	"wipos/internal/browser"
	"wipos/internal/config"
	"wipos/internal/proxy"
	"wipos/internal/queue"
	"wipos/internal/replay"
	"wipos/internal/rules"
	"wipos/internal/site"
)

// blockStreak is how many consecutive block-ish outcomes one worker tolerates
// before its profile is wiped. Below that the warm cookies are worth keeping —
// a block page is often a one-off for that IP, not a burnt profile.
const blockStreak = 3

// Deps wires every shared dependency a worker needs. No package-level globals:
// everything is built in main and passed down.
type Deps struct {
	Cfg       config.Config
	PW        *playwright.Playwright
	Queue     *queue.Queue
	Rules     *rules.Rules // nil when there is no rules file: no marker handling
	Proxy     *proxy.Pool
	Gate      *browser.GatePool
	Done      *queue.Store
	Failed    *queue.Store
	Sink      *site.Sink
	Collected *atomic.Int64
	Scripts   []string // stealth payloads, embedded in stealth.go
	WG        *sync.WaitGroup
	Shards    *shardGate // counts finished pieces of a split query
}

// worker owns one browser with a persistent profile for the whole run, and one
// proxy at a time. The two are rotated together: warm cookies are only useful
// while the IP behind them stays the same.
type worker struct {
	id         int
	deps       *Deps
	profileDir string
	proxy      string
	fp         browser.Fingerprint
	ctx        playwright.BrowserContext
	page       playwright.Page
	blockRuns  int
	stage      atomic.Value // current attempt phase, for the watchdog log
	// pageOK marks the current page as clean and searchable: a WIPO page that
	// answered with hits or with a real no-results. Only then may the next
	// query skip the home reload and type into the page it is already on.
	pageOK bool

	// rc is the worker's HTTP session for replay mode, created on first use
	// and dropped when a response turns out not to be a result page (the
	// fallback then runs the search in the browser instead).
	rc *replay.Client

	// savePDFLogged tracks whether the "save_pdf skipped in replay mode" log
	// has been emitted, so it fires once per worker run instead of per query.
	savePDFLogged bool

	// lastBeat is when the attempt last made progress, as unix nanoseconds. The
	// watchdog reads it, so it is the whole difference between "stuck on one CDP
	// call" and "working through 15,430 pages" — both look identical from the
	// outside if all the watchdog can see is how long the attempt has been
	// running.
	lastBeat atomic.Int64
}

// at records which phase the attempt is in and that it is still moving. Every
// step of a search and every collected page calls it.
func (w *worker) at(stage string) {
	w.stage.Store(stage)
	w.lastBeat.Store(time.Now().UnixNano())
}

// stalledFor is how long the attempt has been without progress.
func (w *worker) stalledFor() time.Duration {
	beat := w.lastBeat.Load()
	if beat == 0 {
		return 0
	}
	return time.Since(time.Unix(0, beat))
}

// ProcPages is one collection goroutine.
func ProcPages(deps *Deps, workerID int) {
	defer deps.WG.Done()
	defer deps.Queue.WorkerDown(func(t queue.Task) {
		log.Printf("task %q: dropped, no live workers left", t.Name)
		deps.Queue.Done()
	})

	w := &worker{
		id:         workerID,
		deps:       deps,
		profileDir: filepath.Join(deps.Cfg.ProfilesDir, fmt.Sprintf("worker-%d", workerID)),
	}
	// Replay mode runs on HTTP alone: the browser is opened on demand, only
	// when a response turns out not to be a result page.
	if !deps.Cfg.Replay() {
		if err := w.launch(false); err != nil {
			log.Printf("worker %d: %v", workerID, err)
			return
		}
	}
	defer w.close()

	for {
		t, ok := deps.Queue.Get()
		if !ok {
			return
		}
		w.handle(t)
		randomPause(w.deps.Cfg.TaskPauseMS, w.deps.Cfg.TaskPauseJitterMS)
	}
}

// launch (re)creates the worker's browser. With wipe the profile is deleted
// first, which also forces a fresh proxy and a new fingerprint; otherwise the
// pinned fingerprint and the current proxy are kept.
func (w *worker) launch(wipe bool) error {
	w.close()
	if wipe {
		// Windows keeps a lock on a profile that was not closed cleanly, so
		// this only works because w.close() ran above.
		if err := os.RemoveAll(w.profileDir); err != nil {
			log.Printf("worker %d: clear profile: %v", w.id, err)
		}
		w.proxy = ""
	}
	if err := w.ensureProfile(); err != nil {
		return err
	}
	if w.proxy == "" {
		w.proxy = w.deps.Proxy.Pick()
	}

	ctx, page, err := w.openContext()
	if err != nil && !wipe {
		// A profile left locked by a crashed run is the usual suspect: wipe it
		// once and retry before writing the worker off.
		log.Printf("worker %d: launch failed (%v), wiping the profile once", w.id, err)
		if rerr := os.RemoveAll(w.profileDir); rerr != nil {
			return fmt.Errorf("launch: %w", err)
		}
		if merr := os.MkdirAll(w.profileDir, 0755); merr != nil {
			return fmt.Errorf("launch: %w", err)
		}
		w.fp = browser.LoadOrGenFingerprint(w.profileDir)
		ctx, page, err = w.openContext()
	}
	if err != nil {
		return fmt.Errorf("launch: %w", err)
	}
	w.ctx, w.page = ctx, page
	w.blockRuns = 0
	log.Printf("worker %d: browser ready (proxy=%q headless=%v)", w.id, w.proxy, w.deps.Cfg.Headless)
	return nil
}

func (w *worker) openContext() (playwright.BrowserContext, playwright.Page, error) {
	return browser.LaunchPersistent(w.deps.PW, browser.LaunchOptions{
		ProfileDir:  w.profileDir,
		Proxy:       w.proxy,
		FP:          w.fp,
		Headless:    w.deps.Cfg.Headless,
		DarkScheme:  rand.Intn(2) == 0,
		InitScripts: w.deps.Scripts,
	})
}

// close shuts the page and the context, which also ends the browser process.
func (w *worker) close() {
	if w.page != nil {
		_ = w.page.Close()
		w.page = nil
	}
	if w.ctx != nil {
		_ = w.ctx.Close()
		w.ctx = nil
	}
}

// healthy reports whether the worker has a usable page.
func (w *worker) healthy() bool {
	return w.ctx != nil && w.page != nil && !w.page.IsClosed()
}

// handle runs one query to a terminal state.
//
// A query is Done only from a terminal state: collected, no results at all,
// failed-listed, or dropped. Everything else requeues, so the WaitGroup behind
// the run can never drain while a query is still in flight.
func (w *worker) handle(t queue.Task) {
	for {
		if t.Attempts >= w.deps.Cfg.MaxAttempts {
			log.Printf("task %q: max attempts (%d) reached, failed-listed", t.Name, w.deps.Cfg.MaxAttempts)
			w.deps.Failed.Append(t.Name)
			w.deps.Queue.Done()
			return
		}
		t.Attempts++
		t.LastAttempt = time.Now().Unix()

		// In replay mode a missing browser is the normal state — it is opened
		// on demand by runAttempt's fallback, not a precondition here.
		if !w.healthy() && !w.deps.Cfg.Replay() {
			if err := w.launch(false); err != nil {
				log.Printf("task %q attempt %d: relaunch failed: %v -> requeued", t.Name, t.Attempts, err)
				randomPause(5000, 15000)
				w.deps.Queue.Requeue(t)
				return
			}
		}

		// The attempt runs under a stall watchdog. A CDP call without its own
		// timeout (a mouse move, a key press) can hang forever when the renderer
		// stalls behind a bad proxy, and no per-call timeout would ever fire. The
		// watchdog turns that hang into an error by killing the browser, and the
		// attempt comes back through the ordinary requeue path.
		//
		// It watches for *silence*, not for duration. A total budget cannot tell
		// a hung page from a long one, and the difference decides whether the
		// collector can walk a 15,430-page crawl at all: the old formula
		// (2 x nav timeout + 2 min) fired six minutes into a crawl that was
		// making progress every few seconds. Every step and every page calls
		// beat, so only a genuinely stuck call runs out the clock.
		attemptStart := time.Now()
		stallBudget := w.deps.Cfg.Watchdog()
		// The result comes back over a buffered channel rather than a shared
		// variable. On a timeout the attempt goroutine is abandoned while it is
		// still inside a CDP call, and it goes on to write the struct this
		// goroutine is reading — a data race, and a torn read of a struct holding
		// a string and an action. A channel with room for one value fixes both:
		// the loser of the select never blocks, and the abandoned goroutine can
		// only ever touch its own copy.
		resultCh := make(chan attemptResult, 1)
		w.at("start")
		go func() {
			var res attemptResult
			defer func() {
				if r := recover(); r != nil {
					res.err = fmt.Errorf("attempt panicked at stage %q: %v", w.stage.Load(), r)
				}
				resultCh <- res
			}()
			res = w.attempt(&t)
		}()

		// Poll often enough that a stall is noticed close to when the budget
		// runs out, but never so often that an idle worker spins.
		tick := stallBudget / 8
		if tick < time.Second {
			tick = time.Second
		}
		if tick > 15*time.Second {
			tick = 15 * time.Second
		}
		ticker := time.NewTicker(tick)
		defer ticker.Stop()

		var res attemptResult
		timedOut := false
	watchdog:
		for {
			select {
			case res = <-resultCh:
				break watchdog
			case <-ticker.C:
				if w.stalledFor() < stallBudget {
					continue
				}
				timedOut = true
				log.Printf("task %q attempt %d: stuck at stage %q for %s (budget %s) — killing the browser",
					t.Name, t.Attempts, w.stage.Load(), w.stalledFor().Round(time.Second), stallBudget)
				w.close()
				break watchdog
			}
		}
		if timedOut {
			// A separate value on purpose: the abandoned goroutine may still be
			// inside w.attempt, and it owns the copy in resultCh.
			res = attemptResult{err: fmt.Errorf("attempt watchdog: stuck at stage %q for %s (budget %s)",
				w.stage.Load(), w.stalledFor().Round(time.Second), stallBudget)}
		}
		took := time.Since(attemptStart).Round(time.Millisecond)
		w.deps.Proxy.ObserveAttempt(w.proxy, took)

		switch {
		case res.err == nil && res.action == "":
			w.deps.Proxy.Success(w.proxy)
			w.blockRuns = 0
			terminal, err := w.finishQuery(&t, res.count)
			if err != nil {
				log.Printf("task %q: %v -> requeued", t.Name, err)
				w.deps.Queue.Requeue(t)
				return
			}
			w.reportDone(&t, terminal, fmt.Sprintf("collected (proxy %q, took %s)", w.proxy, took))
			w.deps.Queue.Done()
			return

		case errors.Is(res.err, errNoResults):
			// A real answer, just an empty one: the count goes in as 0 and the
			// query is done. The old collector filed these in a separate "lost"
			// list, which made a finished query look unfinished on the next run.
			terminal, err := w.finishQuery(&t, 0)
			if err != nil {
				log.Printf("task %q: %v -> requeued", t.Name, err)
				w.deps.Queue.Requeue(t)
				return
			}
			w.deps.Proxy.Success(w.proxy)
			w.blockRuns = 0
			w.reportDone(&t, terminal, fmt.Sprintf("no trademarks (proxy %q, took %s)", w.proxy, took))
			w.deps.Queue.Done()
			return

		case errors.Is(res.err, errNotResults):
			// Neither results nor a known marker: usually an unlisted block
			// screen. Proxy-neutral — the endpoint may be perfectly fine.
			w.onBlockHit(t.Name)
			log.Printf("task %q attempt %d: %v -> requeued (block streak %d, took %s)",
				t.Name, t.Attempts, res.err, w.blockRuns, took)
			randomPause(5000, 15000)
			w.deps.Queue.Requeue(t)
			return

		case res.err != nil:
			// A page that is not WIPO's own is the one failure the collector has
			// to grade carefully, because the three kinds have different causes:
			// a wall is about this IP, a gateway error is about this endpoint,
			// and the site being down is about neither.
			var pe *pageErr
			if errors.As(res.err, &pe) {
				switch pe.kind {
				case site.KindProxyError:
					w.deps.Proxy.Fail(w.proxy)
					w.blockRuns = 0
					log.Printf("task %q attempt %d: %v -> requeued (proxy %q charged, took %s)",
						t.Name, t.Attempts, res.err, w.proxy, took)
					w.switchProxy()
				case site.KindBlock:
					w.onBlockHit(t.Name)
					log.Printf("task %q attempt %d: %v -> requeued (block streak %d, took %s)",
						t.Name, t.Attempts, res.err, w.blockRuns, took)
				case site.KindAppError:
					// WIPO's own "An error occurred processing your request":
					// its fault, not the endpoint's, and usually transient.
					// Nobody is charged, nothing is cooled down — the query is
					// simply put back, and its checkpoint makes the retry
					// resume where the walk stopped.
					log.Printf("task %q attempt %d: %v -> requeued (WIPO application error, took %s)",
						t.Name, t.Attempts, res.err, took)
				default: // KindMaintenance: nobody's fault, so nobody is charged
					log.Printf("task %q attempt %d: %v -> requeued (took %s)", t.Name, t.Attempts, res.err, took)
				}
				randomPause(5000, 15000)
				if t.Sharded() {
					w.deps.Shards.Reset(t.Name)
				}
				w.deps.Queue.Requeue(t)
				return
			}
			// The same failure as a pageErr, seen by the replay engine: it
			// arrives as a sentinel instead of a classified page. Same verdict
			// — requeued, nothing charged, nothing rotated.
			if errors.Is(res.err, site.ErrAppError) {
				log.Printf("task %q attempt %d: %v -> requeued (WIPO application error, took %s)",
					t.Name, t.Attempts, res.err, took)
				randomPause(5000, 15000)
				if t.Sharded() {
					w.deps.Shards.Reset(t.Name)
				}
				w.deps.Queue.Requeue(t)
				return
			}
			// Transport trouble. Being late and being broken are different
			// failures and get different treatment: a timeout or a reset only
			// proves the proxy answered slowly, so it is charged half a failure
			// and its next budgets grow. Junk pages, refused tunnels and dead
			// endpoints take the full one and are rotated away from.
			if isTimeout(res.err) {
				// The budget, not the proxy, may be what ran out. Raising the
				// endpoint's measured latency makes the retry wait long enough,
				// which is the difference between a query that eventually lands
				// and one that burns every attempt against a link that was
				// never going to answer inside the configured timeout.
				w.deps.Proxy.NoteSlow(w.proxy, took, w.resultsTimeout())
				w.deps.Proxy.SoftFail(w.proxy)
				log.Printf("task %q attempt %d: %v -> requeued (late, not broken: %q now gets %s, took %s)",
					t.Name, t.Attempts, res.err, w.proxyLabel(), w.resultsTimeout(), took)
				if t.Sharded() {
					w.deps.Shards.Reset(t.Name)
				}
				w.deps.Queue.Requeue(t)
				return
			}
			if isTransportHiccup(res.err) {
				w.deps.Proxy.SoftFail(w.proxy)
			} else {
				w.deps.Proxy.Fail(w.proxy)
			}
			w.blockRuns = 0
			log.Printf("task %q attempt %d: %v -> requeued (took %s)", t.Name, t.Attempts, res.err, took)
			w.switchProxy()
			randomPause(5000, 15000)
			if t.Sharded() {
				w.deps.Shards.Reset(t.Name)
			}
			w.deps.Queue.Requeue(t)
			return

		case res.action == "skip":
			w.onBlockHit(t.Name)
			log.Printf("task %q: skip marker %q, requeued (took %s)", t.Name, res.marker, took)
			randomPause(3000, 8000)
			if t.Sharded() {
				w.deps.Shards.Reset(t.Name)
			}
			w.deps.Queue.Requeue(t)
			return

		case strings.HasPrefix(res.action, "sleep"):
			secs, _ := rules.ActionSeconds(res.action)
			w.onBlockHit(t.Name)
			log.Printf("task %q: %s marker %q, sleeping %ds here and retrying in the same profile (took %s)",
				t.Name, res.action, res.marker, secs, took)
			time.Sleep(time.Duration(secs) * time.Second)
			continue // same worker, same profile, no attempt spent

		case res.action == "pauseAll":
			w.onBlockHit(t.Name)
			log.Printf("task %q: pauseAll marker %q, freezing every worker for %ds (took %s)",
				t.Name, res.marker, w.deps.Cfg.AllThreadsPauseDuration, took)
			w.deps.Queue.PauseAll()
			continue

		default:
			log.Printf("task %q: unknown action %q, requeued (took %s)", t.Name, res.action, took)
			randomPause(5000, 15000)
			if t.Sharded() {
				w.deps.Shards.Reset(t.Name)
			}
			w.deps.Queue.Requeue(t)
			return
		}
	}
}

// finishQuery records a query as collected: its count in results.txt, its name
// in done.lst, its resume point removed.
//
// A split query passes through the gate first and is only recorded when its
// last shard reports in — the count file would deduplicate repeated counts, but
// done.lst would not, and a query marked collected after one quarter of its
// pages would never be finished on any later run.
func (w *worker) finishQuery(t *queue.Task, count int) (bool, error) {
	if t.Sharded() {
		last, final := w.deps.Shards.Reach(t.Name, count, t.ShardCount)
		if !last {
			return false, nil
		}
		count = final
	}
	if err := w.deps.Sink.AddCount(t.Name, count, time.Now()); err != nil {
		return false, err
	}
	w.deps.Sink.DropCheckpoint(t.Name, t.ShardCount)
	w.deps.Done.Append(t.Name)
	w.deps.Collected.Add(1)
	return true, nil
}

// reportDone logs a finished task, saying which shard it was when the query is
// not finished as a whole yet.
func (w *worker) reportDone(t *queue.Task, terminal bool, detail string) {
	if terminal {
		log.Printf("task %q: %s", t.Name, detail)
		return
	}
	log.Printf("task %q: shard %d/%d finished — %s", t.Name, t.Shard+1, t.ShardCount, detail)
}

// attemptResult is what one attempt produced: a rules action, the marker that
// produced it, an error, or — on a clean collection — the result count from the
// position line, which is what results.txt has to record.
type attemptResult struct {
	action string
	marker string
	count  int
	err    error
}

// attempt runs one search and turns whatever came back into an action, a
// marker or an error.
//
// The marker check lives here, not in the wait loops alone: a page can fail in
// a way that skips the loop's own check (a timeout on the home page, a dead
// click), and the rules file should still get its say before the attempt is
// written off as a plain failure.
func (w *worker) attempt(t *queue.Task) attemptResult {
	res := w.runAttempt(t)
	if res.err == nil || res.action != "" {
		return res
	}
	var m *markerErr
	if errors.As(res.err, &m) {
		return attemptResult{action: m.action, marker: m.marker}
	}
	if action, marker := w.matchRules(); action != "" {
		return attemptResult{action: action, marker: marker}
	}
	return res
}

// runAttempt opens the monitor, searches and collects. The error it returns is
// the raw one — attempt() is what grades it.
//
// A home reload costs 7-12s of a ~25s query, and the results page ships its own
// search box (#AUTO_input is in the select.jsp HTML), so a clean page left by
// the previous query is searched again in place. Only the first attempt may
// skip the navigation and only while the page still holds that box: a retry
// always reloads, because a fresh navigation is how a failed attempt drops
// whatever it left behind.
func (w *worker) runAttempt(t *queue.Task) attemptResult {
	if w.deps.Cfg.Replay() {
		res := w.replayAttempt(t)
		if res.err == nil || !errors.Is(res.err, replay.ErrNotJSON) {
			return res
		}
		// A response that is not a result page at all — a block wall, a
		// maintenance screen, a site that changed shape — is the one thing the
		// browser grades better: it gets a real session and a real fingerprint,
		// and its own classifier decides what the page was. The HTTP session is
		// dropped first so the next attempt does not reuse it either.
		log.Printf("task %q attempt %d: %v — falling back to the browser", t.Name, t.Attempts, res.err)
		w.rc = nil
		if !w.healthy() {
			if err := w.launch(false); err != nil {
				return attemptResult{err: fmt.Errorf("replay: %v (browser fallback: %w)", res.err, err)}
			}
		}
	}
	if t.Attempts > 1 || !w.pageOK {
		w.pageOK = false
		if err := w.openHome(); err != nil {
			return attemptResult{err: err}
		}
	} else if n, err := w.page.Locator(site.SearchInputSel).Count(); err != nil || n == 0 {
		if err := w.openHome(); err != nil {
			return attemptResult{err: err}
		}
	}
	if err := w.submitSearch(t); err != nil {
		return attemptResult{err: err}
	}
	res, err := w.waitOutcome()
	if err != nil {
		return attemptResult{err: err}
	}
	// Either answer is a clean page: it holds the search box the next query
	// needs, or the box check above sends that query home anyway.
	w.pageOK = true
	if res.none {
		w.saveFullPage(t.Name)
		return attemptResult{err: errNoResults}
	}
	count, err := w.collect(t, res.pager)
	if err != nil {
		w.pageOK = false
		return attemptResult{err: err}
	}
	return attemptResult{count: count}
}

// saveFullPage archives the whole current page when save_full is on. It is a
// debugging aid: a failure to read or write it is logged and swallowed, never
// allowed to take down work that already succeeded.
func (w *worker) saveFullPage(query string) {
	// Replay mode has no page to archive — its equivalent artifact is the raw
	// JSON every SaveGrid call writes.
	if !w.deps.Cfg.SaveFull || w.page == nil {
		return
	}
	html, err := w.page.Content()
	if err != nil {
		log.Printf("task %q: page content: %v (collecting continues)", query, err)
		return
	}
	if err := w.deps.Sink.SaveFull(query, html); err != nil {
		log.Printf("task %q: save full page: %v (collecting continues)", query, err)
	}
}

// matchRules runs the rules file over the current page text, if there is one.
// There is no page text in replay mode (HTTP responses are graded by the error
// classification instead), so the rules get nothing to match.
func (w *worker) matchRules() (action, marker string) {
	if w.deps.Rules == nil || w.page == nil {
		return "", ""
	}
	return w.deps.Rules.Match(w.paneText())
}

// replayClient returns the worker's HTTP session, creating it on first use.
//
// The user agent is the profile's pinned fingerprint, so the session and the
// browser a fallback may open later present the same machine to the site, and
// the endpoint comes from the proxy pool exactly as launch() would pick it —
// the pool knows which endpoints still reach WIPO, and the session follows it
// on a rotation.
// ensureProfile makes sure the profile directory exists and loads (or generates)
// the pinned fingerprint for this worker. It is called by both launch() and
// replayClient() to keep the setup in one place.
func (w *worker) ensureProfile() error {
	if err := os.MkdirAll(w.profileDir, 0755); err != nil {
		return fmt.Errorf("profile dir: %w", err)
	}
	w.fp = browser.LoadOrGenFingerprint(w.profileDir)
	return nil
}

func (w *worker) replayClient() (*replay.Client, error) {
	if w.rc != nil {
		w.rc.Timeout = w.resultsTimeout()
		return w.rc, nil
	}
	if w.proxy == "" {
		w.proxy = w.deps.Proxy.Pick()
	}
	if err := w.ensureProfile(); err != nil {
		return nil, err
	}
	c := replay.NewClient(w.fp.UserAgent)
	c.Timeout = w.resultsTimeout()
	if err := c.SetProxy(w.proxy); err != nil {
		log.Printf("worker %d: %v — replay goes through the environment instead", w.id, err)
	}
	w.at("replay session")
	if err := c.EnsureSession(); err != nil {
		return nil, err
	}
	w.rc = c
	return c, nil
}

// replayAttempt runs one query straight against select.jsp: no browser, no
// DOM, one HTTP round trip per result page. The contract mirrors runAttempt —
// a clean count, errNoResults, or an error for handle() to grade.
func (w *worker) replayAttempt(t *queue.Task) attemptResult {
	if _, err := w.replayClient(); err != nil {
		return attemptResult{err: err}
	}
	// The gate brakes on the search, exactly as in the browser path: the site
	// scores the source IP, and page walks are unguarded there too.
	w.deps.Gate.Wait(w.proxy)
	page, err := w.replaySearch(t.Name, 0, w.deps.Cfg.RowsPerPage)
	if err != nil {
		return attemptResult{err: err}
	}
	if page.NumFound == 0 {
		return attemptResult{err: errNoResults}
	}
	count, err := w.replayCollect(t, page)
	if err != nil {
		return attemptResult{err: err}
	}
	return attemptResult{count: count}
}

// replaySearch fetches one page of rows at a row offset. The round trip is
// timed: it is a step over the network like any other, so it feeds the
// watchdog and the pool's latency sample the same way a page load does.
func (w *worker) replaySearch(query string, start, rows int) (*replay.Page, error) {
	c, err := w.replayClient()
	if err != nil {
		return nil, err
	}
	w.at(fmt.Sprintf("replay row %d", start))
	pageStart := time.Now()
	page, err := c.Search(query, start, rows)
	if err != nil {
		return nil, err
	}
	w.deps.Proxy.NoteLatency(w.proxy, time.Since(pageStart))
	return page, nil
}

// replayCollect is collect() over HTTP: the same shard block, the same
// -start-page/-end-page bounds, the same resume checkpoint written after the
// rows, with the pager derived from the Solr numbers instead of read off the
// DOM. Each page's raw JSON lands in the archive the grid HTML would have
// taken — it is the source the TSV rows were parsed from, nothing less.
func (w *worker) replayCollect(t *queue.Task, first *replay.Page) (int, error) {
	pageSize := w.deps.Cfg.RowsPerPage
	if pageSize <= 0 {
		pageSize = 100
	}
	count := first.NumFound
	page := first

	from, to := 1, 0 // open range: the first page to the last
	if t.Sharded() {
		var ok bool
		from, to, ok = pageBlock(count, pageSize, t.ShardCount, t.Shard)
		if !ok {
			log.Printf("task %q shard %d/%d: past the last page of %d rows, nothing to collect",
				t.Name, t.Shard+1, t.ShardCount, count)
			return count, nil
		}
	}
	if s := w.deps.Cfg.StartPage; s > from {
		from = s
	}
	if e := w.deps.Cfg.EndPage; e > 0 && (to == 0 || e < to) {
		to = e
	}
	resume := w.deps.Sink.Checkpoint(t.Name, t.Shard, t.ShardCount)
	if resume >= from {
		if to > 0 && resume >= to {
			log.Printf("task %q: checkpoint page %d already covers %d..%d, nothing to collect",
				t.Name, resume, from, to)
			return count, nil
		}
		from = resume + 1
	}
	if last := lastGridPage(count, pageSize); from > last {
		log.Printf("task %q: starting at page %d, past the last page %d of %d rows, nothing to collect",
			t.Name, from, last, count)
		return count, nil
	}
	if from > 1 {
		log.Printf("task %q: starting at page %d (range %d..%s, %d rows known)",
			t.Name, from, from, pageBoundText(to), count)
	}

	start := time.Now()
	collectedAt := time.Now()
	pages := 0
	seen := map[string]struct{}{}
	firstPage := 0
	lastPage := 0

	for gridPage := from; ; gridPage++ {
		// The search already returned page 1; a shard or a resume starts
		// further in and fetches the offset it actually owns.
		if page == nil || page.Start != (gridPage-1)*pageSize {
			w.at(fmt.Sprintf("replay page %d", gridPage))
			var err error
			if page, err = w.replaySearch(t.Name, (gridPage-1)*pageSize, pageSize); err != nil {
				return count, err
			}
		}
		count = page.NumFound
		if firstPage == 0 {
			firstPage = gridPage
		}

		// The raw JSON is an archive, not the data — rows.tsv is. A full disk
		// must not burn retries on a request that already succeeded.
		if err := w.deps.Sink.SaveGrid(t.Name, gridPage, string(page.Raw)); err != nil {
			log.Printf("task %q: save grid page %d: %v (collecting continues)", t.Name, gridPage, err)
		}

		fresh := make([]site.Row, 0, len(page.Rows))
		for _, r := range page.Rows {
			if _, dup := seen[r.Key()]; dup {
				continue
			}
			seen[r.Key()] = struct{}{}
			fresh = append(fresh, r)
		}
		if err := w.deps.Sink.AddRows(t.Name, fresh, collectedAt); err != nil {
			return count, err
		}
		pages++
		lastPage = gridPage
		log.Printf("task %q: page %d (grid %d, %d-%d of %d), %d new rows, %d in total",
			t.Name, pages, gridPage, page.Start+1, page.Start+len(page.Rows), count, len(fresh), len(seen))
		w.at(fmt.Sprintf("replay page %d", gridPage))
		// Written after the rows, never before: a checkpoint that ran ahead of
		// the data would send the resume past rows that were never written.
		if err := w.deps.Sink.SetCheckpoint(t.Name, t.Shard, t.ShardCount, gridPage); err != nil {
			log.Printf("task %q: checkpoint page %d: %v (a restart would re-walk from the block start)",
				t.Name, gridPage, err)
		}

		if gridPage >= lastGridPage(count, pageSize) {
			break
		}
		// max_pages applies per shard: each shard walks up to MaxPages pages
		// of its own block. The total pages across all shards may exceed MaxPages.
		if max := w.deps.Cfg.MaxPages; max > 0 && pages >= max {
			log.Printf("task %q: max_pages=%d reached, %d of %d rows collected",
				t.Name, max, len(seen), count)
			break
		}
		if to > 0 && gridPage >= to {
			log.Printf("task %q: range end page %d reached, %d of %d rows collected",
				t.Name, to, len(seen), count)
			break
		}
		page = nil
	}

	if w.deps.Cfg.SavePDF && !w.savePDFLogged {
		log.Printf("save_pdf needs the browser path, skipped in replay mode (once per worker)")
		w.savePDFLogged = true
	}
	log.Printf("task %q: %d trademarks, %d rows over %d page(s) (grid pages %d-%d) in %s",
		t.Name, count, len(seen), pages, firstPage, lastPage, time.Since(start).Round(time.Millisecond))
	return count, nil
}

// lastGridPage is the grid page number the last row sits on, and at least 1:
// a zero-count query still has a page the search reported.
func lastGridPage(count, pageSize int) int {
	if pageSize <= 0 {
		return 1
	}
	last := count / pageSize
	if count%pageSize != 0 {
		last++
	}
	if last < 1 {
		last = 1
	}
	return last
}

// collect walks the result pages: read the grid, save it, parse the rows, and
// page on until the last one. The count comes from the position line, so it is
// the total for the whole query and not what happens to fit on page one.
// pageBlock returns the inclusive grid-page range one worker owns when a run's
// threads split a query's pages between them, and false when the worker has
// nothing to collect.
//
// Without this, -threads 4 on the "*" query is four browsers walking the same
// 15,430 pages from page 1 and throwing all but one copy away: the sink
// de-duplicates the output, so the run is not corrupted, but the time is not
// saved either and WIPO sees four times the requests for the same rows. Blocks
// are contiguous rather than interleaved, because every extra page boundary is
// a "go to page" round trip and a strided walk pays one per page instead of one
// per block.
//
// The split is uneven on purpose when the pages do not divide evenly, and the
// last worker takes the remainder — a few idle pages are cheaper than making
// anyone jump backwards to find a neighbour's work.
func pageBlock(totalRows, pageSize, workers, workerID int) (from, to int, ok bool) {
	if pageSize <= 0 || totalRows <= 0 || workers <= 0 || workerID < 0 || workerID >= workers {
		return 1, 0, false
	}
	totalPages := (totalRows + pageSize - 1) / pageSize
	if totalPages < 1 {
		totalPages = 1
	}
	if workers == 1 {
		return 1, totalPages, true
	}
	per := (totalPages + workers - 1) / workers
	from = workerID*per + 1
	if from > totalPages {
		return from, from, false
	}
	to = from + per - 1
	if to > totalPages {
		to = totalPages
	}
	return from, to, true
}

// collect walks this task's block of result pages and returns the result count
// from the position line — the number results.txt records. The count comes from
// the pager and not from len(seen): rows can be deduplicated against an earlier
// attempt, the total cannot.
//
// The block is three things intersected: the task's shard of the pages (when
// the run is split), the process's own -start-page/-end-page bounds, and the
// resume point left by the last run. Missing the resume point is what makes an
// interrupted crawl restart at page 1 and re-walk thousands of pages it has
// already written; missing the block boundaries is what makes a split run
// either skip rows or collect them four times.
func (w *worker) collect(t *queue.Task, pagerText string) (int, error) {
	if err := w.setRowsPerPage(); err != nil {
		return 0, err
	}
	// Switching the page size reloads the grid, so the position line that came
	// with the results page is stale by now — "1 - 10 / 955" while the grid
	// already holds 100 rows. Read it again before walking the pages.
	fresh, freshErr := w.readPager()
	if freshErr == nil {
		pagerText = fresh.PagerText
	} else {
		log.Printf("task %q: pager re-read: %v (keeping %q)", t.Name, freshErr, pagerText)
	}

	shape, err := site.ParsePager(pagerText)
	if err != nil {
		return 0, fmt.Errorf("pager: %w", err)
	}
	count := shape.Total

	// The site's real page size, read once from the fresh position line — the
	// search always lands on page 1, so that line holds a full page — and used
	// for every page number in the walk. Deriving it again from each line's row
	// count would take the tail page (which legitimately holds fewer rows) for a
	// page of its own size: 36 leftover rows of an 100-row grid computed
	// "page 517", naming the archive file and the checkpoint after a page that
	// does not exist, so a restart would resume past the end.
	pageSize := w.deps.Cfg.RowsPerPage
	if n := shape.To - shape.From + 1; n > 0 {
		pageSize = n
	}
	if pageSize != w.deps.Cfg.RowsPerPage && shape.To < shape.Total {
		log.Printf("task %q: grid is showing %d rows per page, not the %d that was asked for",
			t.Name, pageSize, w.deps.Cfg.RowsPerPage)
	}

	from, to := 1, 0 // open range: the site's first page to its last
	if t.Sharded() {
		if freshErr != nil {
			// A shard's block is derived from the page size the grid is really
			// using now; the stale line from before the resize would draw the
			// blocks wrong, and a wrong boundary skips rows rather than merely
			// re-collecting them. Requeue instead of guessing.
			return count, fmt.Errorf("shard %d/%d needs a fresh position line after the page-size change: %w",
				t.Shard+1, t.ShardCount, freshErr)
		}
		var ok bool
		from, to, ok = pageBlock(shape.Total, pageSize, t.ShardCount, t.Shard)
		if !ok {
			log.Printf("task %q shard %d/%d: past the last page of %d rows, nothing to collect",
				t.Name, t.Shard+1, t.ShardCount, shape.Total)
			return count, nil
		}
	}
	if s := w.deps.Cfg.StartPage; s > from {
		from = s
	}
	if e := w.deps.Cfg.EndPage; e > 0 && (to == 0 || e < to) {
		to = e
	}

	// Resume: the checkpoint page was written to rows.tsv, so collecting starts
	// after it. The checkpoint is per query and shard, and it never points past
	// the data — a lost checkpoint only costs a re-walk that dedupe absorbs.
	resume := w.deps.Sink.Checkpoint(t.Name, t.Shard, t.ShardCount)
	if resume >= from {
		if to > 0 && resume >= to {
			log.Printf("task %q: checkpoint page %d already covers %d..%d, nothing to collect",
				t.Name, resume, from, to)
			return count, nil
		}
		from = resume + 1
	}

	// The search always lands on page 1. Anything that starts further in — a
	// process shard, a resume, a split block — has to jump there before
	// collecting anything, otherwise every walk would write the same first page.
	if from > 1 {
		jumped, err := w.gotoPage(from, pagerText)
		if err != nil {
			return count, fmt.Errorf("start at page %d: %w", from, err)
		}
		log.Printf("task %q: starting at page %d (range %d..%s, %d rows known, %s)",
			t.Name, from, from, pageBoundText(to), count, jumped)
		pagerText = jumped
	}

	start := time.Now()
	collectedAt := time.Now()
	pages := 0
	seen := map[string]struct{}{}
	firstPage := 0
	lastPage := 0
	// pageStart times the current page's network step, so a page that took a
	// long time is a measured latency rather than an unattributed stall.
	pageStart := time.Now()

	for {
		p, err := site.ParsePager(pagerText)
		if err != nil {
			return count, err
		}
		// The grid page number. It is what the range end is compared against and
		// what the saved grid file is named after, so a shard's files line up
		// with the whole crawl instead of restarting at 1. pageSize is fixed
		// before the walk (see above): it is the full-page size, not the row
		// count of the line in hand, which is what makes the tail page a
		// numbered page instead of a fraction of one.
		gridPage := (p.From-1)/pageSize + 1
		if firstPage == 0 {
			firstPage = gridPage
		}

		grid, err := w.page.Locator(site.GridSel).First().InnerHTML(playwright.LocatorInnerHTMLOptions{
			Timeout: playwright.Float(15000),
		})
		if err != nil {
			return count, fmt.Errorf("read grid: %w", err)
		}
		// The raw grid is an archive, not the data — rows.tsv is. A full disk
		// or a locked file must not take the attempt down with it and burn
		// retries on a request that already succeeded.
		if err := w.deps.Sink.SaveGrid(t.Name, gridPage, grid); err != nil {
			log.Printf("task %q: save grid page %d: %v (collecting continues)", t.Name, gridPage, err)
		}

		rows, err := site.ParseGrid(grid)
		if err != nil {
			return count, err
		}
		fresh := make([]site.Row, 0, len(rows))
		for _, r := range rows {
			if _, dup := seen[r.Key()]; dup {
				continue // the grid re-served a row we already have
			}
			seen[r.Key()] = struct{}{}
			fresh = append(fresh, r)
		}
		if err := w.deps.Sink.AddRows(t.Name, fresh, collectedAt); err != nil {
			return count, err
		}
		pages++
		lastPage = gridPage
		log.Printf("task %q: page %d (grid %d, %d-%d of %d), %d new rows, %d in total",
			t.Name, pages, gridPage, p.From, p.To, p.Total, len(fresh), len(seen))
		// Each page is a step over the network, so it is both a progress beat
		// for the watchdog and a latency sample for the proxy it went through.
		w.at(fmt.Sprintf("collect page %d", gridPage))
		w.deps.Proxy.NoteLatency(w.proxy, time.Since(pageStart))
		// Written after the rows, never before: a checkpoint that ran ahead of
		// the data would send the resume past rows that were never written.
		if err := w.deps.Sink.SetCheckpoint(t.Name, t.Shard, t.ShardCount, gridPage); err != nil {
			log.Printf("task %q: checkpoint page %d: %v (a restart would re-walk from the block start)",
				t.Name, gridPage, err)
		}

		if p.Last() {
			break
		}
		if max := w.deps.Cfg.MaxPages; max > 0 && pages >= max {
			log.Printf("task %q: max_pages=%d reached, %d of %d rows collected",
				t.Name, max, len(seen), p.Total)
			break
		}
		// A range end — a process shard's -end-page or a split block's last
		// page — is not the end of the data: the site still has pages after it,
		// so the loop stops on the page number rather than on Pager.Last().
		// Skipping this check is how a shard quietly turns back into a full
		// crawl.
		if to > 0 && gridPage >= to {
			log.Printf("task %q: range end page %d reached, %d of %d rows collected",
				t.Name, to, len(seen), p.Total)
			break
		}
		pageStart = time.Now()
		pagerText, err = w.nextPage(pagerText)
		if err != nil {
			return count, err
		}
	}

	// The first page is the one worth printing, and Chromium only prints in
	// headless mode — a doomed call per query in a headed run is pure waste.
	if w.deps.Cfg.SavePDF {
		if !w.deps.Cfg.Headless {
			log.Printf("task %q: save_pdf needs headless=true, skipped", t.Name)
		} else if _, err := w.page.PDF(playwright.PagePdfOptions{
			Path: playwright.String(w.deps.Sink.SavePDFPath(t.Name)),
		}); err != nil {
			log.Printf("task %q: pdf: %v", t.Name, err)
		}
	}
	w.saveFullPage(t.Name)

	log.Printf("task %q: %d trademarks, %d rows over %d page(s) (grid pages %d-%d) in %s",
		t.Name, count, len(seen), pages, firstPage, lastPage, time.Since(start).Round(time.Millisecond))
	return count, nil
}

// pageBoundText renders a range end for a log line: 0 means the site's own
// last page, which is not a number worth inventing.
func pageBoundText(to int) string {
	if to <= 0 {
		return "last"
	}
	return fmt.Sprintf("%d", to)
}

// switchProxy moves to a fresh endpoint on the same profile, so the warm
// cookies stay. A new IP may earn one more block page, which the normal marker
// path handles.
func (w *worker) switchProxy() {
	next := w.deps.Proxy.Pick()
	if next == "" || next == w.proxy {
		return
	}
	w.proxy = next
	// Replay holds no browser to relaunch: the session is dropped instead, so
	// the next attempt re-picks the endpoint and re-handshakes on the new IP.
	// The browser, if one is open from a fallback, is rotated the usual way.
	if w.ctx == nil && w.page == nil {
		w.rc = nil
		return
	}
	if err := w.launch(false); err != nil {
		log.Printf("worker %d: proxy switch failed: %v", w.id, err)
	}
}

// onBlockHit cools the endpoint down and counts the streak; after a few in a
// row the profile is wiped together with the proxy, because by then the
// session itself looks wrong.
func (w *worker) onBlockHit(taskName string) {
	w.deps.Proxy.CoolDown(w.proxy)
	w.blockRuns++
	if w.blockRuns < blockStreak {
		return
	}
	// Nothing to wipe when the block came in without a browser: replay has no
	// profile, and the session is rebuilt on the next attempt anyway.
	if w.ctx == nil && w.page == nil {
		return
	}
	log.Printf("worker %d: %d block pages in a row (task %q), wiping the profile and the proxy",
		w.id, w.blockRuns, taskName)
	if err := w.launch(true); err != nil {
		log.Printf("worker %d: rotation failed: %v", w.id, err)
	}
}

// randomPause sleeps a random time in [baseMs, baseMs+jitterMs].
func randomPause(baseMs, jitterMs int) {
	time.Sleep(time.Duration(float64(baseMs)+rand.Float64()*float64(jitterMs)) * time.Millisecond)
}

// hiccupMarkers are the failures that only prove a proxy is slow, not that it
// is dead. They are matched case-insensitively against the error text, so they
// are spelled the way Chromium and Go spell them.
//
// "err_proxy" is deliberately NOT in this list: it is a prefix of
// ERR_PROXY_CONNECTION_FAILED and ERR_PROXY_AUTH_UNSUPPORTED, both of which mean
// the endpoint itself is finished and must rotate out on a full failure.
var hiccupMarkers = []string{
	// Chromium network codes
	"err_timed_out",
	"err_connection_reset",
	"err_connection_closed",
	"err_connection_aborted",
	"err_networking_changed",
	"err_empty_response",
	"err_socket_not_connected",
	"err_name_not_resolved",
	"err_internet_disconnected",
	// Go and transport wording
	"deadline exceeded",
	"context canceled",
	"timeout",
	"i/o timeout",
	"handshake",
	"eof",
	// our own timeouts, which are the same story seen from this side: the page
	// never finished arriving within the budget we gave it
	"watchdog",
	"never appeared",
	"did not advance",
	"still loading",
}

// timeoutMarkers are the errors that mean a step ran out of its budget without
// finishing. They are a subset of hiccupMarkers, and they get separate treatment
// because a budget that was too small is a different problem from a reset
// connection: the first is answered by waiting longer, the second by rotating.
var timeoutMarkers = []string{
	"err_timed_out",
	"deadline exceeded",
	"timeout",
	"i/o timeout",
	"watchdog",
	"never appeared",
	"did not advance",
	"still loading",
	"page did not load",
}

// isTimeout reports that a step ran into its own deadline.
func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range timeoutMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// isTransportHiccup reports a network-level stall rather than proof that the
// endpoint is dead. Endpoint-level codes (ERR_PROXY_CONNECTION_FAILED,
// ERR_PROXY_AUTH_UNSUPPORTED, ERR_TUNNEL_CONNECTION_FAILED,
// ERR_SOCKS_CONNECTION_FAILED) fall through on purpose: those mean the proxy
// itself is gone and must rotate out on a full failure instead of getting a pass
// as "just slow".
func isTransportHiccup(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range hiccupMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}
