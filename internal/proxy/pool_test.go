package proxy

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// poolOpts is a short way to build a pool in the tests below.
func poolOpts(maxFails int, cooldown time.Duration) Options {
	o := DefaultOptions()
	o.MaxFails = maxFails
	o.Cooldown = cooldown
	return o
}

func TestNewDropsDuplicatesAndBlanks(t *testing.T) {
	p := New([]string{
		"http://a:1", "http://a:1", "", "  ", "socks5://b:2", " http://a:1 ",
	}, poolOpts(3, time.Minute))
	if p.Len() != 2 {
		t.Fatalf("pool has %d proxies (%q), want 2 — a scraped list repeats its working entries", p.Len(), p.proxies)
	}
	if p.proxies[0] != "http://a:1" || p.proxies[1] != "socks5://b:2" {
		t.Errorf("order changed: %q", p.proxies)
	}
}

func TestPickIsDirectWithoutProxies(t *testing.T) {
	p := New(nil, poolOpts(3, time.Minute))
	if got := p.Pick(); got != "" {
		t.Errorf("Pick = %q, want \"\" (direct mode)", got)
	}
	if got := p.Summary(); got == "" {
		t.Error("Summary is empty in direct mode, want the note that there is no proxy")
	}
}

func TestFailBudgetRetiresAnEndpoint(t *testing.T) {
	p := New([]string{"http://good:1", "http://bad:2"}, poolOpts(2, time.Minute))

	for i := 0; i < 2; i++ {
		p.Fail("http://bad:2")
	}
	// The budget is spent, so the endpoint drops out of the rotation entirely.
	for i := 0; i < 20; i++ {
		if got := p.Pick(); got != "http://good:1" {
			t.Fatalf("Pick = %q, want the healthy endpoint", got)
		}
	}

	// A soft failure only charges half, which is the whole point: a slow proxy
	// must not be retired like a dead one.
	soft := New([]string{"http://slow:1"}, poolOpts(2, time.Minute))
	soft.SoftFail("http://slow:1")
	soft.SoftFail("http://slow:1") // 0.5 + 0.5, still under the budget
	if got := soft.Pick(); got != "http://slow:1" {
		t.Errorf("Pick = %q, want the endpoint that is only a little late", got)
	}
	soft.SoftFail("http://slow:1")
	if got := soft.Pick(); got != "http://slow:1" {
		t.Errorf("Pick = %q after crossing the budget, want the pool to keep the run alive", got)
	}
}

func TestSuccessClearsTheFailureCounter(t *testing.T) {
	p := New([]string{"http://flaky:1"}, poolOpts(3, time.Minute))
	p.Fail("http://flaky:1")
	p.Fail("http://flaky:1")
	p.Success("http://flaky:1")
	p.Fail("http://flaky:1")
	// Without Success the third failure would have retired it.
	if got := p.Pick(); got != "http://flaky:1" {
		t.Errorf("Pick = %q, want the endpoint back in the rotation after a success", got)
	}
}

func TestCoolDownParksTheEndpoint(t *testing.T) {
	p := New([]string{"http://blocked:1", "http://ok:2"}, poolOpts(5, time.Minute))
	p.CoolDown("http://blocked:1")
	for i := 0; i < 20; i++ {
		if got := p.Pick(); got != "http://ok:2" {
			t.Fatalf("Pick = %q, want the endpoint that did not serve a block page", got)
		}
	}
}

func TestPickNeverStops(t *testing.T) {
	// Everything is cooling down. A run that stopped here would silently throw
	// away the rest of the list, so the soonest to unblock is handed out instead.
	p := New([]string{"http://a:1", "http://b:2"}, poolOpts(5, time.Minute))
	p.CoolDown("http://a:1")
	p.CoolDown("http://b:2")
	if got := p.Pick(); got == "" {
		t.Error("Pick = \"\" although the list is not empty")
	}
	// The same holds once every endpoint is over budget.
	p2 := New([]string{"http://a:1"}, poolOpts(1, time.Minute))
	p2.Fail("http://a:1")
	if got := p2.Pick(); got != "http://a:1" {
		t.Errorf("Pick = %q, want the budget to be reset rather than the run to end", got)
	}
}

func TestSummaryReportsEveryEndpoint(t *testing.T) {
	p := New([]string{"http://a:1", "http://b:2"}, poolOpts(2, time.Minute))
	p.ObserveAttempt("http://a:1", 900*time.Millisecond)
	p.ObserveAttempt("http://a:1", 1100*time.Millisecond)
	p.Success("http://a:1")
	p.Fail("http://b:2")
	p.CoolDown("http://b:2")

	got := p.Summary()
	for _, want := range []string{"http://a:1", "ok=1", "fail=1.0", "cool=1", "attempts=2", "avg=1s"} {
		if !strings.Contains(got, want) {
			t.Errorf("Summary is missing %q:\n%s", want, got)
		}
	}
}

func TestScaleTimeoutIsTheBaseForAnUnmeasuredProxy(t *testing.T) {
	p := New([]string{"http://a:1"}, DefaultOptions())
	base := 120 * time.Second
	// Guessing slow on a fresh list would make every first attempt crawl, so an
	// endpoint nobody has measured gets the configured value unchanged.
	if got := p.ScaleTimeout("http://a:1", base); got != base {
		t.Errorf("ScaleTimeout = %s, want the configured %s", got, base)
	}
	if got := p.ScaleTimeout("", base); got != base {
		t.Errorf("ScaleTimeout in direct mode = %s, want %s", got, base)
	}
}

// Direct mode is a route like any other and can be slow — the machine's own VPN
// hop, for one. Nothing about it can be failed or retired, but it can be waited
// for. Without a measurement slot for it, a slow direct route burns every attempt
// against the same too-small budget and never learns from the first timeout.
func TestDirectModeLearnsItsBudgetFromATimeout(t *testing.T) {
	o := DefaultOptions()
	o.LatencyRef = 3 * time.Second
	p := New(nil, o)
	base := 180 * time.Second

	// Nothing measured yet: the configured budget stands.
	if got := p.ScaleTimeout("", base); got != base {
		t.Errorf("ScaleTimeout before any measurement = %s, want %s", got, base)
	}

	// A step that ran into its own deadline raises the next one.
	p.NoteSlow("", base, base)
	got := p.ScaleTimeout("", base)
	if got <= base {
		t.Errorf("ScaleTimeout after a blown %s budget = %s, want longer than %s", base, got, base)
	}
	if max := time.Duration(float64(base) * o.ScaleMax); got > max {
		t.Errorf("ScaleTimeout = %s, must stay within the configured ceiling %s", got, max)
	}

	// And a direct run still has nothing to fail, retire or summarise.
	p.Fail("")
	p.SoftFail("")
	p.CoolDown("")
	p.Success("")
	p.ObserveAttempt("", time.Minute)
	if s := p.Summary(); s == "" {
		t.Error("Summary is empty in direct mode, want the note that there is no proxy")
	}
}

func TestScaleTimeoutFollowsMeasuredLatency(t *testing.T) {
	o := DefaultOptions()
	o.LatencyRef = 3 * time.Second
	p := New([]string{"http://slow:1", "http://fast:1"}, o)
	p.NoteLatency("http://slow:1", 9*time.Second) // three times the reference
	p.NoteLatency("http://fast:1", time.Second)   // a third of it

	base := 120 * time.Second
	if got, want := p.ScaleTimeout("http://slow:1", base), 360*time.Second; got != want {
		t.Errorf("slow ScaleTimeout = %s, want %s", got, want)
	}
	// A fast link is never given less than the configured budget: Playwright
	// timeouts also cover the site's own render time, which has nothing to do
	// with the proxy.
	if got := p.ScaleTimeout("http://fast:1", base); got != base {
		t.Errorf("fast ScaleTimeout = %s, want at least the configured %s", got, base)
	}
}

func TestScaleTimeoutIsClamped(t *testing.T) {
	o := DefaultOptions()
	o.LatencyRef = time.Second
	o.ScaleMin = 1
	o.ScaleMax = 4
	p := New([]string{"http://dead:1"}, o)
	// A 10-minute step would mean a 120-minute budget without the ceiling, and
	// the whole run would sit behind one endpoint.
	p.NoteLatency("http://dead:1", 10*time.Minute)
	if got, want := p.ScaleTimeout("http://dead:1", 120*time.Second), 480*time.Second; got != want {
		t.Errorf("ScaleTimeout = %s, want the ceiling %s", got, want)
	}
}

func TestNoteSlowRaisesTheBudgetWithoutCharging(t *testing.T) {
	o := DefaultOptions()
	o.LatencyRef = 3 * time.Second
	p := New([]string{"http://slow:1"}, o)
	budget := 60 * time.Second
	// The attempt spent its whole budget without finishing: that is a known
	// lower bound on what the link needs, and it is not a failure.
	p.NoteSlow("http://slow:1", budget, budget)

	if fails := p.fails["http://slow:1"]; fails != 0 {
		t.Errorf("fails = %v, want 0 — being late is not being broken", fails)
	}
	if got := p.ScaleTimeout("http://slow:1", 60*time.Second); got <= 60*time.Second {
		t.Errorf("ScaleTimeout = %s, want more than the budget that was just blown", got)
	}
	// And it stays in the rotation.
	if got := p.Pick(); got != "http://slow:1" {
		t.Errorf("Pick = %q, want the only working endpoint to survive", got)
	}
}

func TestNoteSlowCapsTheSampleAtTheBudget(t *testing.T) {
	o := DefaultOptions()
	o.LatencyRef = time.Second
	o.ScaleMax = 10
	p := New([]string{"http://x:1"}, o)
	// A step that overran by an hour says the link needs at least the budget it
	// was given, not that it needs an hour.
	p.NoteSlow("http://x:1", time.Hour, 30*time.Second)
	if lat := p.Latency("http://x:1"); lat > time.Minute {
		t.Errorf("Latency = %s, want the sample capped near the budget", lat)
	}
}

func TestLatencyIsAnEWMA(t *testing.T) {
	p := New([]string{"http://a:1"}, DefaultOptions())
	p.NoteLatency("http://a:1", 10*time.Second)
	p.NoteLatency("http://a:1", time.Second)
	// A running mean would still be 5.5s here and would keep every future
	// budget too large long after the endpoint sped up.
	got := p.Latency("http://a:1")
	if got >= 9*time.Second {
		t.Errorf("Latency = %s, want the new fast sample to dominate", got)
	}
	if got <= time.Second {
		t.Errorf("Latency = %s, want the slow sample to still count for something", got)
	}
}

func TestSeedFeedsThePrecheckLatency(t *testing.T) {
	p := New([]string{"http://a:1", "http://b:2"}, DefaultOptions())
	p.Seed([]Probe{
		{Proxy: "http://a:1", Latency: 8 * time.Second},
		{Proxy: "http://b:2", Latency: 500 * time.Millisecond, Err: errors.New("connection refused")},
	})
	// The precheck is the only measurement available before the first page
	// load, so a lost sample is a timeout the first attempt would not survive.
	if got := p.Latency("http://a:1"); got != 8*time.Second {
		t.Errorf("Latency(a) = %s, want the precheck's 8s", got)
	}
	if got := p.Latency("http://b:2"); got != 0 {
		t.Errorf("Latency(b) = %s, want 0 — a failed probe is not a speed", got)
	}
}

func TestAliveAndLogProbesCoverBothOutcomes(t *testing.T) {
	probes := []Probe{
		{Proxy: "http://ok:1", Latency: time.Second},
		{Proxy: "http://dead:2", Latency: 2 * time.Second, Err: errors.New("refused")},
	}
	got := Alive(probes)
	if len(got) != 1 || got[0] != "http://ok:1" {
		t.Errorf("Alive = %q, want only the endpoint that answered", got)
	}
	LogProbes(probes) // must not panic
}

func TestOptionsAreNormalised(t *testing.T) {
	// A config with zeroes must not divide by zero or retire every endpoint
	// instantly.
	p := New([]string{"http://a:1"}, Options{})
	if p.opts.MaxFails < 1 || p.opts.ScaleMin <= 0 || p.opts.ScaleMax < p.opts.ScaleMin {
		t.Fatalf("options not normalised: %+v", p.opts)
	}
	if p.ScaleTimeout("http://a:1", time.Second) <= 0 {
		t.Error("ScaleTimeout returned a non-positive budget for a normalised pool")
	}
	p.Fail("http://a:1")
	if p.Pick() != "http://a:1" {
		t.Error("a single failure retired the endpoint, want the default budget to hold")
	}
}

func TestSummaryFlagsASlowEndpoint(t *testing.T) {
	o := DefaultOptions()
	o.LatencyRef = time.Second
	p := New([]string{"http://a:1"}, o)
	p.NoteLatency("http://a:1", 30*time.Second)
	if got := p.Summary(); !strings.Contains(got, "SLOW") {
		t.Errorf("Summary does not flag the slow endpoint:\n%s", got)
	}
}
