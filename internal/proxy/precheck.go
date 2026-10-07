package proxy

import (
	"crypto/tls"
	"log"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// ProbeOptions configures the up-front health sweep.
type ProbeOptions struct {
	// URL is fetched through every proxy. Any status at all proves the tunnel.
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
	Latency time.Duration
	Err     error
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
			dur, err := reachable(px, opts.URL, opts.Timeout)
			results[i] = Probe{Proxy: px, Latency: dur, Err: err}
		}(i, px)
	}
	wg.Wait()
	return results
}

// Seed records the precheck latency for every endpoint that answered, so the
// first attempt is already sized to the link it runs over.
func (p *Pool) Seed(probes []Probe) {
	for _, pr := range probes {
		if pr.OK() && pr.Latency > 0 {
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

// reachable checks one proxy. HTTP(S) proxies get a real request through
// net/http.ProxyURL — any response status at all proves the tunnel works,
// including the "blocked" page we would rather see than a timeout. SOCKS
// proxies are only port-probed (net/http cannot route through them here), which
// still rejects the listeners that are simply down.
//
// A SOCKS measurement is a TCP handshake and an HTTP one is a full request, so
// the two are not directly comparable. The caller treats them as a first hint
// about which end of the list to trust, and the pool overwrites each sample as
// real page loads come in.
func reachable(px, probeURL string, timeout time.Duration) (time.Duration, error) {
	u, err := url.Parse(px)
	if err != nil {
		return 0, err
	}
	switch u.Scheme {
	case "socks", "socks4", "socks4a", "socks5", "socks5h":
		start := time.Now()
		conn, err := net.DialTimeout("tcp", u.Host, timeout)
		if err != nil {
			return time.Since(start), err
		}
		_ = conn.Close()
		return time.Since(start), nil
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
			return dur, err
		}
		_ = resp.Body.Close()
		return dur, nil
	}
}
