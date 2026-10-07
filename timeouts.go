package main

import (
	"log"
	"time"
)

// Timeouts in this file are not constants, and that is the point.
//
// A free proxy that answers in 400 ms and one that answers in 25 s are both
// "working" to any liveness check, but a fixed two-minute budget is right for
// the first and a guaranteed failure for the second. Since WIPO only answers
// through a proxy, the budget has to follow the link: these helpers ask the
// pool how slow the endpoint in use actually is and scale accordingly.
//
// The scaling itself lives in proxy.Pool, so the pool and the worker cannot
// disagree about what a given measurement means.

// navTimeout is the budget for one page load on the endpoint in use.
func (w *worker) navTimeout() time.Duration {
	return w.deps.Proxy.ScaleTimeout(w.proxy, time.Duration(w.deps.Cfg.NavTimeoutMS)*time.Millisecond)
}

// resultsTimeout is the budget for waiting on the results grid to appear or to
// change, on the endpoint in use.
func (w *worker) resultsTimeout() time.Duration {
	return w.deps.Proxy.ScaleTimeout(w.proxy, time.Duration(w.deps.Cfg.ResultsTimeoutMS)*time.Millisecond)
}

// noteStall records a step that ran long or ran out of time, so the next
// attempt on this proxy waits long enough to succeed.
//
// It is deliberately not a failure. The endpoint did not break, it was late,
// and charging it would retire the only proxy that still reaches WIPO from
// here. What it does get is a raised latency sample: the budget it just blew
// is a known lower bound on what the link needs.
func (w *worker) noteStall(what string, start time.Time, budget time.Duration, err error) {
	spent := time.Since(start)
	if spent < budget/lateFraction {
		return
	}
	w.deps.Proxy.NoteSlow(w.proxy, spent, budget)
	reason := "slow"
	if err != nil {
		reason = err.Error()
	}
	scale := w.deps.Proxy.ScaleTimeout(w.proxy, budget)
	log.Printf("worker %d: %s took %s on %s (budget %s, %s) — next budget there is %s",
		w.id, what, spent.Round(time.Millisecond), w.proxyLabel(), budget.Round(time.Millisecond),
		truncate(reason, 60), scale.Round(time.Millisecond))
}

// proxyLabel names the endpoint in use, or says "direct", so a log line about a
// timeout still says which connection it was about.
func (w *worker) proxyLabel() string {
	if w.proxy == "" {
		return "direct"
	}
	return w.proxy
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
