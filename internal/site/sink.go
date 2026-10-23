package site

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// maxRowLine caps one line the row loader will read. It is far above any real
// row (a record is well under a kilobyte) but high enough to swallow a torn
// append and skip it, which is the point: a torn line must not abort startup.
const maxRowLine = 64 << 20

// Sink owns every file the run writes: the count file, the row file and the
// saved result pages.
//
// All appends go through one mutex. Several workers write at the same time and
// the old collector appended with a bare os.OpenFile per line, which loses
// whole lines when two workers interleave.
//
// The row dedupe is a set of 64-bit hashes (query + row key), not the row keys
// themselves: ten million rows as strings cost multiple gigabytes of heap and
// made restarts on a memory-tight box slow and fragile. The same set is kept
// on disk beside the rows file as "<rows_file>.idx" — one 8-byte hash per
// collected row — so a restart loads it in seconds instead of re-scanning
// every rotated part of a multi-gigabyte TSV.
type Sink struct {
	mu          sync.Mutex
	resultsPath string
	rowsPath    string
	dataDir     string
	rowsMax     int64               // rotate the rows file past this many bytes; 0 = never
	nextPart    int                 // index of the next rotated part (rowsPath.1, .2, ...)
	rowsBytes   int64               // size of the active rows file, for the rotation check
	counted     map[string]struct{} // queries already in the count file
	seen        map[uint64]struct{} // FNV-1a hashes of "query \x00 rowKey" already written
	reindex     bool                // ignore a present index file, rebuild it from the rows files
	saveGrid    bool
	saveFull    bool
}

// NewSink builds a sink. rowsMax is the size at which the rows file is rotated
// (0 = never). Call Load once before the workers start.
func NewSink(resultsPath, rowsPath, dataDir string, rowsMax int64, saveGrid, saveFull bool) *Sink {
	return &Sink{
		resultsPath: resultsPath,
		rowsPath:    rowsPath,
		dataDir:     dataDir,
		rowsMax:     rowsMax,
		nextPart:    1,
		counted:     map[string]struct{}{},
		seen:        map[uint64]struct{}{},
		saveGrid:    saveGrid,
		saveFull:    saveFull,
	}
}

// SetReindex makes the next Load ignore a present index file and rebuild it
// from the rows files. It is the repair and verification option: use it after
// editing or deleting parts of the rows output by hand, since the index is a
// cache and cannot notice that the rows files behind it changed.
func (s *Sink) SetReindex(v bool) { s.reindex = v }

// idxPath is the index file kept beside the rows file.
func (s *Sink) idxPath() string { return s.rowsPath + ".idx" }

// rowHash is the dedupe key of one row of one query, as FNV-1a over the
// query, a NUL separator and the row key. The separator keeps different
// field boundaries apart ("ab"+"c" must not collide with "a"+"bc"). A 64-bit
// FNV-1a is fast and has plenty of headroom here: a birthday collision over
// ten million rows is a one-in-three-million event, and even one would only
// skip a single already-rare row.
func rowHash(query, rowKey string) uint64 {
	const (
		offset = 14695981039346656037
		prime  = 1099511628211
	)
	h := uint64(offset)
	for i := 0; i < len(query); i++ {
		h ^= uint64(query[i])
		h *= prime
	}
	h ^= 0
	h *= prime // the NUL separator, as a hashed byte
	for i := 0; i < len(rowKey); i++ {
		h ^= uint64(rowKey[i])
		h *= prime
	}
	return h
}

// Load primes the sink from what an earlier run already wrote: the count file
// fills counted, the row index file fills the row dedupe set. Both matter for
// a resumed run, and the dedupe set matters most — without it a restart
// re-walks every page of a half-collected query and appends every one of those
// rows a second time. For a crawl the size of the whole database, that turns
// an interruption at page 7,000 into a doubled file.
func (s *Sink) Load() error {
	if err := s.loadCounts(); err != nil {
		return err
	}
	return s.loadRows()
}

func (s *Sink) loadCounts() error {
	raw, err := os.ReadFile(s.resultsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) < 3 {
			continue
		}
		if q := strings.ToLower(strings.TrimSpace(fields[1])); q != "" {
			s.counted[q] = struct{}{}
		}
	}
	return nil
}

// loadRows rebuilds the row dedupe set for a resumed run and seeds the
// rotation state (nextPart, rowsBytes) so an append can keep rotating where
// the last run left off.
//
// The set itself lives in "<rows_file>.idx" — one 8-byte hash per collected
// row — and that file is what gets loaded when it is there: ten million rows
// are ~80 MB of hashes and load in seconds, while re-scanning every rotated
// part of a multi-gigabyte TSV took minutes of startup silence. The TSV scan
// is the fallback (first run, missing or odd-sized index) and rebuilds the
// index file, so every run after the first takes the fast path. -reindex
// forces the slow path as a verification/repair option.
//
// During a scan the pass reports itself every rowsProgressTick — bytes done,
// percent of the total, current file, ETA — into "<rows_file>.idx.progress-log",
// a separate append-only file: the ticks are noise the run log would have to
// carry forever. The main log gets one line about the sizes and one about the
// result.
//
// A line that does not hold a full row is skipped rather than fatal: a file
// can end in a half-written line if the previous run died mid-append, and
// refusing to start over a torn last line would be worse than re-collecting
// one row. The key is recomputed from the written columns rather than stored
// beside them, so the file format stays exactly what it was.
func (s *Sink) loadRows() error {
	start := time.Now()
	parts, maxPart := s.rowParts()
	s.nextPart = maxPart + 1
	if fi, err := os.Stat(s.rowsPath); err == nil {
		s.rowsBytes = fi.Size()
	}

	if !s.reindex {
		if n, ok := s.loadIndex(); ok {
			log.Printf("rows: index loaded, %d unique row(s) in %s (run with -reindex to rebuild it from the rows files)",
				n, time.Since(start).Round(time.Millisecond))
			return nil
		}
	} else {
		log.Printf("rows: reindex requested — rebuilding the dedupe index from the rows files")
	}
	return s.rebuildIndex(parts, start)
}

// loadIndex reads "<rows_file>.idx" into the dedupe set. It reports false for
// any file that is missing or not a whole number of 8-byte entries: both mean
// the index cannot be trusted as written, so the caller falls back to a full
// scan and a rewrite.
func (s *Sink) loadIndex() (int, bool) {
	raw, err := os.ReadFile(s.idxPath())
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("rows: reading %s: %v — falling back to a full scan", s.idxPath(), err)
		}
		return 0, false
	}
	if len(raw)%8 != 0 {
		log.Printf("rows: %s is not a whole number of entries (%d bytes) — falling back to a full scan", s.idxPath(), len(raw))
		return 0, false
	}
	n := len(raw) / 8
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = make(map[uint64]struct{}, n)
	for off := 0; off < n; off++ {
		s.seen[binary.LittleEndian.Uint64(raw[off*8:])] = struct{}{}
	}
	return n, true
}

// rebuildIndex scans every rotated part and the active rows file, fills the
// dedupe set and rewrites the index file in one go.
func (s *Sink) rebuildIndex(parts []string, start time.Time) error {
	// A day-old output directory can hold gigabytes of TSV, and a run that
	// reads that in silence looks hang — so the scan reports itself: one line
	// in the main log about the sizes, then a tick into the progress file.
	total := int64(0)
	for _, p := range append(parts, s.rowsPath) {
		if fi, err := os.Stat(p); err == nil {
			total += fi.Size()
		}
	}
	var t rowsLoadTracker
	t.total = total
	if total > 0 {
		log.Printf("rows: scanning %d rotated part(s) and the active file, %.1f GB — rebuilding the index, progress in %s",
			len(parts), float64(total)/1e9, s.idxPath()+".progress-log")
	} else {
		log.Printf("rows: scanning %d rotated part(s) and the active file", len(parts))
	}
	// The progress goes to its own file: every tick is a line the run log
	// would have to carry forever, and nobody wants to grep through that
	// later. Failure to open the file only means ticks fall back to the log.
	progressPath := s.idxPath() + ".progress-log"
	if pf, err := os.OpenFile(progressPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
		defer func() { _ = pf.Close() }()
		t.out = log.New(pf, "", log.LstdFlags)
	} else {
		log.Printf("rows: progress file %s: %v — ticks go to the run log", progressPath, err)
	}

	stop := make(chan struct{})
	tickerDone := make(chan struct{})
	go func() {
		defer close(tickerDone)
		tick := time.NewTicker(rowsProgressTick)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				t.log(start)
			}
		}
	}()

	var lines int
	for _, p := range parts {
		name := filepath.Base(p)
		t.file.Store(&name)
		n, err := s.loadRowsFile(p, &t)
		if err != nil {
			close(stop)
			<-tickerDone
			return err
		}
		lines += n
	}
	name := filepath.Base(s.rowsPath)
	t.file.Store(&name)
	n, err := s.loadRowsFile(s.rowsPath, &t)
	if err != nil {
		close(stop)
		<-tickerDone
		return err
	}
	lines += n
	close(stop)
	<-tickerDone

	if err := s.writeIndex(); err != nil {
		// Not fatal: the in-memory set is complete for this run, and the
		// next start simply falls back to another scan.
		log.Printf("rows: writing %s: %v", s.idxPath(), err)
	}

	s.mu.Lock()
	unique := len(s.seen)
	s.mu.Unlock()
	log.Printf("rows: %d line(s), %d unique rows, index rebuilt in %s",
		lines, unique, time.Since(start).Round(time.Millisecond))
	return nil
}

// writeIndex rewrites the index file from the current dedupe set, via a
// temporary file and a rename so a crash cannot leave a half-written index
// that the next start would trust.
func (s *Sink) writeIndex() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tmp := s.idxPath() + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, 1<<20)
	buf := make([]byte, 8)
	for h := range s.seen {
		binary.LittleEndian.PutUint64(buf, h)
		if _, err := w.Write(buf); err != nil {
			_ = f.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.idxPath()); err != nil {
		return err
	}
	return nil
}

// rowsProgressTick is how often the dedupe rebuild reports its progress. The
// scan can run for minutes on a grown output directory; with ticks at this
// cadence the progress file answers "is it stuck?" without drowning in lines.
const rowsProgressTick = 10 * time.Second

// rowsLoadTracker is what the dedupe rebuild's ticker reports: bytes consumed
// against the total it was given, plus the file currently being parsed.
type rowsLoadTracker struct {
	total int64
	done  atomic.Int64
	lines atomic.Int64
	file  atomic.Pointer[string]
	out   *log.Logger // where ticks go; nil = the run log
}

// log writes one progress line. It runs on its own goroutine, so everything
// it reads must be atomic.
func (t *rowsLoadTracker) log(start time.Time) {
	w := log.Printf
	if t.out != nil {
		w = t.out.Printf
	}
	lines := t.lines.Load()
	if t.total <= 0 {
		w("rows: %d line(s) so far", lines)
		return
	}
	done := t.done.Load()
	pct := int(done * 100 / t.total)
	rate := float64(done) / time.Since(start).Seconds() // bytes per second
	msg := fmt.Sprintf("rows: %d%% (%.1f/%.1f GB), %d line(s)", pct,
		float64(done)/1e9, float64(t.total)/1e9, lines)
	if file := t.file.Load(); file != nil {
		msg += fmt.Sprintf(", at %s", *file)
	}
	if rate > 0 && done < t.total {
		rem := time.Duration(float64(t.total-done) / rate * float64(time.Second))
		msg += fmt.Sprintf(" — %s left", rem.Round(time.Second))
	}
	w("%s", msg)
}

// rowParts lists the rotated parts of the rows file, "<rows_file>.<n>", from
// .1 up to the highest index present, and returns that highest index. It scans
// the whole directory rather than stopping at the first gap: a missing middle
// part must not hide a later one from the dedupe pass.
func (s *Sink) rowParts() ([]string, int) {
	entries, err := os.ReadDir(filepath.Dir(s.rowsPath))
	if err != nil {
		return nil, 0
	}
	prefix := filepath.Base(s.rowsPath) + "."
	maxPart := 0
	for _, e := range entries {
		suffix, ok := strings.CutPrefix(e.Name(), prefix)
		if !ok {
			continue
		}
		n, err := strconv.Atoi(suffix)
		if err != nil || n < 1 {
			continue
		}
		if n > maxPart {
			maxPart = n
		}
	}
	parts := make([]string, 0, maxPart)
	for i := 1; i <= maxPart; i++ {
		parts = append(parts, fmt.Sprintf("%s.%d", s.rowsPath, i))
	}
	return parts, maxPart
}

// loadRowsFile reads one row file into the dedupe set and returns how many
// lines it held. A missing file is not an error: a fresh run has no rows file,
// and a rotated part can be absent if the numbering ever skips.
func (s *Sink) loadRowsFile(path string, t *rowsLoadTracker) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	s.mu.Lock()
	defer s.mu.Unlock()
	var lines int
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxRowLine)
	for sc.Scan() {
		// Count bytes as they are read, before any validation: torn or
		// invalid lines still cost read time and the progress would sit at
		// zero while the pass is clearly moving.
		t.done.Add(int64(len(sc.Bytes())) + 1)
		fields := strings.Split(sc.Text(), "\t")
		if len(fields) < 13 {
			continue // stamp + query + a full row, or a torn line
		}
		row, ok := RowFromValues(fields[2:13])
		if !ok {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(fields[1]))
		if key == "" {
			continue
		}
		s.seen[rowHash(key, row.Key())] = struct{}{}
		lines++
		t.lines.Add(1)
	}
	if err := sc.Err(); err != nil {
		return lines, fmt.Errorf("%s: %w", path, err)
	}
	return lines, nil
}

// Counted reports whether the query is already in the count file.
func (s *Sink) Counted(query string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.counted[strings.ToLower(query)]
	return ok
}

// AddCount appends one "<date> \t <query> \t <count>" line. The query is
// marked counted only after the write succeeds, so a failed write is retried
// instead of silently dropped.
func (s *Sink) AddCount(query string, count int, at time.Time) error {
	key := strings.ToLower(strings.TrimSpace(query))

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.counted[key]; ok {
		return nil
	}
	if err := appendLine(s.resultsPath, fmt.Sprintf("%s\t%s\t%d", Day(at), key, count)); err != nil {
		return err
	}
	s.counted[key] = struct{}{}
	return nil
}

// AddRows appends one line per trademark:
//
//	<date time> \t <query> \t <brand> \t <holder> \t ...
//
// Rows whose hash was already written for this query are dropped, which is
// what keeps a requeued query from doubling its records. The hashes are
// recorded only after the write went through: a requeued query whose write
// failed has to be able to write the same rows again.
func (s *Sink) AddRows(query string, rows []Row, at time.Time) error {
	key := strings.ToLower(strings.TrimSpace(query))
	stamp := Stamped(at)

	s.mu.Lock()
	defer s.mu.Unlock()
	fresh := make([]Row, 0, len(rows))
	hashes := make([]uint64, 0, len(rows))
	// A batch can repeat a row itself (the site does repeat records across
	// pages); the local set catches that without touching s.seen early.
	batch := make(map[uint64]struct{}, len(rows))
	var b strings.Builder
	for _, r := range rows {
		h := rowHash(key, r.Key())
		if _, dup := s.seen[h]; dup {
			continue
		}
		if _, dup := batch[h]; dup {
			continue
		}
		batch[h] = struct{}{}
		fresh = append(fresh, r)
		hashes = append(hashes, h)
	}
	if len(fresh) == 0 {
		return nil
	}
	for _, r := range fresh {
		b.WriteString(strings.Join(append([]string{stamp, key}, r.Values()...), "\t"))
		b.WriteByte('\n')
	}
	line := strings.TrimSuffix(b.String(), "\n")
	if err := appendLine(s.rowsPath, line); err != nil {
		return err
	}
	s.rowsBytes += int64(len(line)) + 1 // the trailing newline appendLine adds
	// The index follows the rows. An index append that fails is logged but
	// not fatal: the rows are already on disk, and the in-memory set below
	// still covers this run. The cost is one re-collected duplicate after a
	// restart, which is the cheap side of the trade — the rows file is the
	// source of truth, the index is a cache of it.
	if err := appendBytes(s.idxPath(), hashes); err != nil {
		log.Printf("rows: index append: %v — the next start will re-scan", err)
	}
	for _, h := range hashes {
		s.seen[h] = struct{}{}
	}
	s.rotateRows()
	return nil
}

// rotateRows archives the active rows file once it passes the size cap and
// starts a fresh one. It runs under the append mutex, so the rename and the
// next append cannot race.
//
// Rotation is best-effort: a rename that fails only lets the active file grow
// past the cap, and turning that into an AddRows error would requeue a query
// whose rows are already on disk.
func (s *Sink) rotateRows() {
	if s.rowsMax <= 0 || s.rowsBytes < s.rowsMax {
		return
	}
	part := fmt.Sprintf("%s.%d", s.rowsPath, s.nextPart)
	if err := os.Rename(s.rowsPath, part); err != nil {
		log.Printf("rotate rows: %v", err)
		return
	}
	log.Printf("rows file reached %d bytes: %s -> %s", s.rowsBytes, s.rowsPath, part)
	s.nextPart++
	s.rowsBytes = 0
}

// SaveGrid writes the raw grid HTML of one result page. Overwritten, not
// appended: a repeated attempt for the same page must not concatenate.
func (s *Sink) SaveGrid(query string, page int, gridHTML string) error {
	if !s.saveGrid {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return os.WriteFile(filepath.Join(s.dataDir, GridFile(query, page)), []byte(gridHTML), 0644)
}

// SaveFull writes the whole results page HTML of a query (first page only).
func (s *Sink) SaveFull(query string, pageHTML string) error {
	if !s.saveFull {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return os.WriteFile(filepath.Join(s.dataDir, FullFile(query)), []byte(pageHTML), 0644)
}

// SavePDFPath returns where the printed page belongs, so the caller can hand
// the path to Playwright.
func (s *Sink) SavePDFPath(query string) string {
	return filepath.Join(s.dataDir, PDFFile(query))
}

// Checkpoint returns the last grid page of this query (and shard) that reached
// rows.tsv, or 0 when there is none.
//
// It is the difference between resuming and restarting. Rows alone say what a
// run collected but not where it stopped, and the only way to derive a page
// number from them is to count — which is wrong across shards (they write into
// one file) and wrong if the page size ever changes. The number is written
// after the rows, never before, so a checkpoint is always behind the data: a
// crash between the two re-walks a page whose rows are then deduplicated,
// while the opposite order would skip rows that were never written.
func (s *Sink) Checkpoint(query string, shard, shardCount int) int {
	raw, err := os.ReadFile(s.ckptPath(query, shard, shardCount))
	if err != nil {
		return 0
	}
	page, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || page < 0 {
		return 0
	}
	return page
}

// SetCheckpoint records that every row up to grid page was written. Failure is
// the caller's to report but not to act on: a lost checkpoint costs a re-walk
// that dedupe absorbs, a lost row does not.
func (s *Sink) SetCheckpoint(query string, shard, shardCount, page int) error {
	path := s.ckptPath(query, shard, shardCount)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.Itoa(page)), 0644)
}

// DropCheckpoint removes the resume points of a finished query, for this shard
// or for all of them: a leftover checkpoint would send the next run past pages
// that a later, smaller crawl is supposed to collect.
func (s *Sink) DropCheckpoint(query string, shardCount int) {
	if shardCount > 1 {
		for shard := 0; shard < shardCount; shard++ {
			_ = os.Remove(s.ckptPath(query, shard, shardCount))
		}
		return
	}
	_ = os.Remove(s.ckptPath(query, 0, 0))
}

// ckptPath is where one query (or one of its shards) keeps its last page.
func (s *Sink) ckptPath(query string, shard, shardCount int) string {
	name := SanitizeQuery(query)
	if shardCount > 1 {
		name = fmt.Sprintf("%s.s%d", name, shard)
	}
	return filepath.Join(s.dataDir, ".ckpt", name)
}

// appendBytes appends little-endian uint64s. Like appendLine it creates the
// file when missing and returns the error: the caller logs it, because a
// failed index append must not fail a batch whose rows are already on disk.
func appendBytes(path string, hs []uint64) error {
	if len(hs) == 0 {
		return nil
	}
	buf := make([]byte, 8*len(hs))
	for i, h := range hs {
		binary.LittleEndian.PutUint64(buf[i*8:], h)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0644)
	if err != nil {
		return fmt.Errorf("append %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(buf); err != nil {
		return fmt.Errorf("append %s: %w", path, err)
	}
	return nil
}

// appendLine appends text plus a newline. The file is created when missing, and
// the error is returned: the caller decides whether to requeue, because a
// silent drop here loses a collected query for good.
func appendLine(path, line string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0644)
	if err != nil {
		return fmt.Errorf("append %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	w := bufio.NewWriter(f)
	if _, err := w.WriteString(line + "\n"); err != nil {
		return fmt.Errorf("append %s: %w", path, err)
	}
	return w.Flush()
}
