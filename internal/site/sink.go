package site

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
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
// made restarts on a memory-tight box slow and fragile.
//
// The set is kept per query, and it is dropped again the moment the query is
// collected (Forget). Its whole job is to stop a requeued or resumed query from
// writing rows it already wrote — a query in done.lst is never collected again,
// so its hashes are dead weight. That is the difference between a heap that
// grows with everything ever collected (measured at 5.5 GB and 1.3 GB of index
// file for a fifth of this input, and still climbing) and one that holds only
// the handful of queries in flight.
//
// On disk the same rule is written as "<rows_file>.idx": a magic header
// followed by one block per batch of rows,
//
//	[uint32 query length][query][uint32 n][n × 8-byte hash]
//
// and a start rewrites the file with the blocks of finished queries removed —
// a compaction that is free, because the file has to be read end to end anyway
// to build the set. A restart therefore loads in milliseconds instead of
// reading a gigabyte of hashes, and the file never grows past the queries that
// are still open. The rows files remain the source of truth: the index is a
// cache of them, and -reindex rebuilds it.
type Sink struct {
	mu          sync.Mutex
	resultsPath string
	rowsPath    string
	dataDir     string
	rowsMax     int64               // rotate the rows file past this many bytes; 0 = never
	nextPart    int                 // index of the next rotated part (rowsPath.1, .2, ...)
	rowsBytes   int64               // size of the active rows file, for the rotation check
	counted     map[string]struct{} // queries already in the count file
	seen        map[string]map[uint64]struct{}
	reindex     bool // ignore a present index file, rebuild it from the rows files
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
		seen:        map[string]map[uint64]struct{}{},
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
// fills counted, the row index file fills the row dedupe set — but only for the
// queries isDone reports as still open, because a finished query's hashes are
// never needed again (see Forget).
//
// isDone is passed in rather than held: done.lst belongs to the run, not to the
// sink, and a sink called with no filter simply keeps everything.
func (s *Sink) Load(isDone func(string) bool) error {
	if err := s.loadCounts(); err != nil {
		return err
	}
	return s.loadRows(orNotDone(isDone))
}

// orNotDone makes a missing filter mean "nothing is finished yet", which is the
// only safe default: a wrongly-dropped hash writes a duplicate row, a wrongly
// kept one only costs memory.
func orNotDone(isDone func(string) bool) func(string) bool {
	if isDone == nil {
		return func(string) bool { return false }
	}
	return isDone
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
// The fast path is "<rows_file>.idx" (format in the Sink doc): it is read end
// to end, the blocks of finished queries are dropped, and what is left is
// written back — so both the file and the heap hold only the queries that are
// still open. The TSV scan is the fallback (first run, the old flat index, an
// unreadable index) and rebuilds the file from the rows files, which are the
// source of truth. -reindex forces the slow path as a verification/repair
// option.
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
func (s *Sink) loadRows(isDone func(string) bool) error {
	start := time.Now()
	parts, maxPart := s.rowParts()
	s.nextPart = maxPart + 1
	if fi, err := os.Stat(s.rowsPath); err == nil {
		s.rowsBytes = fi.Size()
	}

	if !s.reindex {
		if queries, rows, ok := s.loadIndex(isDone); ok {
			log.Printf("rows: index loaded, %d open query(ies), %d unique row(s), in %s (run with -reindex to rebuild it from the rows files)",
				queries, rows, time.Since(start).Round(time.Millisecond))
			return nil
		}
	} else {
		log.Printf("rows: reindex requested — rebuilding the dedupe index from the rows files")
	}
	return s.rebuildIndex(parts, start, isDone)
}

// idxMagic marks the block format of the index. A file that does not start
// with it is absent, empty or the old flat "eight bytes a hash" layout — and
// the old layout cannot say which query a hash belongs to, so it cannot be
// filtered down to the open queries. Either way the caller falls back to a
// scan, which rebuilds the file in the current format.
const idxMagic = "WIPIDXv2"

// Bounds on one index block. A block claiming something outside them is not a
// block: it is a torn tail or a corrupted file, and the pass stops there rather
// than allocating gigabytes from a bad length.
const (
	maxIndexQueryLen = 64 << 10
	maxIndexHashes   = 16 << 20
)

// loadIndex reads "<rows_file>.idx" into the dedupe set, keeping only the
// queries isDone says are still open, and rewrites the file without the rest.
// It reports false — leaving the set alone — when the file is missing, in the
// old format or unreadable, so the caller can fall back to the rows files.
//
// The rewrite is what keeps both the file and the heap proportional to the
// queries in flight instead of to everything ever collected: the file has to
// be read end to end anyway to build the set, so dropping finished blocks on
// the way out costs nothing beyond one rename.
func (s *Sink) loadIndex(isDone func(string) bool) (int, int, bool) {
	path := s.idxPath()
	f, err := os.Open(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("rows: reading %s: %v — falling back to a full scan", path, err)
		}
		return 0, 0, false
	}
	defer func() { _ = f.Close() }()

	head := make([]byte, len(idxMagic))
	if _, err := io.ReadFull(f, head); err != nil || string(head) != idxMagic {
		if err == nil {
			log.Printf("rows: %s is the old flat index — a full scan rebuilds it per query (one start only)", path)
		}
		return 0, 0, false
	}
	queries, rows, err := s.compactIndex(f, isDone)
	if err != nil {
		log.Printf("rows: rewriting %s: %v — falling back to a full scan", path, err)
		return 0, 0, false
	}
	return queries, rows, true
}

// compactIndex reads index block after block, keeps the ones whose query is
// still open — in memory and in the rewritten file — and reports how much it
// kept. A block that does not read whole is a torn tail from a crash
// mid-append: everything before it was written and is kept, and the rewrite
// removes the half block so the next start does not meet it again. The rows
// behind a lost block are still in the TSV, so the cost is one page written
// twice after a restart, not a row lost.
func (s *Sink) compactIndex(r io.Reader, isDone func(string) bool) (int, int, error) {
	tmp, err := os.Create(s.idxPath() + ".tmp")
	if err != nil {
		return 0, 0, err
	}
	w := bufio.NewWriterSize(tmp, 1<<20)
	if _, err := w.WriteString(idxMagic); err != nil {
		_ = tmp.Close()
		return 0, 0, err
	}
	br := bufio.NewReaderSize(r, 1<<20)

	s.mu.Lock()
	defer s.mu.Unlock()
	queries, rows := 0, 0
	for {
		query, hs, err := readIndexBlock(br)
		if err != nil {
			break // EOF or a torn tail: stop at the last whole block
		}
		if isDone(query) {
			continue
		}
		if err := writeIndexBlock(w, query, hs); err != nil {
			_ = tmp.Close()
			return 0, 0, err
		}
		set := s.seen[query]
		if set == nil {
			set = make(map[uint64]struct{}, len(hs))
			s.seen[query] = set
			queries++
		}
		for _, h := range hs {
			if _, dup := set[h]; !dup {
				set[h] = struct{}{}
				rows++
			}
		}
	}
	if err := w.Flush(); err != nil {
		_ = tmp.Close()
		return 0, 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, 0, err
	}
	if err := os.Rename(s.idxPath()+".tmp", s.idxPath()); err != nil {
		return 0, 0, err
	}
	return queries, rows, nil
}

// readIndexBlock reads one "[query length][query][n][hashes]" block.
func readIndexBlock(r io.Reader) (string, []uint64, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return "", nil, err
	}
	qlen := binary.LittleEndian.Uint32(hdr[:])
	if qlen == 0 || qlen > maxIndexQueryLen {
		return "", nil, fmt.Errorf("index block: query length %d", qlen)
	}
	qb := make([]byte, qlen)
	if _, err := io.ReadFull(r, qb); err != nil {
		return "", nil, err
	}
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return "", nil, err
	}
	n := binary.LittleEndian.Uint32(hdr[:])
	if uint64(n) > maxIndexHashes {
		return "", nil, fmt.Errorf("index block: %d hashes", n)
	}
	raw := make([]byte, 8*int(n))
	if _, err := io.ReadFull(r, raw); err != nil {
		return "", nil, err
	}
	hs := make([]uint64, int(n))
	for i := range hs {
		hs[i] = binary.LittleEndian.Uint64(raw[i*8:])
	}
	return string(qb), hs, nil
}

// writeIndexBlock appends one block. The two lengths are what make the format
// self-delimiting, which is what lets a start tell a whole file from a torn
// tail without any bookkeeping beside it.
func writeIndexBlock(w io.Writer, query string, hs []uint64) error {
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(query)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := io.WriteString(w, query); err != nil {
		return err
	}
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(hs)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(hs) == 0 {
		return nil
	}
	buf := make([]byte, 8*len(hs))
	for i, h := range hs {
		binary.LittleEndian.PutUint64(buf[i*8:], h)
	}
	_, err := w.Write(buf)
	return err
}

// appendIndex appends one block of hashes for one query, creating the file
// (and its header) when missing. It returns the error: the caller logs it,
// because a failed index append must not fail a batch whose rows are already on
// disk — the cost is a duplicate page after the next restart, not a lost row.
func (s *Sink) appendIndex(query string, hs []uint64) error {
	if len(hs) == 0 {
		return nil
	}
	f, err := os.OpenFile(s.idxPath(), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0644)
	if err != nil {
		return fmt.Errorf("append %s: %w", s.idxPath(), err)
	}
	defer func() { _ = f.Close() }()
	w := bufio.NewWriterSize(f, 64<<10)
	if fi, err := f.Stat(); err == nil && fi.Size() == 0 {
		if _, err := w.WriteString(idxMagic); err != nil {
			return fmt.Errorf("append %s: %w", s.idxPath(), err)
		}
	}
	if err := writeIndexBlock(w, query, hs); err != nil {
		return fmt.Errorf("append %s: %w", s.idxPath(), err)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("append %s: %w", s.idxPath(), err)
	}
	return nil
}

// rebuildIndex scans every rotated part and the active rows file, fills the
// dedupe set and rewrites the index file in one go.
func (s *Sink) rebuildIndex(parts []string, start time.Time, isDone func(string) bool) error {
	// Whatever a partial fast path may have left behind, the scan is the whole
	// truth: start the set from scratch so the file this writes and the heap
	// this fills describe exactly the rows files.
	s.mu.Lock()
	s.seen = map[string]map[uint64]struct{}{}
	s.mu.Unlock()

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
		n, err := s.loadRowsFile(p, &t, isDone)
		if err != nil {
			close(stop)
			<-tickerDone
			return err
		}
		lines += n
	}
	name := filepath.Base(s.rowsPath)
	t.file.Store(&name)
	n, err := s.loadRowsFile(s.rowsPath, &t, isDone)
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
	queries, unique := len(s.seen), 0
	for _, set := range s.seen {
		unique += len(set)
	}
	s.mu.Unlock()
	log.Printf("rows: scan kept %d row(s) for %d open query(ies) (%d unique hashes), index rebuilt in %s",
		lines, queries, unique, time.Since(start).Round(time.Millisecond))
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
	if _, err := w.WriteString(idxMagic); err != nil {
		_ = f.Close()
		return err
	}
	for query, set := range s.seen {
		hs := make([]uint64, 0, len(set))
		for h := range set {
			hs = append(hs, h)
		}
		if err := writeIndexBlock(w, query, hs); err != nil {
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
	return os.Rename(tmp, s.idxPath())
}

// Forget drops a finished query's dedupe hashes. Their whole job is to stop a
// requeued or resumed query from writing rows it already wrote, and a query in
// done.lst is never collected again — keeping them is what turned the index
// into a gigabyte file and the heap into five.
func (s *Sink) Forget(query string) {
	key := strings.ToLower(strings.TrimSpace(query))
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.seen, key)
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
// rows it kept. A missing file is not an error: a fresh run has no rows file,
// and a rotated part can be absent if the numbering ever skips.
//
// Rows of queries isDone reports as finished are skipped: their hashes are
// dead weight, and dropping them here is what keeps a rebuilt index (and the
// heap behind it) down to the queries still in flight instead of to every row
// ever collected.
func (s *Sink) loadRowsFile(path string, t *rowsLoadTracker, isDone func(string) bool) (int, error) {
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
		t.lines.Add(1)
		fields := strings.Split(sc.Text(), "\t")
		if len(fields) < 13 {
			continue // stamp + query + a full row, or a torn line
		}
		key := strings.ToLower(strings.TrimSpace(fields[1]))
		if key == "" || isDone(key) {
			continue
		}
		row, ok := RowFromValues(fields[2:13])
		if !ok {
			continue
		}
		s.remember(key, row.Key())
		lines++
	}
	if err := sc.Err(); err != nil {
		return lines, fmt.Errorf("%s: %w", path, err)
	}
	return lines, nil
}

// remember records one row key of one query in the dedupe set. The caller
// holds the mutex.
func (s *Sink) remember(key, rowKey string) {
	set := s.seen[key]
	if set == nil {
		set = make(map[uint64]struct{})
		s.seen[key] = set
	}
	set[rowHash(key, rowKey)] = struct{}{}
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
	set := s.seen[key]
	if set == nil {
		set = make(map[uint64]struct{}, len(rows))
	}
	fresh := make([]Row, 0, len(rows))
	hashes := make([]uint64, 0, len(rows))
	// A batch can repeat a row itself (the site does repeat records across
	// pages); the local set catches that without touching the query's set early.
	batch := make(map[uint64]struct{}, len(rows))
	var b strings.Builder
	for _, r := range rows {
		h := rowHash(key, r.Key())
		if _, dup := set[h]; dup {
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
	if err := s.appendIndex(key, hashes); err != nil {
		log.Printf("rows: index append: %v — the next start will re-scan", err)
	}
	// Only now, with the rows on disk, does the query own these keys: a write
	// that failed above has to leave them free for the retry.
	s.seen[key] = set
	for _, h := range hashes {
		set[h] = struct{}{}
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

// Checkpoint returns where this query (and shard) stopped: the last grid page
// that reached rows.tsv, and how many rows the query has written up to and
// including that page. The page is 0 when there is none; rows is -1 when it is
// unknown — a checkpoint written by an older build carries only the page.
//
// It is the difference between resuming and restarting. Rows alone say what a
// run collected but not where it stopped, and the only way to derive a page
// number from them is to count — which is wrong across shards (they write into
// one file) and wrong if the page size ever changes. The row total is what the
// completeness check in finishQuery holds the walk against: a run that
// resumed at page 500 still has to account for the 499 pages before it.
//
// Both numbers are written after the rows, never before, so a checkpoint is
// always behind the data: a crash between the two re-walks a page whose rows
// are then deduplicated, while the opposite order would skip rows that were
// never written.
func (s *Sink) Checkpoint(query string, shard, shardCount int) (page, rows int) {
	raw, err := os.ReadFile(s.ckptPath(query, shard, shardCount))
	if err != nil {
		return 0, -1
	}
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return 0, -1
	}
	page, err = strconv.Atoi(fields[0])
	if err != nil || page < 0 {
		return 0, -1
	}
	if len(fields) < 2 {
		return page, -1 // an older checkpoint: page only
	}
	n, err := strconv.Atoi(fields[1])
	if err != nil || n < 0 {
		return page, -1
	}
	return page, n
}

// SetCheckpoint records that every row up to grid page was written, and how
// many rows of this query that makes in total. Failure is the caller's to
// report but not to act on: a lost checkpoint costs a re-walk that dedupe
// absorbs, a lost row does not.
func (s *Sink) SetCheckpoint(query string, shard, shardCount, page, rows int) error {
	path := s.ckptPath(query, shard, shardCount)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(fmt.Sprintf("%d\t%d", page, rows)), 0644)
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
