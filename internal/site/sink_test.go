package site

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestSink(t *testing.T) *Sink {
	t.Helper()
	dir := t.TempDir()
	return NewSink(
		filepath.Join(dir, "results.txt"),
		filepath.Join(dir, "trademarks.tsv"),
		dir,
		true, true,
	)
}

func TestAddCountWritesOnce(t *testing.T) {
	s := newTestSink(t)
	day := time.Date(2026, 3, 4, 10, 30, 0, 0, time.UTC)

	if err := s.AddCount("Acme", 955, day); err != nil {
		t.Fatalf("AddCount: %v", err)
	}
	// A second count for the same query must not append: the count file is one
	// line per query, whatever the workers do.
	if err := s.AddCount("acme", 12, day.Add(time.Hour)); err != nil {
		t.Fatalf("AddCount (repeat): %v", err)
	}
	got := readFile(t, s.resultsPath)
	if want := "2026-03-04\tacme\t955\n"; got != want {
		t.Errorf("results file = %q, want %q", got, want)
	}
	if !s.Counted("ACME") {
		t.Error("Counted(ACME) = false after a successful AddCount(acme)")
	}
}

func TestLoadResumesFromExistingCountFile(t *testing.T) {
	s := newTestSink(t)
	writeFile(t, s.resultsPath, "2026-03-01\tacme\t10\nbroken line\n2026-03-02\tother\t3\n")
	before := readFile(t, s.resultsPath)
	if err := s.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !s.Counted("acme") || !s.Counted("Other") {
		t.Error("Load did not pick the existing queries up")
	}
	if err := s.AddCount("acme", 999, time.Now()); err != nil {
		t.Fatalf("AddCount: %v", err)
	}
	if after := readFile(t, s.resultsPath); after != before {
		t.Errorf("count file changed:\n%s", after)
	}
	if err := s.AddCount("fresh", 1, time.Now()); err != nil {
		t.Fatalf("AddCount (new): %v", err)
	}
	if after := readFile(t, s.resultsPath); after == before {
		t.Error("a query that was not in the file did not get a line")
	}
}

func TestAddRowsDeduplicates(t *testing.T) {
	s := newTestSink(t)
	at := time.Date(2026, 3, 4, 10, 30, 15, 0, time.UTC)
	rows := []Row{
		{Brand: "ACME", Holder: "Kronoplus", RecordID: "ROM.1", Number: 0},
		{Brand: "ACME TWO", Holder: "Kronoplus", RecordID: "ROM.2", Number: 1},
	}
	if err := s.AddRows("acme", rows, at); err != nil {
		t.Fatalf("AddRows: %v", err)
	}
	// A requeued query re-reads the same page: the rows must not be written
	// twice.
	if err := s.AddRows("acme", rows, at); err != nil {
		t.Fatalf("AddRows (repeat): %v", err)
	}
	// Another query may well have an identically numbered record.
	if err := s.AddRows("other", rows[:1], at); err != nil {
		t.Fatalf("AddRows (other query): %v", err)
	}

	got := readFile(t, s.rowsPath)
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("rows file has %d lines, want 3:\n%s", len(lines), got)
	}
	if !strings.HasPrefix(lines[0], "2026-03-04 10:30:15\tacme\tACME\t") {
		t.Errorf("row 0 = %q", lines[0])
	}
	if fields := strings.Split(lines[0], "\t"); len(fields) != 13 {
		t.Errorf("row 0 has %d fields, want 13 (stamp, query, 11 grid columns)", len(fields))
	}
	if !strings.HasPrefix(lines[2], "2026-03-04 10:30:15\tother\t") {
		t.Errorf("row 2 = %q, want the other query's record", lines[2])
	}
}

func TestAddRowsKeepsKeysForARetryAfterAFailedWrite(t *testing.T) {
	dir := t.TempDir()
	rowsPath := filepath.Join(dir, "trademarks.tsv")
	s := NewSink(filepath.Join(dir, "results.txt"), rowsPath, dir, false, false)
	row := Row{Brand: "ACME", RecordID: "ROM.1", Number: 0}

	// A write that cannot succeed: the rows file is a directory.
	if err := os.Mkdir(rowsPath, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := s.AddRows("acme", []Row{row}, time.Now()); err == nil {
		t.Fatal("AddRows into a directory returned no error")
	}
	// Now the path is writable again. The row must still be written: the key was
	// not recorded, because nothing reached the file.
	if err := os.Remove(rowsPath); err != nil {
		t.Fatalf("rmdir: %v", err)
	}
	if err := s.AddRows("acme", []Row{row}, time.Now()); err != nil {
		t.Fatalf("AddRows after the failure: %v", err)
	}
	if got := strings.Count(readFile(t, rowsPath), "\n"); got != 1 {
		t.Errorf("rows file has %d lines, want 1", got)
	}
}

func TestSaveGridOverwrites(t *testing.T) {
	s := newTestSink(t)
	if err := s.SaveGrid("Acme Corp", 2, "<tbody>first</tbody>"); err != nil {
		t.Fatalf("SaveGrid: %v", err)
	}
	if err := s.SaveGrid("Acme Corp", 2, "<tbody>second</tbody>"); err != nil {
		t.Fatalf("SaveGrid (repeat): %v", err)
	}
	path := filepath.Join(s.dataDir, GridFile("Acme Corp", 2))
	if got, want := readFile(t, path), "<tbody>second</tbody>"; got != want {
		t.Errorf("%s = %q, want %q (a repeated attempt must not concatenate)", filepath.Base(path), got, want)
	}
	if got, want := filepath.Base(path), "acme corp_p2.raw"; got != want {
		t.Errorf("grid file = %q, want %q", got, want)
	}
}

func TestSavePDFPath(t *testing.T) {
	s := newTestSink(t)
	if got, want := filepath.Base(s.SavePDFPath("ACME")), "acme.pdf"; got != want {
		t.Errorf("pdf file = %q, want %q", got, want)
	}
}

// TestSinkUnderConcurrentWorkers is the reason every append goes through one
// mutex: three workers finishing at the same time used to interleave into
// half-written lines.
func TestSinkUnderConcurrentWorkers(t *testing.T) {
	s := newTestSink(t)
	at := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < 3; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				q := "query" + string(rune('a'+w))
				if err := s.AddCount(q, i, at); err != nil {
					t.Errorf("AddCount: %v", err)
					return
				}
				if err := s.AddRows(q, []Row{{
					Brand:    "brand " + string(rune('a'+w)),
					RecordID: "ROM." + string(rune('a'+w)) + string(rune('0'+i%10)),
					Number:   i,
				}}, at); err != nil {
					t.Errorf("AddRows: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	if lines := strings.Count(readFile(t, s.resultsPath), "\n"); lines != 3 {
		t.Errorf("count file has %d lines, want 3", lines)
	}
	for i, line := range strings.Split(strings.TrimSuffix(readFile(t, s.rowsPath), "\n"), "\n") {
		if fields := strings.Split(line, "\t"); len(fields) != 13 {
			t.Errorf("row %d has %d fields, want 13: %q", i, len(fields), line)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// A restarted run must not append the pages the previous run already wrote.
// Without loading rows.tsv the only dedupe state is the one in memory, so an
// interruption at page 7,000 of the database crawl would double every row up to
// there on the next start.
func TestLoadRestoresRowDedupe(t *testing.T) {
	s1 := newTestSink(t)
	rows := []Row{
		{Brand: "ACE HOLDINGS", Holder: "ACME INC", RecordID: "R1001", Number: 1},
		{Brand: "BOLT", Holder: "BOLT CO", RegNumber: "123456", RegDate: "2020-01-01", Number: 2},
		// No record id: its key falls back to the concatenated columns, which
		// the reload has to reproduce from the written line alone.
		{Brand: "NO-ID ROW", Holder: "X", RegNumber: "777", RegDate: "2021-02-03", Number: 3},
	}
	if err := s1.AddRows("the-royal", rows, time.Now()); err != nil {
		t.Fatalf("AddRows: %v", err)
	}
	before := readFile(t, s1.rowsPath)

	// A fresh sink over the same files, as after a restart.
	s2 := NewSink(s1.resultsPath, s1.rowsPath, s1.dataDir, true, true)
	if err := s2.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := s2.AddRows("the-royal", rows, time.Now()); err != nil {
		t.Fatalf("AddRows after reload: %v", err)
	}
	if after := readFile(t, s2.rowsPath); after != before {
		lines := func(s string) int { return len(strings.Split(strings.TrimSpace(s), "\n")) }
		t.Errorf("a resumed run re-appended rows: %d lines, want %d", lines(after), lines(before))
	}

	// A row the first run never saw must still land — the reloaded dedupe must
	// not swallow everything.
	fresh := Row{Brand: "THE NEW ONE", RecordID: "R1002", Number: 9}
	if err := s2.AddRows("the-royal", []Row{fresh}, time.Now()); err != nil {
		t.Fatalf("AddRows fresh: %v", err)
	}
	after := readFile(t, s2.rowsPath)
	if !strings.Contains(after, "THE NEW ONE") {
		t.Error("a genuinely new row was dropped as a duplicate")
	}
}

// The checkpoint is the difference between resuming a half-collected query and
// re-walking it from page 1. It must be per query and shard, survive a
// "restart" (a fresh sink over the same directory), and be removable only when
// the query as a whole is done.
func TestCheckpointSurvivesARestart(t *testing.T) {
	s1 := newTestSink(t)

	if got := s1.Checkpoint("*", 0, 0); got != 0 {
		t.Errorf("Checkpoint on a fresh sink = %d, want 0", got)
	}
	if err := s1.SetCheckpoint("*", 0, 0, 4821); err != nil {
		t.Fatalf("SetCheckpoint: %v", err)
	}

	// Shards keep their own resume points in the same directory: block 2 at
	// 7000 says nothing about where block 0 stopped.
	if err := s1.SetCheckpoint("*", 2, 4, 9000); err != nil {
		t.Fatalf("SetCheckpoint shard: %v", err)
	}
	if got := s1.Checkpoint("*", 2, 4); got != 9000 {
		t.Errorf("Checkpoint(shard 2) = %d, want 9000", got)
	}

	// Same directory, new sink — as after the process died and came back.
	s2 := NewSink(s1.resultsPath, s1.rowsPath, s1.dataDir, true, true)
	if got := s2.Checkpoint("*", 0, 0); got != 4821 {
		t.Errorf("Checkpoint after restart = %d, want 4821", got)
	}
	if got := s2.Checkpoint("*", 2, 4); got != 9000 {
		t.Errorf("Checkpoint(shard 2) after restart = %d, want 9000", got)
	}
	if got := s2.Checkpoint("*", 1, 4); got != 0 {
		t.Errorf("Checkpoint(shard 1, never written) = %d, want 0", got)
	}

	// A torn checkpoint reads as "no checkpoint" rather than as page -1: the
	// cost of a bad file is a re-walk that dedupe absorbs, never skipped rows.
	writeFile(t, s2.ckptPath("broken", 0, 0), "not a page")
	if got := s2.Checkpoint("broken", 0, 0); got != 0 {
		t.Errorf("Checkpoint of a corrupt file = %d, want 0", got)
	}

	// Finishing the query removes every shard's resume point; a leftover would
	// send the next run past pages it is supposed to collect.
	s2.DropCheckpoint("*", 4)
	for shard := 0; shard < 4; shard++ {
		if got := s2.Checkpoint("*", shard, 4); got != 0 {
			t.Errorf("Checkpoint(shard %d) after drop = %d, want 0", shard, got)
		}
	}
	s2.DropCheckpoint("broken", 0)
	if _, err := os.Stat(s2.ckptPath("broken", 0, 0)); !os.IsNotExist(err) {
		t.Errorf("single checkpoint still on disk after drop (err = %v)", err)
	}
}
