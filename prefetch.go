package main

import (
	"context"
	"errors"
	"sync"

	"wipos/internal/replay"
)

// errNoPrefetcher is a programming error, not a runtime condition: the writer
// only asks a pipeline it created for a page.
var errNoPrefetcher = errors.New("page prefetcher is not running")

// pageFetcher fetches one grid page. It takes a context so an early stop can
// abandon the requests already in flight instead of waiting out their
// deadlines.
type pageFetcher func(ctx context.Context, gridPage int) (*replay.Page, error)

// fetchedPage is one answer travelling from a fetcher to the writer.
type fetchedPage struct {
	gridPage int
	page     *replay.Page
	err      error
}

// pagePrefetcher fetches a window of grid pages concurrently and hands them to
// the writing loop strictly in page order.
//
// The replay page walk is dominated by the round trip, not by the write: a page
// costs 0.6–3 s over a proxy and a few milliseconds on disk. Fetching the next
// pages while the current one is being written turns those round trips into
// overlap instead of a queue, and a query with a thousand pages gets faster
// without touching the output format or the checkpoint rules.
//
// Ordering is the point. Row writes and checkpoints still happen one page at a
// time, in grid order; a page that finishes early waits in pending until the
// pages before it have been written. An error is delivered at its own place in
// the walk too, so the worker grades (and rotates a proxy for) exactly the page
// the sequential walk would have failed on.
//
// The window is bounded: a worker may only run ahead of the page the writer has
// consumed by ahead pages. Without that bound a consumer stuck on one slow
// answer would let the producer race through the whole query, wasting requests
// and looking far more aggressive than configured.
type pagePrefetcher struct {
	ctx    context.Context
	cancel context.CancelFunc
	fetch  pageFetcher
	first  int // first page the pipeline owns, inclusive
	last   int // last page the pipeline owns, inclusive
	ahead  int

	jobs     chan int
	out      chan fetchedPage
	advanced chan int
	wg       sync.WaitGroup

	// pending holds answers that arrived before the page the writer is waiting
	// for. Only the writer touches it, so no lock is needed.
	pending map[int]fetchedPage
}

// newPagePrefetcher starts a pipeline over pages first..last. It returns nil
// when the pipeline is off (ahead <= 0) or there is nothing to prefetch, so the
// caller can keep a single code path and call Get only on a non-nil value.
// parent bounds the whole pipeline; canceling it stops every request.
func newPagePrefetcher(parent context.Context, fetch pageFetcher, first, last, ahead int) *pagePrefetcher {
	if ahead <= 0 || fetch == nil || first > last {
		return nil
	}
	ctx, cancel := context.WithCancel(parent)
	p := &pagePrefetcher{
		ctx:      ctx,
		cancel:   cancel,
		fetch:    fetch,
		first:    first,
		last:     last,
		ahead:    ahead,
		jobs:     make(chan int),
		out:      make(chan fetchedPage, ahead),
		advanced: make(chan int, 1),
		pending:  make(map[int]fetchedPage, ahead),
	}
	p.wg.Add(ahead + 1)
	for i := 0; i < ahead; i++ {
		go p.worker()
	}
	go p.scheduler()
	return p
}

// Get returns the page at gridPage, fetching it directly when it falls outside
// the prefetched range (the site's count can grow while a walk is running).
func (p *pagePrefetcher) Get(gridPage int) (*replay.Page, error) {
	if p == nil {
		return nil, errNoPrefetcher
	}
	if gridPage < p.first || gridPage > p.last {
		return p.fetch(p.ctx, gridPage)
	}
	for {
		if r, ok := p.pending[gridPage]; ok {
			delete(p.pending, gridPage)
			if r.err == nil {
				p.advance(gridPage)
			}
			return r.page, r.err
		}
		select {
		case r := <-p.out:
			if r.gridPage == gridPage {
				if r.err == nil {
					p.advance(gridPage)
				}
				return r.page, r.err
			}
			p.pending[r.gridPage] = r
		case <-p.ctx.Done():
			return nil, p.ctx.Err()
		}
	}
}

// Close cancels the pipeline and waits for every fetcher to leave the HTTP
// client. Waiting matters: the worker may rotate the proxy or drop the session
// the moment the attempt returns, and no request may still be using it then.
func (p *pagePrefetcher) Close() {
	if p == nil {
		return
	}
	p.cancel()
	p.wg.Wait()
}

// advance tells the scheduler which page the writer has consumed. The channel
// holds one value and the newest page replaces an unread older one: the
// scheduler only needs the high-water mark, and dropping a stale value is what
// keeps the writer from ever blocking on its own progress.
func (p *pagePrefetcher) advance(gridPage int) {
	select {
	case <-p.advanced:
	default:
	}
	select {
	case p.advanced <- gridPage:
	default:
	}
}

// scheduler hands page numbers to the fetchers, keeping the window at most
// ahead pages past the last page the writer consumed.
func (p *pagePrefetcher) scheduler() {
	defer p.wg.Done()
	defer close(p.jobs)

	consumed := p.first - 1
	next := p.first
	for next <= p.last {
		limit := consumed + p.ahead
		if limit > p.last {
			limit = p.last
		}
		for next <= limit {
			select {
			case p.jobs <- next:
				next++
			case page := <-p.advanced:
				if page > consumed {
					consumed = page
					limit = consumed + p.ahead
					if limit > p.last {
						limit = p.last
					}
				}
			case <-p.ctx.Done():
				return
			}
		}
		select {
		case page := <-p.advanced:
			if page > consumed {
				consumed = page
			}
		case <-p.ctx.Done():
			return
		}
	}
}

// worker fetches the page numbers the scheduler assigns.
func (p *pagePrefetcher) worker() {
	defer p.wg.Done()
	for {
		select {
		case gridPage, ok := <-p.jobs:
			if !ok {
				return
			}
			page, err := p.fetch(p.ctx, gridPage)
			select {
			case p.out <- fetchedPage{gridPage: gridPage, page: page, err: err}:
			case <-p.ctx.Done():
				return
			}
		case <-p.ctx.Done():
			return
		}
	}
}
