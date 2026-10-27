package proxy

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// probeTarget is what every probe in these tests asks for. The HTTP cases never
// reach it — the request is served by the fake proxy below — and the SOCKS case
// only ever dials its host:port.
const probeTarget = "http://www3.wipo.int/madrid/monitor/en/"

// fakeProxy is an HTTP proxy in the only sense the probe needs: it accepts the
// absolute-URI request a client sends through a proxy and answers with whatever
// status the test wants. That status is the whole subject here — a 502 from an
// endpoint used to be counted as alive.
func fakeProxy(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A tunnel that fetches the page is a tunnel: measured, verified, and the
// latency seeded from it.
func TestHTTPProxyMustAnswer200(t *testing.T) {
	srv := fakeProxy(t, http.StatusOK, strings.Repeat("<html>madrid monitor</html>", 40))
	latency, portOnly, err := reachable("http://"+srv.Listener.Addr().String(), probeTarget, 5*time.Second)
	if err != nil {
		t.Fatalf("reachable(200 proxy) = %v, want nil", err)
	}
	if portOnly {
		t.Error("an HTTP proxy was reported as port-only: it served a real response")
	}
	if latency <= 0 {
		t.Errorf("latency = %s, want a measurement to seed the pool from", latency)
	}
}

// An endpoint that cannot reach the site is not alive, whatever it answers
// with. Accepting these is what put broken proxies in front of the run's first
// attempts of the morning. 429 is the exception: it is the site rate-limiting,
// not the tunnel failing, so the endpoint stays usable.
func TestHTTPProxyFailuresAreDropped(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"bad gateway": {http.StatusBadGateway, "<html>502 Bad Gateway</html>"},
		"proxy auth":  {http.StatusProxyAuthRequired, "<html>407 Proxy Authentication Required</html>"},
		"forbidden":   {http.StatusForbidden, "<html>403 Forbidden</html>"},
		"not found":   {http.StatusNotFound, "<html>404</html>"},
		"server err":  {http.StatusInternalServerError, "boom"},
	} {
		srv := fakeProxy(t, tc.status, tc.body)
		if _, _, err := reachable("http://"+srv.Listener.Addr().String(), probeTarget, 5*time.Second); err == nil {
			t.Errorf("%s: reachable returned nil, want the endpoint dropped", name)
		}
	}

	rated := fakeProxy(t, http.StatusTooManyRequests, "slow down")
	if _, _, err := reachable("http://"+rated.Listener.Addr().String(), probeTarget, 5*time.Second); err != nil {
		t.Errorf("a rate-limited endpoint was dropped: %v — the tunnel works, the site is the one saying no", err)
	}
}

// listener opens a TCP port that speaks nothing, and closes it on the way out.
func listener(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	// Accept and drop, so a client's connect always completes quickly.
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return l.Addr().String()
}

// The failure this check exists for: a SOCKS server whose port is open but
// which refuses or never completes a tunnel used to pass the precheck in 1 ms
// and then fail every request at runtime.
func TestSOCKS5IsCheckedThroughTheTunnel(t *testing.T) {
	addr := listener(t)
	if _, portOnly, err := reachable("socks5://"+addr, probeTarget, 3*time.Second); err == nil {
		t.Error("a port that speaks no SOCKS was accepted as a working proxy")
	} else if portOnly {
		t.Error("the SOCKS5 probe fell back to a port check; the tunnel went unverified")
	}

	// A closed port is a closed port.
	if _, _, err := reachable("socks5://127.0.0.1:1", probeTarget, time.Second); err == nil {
		t.Error("a closed SOCKS port was accepted")
	}
}

// SOCKS4 has no client in the toolkit, so it keeps the port probe — flagged, so
// neither its health nor its speed is overstated.
func TestSOCKS4IsReportedAsPortOnly(t *testing.T) {
	addr := listener(t)
	latency, portOnly, err := reachable("socks4://"+addr, probeTarget, 3*time.Second)
	if err != nil {
		t.Fatalf("reachable(socks4 on an open port) = %v, want nil", err)
	}
	if !portOnly {
		t.Error("a SOCKS4 probe should report port-only: nothing verified the tunnel")
	}
	if latency <= 0 {
		t.Errorf("latency = %s, want the connect time logged", latency)
	}
	if _, _, err := reachable("socks4://127.0.0.1:1", probeTarget, time.Second); err == nil {
		t.Error("a closed SOCKS4 port was accepted")
	}
}

// A port-only probe must never become the pool's idea of a fast route: a TCP
// handshake is not a page load, and seeding one is how a dead endpoint ends up
// with the smallest timeout scale in the list.
func TestSeedSkipsPortOnlyProbes(t *testing.T) {
	p := New([]string{"socks4://open:1080"}, DefaultOptions())
	p.Seed([]Probe{
		{Proxy: "socks4://open:1080", Latency: time.Millisecond, PortOnly: true},
	})
	if got := p.Latency("socks4://open:1080"); got != 0 {
		t.Errorf("Latency = %s, want 0 — a port probe seeded the pool", got)
	}

	p.Seed([]Probe{{Proxy: "socks4://open:1080", Latency: 1500 * time.Millisecond}})
	if got := p.Latency("socks4://open:1080"); got != 1500*time.Millisecond {
		t.Errorf("Latency = %s, want 1.5s — a real measurement must seed", got)
	}
}

// Alive is what decides which endpoints the run may use: a failed probe, in any
// of its flavours, has to be out.
func TestAliveDropsFailuresAndKeepsBothKindsOfSuccess(t *testing.T) {
	good := "http://good:1"
	probes := []Probe{
		{Proxy: good, Latency: time.Second},
		{Proxy: "socks4://portonly:1", Latency: time.Millisecond, PortOnly: true},
		{Proxy: "socks5://dead:1", Err: errors.New("refused")},
	}
	alive := Alive(probes)
	if len(alive) != 2 {
		t.Fatalf("Alive = %v, want the two that answered", alive)
	}
	if alive[0] != good || alive[1] != "socks4://portonly:1" {
		t.Errorf("Alive = %v, want input order preserved", alive)
	}
}

// The proxy URL is parsed, not concatenated: a malformed endpoint must fail the
// probe rather than be dialled as something else.
func TestReachableRejectsABadTarget(t *testing.T) {
	srv := fakeProxy(t, http.StatusOK, "ok")
	if _, _, err := reachable("http://"+srv.Listener.Addr().String(), "://nope", 2*time.Second); err == nil {
		t.Error("a malformed probe URL was accepted")
	}
	if _, _, err := reachable("http://%zz", probeTarget, time.Second); err == nil {
		t.Error("a malformed proxy URL was accepted")
	}
}
