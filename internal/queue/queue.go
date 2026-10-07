// Package queue turns the input list into deduplicated tasks, remembers which
// ones are already handled, and hands them to the workers.
package queue

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Task is one search term queued for collection. A sharded task carries a piece
// of that term's result pages instead of the whole set — see ShardTasks.
type Task struct {
	Name        string
	LastAttempt int64 // unix time of the last attempt (0 = never tried)
	Attempts    int
	Shard       int // 0-based piece of the query's pages; meaningless below 2
	ShardCount  int // how many pieces the query was split into, 0 = not split
}

// Sharded reports whether this task owns only part of its query's pages.
func (t Task) Sharded() bool { return t.ShardCount > 1 }

// LoadLines reads a text file with one entry per line.
//
// Lines starting with '#' are comments, so a hand-edited list can carry a header
// explaining what belongs in it. Nothing in a proxy list or a search-term list
// starts with '#', and a term that does would fail the search anyway — far less
// costly than a commented line turning into a live query.
func LoadLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // a pathological line must not kill the scan
	first := true
	for sc.Scan() {
		line := strings.TrimSpace(strings.TrimSuffix(sc.Text(), "\r"))
		if first {
			// A UTF-8 BOM is invisible and survives TrimSpace, so a list saved
			// by any Windows editor turns its first term into "\uFEFFterm" — a query
			// that returns a handful of nonsense hits and is then recorded as a
			// real result. Strip it from the first line only: a BOM anywhere else
			// is part of that term.
			line = strings.TrimPrefix(line, "\uFEFF")
			first = false
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s does not contain any list", path)
	}
	return out, nil
}

// Store is a thread-safe append-only set of names backed by a list file
// (done.lst / failed.lst). Appends only log their errors: a full disk or a
// locked file must not take a worker down with it, and a lost line just means
// the query is collected again.
type Store struct {
	mu   sync.Mutex
	path string
	set  map[string]struct{}
}

// LoadStore reads one name per line into a lowercased set. A missing file
// yields an empty store, not an error — that is a fresh run.
func LoadStore(path string) (*Store, error) {
	s := &Store{path: path, set: map[string]struct{}{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return s, err
	}
	// Same BOM rule as LoadLines: a done list saved by a Windows editor starts
	// with one, and an invisible prefix on the first name means that query is
	// never recognised as already collected and gets run again.
	for _, line := range strings.Split(strings.TrimPrefix(string(raw), "\uFEFF"), "\n") {
		if name := strings.ToLower(strings.TrimSpace(strings.TrimSuffix(line, "\r"))); name != "" {
			s.set[name] = struct{}{}
		}
	}
	return s, nil
}

// Has reports whether the name is already stored. The comparison is
// case-insensitive: the monitor is case-insensitive too, so "Acme" and "acme"
// are the same query and must not be collected twice.
func (s *Store) Has(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.set[strings.ToLower(strings.TrimSpace(name))]
	return ok
}

// Len returns the number of stored names.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.set)
}

// Append records the name in memory and in the backing file.
func (s *Store) Append(name string) {
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.set[key]; ok {
		return
	}
	s.set[key] = struct{}{}
	f, err := os.OpenFile(s.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0644)
	if err != nil {
		log.Printf("append %s: %v", s.path, err)
		return
	}
	defer func() { _ = f.Close() }()
	if _, err := fmt.Fprintf(f, "%s\n", name); err != nil {
		log.Printf("append %s: %v", s.path, err)
	}
}

// BuildTasks lowercases the input, drops duplicates and everything already
// recorded in done.lst, and returns the rest in file order.
//
// failed.lst is deliberately NOT consulted: it is a list of queries that never
// got a fair shot, and a fresh run has proxies that may well work. Skipping
// them would make one bad afternoon permanent.
func BuildTasks(input string, done *Store) ([]Task, error) {
	names, err := LoadLines(input)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(names))
	tasks := make([]Task, 0, len(names))
	for _, n := range names {
		name := strings.ToLower(strings.TrimSpace(n))
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		if done.Has(name) {
			continue
		}
		tasks = append(tasks, Task{Name: name})
	}
	return tasks, nil
}

// ShardTasks splits every query into n pieces of its result pages.
//
// It exists because a task is owned by exactly one worker: with a single input
// line and -threads 4, three of the four browsers would sit idle while the
// fourth walked all 15,430 pages of "*". The pieces are separate tasks, so the
// ordinary channel distributes them across the workers on its own; the query is
// only marked collected once the last piece lands (see shardGate in the root
// package).
//
// n <= 1 returns the list untouched, so -split-pages without threads is a no-op
// rather than a rewrite of every task.
func ShardTasks(tasks []Task, n int) []Task {
	if n <= 1 {
		return tasks
	}
	out := make([]Task, 0, len(tasks)*n)
	for _, t := range tasks {
		for shard := 0; shard < n; shard++ {
			piece := t
			piece.Shard = shard
			piece.ShardCount = n
			out = append(out, piece)
		}
	}
	return out
}

// Queue is a buffered task channel with WaitGroup accounting.
//
// A task is Done only from a terminal state (collected, no results,
// failed-listed, dropped). A requeue leaves it counted, so a query that keeps
// failing cannot make the run exit early. The channel is closed only after
// every task is terminal, so a requeue from a dying worker can never panic on
// a closed channel.
type Queue struct {
	ch            chan Task
	wg            sync.WaitGroup
	live          atomic.Int64
	pauseMu       sync.Mutex // guards pausedUntil only, never held across a wait
	pausedUntil   time.Time
	pauseDuration time.Duration
}

// NewQueue buffers every task and counts the unfinished work.
func NewQueue(tasks []Task, workers int, pauseDuration time.Duration) *Queue {
	q := &Queue{
		ch:            make(chan Task, len(tasks)),
		pauseDuration: pauseDuration,
	}
	q.wg.Add(len(tasks))
	q.live.Store(int64(workers))
	for _, t := range tasks {
		q.ch <- t
	}
	return q
}

// Get takes the next task, blocking until one is free, the queue is closed or a
// pauseAll freeze is in effect.
//
// The pause is a deadline, not a held lock. An earlier version held the gate
// mutex across the channel receive, which deadlocked the whole run: the last
// idle workers sat in Get holding the lock, the worker that hit the marker
// blocked in PauseAll behind them, and nothing was ever collected again.
func (q *Queue) Get() (Task, bool) {
	q.waitUnpaused()
	t, ok := <-q.ch
	return t, ok
}

// waitUnpaused blocks until the newest pauseAll deadline has passed.
func (q *Queue) waitUnpaused() {
	q.pauseMu.Lock()
	until := q.pausedUntil
	q.pauseMu.Unlock()
	if left := time.Until(until); left > 0 {
		time.Sleep(left)
	}
}

// Requeue puts an unfinished task back. It stays counted.
func (q *Queue) Requeue(t Task) {
	q.ch <- t
}

// Done marks one task terminally finished.
func (q *Queue) Done() {
	q.wg.Done()
}

// PauseAll freezes the intake of every worker for the configured duration.
//
// The window runs from the last marker: two markers three seconds apart freeze
// the fleet for the full duration after the second one, not for what is left of
// the first. A marker in the middle of a freeze must never cut it short — that
// is exactly when a site is pushing hardest. A worker already inside an attempt
// is not interrupted; it notices at its next Get.
func (q *Queue) PauseAll() {
	if q.pauseDuration <= 0 {
		return
	}
	q.pauseMu.Lock()
	q.pausedUntil = time.Now().Add(q.pauseDuration)
	q.pauseMu.Unlock()
}

// Len reports how many tasks are still in flight.
func (q *Queue) Len() int { return len(q.ch) }

// WorkerDown runs when a worker exits for good. Only the last one left drains
// the channel, so a single worker dying does not throw away the whole queue.
func (q *Queue) WorkerDown(drop func(Task)) {
	if q.live.Add(-1) == 0 {
		for t := range q.ch {
			drop(t)
		}
	}
}

// WaitAndClose blocks until every task is terminal, then closes the channel.
func (q *Queue) WaitAndClose() {
	q.wg.Wait()
	close(q.ch)
}
