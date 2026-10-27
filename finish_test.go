package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wipos/internal/config"
	"wipos/internal/queue"
	"wipos/internal/site"
)

// finishHarness is a worker wired only for finishQuery: the sink that writes
// the output files and the three stores that decide what a query becomes.
type finishHarness struct {
	w      *worker
	dir    string
	done   *queue.Store
	short  *queue.Store
	sink   *site.Sink
	colled atomic.Int64
}

func newFinishHarness(t *testing.T) *finishHarness {
	t.Helper()
	dir := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.ShortFile = filepath.Join(dir, "short.lst")

	load := func(name string) *queue.Store {
		t.Helper()
		s, err := queue.LoadStore(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("LoadStore(%s): %v", name, err)
		}
		return s
	}
	h := &finishHarness{
		dir:   dir,
		done:  load("done.lst"),
		short: load("short.lst"),
		sink: site.NewSink(
			filepath.Join(dir, "results.txt"),
			filepath.Join(dir, "trademarks.tsv"),
			dir, 0, false, false),
	}
	h.sink.Load(func(string) bool { return false })
	h.w = &worker{deps: &Deps{
		Cfg:       cfg,
		Sink:      h.sink,
		Done:      h.done,
		Short:     h.short,
		Collected: &h.colled,
		Shards:    newShardGate(),
	}}
	return h
}

func (h *finishHarness) results(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(h.dir, "results.txt"))
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read results: %v", err)
	}
	return string(raw)
}

// A walk that reached the end with the rows the position line promised is
// collected: counted, marked done, checkpoint gone, hashes released.
func TestFinishQueryCollectsACompleteWalk(t *testing.T) {
	h := newFinishHarness(t)
	tk := &queue.Task{Name: "the-royal"}
	if err := h.sink.SetCheckpoint(tk.Name, 0, 0, 3, 300); err != nil {
		t.Fatalf("SetCheckpoint: %v", err)
	}

	outcome, err := h.w.finishQuery(tk, 300, 300, false)
	if err != nil {
		t.Fatalf("finishQuery: %v", err)
	}
	if outcome != finishCollected {
		t.Fatalf("outcome = %v, want finishCollected", outcome)
	}
	if !h.done.Has("the-royal") {
		t.Error("a complete walk was not marked collected")
	}
	if !strings.Contains(h.results(t), "the-royal\t300") {
		t.Errorf("results.txt = %q, want the count", h.results(t))
	}
	if page, _ := h.sink.Checkpoint(tk.Name, 0, 0); page != 0 {
		t.Errorf("checkpoint survived a completed query (page %d)", page)
	}
	if h.colled.Load() != 1 {
		t.Errorf("Collected = %d, want 1", h.colled.Load())
	}
}

// A gap smaller than one page is the repeats the site serves across a page
// boundary: accepting it is the whole point of measuring in pages, or every run
// would re-walk the same query forever.
func TestFinishQueryAcceptsASubPageGap(t *testing.T) {
	h := newFinishHarness(t)
	outcome, err := h.w.finishQuery(&queue.Task{Name: "gap"}, 300, 250, false)
	if err != nil {
		t.Fatalf("finishQuery: %v", err)
	}
	if outcome != finishCollected {
		t.Errorf("outcome = %v, want finishCollected (50 rows missing of a 100-row page)", outcome)
	}
	if !h.done.Has("gap") {
		t.Error("the query was not collected")
	}
}

// A walk that ends a whole page (or more) short of the position line has a hole
// in it. It must be counted but not collected: done.lst is permanent, and the
// next run has to walk it again — with the checkpoint gone, so it starts where
// the hole can actually be filled.
func TestFinishQueryRefusesAShortWalk(t *testing.T) {
	h := newFinishHarness(t)
	tk := &queue.Task{Name: "tv di gi"}
	if err := h.sink.SetCheckpoint(tk.Name, 0, 0, 88, 2974); err != nil {
		t.Fatalf("SetCheckpoint: %v", err)
	}

	outcome, err := h.w.finishQuery(tk, 8774, 2974, false)
	if err != nil {
		t.Fatalf("finishQuery: %v", err)
	}
	if outcome != finishShort {
		t.Fatalf("outcome = %v, want finishShort", outcome)
	}
	if h.done.Has("tv di gi") {
		t.Error("a short walk was marked collected — done.lst is permanent, the hole would never be filled")
	}
	if !h.short.Has("tv di gi") {
		t.Error("the query was not recorded in short.lst")
	}
	if !strings.Contains(h.results(t), "tv di gi\t8774") {
		t.Errorf("results.txt = %q, want the count recorded anyway", h.results(t))
	}
	if page, _ := h.sink.Checkpoint(tk.Name, 0, 0); page != 0 {
		t.Errorf("checkpoint kept at page %d: the next run would resume straight past the hole", page)
	}
	if h.colled.Load() != 0 {
		t.Errorf("Collected = %d, want 0", h.colled.Load())
	}
	// The record carries the numbers, not just the name: someone reading
	// short.lst has to see what is missing without re-deriving it.
	line, err := os.ReadFile(filepath.Join(h.dir, "short.lst"))
	if err != nil {
		t.Fatalf("read short.lst: %v", err)
	}
	if got := string(line); !strings.Contains(got, "tv di gi\t8774\t2974") {
		t.Errorf("short.lst = %q, want 'query<TAB>count<TAB>rows'", got)
	}
}

// The second shortfall in a row is in the data, not in the walk: re-walking a
// third time would cost the same pages and fix the same nothing, so it is
// accepted and keeps its line as the record.
func TestFinishQueryAcceptsARepeatedShortfall(t *testing.T) {
	h := newFinishHarness(t)
	tk := &queue.Task{Name: "again"}
	h.short.Append("again") // a previous run already found it short

	outcome, err := h.w.finishQuery(tk, 8774, 2974, false)
	if err != nil {
		t.Fatalf("finishQuery: %v", err)
	}
	if outcome != finishCollected {
		t.Fatalf("outcome = %v, want finishCollected on the second shortfall", outcome)
	}
	if !h.done.Has("again") {
		t.Error("the repeated shortfall was not accepted")
	}
	if !h.short.Has("again") {
		t.Error("the line in short.lst must stay as the record of what is missing")
	}
}

// max_pages stops the walk short of the data. That is not a collected query:
// its checkpoint is what a later run continues from, and done.lst would end
// that resume for good.
func TestFinishQueryRefusesAPartialWalk(t *testing.T) {
	h := newFinishHarness(t)
	tk := &queue.Task{Name: "star"}
	if err := h.sink.SetCheckpoint(tk.Name, 0, 0, 7, 700); err != nil {
		t.Fatalf("SetCheckpoint: %v", err)
	}

	outcome, err := h.w.finishQuery(tk, 1528768, 700, true)
	if err != nil {
		t.Fatalf("finishQuery: %v", err)
	}
	if outcome != finishPartial {
		t.Fatalf("outcome = %v, want finishPartial", outcome)
	}
	if h.done.Has("star") {
		t.Error("a partial walk was marked collected")
	}
	if h.results(t) != "" {
		t.Errorf("results.txt = %q, want nothing written for a partial walk", h.results(t))
	}
	if page, _ := h.sink.Checkpoint(tk.Name, 0, 0); page != 7 {
		t.Errorf("checkpoint = %d, want 7 kept for the run that continues", page)
	}
	if h.short.Has("star") {
		t.Error("a partial walk is not a shortfall — max_pages is a setting, not a hole")
	}
}

// An unknown row total (a checkpoint written by an older build, a shard, a
// bounded range) skips the check rather than guessing: wrongly refusing a good
// walk costs a full re-collection, wrongly accepting one costs a hole nobody
// can see either way.
func TestFinishQuerySkipsTheCheckWhenRowsAreUnknown(t *testing.T) {
	h := newFinishHarness(t)
	outcome, err := h.w.finishQuery(&queue.Task{Name: "unknown"}, 9000, -1, false)
	if err != nil {
		t.Fatalf("finishQuery: %v", err)
	}
	if outcome != finishCollected {
		t.Errorf("outcome = %v, want finishCollected (rows unknown = no verdict)", outcome)
	}
}

// A split query is only recorded when its last piece reports in: the count file
// would deduplicate repeated counts, done.lst would not.
func TestFinishQueryWaitsForTheLastShard(t *testing.T) {
	h := newFinishHarness(t)
	first := &queue.Task{Name: "big", Shard: 0, ShardCount: 2}
	outcome, err := h.w.finishQuery(first, 100, 100, false)
	if err != nil {
		t.Fatalf("finishQuery: %v", err)
	}
	if outcome != finishShard {
		t.Fatalf("outcome = %v, want finishShard", outcome)
	}
	if h.done.Has("big") || h.results(t) != "" {
		t.Error("the query was recorded after one piece of two")
	}

	last := &queue.Task{Name: "big", Shard: 1, ShardCount: 2}
	outcome, err = h.w.finishQuery(last, 5000, -1, false)
	if err != nil {
		t.Fatalf("finishQuery (last shard): %v", err)
	}
	if outcome != finishCollected || !h.done.Has("big") {
		t.Errorf("outcome = %v, done = %v, want collected once the last piece landed",
			outcome, h.done.Has("big"))
	}
}

// shortOfCount is where the verdict is made, so its boundaries are pinned: a
// page short is short, one row under a page is not, and no total is no verdict.
func TestShortOfCountBoundaries(t *testing.T) {
	h := newFinishHarness(t)
	page := config.DefaultConfig().RowsPerPage
	tk := &queue.Task{Name: "q"}

	for _, tc := range []struct {
		name        string
		count, rows int
		wantShort   bool
	}{
		{"exactly one page short", 1000, 1000 - page, true},
		{"one row under a page", 1000, 1000 - page + 1, false},
		{"complete", 1000, 1000, false},
		{"more rows than promised", 1000, 1100, false},
		{"unknown rows", 1000, -1, false},
		{"no rows reported", 1000, 0, true},
	} {
		got, _ := h.w.shortOfCount(tk, tc.count, tc.rows)
		if got != tc.wantShort {
			t.Errorf("%s: shortOfCount(%d, %d) = %v, want %v",
				tc.name, tc.count, tc.rows, got, tc.wantShort)
		}
	}

	// Only a walk of the whole result set can be held against the total.
	bounded := &queue.Task{Name: "q"}
	h.w.deps.Cfg.EndPage = 50
	if short, _ := h.w.shortOfCount(bounded, 1000, 0); short {
		t.Error("a -end-page slice was measured against the whole result set")
	}
	h.w.deps.Cfg.EndPage = 0
	h.w.deps.Cfg.MaxPages = 10
	if short, _ := h.w.shortOfCount(bounded, 1000, 0); short {
		t.Error("a max_pages walk was measured against the whole result set")
	}
	h.w.deps.Cfg.MaxPages = 0
	if short, _ := h.w.shortOfCount(&queue.Task{Name: "q", Shard: 1, ShardCount: 4}, 1000, 0); short {
		t.Error("a shard was measured against the whole result set")
	}
}

// The clock behind short.lst's record: one line per query, whatever the run.
func TestFinishQueryRecordsTheDate(t *testing.T) {
	h := newFinishHarness(t)
	if _, err := h.w.finishQuery(&queue.Task{Name: "dated"}, 500, 0, false); err != nil {
		t.Fatalf("finishQuery: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(h.dir, "short.lst"))
	if err != nil {
		t.Fatalf("read short.lst: %v", err)
	}
	if !strings.Contains(string(raw), time.Now().Format("2006-01-02")) {
		t.Errorf("short.lst = %q, want today's date", raw)
	}
}
