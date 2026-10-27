package proxy

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"golang.org/x/net/proxy"
)

// ProbeOptions configures the up-front health sweep.
type ProbeOptions struct {
	// URL is fetched through every proxy. It has to come back with 200: a
	// tunnel that answers 407 or 502 is not a way in to the site, and treating
	// it as alive is what used to put a broken endpoint in front of the first
	// attempts of a run.
	URL string
	// Timeout caps one probe. It is deliberately short: this is a liveness
	// check, and anything that has not answered in this long is not going to
	// make the collector's own timeouts any better.
	Timeout time.Duration
	// Concurrency is how many probes are in flight at once.
	Concurrency int
}

// Probe is what the precheck learned about one endpoint.
type Probe struct {
	Proxy string
	// Latency is how long the probe took. It is kept rather than logged and
	// dropped, because it is the pool's first latency sample — the one that
	// sizes the very first attempt's timeouts before any page has been walked.
	// A port-only probe never seeds one: a TCP handshake is not a page load.
	Latency time.Duration
	// PortOnly marks a probe that only proved the proxy's own port is open —
	// SOCKS4, whose dialect this build has no client for. It counts as
	// reachable (a closed port is certainly dead) but says nothing about the
	// tunnel to the site.
	PortOnly bool
	Err      error
}

// OK reports whether the endpoint answered.
func (p Probe) OK() bool { return p.Err == nil }

// HealthCheck probes every proxy once, in parallel, and returns the results in
// the input order.
//
// The list is a dump of free public proxies, so most of it is dead on arrival.
// Probing up front is what keeps a worker from burning its five attempts on a
// closed port. The latency of every survivor comes back with it so the pool can
// start with a real speed measurement instead of guessing on the first query.
func HealthCheck(proxies []string, opts ProbeOptions) []Probe {
	if opts.Concurrency < 1 {
		opts.Concurrency = 8
	}
	results := make([]Probe, len(proxies))
	// Eight at a time by default: enough to sweep a long list in a fraction of
	// the time, not so many that the probe itself looks like an attack.
	sem := make(chan struct{}, opts.Concurrency)
	var wg sync.WaitGroup
	for i, px := range proxies {
		wg.Add(1)
		go func(i int, px string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			dur, portOnly, err := reachable(px, opts.URL, opts.Timeout)
			results[i] = Probe{Proxy: px, Latency: dur, PortOnly: portOnly, Err: err}
		}(i, px)
	}
	wg.Wait()
	return results
}

// Seed records the precheck latency for every endpoint that really fetched
// the page, so the first attempt is already sized to the link it runs over.
// Port-only probes are skipped: a handshake to the proxy itself is not a
// measurement of the route to the site, and seeding one made a dead SOCKS
// endpoint look like the fastest thing in the list.
func (p *Pool) Seed(probes []Probe) {
	for _, pr := range probes {
		if pr.OK() && !pr.PortOnly && pr.Latency > 0 {
			p.NoteLatency(pr.Proxy, pr.Latency)
		}
	}
}

// LogProbes writes one line per endpoint, the dead ones included: which of a
// hundred scraped proxies failed and how is the only way to judge the list.
func LogProbes(probes []Probe) {
	for _, pr := range probes {
		if pr.Err != nil {
			log.Printf("proxy precheck %s: FAIL after %s: %v", pr.Proxy, pr.Latency.Round(time.Millisecond), pr.Err)
			continue
		}
		if pr.PortOnly {
			log.Printf("proxy precheck %s: port open in %s — NOT verified through the proxy (no client for this dialect)",
				pr.Proxy, pr.Latency.Round(time.Millisecond))
			continue
		}
		log.Printf("proxy precheck %s: ok in %s", pr.Proxy, pr.Latency.Round(time.Millisecond))
	}
}

// Alive filters the probes down to the endpoints that answered.
func Alive(probes []Probe) []string {
	out := make([]string, 0, len(probes))
	for _, pr := range probes {
		if pr.OK() {
			out = append(out, pr.Proxy)
		}
	}
	return out
}

// reachable checks one proxy by actually using it the way the collector will.
//
// HTTP(S) proxies get a real request through net/http.ProxyURL and have to come
// back with 200 — a 407 or a 502 is an endpoint that cannot reach the site, and
// accepting it put broken proxies in front of the run's first attempts.
//
// SOCKS5 gets the same treatment through a real dial and an HTTP request over
// it, which is the check that was missing: a bare TCP handshake to the proxy's
// own port passed an endpoint that then refused every tunneled connection at
// runtime (measured: "ok in 1ms" for a SOCKS server that answered nothing).
//
// SOCKS4 has no client in the standard toolkit, so it keeps the port probe —
// reported as port-only, so neither its latency nor its health is overstated.
//
// It returns how long the probe took, whether it was only a port probe, and why
// the endpoint should be dropped.
func reachable(px, probeURL string, timeout time.Duration) (time.Duration, bool, error) {
	u, err := url.Parse(px)
	if err != nil {
		return 0, false, err
	}
	switch u.Scheme {
	case "socks5", "socks5h":
		return probeSOCKS(u, probeURL, timeout)
	case "socks", "socks4", "socks4a":
		start := time.Now()
		conn, err := net.DialTimeout("tcp", u.Host, timeout)
		if err != nil {
			return time.Since(start), true, err
		}
		_ = conn.Close()
		return time.Since(start), true, nil
	default: // http, https
		tr := &http.Transport{
			Proxy:           http.ProxyURL(u),
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
		defer tr.CloseIdleConnections()
		client := &http.Client{Transport: tr, Timeout: timeout}
		start := time.Now()
		resp, err := client.Get(probeURL)
		dur := time.Since(start)
		if err != nil {
			return dur, false, err
		}
		// The body is read, not just the headers: the first byte proves the
		// tunnel, the rest is the latency the pool sizes its budgets from.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<20))
		_ = resp.Body.Close()
		if !probeStatusOK(resp.StatusCode) {
			return dur, false, fmt.Errorf("HTTP %s through the proxy", resp.Status)
		}
		return dur, false, nil
	}
}

// probeStatusOK is the bar an endpoint has to clear to be used. 200 is the
// only success; 429 is accepted because it proves the tunnel works and the
// *site* is the one saying no — an endpoint is not broken for being rate
// limited. Everything else (407, 403, 5xx) is an endpoint that cannot carry
// this run's traffic.
func probeStatusOK(status int) bool {
	return status == http.StatusOK || status == http.StatusTooManyRequests
}

// probeSOCKS fetches probeURL by dialing through a SOCKS5 proxy: connect to the
// proxy, ask it to reach the site, then speak HTTP over the tunnel. Both halves
// have to work, which is the entire point — the port being open says nothing
// about either, and the domain name is handed to the proxy to resolve, exactly
// as the collector's own traffic is.
func probeSOCKS(proxyURL *url.URL, probeURL string, timeout time.Duration) (time.Duration, bool, error) {
	target, err := url.Parse(probeURL)
	if err != nil {
		return 0, false, err
	}
	host := target.Hostname()
	port := target.Port()
	if port == "" {
		if target.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	dialer, err := proxy.FromURL(proxyURL, timeoutDialer{d: timeout})
	if err != nil {
		// An unsupported dialect falls back to the port probe rather than
		// dropping the endpoint: a closed port is certainly dead, and the log
		// says plainly that the tunnel was not checked.
		start := time.Now()
		conn, derr := net.DialTimeout("tcp", proxyURL.Host, timeout)
		if derr != nil {
			return time.Since(start), true, derr
		}
		_ = conn.Close()
		return time.Since(start), true, nil
	}

	start := time.Now()
	conn, err := dialer.Dial("tcp", net.JoinHostPort(host, port))
	if err != nil {
		return time.Since(start), false, err
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return time.Since(start), false, err
	}
	if target.Scheme == "https" {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: host, InsecureSkipVerify: true})
		if err := tlsConn.Handshake(); err != nil {
			return time.Since(start), false, err
		}
		conn = tlsConn
	}
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUser-Agent: %s\r\nAccept: */*\r\nConnection: close\r\n\r\n",
		target.RequestURI(), target.Host, probeUA)
	if _, err := io.WriteString(conn, req); err != nil {
		return time.Since(start), false, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodGet})
	if err != nil {
		return time.Since(start), false, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<20))
	if !probeStatusOK(resp.StatusCode) {
		return time.Since(start), false, fmt.Errorf("HTTP %s through the proxy", resp.Status)
	}
	return time.Since(start), false, nil
}

// probeUA is what the probe says it is. A blank user agent on a health check is
// itself a suspicious thing to be.
const probeUA = "wipos-precheck/1"

// timeoutDialer bounds the connection a SOCKS probe makes to the proxy itself.
// x/net/proxy's default forward dialer has no deadline, and a probe that hangs
// for a minute per endpoint turns a 37-entry list into a minute of startup.
type timeoutDialer struct{ d time.Duration }

func (t timeoutDialer) Dial(network, addr string) (net.Conn, error) {
	return net.DialTimeout(network, addr, t.d)
}
