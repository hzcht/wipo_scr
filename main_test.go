package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"wipos/internal/config"
	"wipos/internal/queue"
)

// isTransportHiccup decides whether an endpoint is merely slow or plainly
// unusable, and that decision is what rotates a proxy out of the run. A wrong
// "hiccup" keeps a dead proxy in front of the collector for five more attempts;
// a wrong "hard failure" throws away a slow but working endpoint. Both cost
// queries, so the boundaries are pinned here.
func TestIsTransportHiccup(t *testing.T) {
	soft := []error{
		context.DeadlineExceeded,
		errors.New("net::ERR_TIMED_OUT at https://www3.wipo.int/"),
		errors.New("net::ERR_CONNECTION_RESET"),
		errors.New("net::ERR_EMPTY_RESPONSE"),
		errors.New("net::ERR_NAME_NOT_RESOLVED"),
		fmt.Errorf("results never appeared: %w", context.DeadlineExceeded),
		errors.New("page did not advance past \"1 - 100 / 955\""),
		errors.New("grid still loading after 3m0s"),
		errors.New("attempt watchdog: stuck at stage \"next page\" (4m0s)"),
		// A browser that did not come up in time is a silent proxy, and a silent
		// proxy on a free list is usually a slow one — half a failure is the right
		// charge, a full one would throw away a working endpoint.
		errors.New("launch: context deadline exceeded"),
		errors.New("remote error: tls: handshake failure"),
		errors.New("unexpected EOF"),
		errors.New("context canceled"),
	}
	for _, err := range soft {
		if !isTransportHiccup(err) {
			t.Errorf("isTransportHiccup(%v) = false, want true (a slow proxy, not a dead one)", err)
		}
	}

	// The proxy itself is finished: those must cost a full failure, or a dead
	// endpoint stays in front of the collector for five more attempts.
	hard := []error{
		nil,
		errors.New("query has no trademarks"),
		errors.New("net::ERR_PROXY_CONNECTION_FAILED"),
		errors.New("net::ERR_PROXY_AUTH_UNSUPPORTED"),
		errors.New("net::ERR_TUNNEL_CONNECTION_FAILED 407"),
		errors.New("net::ERR_SOCKS_CONNECTION_FAILED"),
		errors.New("not a results page"),
	}
	for _, err := range hard {
		if isTransportHiccup(err) {
			t.Errorf("isTransportHiccup(%v) = true, want false", err)
		}
	}
}

// isTimeout is what separates "the budget was too small" from "the link is
// broken", and the two get opposite treatment: a timeout raises the endpoint's
// measured latency and retries in place, while a hard failure rotates out. A
// timeout misread as a hard failure retires the only proxy that still reaches
// WIPO from this machine, which is how a working crawl dies.
func TestIsTimeout(t *testing.T) {
	late := []error{
		context.DeadlineExceeded,
		errors.New("net::ERR_TIMED_OUT at https://www3.wipo.int/"),
		errors.New("home: net/http: TLS handshake timeout"),
		errors.New("attempt watchdog: stuck at stage \"next page\" for 4m0s"),
		errors.New("home: search box never appeared (1 - 30 / 163)"),
		errors.New("page did not advance past \"1 - 30 / 163\""),
		errors.New("grid still loading after 3m0s"),
	}
	for _, err := range late {
		if !isTimeout(err) {
			t.Errorf("isTimeout(%v) = false, want true (late, so raise the budget and retry)", err)
		}
	}

	// A reset or a dead proxy is broken, not late. Rotating away is correct and
	// waiting longer is not, so these must not match even though they are all
	// transport hiccups.
	broken := []error{
		nil,
		errors.New("net::ERR_CONNECTION_RESET"),
		errors.New("net::ERR_PROXY_CONNECTION_FAILED"),
		errors.New("net::ERR_TUNNEL_CONNECTION_FAILED 407"),
		errors.New("query has no trademarks"),
		errors.New("not a results page"),
	}
	for _, err := range broken {
		if isTimeout(err) {
			t.Errorf("isTimeout(%v) = true, want false (broken, so rotate instead of waiting)", err)
		}
	}
}

// The stall watchdog exists to catch a call that never returns. It must not
// mistake an attempt that is merely long for a stuck one, which is what made a
// 15,430-page crawl impossible when the watchdog was a total budget.
func TestWorkerBeatClearsTheStallClock(t *testing.T) {
	w := &worker{id: 1}
	w.at("open home")

	if got := w.stalledFor(); got > time.Second {
		t.Errorf("stalledFor() = %s right after a beat, want ~0", got)
	}

	// A heartbeat from a page walk keeps the clock at zero no matter how long the
	// attempt has been running. Only the gap between beats is what counts.
	for i := 0; i < 5; i++ {
		w.at("collect page 1")
		if got := w.stalledFor(); got > time.Second {
			t.Errorf("stalledFor() = %s after beat %d, want ~0", got, i)
		}
	}

	// Force the clock back by hand: a real test cannot wait out a real budget.
	w.lastBeat.Store(time.Now().Add(-10 * time.Minute).UnixNano())
	if got := w.stalledFor(); got < 9*time.Minute {
		t.Errorf("stalledFor() = %s after a 10m silence, want ~10m", got)
	}
}

// A worker that has not started must not read as stalled, or the watchdog would
// fire before the first beat lands.
func TestWorkerStallBeforeTheFirstBeat(t *testing.T) {
	w := &worker{id: 1}
	if got := w.stalledFor(); got != 0 {
		t.Errorf("stalledFor() = %s before the first beat, want 0", got)
	}
}

// The config-derived stall budget must clear the longest step a slow endpoint is
// allowed to take. Deriving it any lower kills attempts that were still inside
// their own timeouts — the failure mode this replaced.
func TestWatchdogBudgetClearsTheLongestStep(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.Config
	}{
		{"defaults", config.DefaultConfig()},
		{"slow budgets", func() config.Config {
			c := config.DefaultConfig()
			c.NavTimeoutMS = 120000
			c.ResultsTimeoutMS = 180000
			c.TimeoutScaleMax = 3
			return c
		}()},
		{"explicit", func() config.Config {
			c := config.DefaultConfig()
			c.WatchdogMS = 90000
			return c
		}()},
	}
	for _, tc := range cases {
		longest := time.Duration(tc.cfg.NavTimeoutMS) * time.Millisecond
		if r := time.Duration(tc.cfg.ResultsTimeoutMS) * time.Millisecond; r > longest {
			longest = r
		}
		got := tc.cfg.Watchdog()
		if tc.cfg.WatchdogMS > 0 {
			if got != 90*time.Second {
				t.Errorf("%s: Watchdog() = %s, want the explicit 90s", tc.name, got)
			}
			continue
		}
		if got <= longest {
			t.Errorf("%s: Watchdog() = %s, must exceed the longest step %s", tc.name, got, longest)
		}
	}
}

func TestMigrateLegacyState(t *testing.T) {
	dir := t.TempDir()
	inTempDir(t, dir)

	// The old collector kept two files: "lost" with bare queries, "counters"
	// with "<query> \t <pager text>". Both are already-collected queries.
	if err := os.WriteFile("lost", []byte("acme\nother\n"), 0644); err != nil {
		t.Fatalf("write lost: %v", err)
	}
	if err := os.WriteFile("counters", []byte("third\t1 - 100 / 955\nfourth\t1 - 10 / 10\n"), 0644); err != nil {
		t.Fatalf("write counters: %v", err)
	}

	done, err := queue.LoadStore(filepath.Join(dir, "done.lst"))
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	migrateLegacyState(done)

	for _, want := range []string{"acme", "other", "third", "fourth"} {
		if !done.Has(want) {
			t.Errorf("done.lst is missing %q after the migration", want)
		}
	}
	if done.Len() != 4 {
		t.Errorf("done.lst has %d entries, want 4", done.Len())
	}

	// It has to survive a restart, or the migration is worth nothing.
	reloaded, err := queue.LoadStore(filepath.Join(dir, "done.lst"))
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	if reloaded.Len() != 4 {
		t.Errorf("the migrated done.lst has %d entries after a reload, want 4", reloaded.Len())
	}
}

func TestMigrateLegacyStateWithoutTheOldFiles(t *testing.T) {
	dir := t.TempDir()
	inTempDir(t, dir)

	done, err := queue.LoadStore(filepath.Join(dir, "done.lst"))
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	migrateLegacyState(done) // must log and carry on when the old files are gone
	if done.Len() != 0 {
		t.Errorf("done.lst has %d entries, want 0", done.Len())
	}
}

func TestRandomPauseStaysInRange(t *testing.T) {
	const base, jitter = 20, 10
	for i := 0; i < 50; i++ {
		start := time.Now()
		randomPause(base, jitter)
		waited := time.Since(start)
		if waited < base*time.Millisecond || waited > (base+jitter+200)*time.Millisecond {
			t.Fatalf("randomPause(%d, %d) waited %s, want %d..%dms", base, jitter, waited, base, base+jitter)
		}
	}
}

// inTempDir moves into dir for the duration of the test: the legacy migration
// reads "./lost" and "./counters", which is where the old collector left them.
func inTempDir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir %s: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
}

// pageBlock decides which pages each thread of a split run owns. Two properties
// decide whether a split run collects the whole database at all: every page
// belongs to exactly one block (a gap loses rows, an overlap duplicates them),
// and a run with more threads than pages does not invent pages past the end.
func TestPageBlock(t *testing.T) {
	t.Run("the whole database over four threads", func(t *testing.T) {
		const rows, perPage, workers = 1542912, 100, 4
		// ceil(1542912 / 100): the database grows, so the number of pages is
		// derived rather than assumed — hard-coding it is how a comment drifted
		// one page ahead of reality before.
		totalPages := (rows + perPage - 1) / perPage
		if totalPages != 15430 {
			t.Fatalf("page count for %d rows at %d per page = %d, want 15430", rows, perPage, totalPages)
		}
		covered := map[int]int{}
		for shard := 0; shard < workers; shard++ {
			from, to, ok := pageBlock(rows, perPage, workers, shard)
			if !ok {
				t.Fatalf("shard %d: ok = false, it should have pages", shard)
			}
			for p := from; p <= to; p++ {
				covered[p]++
			}
		}
		for p := 1; p <= totalPages; p++ {
			switch n := covered[p]; {
			case n == 0:
				t.Errorf("page %d belongs to no block: rows would be lost", p)
			case n > 1:
				t.Errorf("page %d belongs to %d blocks: rows would be duplicated", p, n)
			}
		}
		if len(covered) != totalPages {
			t.Errorf("blocks cover %d pages, want %d", len(covered), totalPages)
		}
	})

	t.Run("one thread walks everything", func(t *testing.T) {
		from, to, ok := pageBlock(955, 100, 1, 0)
		if !ok || from != 1 || to != 10 {
			t.Errorf("pageBlock(955, 100, 1, 0) = %d..%d ok=%v, want 1..10 ok=true", from, to, ok)
		}
	})

	t.Run("uneven division leaves nobody out", func(t *testing.T) {
		// 10 pages over 3 threads: 4 + 4 + 2, with the remainder at the end.
		want := [][2]int{{1, 4}, {5, 8}, {9, 10}}
		for shard, w := range want {
			from, to, ok := pageBlock(1000, 100, 3, shard)
			if !ok || from != w[0] || to != w[1] {
				t.Errorf("shard %d = %d..%d ok=%v, want %d..%d ok=true", shard, from, to, ok, w[0], w[1])
			}
		}
	})

	t.Run("more threads than pages", func(t *testing.T) {
		// One page, four threads: the first takes it and the rest report empty
		// rather than walking pages that do not exist.
		if from, to, ok := pageBlock(42, 100, 4, 0); !ok || from != 1 || to != 1 {
			t.Errorf("shard 0 = %d..%d ok=%v, want 1..1 ok=true", from, to, ok)
		}
		for shard := 1; shard < 4; shard++ {
			if _, _, ok := pageBlock(42, 100, 4, shard); ok {
				t.Errorf("shard %d: ok = true, want false — there is no page for it", shard)
			}
		}
	})

	t.Run("nonsense inputs produce nothing", func(t *testing.T) {
		cases := []struct{ rows, pageSize, workers, id int }{
			{0, 100, 4, 0},     // no rows
			{1000, 0, 4, 0},    // no page size
			{1000, 100, 0, 0},  // no workers
			{1000, 100, 4, 4},  // worker out of range
			{1000, 100, 4, -1}, // negative worker
		}
		for _, c := range cases {
			if _, _, ok := pageBlock(c.rows, c.pageSize, c.workers, c.id); ok {
				t.Errorf("pageBlock(%d, %d, %d, %d): ok = true, want false",
					c.rows, c.pageSize, c.workers, c.id)
			}
		}
	})
}

// shardGate is what keeps a split query from being marked collected after its
// first quarter — and what makes the recorded count the one the site actually
// reported, not the first shard's possibly stale reading.
func TestShardGate(t *testing.T) {
	g := newShardGate()

	// Three of four shards in: the query is not done and nothing is recorded.
	for shard, count := range []int{100, 300, 200} {
		if last, _ := g.Reach("*", count, 4); last {
			t.Fatalf("shard %d of 4 reported last = true, want false", shard)
		}
	}
	last, final := g.Reach("*", 250, 4)
	if !last {
		t.Error("4th shard: last = false, want true — the query is complete")
	}
	// The largest count wins: the total can grow while a split run walks it,
	// and recording the low early reading would contradict rows.tsv.
	if final != 300 {
		t.Errorf("final count = %d, want 300 (the largest any shard saw)", final)
	}

	// A second query is tracked on its own.
	if last, _ := g.Reach("other", 5, 2); last {
		t.Error("first shard of a different query reported last = true")
	}
	if last, final := g.Reach("other", 5, 2); !last || final != 5 {
		t.Errorf("second shard of the other query: last=%v final=%d, want true/5", last, final)
	}

	// Names go through the same lowercasing as everywhere else.
	if last, _ := g.Reach("ACME", 1, 1); !last {
		t.Error("single shard under a different case: last = false, want true")
	}
	if last, _ := g.Reach("  acme  ", 1, 1); !last {
		t.Error("single shard with padding: last = false, want true")
	}
}
