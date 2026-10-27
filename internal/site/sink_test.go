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
		0, // no rotation unless a test asks for it
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
	if err := s.Load(nil); err != nil {
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
	s := NewSink(filepath.Join(dir, "results.txt"), rowsPath, dir, 0, false, false)
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

// idxSize is the on-disk size of an index holding hashes for one query: the
// magic header plus one "[len][query][n][hashes]" block.
func idxSize(query string, hashes int) int64 {
	return int64(len(idxMagic) + 4 + len(query) + 4 + 8*hashes)
}

// The dedupe index is what makes a restart fast: one block per batch of rows
// instead of re-scanning gigabytes of TSV. It must be written as rows land,
// load on the next start, and produce the same dedupe as the full scan would.
func TestRowIndexWrittenAndLoaded(t *testing.T) {
	s1 := newTestSink(t)
	rows := []Row{
		{Brand: "ACE", RecordID: "R1", Number: 1},
		{Brand: "BOLT", RecordID: "R2", Number: 2},
	}
	if err := s1.AddRows("q", rows, time.Now()); err != nil {
		t.Fatalf("AddRows: %v", err)
	}
	raw, err := os.ReadFile(s1.idxPath())
	if err != nil {
		t.Fatalf("reading index: %v", err)
	}
	if got, want := int64(len(raw)), idxSize("q", 2); got != want {
		t.Errorf("index holds %d bytes, want %d (magic + one block for 2 hashes)", got, want)
	}
	if !strings.HasPrefix(string(raw), idxMagic) {
		t.Errorf("index does not start with the %q header", idxMagic)
	}

	// Restart: Load takes the index fast path and still refuses the old rows.
	s2 := NewSink(s1.resultsPath, s1.rowsPath, s1.dataDir, 0, false, false)
	if err := s2.Load(nil); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := s2.AddRows("q", rows, time.Now()); err != nil {
		t.Fatalf("AddRows after index load: %v", err)
	}
	if n := strings.Count(readFile(t, s2.rowsPath), "\n"); n != 2 {
		t.Errorf("rows file has %d lines, want 2 (index dedupe failed)", n)
	}
}

// The index is only worth carrying for queries that are still open. Loading
// with a done filter must drop the finished query's blocks from the file (a
// compaction, not a separate pass) and leave the open one's dedupe intact —
// that is what keeps the index, and the heap behind it, from growing with
// everything ever collected.
func TestIndexCompactsFinishedQueriesAway(t *testing.T) {
	s1 := newTestSink(t)
	closed := []Row{{Brand: "OLD", RecordID: "R1", Number: 1}}
	open := []Row{{Brand: "NEW", RecordID: "R2", Number: 1}}
	if err := s1.AddRows("finished", closed, time.Now()); err != nil {
		t.Fatalf("AddRows: %v", err)
	}
	if err := s1.AddRows("open", open, time.Now()); err != nil {
		t.Fatalf("AddRows: %v", err)
	}

	s2 := NewSink(s1.resultsPath, s1.rowsPath, s1.dataDir, 0, false, false)
	if err := s2.Load(func(q string) bool { return q == "finished" }); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if fi, err := os.Stat(s2.idxPath()); err != nil || fi.Size() != idxSize("open", 1) {
		t.Errorf("index size = %d (err=%v), want %d — the finished query's block must be gone",
			sizeOf(fi), err, idxSize("open", 1))
	}
	// The open query still dedupes...
	if err := s2.AddRows("open", open, time.Now()); err != nil {
		t.Fatalf("AddRows: %v", err)
	}
	if n := strings.Count(readFile(t, s2.rowsPath), "\n"); n != 2 {
		t.Errorf("rows file has %d lines, want 2 (the open query lost its dedupe)", n)
	}
	// ...and a finished query is collected again on purpose: its hashes are
	// gone, because nothing will ever ask for them while it is in done.lst.
	if err := s2.AddRows("finished", closed, time.Now()); err != nil {
		t.Fatalf("AddRows: %v", err)
	}
	if n := strings.Count(readFile(t, s2.rowsPath), "\n"); n != 3 {
		t.Errorf("rows file has %d lines, want 3 (a finished query is re-collected)", n)
	}
}

// Forgetting a query releases its hashes: after the query is recorded as done
// nothing can write it again, so keeping them is what made the index grow to a
// gigabyte.
func TestForgetDropsTheQueryHashes(t *testing.T) {
	s := newTestSink(t)
	if err := s.AddRows("q", []Row{{Brand: "ACE", RecordID: "R1", Number: 1}}, time.Now()); err != nil {
		t.Fatalf("AddRows: %v", err)
	}
	if len(s.seen["q"]) != 1 {
		t.Fatalf("the query holds %d hash(es), want 1", len(s.seen["q"]))
	}
	s.Forget("Q")
	if _, ok := s.seen["q"]; ok {
		t.Error("Forget did not release the query")
	}
	s.Forget("never-seen") // must not panic on an unknown query
}

func sizeOf(fi os.FileInfo) int64 {
	if fi == nil {
		return -1
	}
	return fi.Size()
}

// An index that is not a whole number of entries cannot be trusted (a crash
// mid-append), and neither can one that was removed by hand: both must fall
// back to the full scan and come back with the same dedupe.
func TestLoadFallsBackWhenIndexIsUnusable(t *testing.T) {
	s1 := newTestSink(t)
	rows := []Row{{Brand: "ACE", RecordID: "R1", Number: 1}}
	if err := s1.AddRows("q", rows, time.Now()); err != nil {
		t.Fatalf("AddRows: %v", err)
	}

	for name, sabotage := range map[string]func(*Sink){
		"odd-sized index": func(s *Sink) {
			writeFile(t, s.idxPath(), "x") // 1 byte: not the format header either
		},
		"old flat index": func(s *Sink) {
			// The pre-block layout: bare hashes, no header, no query to filter by.
			writeBytes(t, s.idxPath(), make([]byte, 16))
		},
		"missing index": func(s *Sink) {
			if err := os.Remove(s.idxPath()); err != nil {
				t.Fatalf("remove index: %v", err)
			}
		},
	} {
		s1.seen = map[string]map[uint64]struct{}{} // forget the in-memory state, as on restart
		s1.reindex = false
		sabotage(s1)
		s2 := NewSink(s1.resultsPath, s1.rowsPath, s1.dataDir, 0, false, false)
		if err := s2.Load(nil); err != nil {
			t.Fatalf("%s: Load: %v", name, err)
		}
		if err := s2.AddRows("q", rows, time.Now()); err != nil {
			t.Fatalf("%s: AddRows after fallback: %v", name, err)
		}
		if n := strings.Count(readFile(t, s2.rowsPath), "\n"); n != 1 {
			t.Errorf("%s: rows file has %d lines, want 1 (fallback dedupe failed)", name, n)
		}
		// The fallback must have healed the index on the way out, in the
		// current format.
		if fi, err := os.Stat(s2.idxPath()); err != nil || fi.Size() != idxSize("q", 1) {
			t.Errorf("%s: index not rebuilt (size=%d, err=%v), want %d", name, sizeOf(fi), err, idxSize("q", 1))
		}
	}
}

// -reindex ignores a present index and rebuilds it from the rows files —
// the verification/repair option after the rows output was edited by hand.
func TestReindexRebuildsFromRowsFiles(t *testing.T) {
	s1 := newTestSink(t)
	rows := []Row{{Brand: "ACE", RecordID: "R1", Number: 1}}
	if err := s1.AddRows("q", rows, time.Now()); err != nil {
		t.Fatalf("AddRows: %v", err)
	}
	// Poison the index with a hash for a row that was never written. Without
	// reindex the restart trusts it and the row is lost; with reindex the
	// rows files are the source of truth and the index is rewritten.
	poison := append([]byte{0, 0, 0, 0, 0, 0, 0, 0}, readFileAsBytes(t, s1.idxPath())...)
	writeBytes(t, s1.idxPath(), poison)

	s2 := NewSink(s1.resultsPath, s1.rowsPath, s1.dataDir, 0, false, false)
	s2.SetReindex(true)
	if err := s2.Load(nil); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := s2.AddRows("q", rows, time.Now()); err != nil {
		t.Fatalf("AddRows after reindex: %v", err)
	}
	if n := strings.Count(readFile(t, s2.rowsPath), "\n"); n != 1 {
		t.Errorf("rows file has %d lines, want 1", n)
	}
	if fi, err := os.Stat(s2.idxPath()); err != nil || fi.Size() != idxSize("q", 1) {
		t.Errorf("reindex did not rewrite the index (size=%d, err=%v), want %d", sizeOf(fi), err, idxSize("q", 1))
	}
}

// A rebuild must skip the rows of finished queries: those hashes are never
// read again, and carrying them is what made the old index a gigabyte.
func TestReindexSkipsFinishedQueries(t *testing.T) {
	dir := t.TempDir()
	rowsPath := filepath.Join(dir, "trademarks.tsv")
	s1 := NewSink(filepath.Join(dir, "results.txt"), rowsPath, dir, 0, false, false)
	if err := s1.AddRows("done already", []Row{{Brand: "A", RecordID: "R1", Number: 1}}, time.Now()); err != nil {
		t.Fatalf("AddRows: %v", err)
	}
	if err := s1.AddRows("still open", []Row{{Brand: "B", RecordID: "R2", Number: 1}}, time.Now()); err != nil {
		t.Fatalf("AddRows: %v", err)
	}

	s2 := NewSink(s1.resultsPath, s1.rowsPath, s1.dataDir, 0, false, false)
	s2.SetReindex(true)
	if err := s2.Load(func(q string) bool { return q == "done already" }); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := s2.seen["done already"]; ok {
		t.Error("a finished query's rows were kept by the rebuild")
	}
	if len(s2.seen["still open"]) != 1 {
		t.Errorf("the open query holds %d hash(es), want 1", len(s2.seen["still open"]))
	}
	if fi, err := os.Stat(s2.idxPath()); err != nil || fi.Size() != idxSize("still open", 1) {
		t.Errorf("index size = %d (err=%v), want %d", sizeOf(fi), err, idxSize("still open", 1))
	}
}

// The hash must depend on the field boundary: a record id "12" of query "ab"
// is a different row from record id "21" of query "a".
func TestRowHashSeparatesFields(t *testing.T) {
	if rowHash("ab", "12") == rowHash("a", "b12") {
		t.Error("rowHash collides across the query/rowKey boundary")
	}
	if rowHash("q", "x") != rowHash("q", "x") {
		t.Error("rowHash is not deterministic")
	}
	if rowHash("a", "x") == rowHash("b", "x") {
		t.Error("rowHash ignores the query — different queries would dedupe each other")
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

func readFileAsBytes(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return raw
}

func writeBytes(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
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
	s2 := NewSink(s1.resultsPath, s1.rowsPath, s1.dataDir, 0, true, true)
	if err := s2.Load(nil); err != nil {
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

// A rows file that grows past the cap is renamed to "<path>.1" and a fresh one
// is started, so the active file never stays huge.
func TestRowsRotateOnSize(t *testing.T) {
	dir := t.TempDir()
	rowsPath := filepath.Join(dir, "trademarks.tsv")
	s := NewSink(filepath.Join(dir, "results.txt"), rowsPath, dir, 1, false, false)
	at := time.Date(2026, 3, 4, 10, 30, 15, 0, time.UTC)

	if err := s.AddRows("acme", []Row{{Brand: "ACME", RecordID: "ROM.1", Number: 0}}, at); err != nil {
		t.Fatalf("AddRows: %v", err)
	}
	if _, err := os.Stat(rowsPath + ".1"); err != nil {
		t.Fatalf("first rotation did not happen: %v", err)
	}
	if _, err := os.Stat(rowsPath); !os.IsNotExist(err) {
		t.Errorf("active rows file survived a rotation (err = %v)", err)
	}

	if err := s.AddRows("acme", []Row{{Brand: "BOLT", RecordID: "ROM.2", Number: 1}}, at); err != nil {
		t.Fatalf("AddRows (second): %v", err)
	}
	if _, err := os.Stat(rowsPath + ".2"); err != nil {
		t.Fatalf("second rotation did not happen: %v", err)
	}
}

// Every rotated part has to be read back on startup. Missing one would let a
// resumed run re-append rows that older parts already hold.
func TestLoadReadsRotatedParts(t *testing.T) {
	dir := t.TempDir()
	rowsPath := filepath.Join(dir, "trademarks.tsv")
	rows := []Row{
		{Brand: "ACE", RecordID: "R1", Number: 1},
		{Brand: "BOLT", RecordID: "R2", Number: 2},
	}
	at := time.Now()

	// A cap of one byte per row splits the two records into parts .1 and .2.
	s1 := NewSink(filepath.Join(dir, "results.txt"), rowsPath, dir, 1, false, false)
	for _, r := range rows {
		if err := s1.AddRows("q", []Row{r}, at); err != nil {
			t.Fatalf("AddRows: %v", err)
		}
	}
	for _, part := range []string{rowsPath + ".1", rowsPath + ".2"} {
		if _, err := os.Stat(part); err != nil {
			t.Fatalf("expected rotated part %s: %v", part, err)
		}
	}

	// Restart with rotation effectively off, so the new write is observable in
	// the active file while the parts stay put.
	s2 := NewSink(s1.resultsPath, s1.rowsPath, s1.dataDir, 1<<30, false, false)
	if err := s2.Load(nil); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := s2.AddRows("q", append(rows, Row{Brand: "NEW", RecordID: "R3", Number: 3}), at); err != nil {
		t.Fatalf("AddRows after reload: %v", err)
	}
	got := readFile(t, s2.rowsPath)
	if n := strings.Count(got, "\n"); n != 1 || !strings.Contains(got, "R3") {
		t.Errorf("resumed write = %d lines, want only the one new row:\n%s", n, got)
	}
}

// The checkpoint is the difference between resuming a half-collected query and
// re-walking it from page 1. It must be per query and shard, survive a
// "restart" (a fresh sink over the same directory), and be removable only when
// the query as a whole is done.
func TestCheckpointSurvivesARestart(t *testing.T) {
	s1 := newTestSink(t)

	if page, rows := s1.Checkpoint("*", 0, 0); page != 0 || rows != -1 {
		t.Errorf("Checkpoint on a fresh sink = (%d, %d), want (0, -1)", page, rows)
	}
	if err := s1.SetCheckpoint("*", 0, 0, 4821, 482100); err != nil {
		t.Fatalf("SetCheckpoint: %v", err)
	}
	if page, rows := s1.Checkpoint("*", 0, 0); page != 4821 || rows != 482100 {
		t.Errorf("Checkpoint = (%d, %d), want (4821, 482100) — the row total is what the completeness check holds a resumed walk against",
			page, rows)
	}

	// Shards keep their own resume points in the same directory: block 2 at
	// 7000 says nothing about where block 0 stopped.
	if err := s1.SetCheckpoint("*", 2, 4, 9000, 900000); err != nil {
		t.Fatalf("SetCheckpoint shard: %v", err)
	}
	if page, _ := s1.Checkpoint("*", 2, 4); page != 9000 {
		t.Errorf("Checkpoint(shard 2) = %d, want 9000", page)
	}

	// Same directory, new sink — as after the process died and came back.
	s2 := NewSink(s1.resultsPath, s1.rowsPath, s1.dataDir, 0, true, true)
	if page, rows := s2.Checkpoint("*", 0, 0); page != 4821 || rows != 482100 {
		t.Errorf("Checkpoint after restart = (%d, %d), want (4821, 482100)", page, rows)
	}
	if page, _ := s2.Checkpoint("*", 2, 4); page != 9000 {
		t.Errorf("Checkpoint(shard 2) after restart = %d, want 9000", page)
	}
	if page, _ := s2.Checkpoint("*", 1, 4); page != 0 {
		t.Errorf("Checkpoint(shard 1, never written) = %d, want 0", page)
	}

	// A torn checkpoint reads as "no checkpoint" rather than as page -1: the
	// cost of a bad file is a re-walk that dedupe absorbs, never skipped rows.
	writeFile(t, s2.ckptPath("broken", 0, 0), "not a page")
	if page, _ := s2.Checkpoint("broken", 0, 0); page != 0 {
		t.Errorf("Checkpoint of a corrupt file = %d, want 0", page)
	}

	// A checkpoint written by an older build carries only the page. It still
	// resumes — with an unknown row total, which simply skips the completeness
	// check rather than guessing a number.
	writeFile(t, s2.ckptPath("legacy", 0, 0), "512\n")
	if page, rows := s2.Checkpoint("legacy", 0, 0); page != 512 || rows != -1 {
		t.Errorf("Checkpoint of a page-only file = (%d, %d), want (512, -1)", page, rows)
	}

	// Finishing the query removes every shard's resume point; a leftover would
	// send the next run past pages it is supposed to collect.
	s2.DropCheckpoint("*", 4)
	for shard := 0; shard < 4; shard++ {
		if page, _ := s2.Checkpoint("*", shard, 4); page != 0 {
			t.Errorf("Checkpoint(shard %d) after drop = %d, want 0", shard, page)
		}
	}
	s2.DropCheckpoint("broken", 0)
	if _, err := os.Stat(s2.ckptPath("broken", 0, 0)); !os.IsNotExist(err) {
		t.Errorf("single checkpoint still on disk after drop (err = %v)", err)
	}
}
