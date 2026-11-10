package main

import (
	"testing"

	"wipos/internal/config"
	"wipos/internal/proxy"
)

// replayWorker builds a worker that looks exactly like one mid-run in replay
// mode: no browser open, no HTTP session yet. It is enough to drive the
// rotation paths, which is where the run spent its restarts.
func replayWorker(t *testing.T, proxies []string, current string) *worker {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Mode = "replay"
	return &worker{
		id: 1,
		deps: &Deps{
			Cfg:   cfg,
			Proxy: proxy.New(proxies, proxy.DefaultOptions()),
		},
		proxy: current,
	}
}

// The regression this pins: rotation in replay mode used to go through
// launch(), so every 403 closed and relaunched Chromium — 343 launches in a
// 10.5 h run whose only browser was opened 17 times by the fallback. A rotation
// must now only drop the session and the browser, never start one.
func TestSwitchProxyInReplayDropsTheSessionWithoutRelaunching(t *testing.T) {
	const (
		a = "http://a:1"
		b = "http://b:2"
	)
	w := replayWorker(t, []string{a, b}, a)
	// Take the current endpoint out of the running so the pick is the other
	// one and not a coin flip.
	w.deps.Proxy.CoolDown(a)

	w.switchProxy()

	if w.proxy != b {
		t.Fatalf("proxy = %q, want %q — the rotation did not move", w.proxy, b)
	}
	if w.rc != nil {
		t.Error("the HTTP session survived the rotation; it is bound to the old IP")
	}
	// A browser started here would be the bug: nothing in replay asks for one
	// except the fallback, and a relaunch is what the 343 starts were.
	if w.ctx != nil || w.page != nil {
		t.Error("a rotation opened a browser in replay mode")
	}
	if w.relaunchOnRotation() {
		t.Error("replay reports that a rotation should relaunch the browser")
	}
}

// The other half of the same coin: browser mode still rotates the browser,
// because there the profile and the cookies are the thing being moved.
func TestBrowserModeRelaunchesOnRotation(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Mode = "browser"
	w := &worker{deps: &Deps{Cfg: cfg}}
	if !w.relaunchOnRotation() {
		t.Error("browser mode must relaunch on rotation")
	}
}

// One endpoint, already in use: there is nowhere to rotate to, and the worker
// must not drop a perfectly good session over a failed pick.
func TestSwitchProxyKeepsTheSessionWhenThePickIsTheSame(t *testing.T) {
	const only = "http://a:1"
	w := replayWorker(t, []string{only}, only)

	w.switchProxy()

	if w.proxy != only {
		t.Errorf("proxy = %q, want %q", w.proxy, only)
	}
}

// After blockStreak refusals in a row the endpoint and the session are written
// off. In replay there is no profile worth wiping — the fingerprint only
// supplies the user agent — so the worker must not start a browser to wipe one.
func TestOnBlockHitInReplayDropsEndpointAndSession(t *testing.T) {
	const (
		a = "http://a:1"
		b = "http://b:2"
	)
	w := replayWorker(t, []string{a, b}, a)
	w.blockRuns = blockStreak - 1

	w.onBlockHit("some query")

	if w.blockRuns != blockStreak {
		t.Fatalf("blockRuns = %d, want %d", w.blockRuns, blockStreak)
	}
	if w.proxy != "" {
		t.Errorf("proxy = %q, want empty so the next attempt picks a fresh endpoint", w.proxy)
	}
	if w.rc != nil {
		t.Error("the session survived a burnt endpoint")
	}
	if w.ctx != nil || w.page != nil {
		t.Error("a block streak opened a browser in replay mode")
	}
}

// Below the streak the endpoint keeps its warm session: a single block page is
// usually a one-off for that IP, not a burnt identity.
func TestOnBlockHitBelowStreakKeepsTheEndpoint(t *testing.T) {
	const a = "http://a:1"
	w := replayWorker(t, []string{a}, a)
	w.blockRuns = 0

	w.onBlockHit("some query")

	if w.blockRuns != 1 {
		t.Errorf("blockRuns = %d, want 1", w.blockRuns)
	}
	if w.proxy != a {
		t.Errorf("proxy = %q, want %q kept", w.proxy, a)
	}
}

// A cooled endpoint must not be handed straight back on the next pick while a
// healthy one is available — that is the whole point of cooling it down.
func TestRotationLeavesACooledEndpoint(t *testing.T) {
	const (
		a = "http://a:1"
		b = "http://b:2"
	)
	w := replayWorker(t, []string{a, b}, a)
	w.deps.Proxy.CoolDown(a)

	for i := 0; i < 20; i++ {
		if got := w.deps.Proxy.Pick(); got != b {
			t.Fatalf("Pick = %q on iteration %d, want the endpoint that is not cooling down", got, i)
		}
	}
}
