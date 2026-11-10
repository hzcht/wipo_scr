package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"wipos/internal/replay"
)

// fakePage is a page carrying only what the ordering tests need to identify
// it: the offset the walk asked for.
func fakePage(gridPage, pageSize int) *replay.Page {
	return &replay.Page{Start: (gridPage - 1) * pageSize}
}

// The pipeline exists to overlap round trips, but it must never reorder them:
// rows and checkpoints are written page by page, and a page that finished early
// waits until every page before it has been consumed.
func TestPagePrefetcherReturnsPagesInOrderAndOverlaps(t *testing.T) {
	const (
		pages = 6
		ahead = 3
	)
	var inFlight, peak atomic.Int32
	started := make(chan int, pages)
	release := make(chan struct{})

	fetch := func(ctx context.Context, gridPage int) (*replay.Page, error) {
		n := inFlight.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		started <- gridPage
		select {
		case <-release:
		case <-ctx.Done():
			inFlight.Add(-1)
			return nil, ctx.Err()
		}
		inFlight.Add(-1)
		return fakePage(gridPage, 100), nil
	}

	p := newPagePrefetcher(context.Background(), fetch, 1, pages, ahead)
	defer p.Close()

	// Three fetches must be in flight while the writer is still on page one.
	for i := 0; i < ahead; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d fetches started, want %d in flight", i, ahead)
		}
	}
	if got := peak.Load(); got < 2 {
		t.Errorf("peak concurrency = %d, want at least 2 — the fetches are not overlapping", got)
	}
	close(release)

	for gridPage := 1; gridPage <= pages; gridPage++ {
		page, err := p.Get(gridPage)
		if err != nil {
			t.Fatalf("Get(%d): %v", gridPage, err)
		}
		if got, want := page.Start, (gridPage-1)*100; got != want {
			t.Fatalf("Get(%d) returned offset %d, want %d — the pages were reordered", gridPage, got, want)
		}
	}
}

// An error belongs at its own place in the walk, not wherever it happened to
// finish: the worker charges the proxy and resumes on exactly the page the
// sequential walk would have failed on.
func TestPagePrefetcherDeliversErrorsAtTheirPage(t *testing.T) {
	wantErr := errors.New("page 3 was refused")
	fetch := func(ctx context.Context, gridPage int) (*replay.Page, error) {
		if gridPage == 3 {
			return nil, wantErr
		}
		return fakePage(gridPage, 100), nil
	}
	p := newPagePrefetcher(context.Background(), fetch, 1, 5, 2)
	defer p.Close()

	for _, gridPage := range []int{1, 2} {
		if _, err := p.Get(gridPage); err != nil {
			t.Fatalf("Get(%d): %v", gridPage, err)
		}
	}
	if _, err := p.Get(3); !errors.Is(err, wantErr) {
		t.Fatalf("Get(3) = %v, want %v", err, wantErr)
	}
	// The failure of one page must not swallow the pages already fetched
	// behind it.
	if page, err := p.Get(4); err != nil || page.Start != 300 {
		t.Fatalf("Get(4) = %+v, %v — the page behind the error was lost", page, err)
	}
}

// The site's count can grow while a walk runs. A page outside the prefetched
// range is fetched on demand instead of waiting forever for a result nobody
// scheduled.
func TestPagePrefetcherFetchesOutsideItsRangeDirectly(t *testing.T) {
	var direct atomic.Int32
	fetch := func(ctx context.Context, gridPage int) (*replay.Page, error) {
		if gridPage == 9 {
			direct.Add(1)
		}
		return fakePage(gridPage, 100), nil
	}
	p := newPagePrefetcher(context.Background(), fetch, 1, 2, 1)
	defer p.Close()

	page, err := p.Get(9)
	if err != nil {
		t.Fatalf("Get(9): %v", err)
	}
	if page.Start != 800 || direct.Load() != 1 {
		t.Errorf("Get(9) = offset %d, direct fetches %d; want 800 and 1", page.Start, direct.Load())
	}
}

// Without that bound a writer stuck on one slow page would let the producer run
// through the whole query: wasted requests and far more traffic than configured.
func TestPagePrefetcherDoesNotRunAheadWithoutConsumption(t *testing.T) {
	var scheduled atomic.Int32
	fetch := func(ctx context.Context, gridPage int) (*replay.Page, error) {
		scheduled.Add(1)
		return fakePage(gridPage, 100), nil
	}
	p := newPagePrefetcher(context.Background(), fetch, 1, 100, 3)
	defer p.Close()

	time.Sleep(100 * time.Millisecond)
	if got := scheduled.Load(); got > 3 {
		t.Errorf("%d pages fetched without consuming one, want at most the window of 3", got)
	}
}

// Closing the walk must abort the requests still in flight, not wait out their
// deadlines: the worker drops the session and rotates the proxy right after the
// attempt returns, and no request may still be using it then.
func TestPagePrefetcherCloseCancelsInflightFetches(t *testing.T) {
	started := make(chan struct{}, 8)
	fetch := func(ctx context.Context, gridPage int) (*replay.Page, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	p := newPagePrefetcher(context.Background(), fetch, 1, 100, 2)
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d fetches started", i)
		}
	}

	done := make(chan struct{})
	go func() {
		p.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close waited for the fetches instead of canceling them")
	}
}
