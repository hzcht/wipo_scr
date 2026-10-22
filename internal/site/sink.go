package site

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
type Sink struct {
	mu          sync.Mutex
	resultsPath string
	rowsPath    string
	dataDir     string
	rowsMax     int64                          // rotate the rows file past this many bytes; 0 = never
	nextPart    int                            // index of the next rotated part (rowsPath.1, .2, ...)
	rowsBytes   int64                          // size of the active rows file, for the rotation check
	counted     map[string]struct{}            // queries already in the count file
	rowSeen     map[string]map[string]struct{} // query -> row keys already written
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
		rowSeen:     map[string]map[string]struct{}{},
		saveGrid:    saveGrid,
		saveFull:    saveFull,
	}
}

// Load primes the sink from what an earlier run already wrote: the count file
// fills counted, the row file fills rowSeen. Both matter for a resumed run, and
// the row file matters most — without it a restart re-walks every page of a
// half-collected query and appends every one of those rows a second time. For a
// crawl the size of the whole database, that turns an interruption at page
// 7,000 into a doubled file.
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

// loadRows rebuilds the dedupe set from the row files: every rotated part,
// oldest to newest, then the active file. It also seeds the rotation state
// (nextPart, rowsBytes) so an append can keep rotating where the last run left
// off.
//
// A line that does not hold a full row is skipped rather than fatal: a file can
// end in a half-written line if the previous run died mid-append, and refusing
// to start over a torn last line would be worse than re-collecting one row. The
// key is recomputed from the written columns rather than stored beside them, so
// the file format stays exactly what it was.
func (s *Sink) loadRows() error {
	start := time.Now()
	parts, maxPart := s.rowParts()
	s.nextPart = maxPart + 1
	// The pass answers "are these rows already collected?" for the whole run,
	// so it reads every rotated part before the first query can go out. On a
	// day-old output directory that is gigabytes of TSV and the only thing the
	// run would otherwise do in silence — hence a line per part plus the
	// summary at the end.
	log.Printf("rows: rebuilding dedupe from %d rotated part(s) and the active file", len(parts))
	var lines int
	for _, p := range parts {
		n, err := s.loadRowsFile(p)
		if err != nil {
			return err
		}
		lines += n
	}
	n, err := s.loadRowsFile(s.rowsPath)
	if err != nil {
		return err
	}
	lines += n
	if fi, err := os.Stat(s.rowsPath); err == nil {
		s.rowsBytes = fi.Size()
	}

	s.mu.Lock()
	unique := 0
	for _, seen := range s.rowSeen {
		unique += len(seen)
	}
	s.mu.Unlock()
	log.Printf("rows: %d line(s), %d unique rows, dedupe rebuilt in %s",
		lines, unique, time.Since(start).Round(time.Millisecond))
	return nil
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
// and a rotated part can be absent if the numbering ever skips. A missing file
// is also the only thing walked through silently here — the loader logs the
// size of every part it does read, so a long pass is visibly moving.
func (s *Sink) loadRowsFile(path string) (int, error) {
	fi, err := os.Stat(path)
	if err == nil {
		log.Printf("rows: loading %s (%d MB)", filepath.Base(path), fi.Size()>>20)
	}
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
		seen := s.rowSeen[key]
		if seen == nil {
			seen = map[string]struct{}{}
			s.rowSeen[key] = seen
		}
		seen[row.Key()] = struct{}{}
		lines++
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
	// The query is terminal now, so its row-dedupe state can go.
	delete(s.rowSeen, key)
	return nil
}

// AddRows appends one line per trademark:
//
//	<date time> \t <query> \t <brand> \t <holder> \t ...
//
// Rows whose key was already written for this query are dropped, which is what
// keeps a requeued query from doubling its records. The keys are recorded only
// after the write went through: a requeued query whose write failed has to be
// able to write the same rows again.
func (s *Sink) AddRows(query string, rows []Row, at time.Time) error {
	key := strings.ToLower(strings.TrimSpace(query))
	stamp := Stamped(at)

	s.mu.Lock()
	defer s.mu.Unlock()
	seen := s.rowSeen[key]
	if seen == nil {
		seen = map[string]struct{}{}
	}
	fresh := make([]Row, 0, len(rows))
	var b strings.Builder
	for _, r := range rows {
		if _, dup := seen[r.Key()]; !dup {
			fresh = append(fresh, r)
		}
	}
	for _, r := range fresh {
		b.WriteString(strings.Join(append([]string{stamp, key}, r.Values()...), "\t"))
		b.WriteByte('\n')
	}
	if len(fresh) == 0 {
		return nil
	}
	line := strings.TrimSuffix(b.String(), "\n")
	if err := appendLine(s.rowsPath, line); err != nil {
		return err
	}
	s.rowsBytes += int64(len(line)) + 1 // the trailing newline appendLine adds
	for _, r := range fresh {
		seen[r.Key()] = struct{}{}
	}
	s.rowSeen[key] = seen
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
