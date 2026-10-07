package main

import (
	"strings"
	"sync"
)

// shardGate counts the finished pieces of a split query, so its count, its done
// entry and its checkpoint cleanup happen exactly once — when the last shard
// reports in.
//
// Without it every shard would write its own "collected" line for the same
// query (the count file deduplicates, but done.lst and Collected do not) and,
// worse, the first shard to finish would free the row-dedupe set the others are
// still relying on.
type shardGate struct {
	mu     sync.Mutex
	done   map[string]int // query -> shards that finished
	counts map[string]int // query -> the largest count a shard saw
}

func newShardGate() *shardGate {
	return &shardGate{
		done:   map[string]int{},
		counts: map[string]int{},
	}
}

// Reach records that one shard of the query finished with count rows in
// result, and reports whether it was the last one. The count that survives is
// the largest the shards observed: the total can grow while a split run walks
// it (the site updates underneath), and the lower early reading would record a
// number smaller than the rows actually in rows.tsv.
func (g *shardGate) Reach(name string, count, shardCount int) (last bool, final int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	key := strings.ToLower(strings.TrimSpace(name))
	g.done[key]++
	if c, ok := g.counts[key]; !ok || count > c {
		g.counts[key] = count
	}
	if g.done[key] < shardCount {
		return false, 0
	}
	return true, g.counts[key]
}

// Reset removes a query's gate state. Called when a split query is requeued
// (e.g., after a failure) so the next attempt starts with a clean slate.
// Without this, a requeued query would incorrectly think its shards already
// finished and skip writing its results.
func (g *shardGate) Reset(name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	key := strings.ToLower(strings.TrimSpace(name))
	delete(g.done, key)
	delete(g.counts, key)
}
