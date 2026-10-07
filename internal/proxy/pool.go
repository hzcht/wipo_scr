// Package proxy rotates the outbound proxies with three protections: a failure
// budget for broken endpoints, a time-based cooldown for endpoints that answered
// with a block page, and per-endpoint latency that stretches the timeouts.
//
// WIPO does not answer this host's bare IP, so a proxy is not an optional extra
// here — it is the only way in. An empty list still means "direct", which the log
// states loudly.
//
// The latency part is the important one. A free proxy that answers in 400 ms and
// one that answers in 25 s are both "working" as far as a boolean check goes, but
// a fixed 2-minute budget is right for the first and a guaranteed failure for the
// second. Charging the slow one for being slow throws away the only proxy that
// works from here; waiting it out is what the latency-aware budget is for.
package proxy

import (
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// Options is the pool's tuning, all of it from the config file.
type Options struct {
	// MaxFails retires an endpoint once its failure budget is spent.
	MaxFails int
	// Cooldown parks an endpoint that served a block page.
	Cooldown time.Duration
	// LatencyRef is what one normal request step costs on a healthy link to
	// this site. It is the yardstick a measured endpoint is compared against:
	// half of it is fast, twice it is slow, and the timeout scale follows.
	LatencyRef time.Duration
	// ScaleMin and ScaleMax clamp how far a measured latency may stretch or
	// shrink a timeout. Without the ceiling one very slow endpoint would get a
	// budget of half an hour and stall the whole run behind it.
	ScaleMin float64
	ScaleMax float64
}

// DefaultOptions is what the collector uses when nothing is configured.
func DefaultOptions() Options {
	return Options{
		MaxFails:   5,
		Cooldown:   15 * time.Minute,
		LatencyRef: 3 * time.Second,
		ScaleMin:   1.0,
		ScaleMax:   4.0,
	}
}

func (o Options) normalised() Options {
	d := DefaultOptions()
	if o.MaxFails < 1 {
		o.MaxFails = d.MaxFails
	}
	if o.Cooldown <= 0 {
		o.Cooldown = d.Cooldown
	}
	if o.LatencyRef <= 0 {
		o.LatencyRef = d.LatencyRef
	}
	if o.ScaleMin <= 0 {
		o.ScaleMin = d.ScaleMin
	}
	if o.ScaleMax < o.ScaleMin {
		o.ScaleMax = d.ScaleMin
	}
	return o
}

// slowFactor is the measured latency above which an endpoint counts as slow for
// weighting purposes. Twice the reference is a deliberate line: well under it and
// the run is wasting good endpoints, far over it and every page click feels it.
const slowFactor = 2.0

// Pool hands out proxies and remembers how each one behaved.
//
// It never hard-stops: when every proxy is over budget or cooling down, the
// budget is reset and the least-cooling endpoint is returned. A run that stops
// because the free-proxy list rotted would be worse than a run on a bad proxy.
type Pool struct {
	mu            sync.Mutex
	opts          Options
	proxies       []string
	fails         map[string]float64
	cooldownUntil map[string]time.Time
	oks           map[string]int
	cools         map[string]int

	// attempts/total track whole attempts, which include paging and are
	// therefore a measure of work done rather than of the link.
	attempts map[string]int
	total    map[string]time.Duration

	// latency is an exponentially weighted mean of single request steps, which
	// is the only measurement that says something about the proxy itself. It is
	// what the timeout scale is built from.
	latency    map[string]time.Duration
	latSamples map[string]int
}

// ewmaAlpha weights a new sample against the running mean. A proxy list is full
// of endpoints whose speed changes from minute to minute, so the mean has to
// forget quickly, but not so fast that one slow packet doubles every budget.
const ewmaAlpha = 0.3

// New builds a pool over raw proxy strings. An empty list means direct
// connections (Pick returns "").
//
// Duplicates are dropped, order is kept. A scraped list usually repeats its
// working entries several times over, and leaving them in means the precheck
// probes the same endpoint over and over and the rotation keeps handing out the
// same handful of proxies.
func New(proxies []string, opts Options) *Pool {
	seen := make(map[string]struct{}, len(proxies))
	unique := make([]string, 0, len(proxies))
	for _, px := range proxies {
		px = strings.TrimSpace(px)
		if px == "" {
			continue
		}
		if _, dup := seen[px]; dup {
			continue
		}
		seen[px] = struct{}{}
		unique = append(unique, px)
	}
	return &Pool{
		opts:          opts.normalised(),
		proxies:       unique,
		fails:         map[string]float64{},
		cooldownUntil: map[string]time.Time{},
		oks:           map[string]int{},
		cools:         map[string]int{},
		attempts:      map[string]int{},
		total:         map[string]time.Duration{},
		latency:       map[string]time.Duration{},
		latSamples:    map[string]int{},
	}
}

// Len is the number of configured endpoints.
func (p *Pool) Len() int { return len(p.proxies) }

// Latency returns the measured step latency for an endpoint, or zero when it has
// never been measured. An unmeasured endpoint — which includes direct mode until
// its first timeout — gets the configured timeouts unchanged, because guessing
// slow on the first try would make every fresh list crawl.
func (p *Pool) Latency(px string) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.latency[px]
}

// ScaleTimeout stretches a configured timeout for the endpoint in use.
//
// The factor is the measured latency over the reference, so a proxy that needs
// 12 s where a good one needs 3 s gets four times the budget — and never more
// than ScaleMax, or one dying endpoint would hold up the queue. An unmeasured
// endpoint gets the configured value unchanged: guessing slow on the first try
// would make every fresh list crawl.
func (p *Pool) ScaleTimeout(px string, base time.Duration) time.Duration {
	lat := p.Latency(px)
	if lat <= 0 || base <= 0 {
		return base
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	scale := float64(lat) / float64(p.opts.LatencyRef)
	switch {
	case scale < p.opts.ScaleMin:
		scale = p.opts.ScaleMin
	case scale > p.opts.ScaleMax:
		scale = p.opts.ScaleMax
	}
	return time.Duration(float64(base) * scale)
}

// NoteLatency records one request step that went through, so the next timeout
// for this endpoint is sized to it.
func (p *Pool) NoteLatency(px string, d time.Duration) {
	if d <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.noteLatencyLocked(px, d)
}

func (p *Pool) noteLatencyLocked(px string, d time.Duration) {
	if prev, ok := p.latency[px]; ok && p.latSamples[px] > 0 {
		p.latency[px] = time.Duration(ewmaAlpha*float64(d) + (1-ewmaAlpha)*float64(prev))
	} else {
		p.latency[px] = d
	}
	p.latSamples[px]++
}

// NoteSlow records a step that ran into its own deadline. It is deliberately not
// a failure: the endpoint did not break, it was just late, and charging it would
// retire the only proxy that still works.
//
// The sample is pushed up to at least the budget it blew, because that is a known
// lower bound on what the link needs, which makes the very next attempt wait long
// enough to succeed.
//
// Direct mode is a real route that can be slow — the machine's own VPN hop, say —
// so it gets a measurement slot too. There is nothing to fail or retire there,
// but there is plenty to wait longer for, and without this a slow direct route
// would burn every attempt against the same too-small budget and never learn.
func (p *Pool) NoteSlow(px string, spent, budget time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if spent > budget {
		spent = budget
	}
	p.noteLatencyLocked(px, spent)
}

// Pick returns a usable proxy, or "" when none are configured.
//
// The choice stays random: WIPO scores the source IP, so a collector that always
// preferred the fastest endpoint would hammer one address until it got blocked.
// Measured-slow endpoints are weighted down rather than dropped, which keeps the
// rotation spread across the list while steering around the known-bad ones.
func (p *Pool) Pick() string {
	if len(p.proxies) == 0 {
		return ""
	}
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()

	usable := p.usableLocked(now)
	if len(usable) == 0 {
		// Everything is cooling down: take the endpoint that unparkes soonest.
		var coolest string
		var soonest time.Duration
		for _, px := range p.proxies {
			if until, ok := p.cooldownUntil[px]; ok {
				if left := until.Sub(now); left > 0 && (coolest == "" || left < soonest) {
					coolest, soonest = px, left
				}
			}
		}
		if coolest != "" {
			return coolest
		}
		// Everything is over the fail budget: reset it and try again.
		clear(p.fails)
		if usable = p.usableLocked(now); len(usable) == 0 {
			return p.proxies[rand.Intn(len(p.proxies))]
		}
	}

	total := 0.0
	weights := make([]float64, len(usable))
	for i, px := range usable {
		w := 1.0
		if lat := p.latency[px]; lat > 0 && lat > time.Duration(float64(p.opts.LatencyRef)*slowFactor) {
			w = 0.5
		}
		// A proxy that has failed is still worth trying, less so.
		w *= 1 / (1 + p.fails[px])
		weights[i] = w
		total += w
	}
	target := rand.Float64() * total
	for i, w := range weights {
		target -= w
		if target <= 0 {
			return usable[i]
		}
	}
	return usable[len(usable)-1]
}

// usableLocked returns the endpoints that are under budget and not cooling down,
// clearing cooldowns that have expired on the way past. Also periodically
// purges expired cooldown entries to keep the map from growing unbounded in
// long runs where Pick() may not be called for a while.
func (p *Pool) usableLocked(now time.Time) []string {
	var live []string
	for _, px := range p.proxies {
		if p.fails[px] >= float64(p.opts.MaxFails) {
			continue
		}
		if until, ok := p.cooldownUntil[px]; ok {
			if until.After(now) {
				continue
			}
			delete(p.cooldownUntil, px)
		}
		live = append(live, px)
	}
	// Periodic purge: if the cooldown map is significantly larger than the
	// proxy list, scan and delete expired entries. This prevents unbounded
	// growth when Pick() is called infrequently (e.g., long idle periods).
	if len(p.cooldownUntil) > len(p.proxies)*2 {
		for px, until := range p.cooldownUntil {
			if !until.After(now) {
				delete(p.cooldownUntil, px)
			}
		}
	}
	return live
}

// Fail charges a full failure: the endpoint served junk, refused the tunnel or
// is plainly blocked.
func (p *Pool) Fail(px string) {
	if px == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fails[px]++
}

// SoftFail charges half a failure, for transport hiccups on a slow link
// (timeout, reset). The proxy answered, just late — a slow proxy must not be
// rotated out like a dead one.
func (p *Pool) SoftFail(px string) {
	if px == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fails[px] += 0.5
}

// ObserveAttempt records one finished attempt duration, so Summary can show which
// endpoint is slow and not merely alive. This covers the whole attempt, paging
// included, so it measures work rather than the link; use NoteLatency for that.
func (p *Pool) ObserveAttempt(px string, d time.Duration) {
	if px == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.total[px] += d
	p.attempts[px]++
}

// Success clears the endpoint's failure counter.
func (p *Pool) Success(px string) {
	if px == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.fails, px)
	p.oks[px]++
}

// CoolDown parks the endpoint for a while after it served a block page.
func (p *Pool) CoolDown(px string) {
	if px == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cooldownUntil[px] = time.Now().Add(p.opts.Cooldown)
	p.cools[px]++
}

// Summary renders one line per proxy with the ok/fail/cooled counters, the mean
// attempt duration and the measured step latency that timeouts are built from.
func (p *Pool) Summary() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.proxies) == 0 {
		return " (no proxies configured, direct mode)"
	}
	var b strings.Builder
	for _, px := range p.proxies {
		avg := time.Duration(0)
		if n := p.attempts[px]; n > 0 {
			avg = p.total[px] / time.Duration(n)
		}
		lat := p.latency[px]
		verdict := "unmeasured"
		switch {
		case lat == 0:
		case lat > time.Duration(float64(p.opts.LatencyRef)*slowFactor):
			verdict = "SLOW"
		case lat < p.opts.LatencyRef/2:
			verdict = "fast"
		}
		fmt.Fprintf(&b, "\n  %s ok=%d fail=%.1f cool=%d attempts=%d avg=%s step=%s %s",
			px, p.oks[px], p.fails[px], p.cools[px], p.attempts[px], avg.Round(time.Millisecond),
			lat.Round(time.Millisecond), verdict)
	}
	return b.String()
}
