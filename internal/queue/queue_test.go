package queue

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestLoadLinesStripsAByteOrderMark(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.txt")
	if err := os.WriteFile(path, []byte("\uFEFFACME\nbeta\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := LoadLines(path)
	if err != nil {
		t.Fatalf("LoadLines: %v", err)
	}
	want := []string{"ACME", "beta"}
	if len(got) != len(want) {
		t.Fatalf("LoadLines = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestLoadStoreStripsAByteOrderMark(t *testing.T) {
	path := filepath.Join(t.TempDir(), "done.lst")
	if err := os.WriteFile(path, []byte("\uFEFFACME\nbeta\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	s, err := LoadStore(path)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	// Without the fix the first name keeps an invisible prefix and is never
	// matched, so the query behind it is collected a second time.
	if !s.Has("ACME") {
		t.Error("ACME not recognised as done: the BOM survived the read")
	}
	if !s.Has("beta") {
		t.Error("beta not recognised as done")
	}
}

func TestLoadLinesSkipsCommentsAndBlanks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.txt")
	raw := "# a header\n\nACME\r\n  spaced  \n#another\nacme\n"
	if err := os.WriteFile(path, []byte(raw), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := LoadLines(path)
	if err != nil {
		t.Fatalf("LoadLines: %v", err)
	}
	want := []string{"ACME", "spaced", "acme"}
	if len(got) != len(want) {
		t.Fatalf("LoadLines = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestLoadLinesEmptyListIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.txt")
	if err := os.WriteFile(path, []byte("# only comments\n\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadLines(path); err == nil {
		t.Error("a list with nothing in it must not look like a working list")
	}
	if _, err := LoadLines(filepath.Join(t.TempDir(), "missing.txt")); err == nil {
		t.Error("a missing file must be an error, not an empty list")
	}
}

func TestBuildTasksSkipsDoneButRetriesFailed(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "task.txt")
	raw := "# header\nACME\nacme\nOther\nFresh\n"
	if err := os.WriteFile(input, []byte(raw), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	done, err := LoadStore(filepath.Join(dir, "done.lst"))
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	failed, err := LoadStore(filepath.Join(dir, "failed.lst"))
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	done.Append("acme")
	failed.Append("other")

	tasks, err := BuildTasks(input, done)
	if err != nil {
		t.Fatalf("BuildTasks: %v", err)
	}
	got := make([]string, 0, len(tasks))
	for _, task := range tasks {
		got = append(got, task.Name)
	}
	// "acme" is done (skipped), "ACME" is its duplicate, "other" is in
	// failed.lst and gets another chance.
	if len(got) != 2 || got[0] != "other" || got[1] != "fresh" {
		t.Errorf("BuildTasks = %q, want [other fresh]", got)
	}
}

func TestStoreIsCaseInsensitiveAndAppendOnlyOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "done.lst")
	s, err := LoadStore(path)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	if s.Has("acme") {
		t.Error("a fresh store already has a query in it")
	}
	s.Append("ACME")
	s.Append("acme")
	s.Append("  acme  ")
	if !s.Has("Acme") {
		t.Error("Has(Acme) = false after Append(ACME)")
	}
	if s.Len() != 1 {
		t.Errorf("Len = %d, want 1", s.Len())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(raw) != "ACME\n" {
		t.Errorf("done.lst = %q, want the first spelling once", raw)
	}
}

func TestStoreSurvivesAReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "done.lst")
	first, _ := LoadStore(path)
	first.Append("acme")
	second, err := LoadStore(path)
	if err != nil {
		t.Fatalf("LoadStore: %v", err)
	}
	if !second.Has("acme") {
		t.Error("the reloaded store forgot what the first one collected")
	}
}

func TestQueueRequeueKeepsTheRunAlive(t *testing.T) {
	q := NewQueue([]Task{{Name: "a"}, {Name: "b"}}, 1, 0)

	// One worker drains the queue: "a" is requeued the first time it comes
	// round, everything else is finished. The requeue keeps its place in the
	// queue, so the order of the second pass is not the test's business.
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		retried := false
		for {
			task, ok := q.Get()
			if !ok {
				return
			}
			if task.Name == "a" && !retried {
				retried = true
				q.Requeue(task) // still in flight, no Done
				continue
			}
			q.Done()
		}
	}()

	closed := make(chan struct{})
	go func() { q.WaitAndClose(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("WaitAndClose did not return although every task is terminal")
	}
	<-drained
	if q.Len() != 0 {
		t.Errorf("%d tasks left in the queue", q.Len())
	}
}

func TestPauseAllDoesNotBlockIntakeForever(t *testing.T) {
	// The regression this guards: PauseAll used to hold the intake mutex while
	// it slept, and Get held the same mutex across its channel receive. With
	// every worker idle in Get, the worker that hit the marker waited behind
	// them for the whole pause and the run never finished.
	q := NewQueue([]Task{{Name: "a"}, {Name: "b"}, {Name: "c"}}, 3, 40*time.Millisecond)

	// Two workers are already parked in Get when the marker arrives.
	stop := make(chan struct{})
	var parked sync.WaitGroup
	parked.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer parked.Done()
			<-stop
			if _, ok := q.Get(); !ok {
				return
			}
			q.Done()
		}()
	}
	time.Sleep(20 * time.Millisecond)

	done := make(chan struct{})
	go func() { q.PauseAll(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		close(stop)
		t.Fatal("PauseAll blocked behind workers waiting for a task")
	}
	close(stop)
	parked.Wait()

	// The last task is still collectable, and the run finishes.
	if _, ok := q.Get(); !ok {
		t.Error("Get returned a closed queue while a task was still pending")
	}
	q.Done()
	closed := make(chan struct{})
	go func() { q.WaitAndClose(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the queue did not finish after the pause")
	}
}

func TestPauseAllExtendsTheWindow(t *testing.T) {
	q := NewQueue([]Task{{Name: "a"}}, 1, 60*time.Millisecond)
	first := time.Now()
	q.PauseAll()
	time.Sleep(30 * time.Millisecond)
	q.PauseAll() // a second marker lands while the first freeze is still running
	// The window runs from the last marker, so intake opens about 90 ms after
	// the first one — a marker in the middle of a freeze must not cut it short.
	// The wait itself happens in Get, where a worker notices the freeze.
	if _, ok := q.Get(); !ok {
		t.Fatal("Get returned a closed queue")
	}
	if held := time.Since(first); held < 80*time.Millisecond {
		t.Errorf("intake opened after %s, want the second window to run its course", held)
	}
	q.Done()
	q.WaitAndClose()
}

func TestWorkerDownDrainsOnlyWhenTheLastOneLeaves(t *testing.T) {
	q := NewQueue([]Task{{Name: "a"}, {Name: "b"}, {Name: "c"}}, 2, 0)

	var mu sync.Mutex
	collected, dropped := 0, 0
	record := func(Task) {
		mu.Lock()
		dropped++
		mu.Unlock()
		q.Done()
	}

	// One worker collects a single task and leaves; the other leaves without
	// taking anything. Which one gets which task is not the point — the point
	// is that the run still finishes and no task is left counted as in flight.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, ok := q.Get(); ok {
			mu.Lock()
			collected++
			mu.Unlock()
			q.Done()
		}
		q.WorkerDown(record)
		// Give the first WorkerDown a chance to run before the last one.
		time.Sleep(50 * time.Millisecond)
	}()
	q.WorkerDown(record)

	closed := make(chan struct{})
	go func() { q.WaitAndClose(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the queue never finished after the last worker left")
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if collected+dropped != 3 {
		t.Errorf("%d tasks reached a terminal state (collected %d, dropped %d), want 3 — "+
			"one is still counted as in flight", collected+dropped, collected, dropped)
	}
	if dropped != 2 {
		t.Errorf("dropped %d tasks, want 2 (whatever the last worker left behind)", dropped)
	}
}

// ShardTasks is what turns "-threads 4" on one huge query into four workers
// with work instead of one with work and three idling: every query becomes one
// task per thread, all carrying the same name and their own piece index.
func TestShardTasks(t *testing.T) {
	t.Run("n is one, nothing changes", func(t *testing.T) {
		in := []Task{{Name: "*"}, {Name: "acme"}}
		out := ShardTasks(in, 1)
		if len(out) != len(in) {
			t.Fatalf("ShardTasks(2 tasks, 1) = %d tasks, want 2", len(out))
		}
		for i, task := range out {
			if task.Shard != 0 || task.ShardCount != 0 {
				t.Errorf("task %d = shard %d/%d, want unsplit 0/0", i, task.Shard, task.ShardCount)
			}
		}
	})

	t.Run("every query gets a piece per thread", func(t *testing.T) {
		in := []Task{{Name: "*"}, {Name: "acme", Attempts: 2}}
		out := ShardTasks(in, 3)
		if len(out) != 6 {
			t.Fatalf("ShardTasks(2 queries, 3) = %d tasks, want 6", len(out))
		}
		shardsOf := map[string][]int{}
		for _, task := range out {
			if !task.Sharded() {
				t.Errorf("task %q shard %d/%d: Sharded() = false", task.Name, task.Shard, task.ShardCount)
			}
			shardsOf[task.Name] = append(shardsOf[task.Name], task.Shard)
			if task.Name == "acme" && task.Attempts != 2 {
				t.Errorf("attempts = %d, want the original 2 carried over", task.Attempts)
			}
		}
		// Each query must appear exactly once per shard index — a missing index
		// leaves a hole of uncollected pages, a duplicate one walks them twice.
		for name, shards := range shardsOf {
			seen := map[int]bool{}
			for _, s := range shards {
				if seen[s] {
					t.Errorf("%q: shard %d handed out twice", name, s)
				}
				seen[s] = true
			}
			for want := 0; want < 3; want++ {
				if !seen[want] {
					t.Errorf("%q: shard %d missing", name, want)
				}
			}
		}
	})
}
