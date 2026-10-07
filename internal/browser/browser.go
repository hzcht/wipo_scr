// Package browser starts the browsers and keeps them looking like a browser.
//
// The collector opens a lot of pages through a rotating set of proxies, so the
// two things that matter here are: each worker keeps its own persistent
// Chromium profile (cookies, local storage and a pinned fingerprint survive
// attempts and restarts, exactly like a returning human), and nothing that the
// data does not need is loaded over the wire.
package browser

import (
	"encoding/json"
	"log"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	playwright "github.com/mxschmitt/playwright-go"
)

// UserAgents are the built-in identities. They must match the real engine:
// Playwright drives a recent Chromium, so only fresh desktop Chrome strings are
// shipped. An old Firefox or Safari string next to a Chrome engine is a mismatch
// signal on its own. Add more through the ua_file in the config.
//
// NOTE: no rand.Seed anywhere in this package, on purpose. Since Go 1.20 the
// global rand is auto-seeded; reseeding it with second-granularity time resets
// the sequence, and then every concurrent worker draws the same "random" values
// — three browsers with an identical pause pattern.
var UserAgents = []string{
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 11.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Windows NT 11.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36",
	"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36",
	"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36",
}

var locales = []string{"en-US", "en-GB", "en-CA", "en-AU", "en-DE", "en-NL"}

// LoadUserAgents appends user agents from a file to the built-in list and
// returns how many were added. It is called once from main, before the first
// browser starts, so the list is fixed for the whole run — a UA that changed
// halfway through would be a different machine claiming the same cookies.
//
// A missing file is not an error: the built-in list is a working set on its own.
func LoadUserAgents(path string) int {
	if path == "" {
		return 0
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("ua file %s: %v", path, err)
		}
		return 0
	}
	seen := make(map[string]struct{}, len(UserAgents)+16)
	for _, ua := range UserAgents {
		seen[ua] = struct{}{}
	}
	added := 0
	for _, line := range strings.Split(string(raw), "\n") {
		ua := strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if ua == "" || strings.HasPrefix(ua, "#") {
			continue
		}
		if _, dup := seen[ua]; dup {
			continue
		}
		seen[ua] = struct{}{}
		UserAgents = append(UserAgents, ua)
		added++
	}
	return added
}

// Fingerprint is the stable per-profile identity. A profile that keeps its UA,
// locale and viewport across restarts looks like the same person coming back;
// one that reshuffles on every launch looks like a farm.
type Fingerprint struct {
	UserAgent string `json:"user_agent"`
	Locale    string `json:"locale"`
	ViewportW int    `json:"viewport_w"`
	ViewportH int    `json:"viewport_h"`
}

// LoadOrGenFingerprint reuses the profile's pinned fingerprint or writes a new
// one. A storage failure is logged and never fails the launch.
func LoadOrGenFingerprint(profileDir string) Fingerprint {
	path := filepath.Join(profileDir, "fingerprint.json")
	if raw, err := os.ReadFile(path); err == nil {
		var fp Fingerprint
		if json.Unmarshal(raw, &fp) == nil && fp.UserAgent != "" && fp.ViewportW > 0 && fp.ViewportH > 0 {
			return fp
		}
	}
	fp := Fingerprint{
		UserAgent: UserAgents[rand.Intn(len(UserAgents))],
		Locale:    locales[rand.Intn(len(locales))],
		ViewportW: 1280 + rand.Intn(480),
		ViewportH: 800 + rand.Intn(280),
	}
	if raw, err := json.MarshalIndent(fp, "", "  "); err == nil {
		if werr := os.WriteFile(path, raw, 0644); werr != nil {
			log.Printf("fingerprint store %s: %v", path, werr)
		}
	}
	return fp
}

// DefaultArgs is the Chromium flag set: drop the automation tell, ignore
// certificate errors (half the free proxies present self-signed certs) and keep
// WebRTC from announcing the real IP next to a proxied one.
func DefaultArgs() []string {
	return []string{
		"--disable-blink-features=AutomationControlled",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-infobars",
		"--ignore-certificate-errors",
		"--force-webrtc-ip-handling-policy=disable_non_proxied_udp",
		// WIPO's monitor answers HTTP/2 with headers and then never finishes the
		// body: the page stays at 0 bytes of <body> until the navigation times
		// out. Measured on 2026-09-28 through a working proxy — over h1 the same
		// URL renders in seconds. There is nothing on this site to gain from h2.
		"--disable-http2",
	}
}

// blockedResourceTypes never carry result data. On a proxied run the image and
// font bytes of every page are the single biggest source of latency, and the
// grid's trademark thumbnails are decoration for us — the <td> already holds
// the text we parse.
var blockedResourceTypes = map[string]bool{
	"image": true,
	"media": true,
	"font":  true,
}

// blockedHosts are analytics, ad and consent vendors: pure overhead for a
// scraper, and a well-known per-IP bot signal.
var blockedHosts = []string{
	"google-analytics.com",
	"googletagmanager.com",
	"doubleclick.net",
	"analytics.",
	"amplitude",
	"hotjar",
	"segment.io",
	"segment.com",
	"intercom",
	"criteo",
	"taboola",
	"adservice",
	"facebook.net",
	"quantserve",
	"scorecardresearch",
	"newrelic",
	"sentry.io",
	"bugsnag",
	"fullstory",
	"mixpanel",
	"optimizely",
	"clarity.ms",
	"onetrust",
	"cookielaw",
	"usercentrics",
	"cloudflareinsights.com",
}

// blockedURLs is deliberately tiny. An earlier version of this list also blocked
// branddb.*, imageEditor/* and pdf-all, on the theory that a keyword search does
// not need them. That is wrong, and expensively so: those bundles are what build
// the search form. With them blocked, #simple_search_container stays empty,
// #AUTO_input is never created, the mode links never appear and jQuery.fn.solrManager
// is undefined — the page then looks healthy enough to scrape while producing
// nothing at all.
//
// What is left is only third-party chrome: Matomo analytics and the IP-portal
// web-component navbar, which is not part of the monitor. The monitor's own
// bundles are all load-bearing, and the site's <head> is synchronous, so the only
// real lever on its load time is not fetching anything rather than discarding
// something after it arrived.
//
// The patterns are lowercase: they are matched against a lowercased URL.
var blockedURLs = []string{
	"wipoanalytics.wipo.int", // Matomo page tracking
	"webcomponents.wipo.int", // the IP-portal navbar and its polyfill loader
}

// shouldBlock decides whether a request may be dropped.
func shouldBlock(route playwright.Route) bool {
	req := route.Request()
	return shouldBlockURL(req.URL(), req.ResourceType())
}

// shouldBlockURL is the decision itself, kept free of Playwright types so it can
// be tested against the real URLs the monitor asks for. Documents, scripts,
// stylesheets, XHR and fetch always pass: they are the page and the results
// grid itself. The exceptions are the site's own optional bundles, which are
// scripts and stylesheets by type and therefore have to be named one by one.
func shouldBlockURL(rawURL, resourceType string) bool {
	if blockedResourceTypes[resourceType] {
		return true
	}
	raw := strings.ToLower(rawURL)
	host := raw
	if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	for _, bad := range blockedHosts {
		if strings.Contains(host, bad) {
			return true
		}
	}
	for _, bad := range blockedURLs {
		if strings.Contains(raw, bad) {
			return true
		}
	}
	return false
}

// LaunchOptions configures one Chromium instance.
type LaunchOptions struct {
	ProfileDir  string
	Proxy       string // raw "http://…", "socks5://…"; "" = direct
	FP          Fingerprint
	Headless    bool
	DarkScheme  bool
	InitScripts []string
}

// LaunchPersistent starts Chromium on a persistent profile and returns its
// context and first page.
//
// The routes are installed before the first page exists so that no request
// escapes unfiltered, and service workers are blocked because their traffic
// never shows up in request interception — they would simply walk around the
// block list.
// installRoutes is the request filter. Blocking is not optional and has no
// bypass: routing a request that was never blocked is how the SPA breaks
// silently, and the monitoring build is new enough that a whitelist cannot be
// trusted across deploys. Only the two analytics hosts in shouldBlock are cut.
func installRoutes(ctx playwright.BrowserContext) error {
	return ctx.Route("**/*", func(r playwright.Route) {
		if shouldBlock(r) {
			_ = r.Abort()
			return
		}
		_ = r.Continue()
	})
}

func LaunchPersistent(pw *playwright.Playwright, opts LaunchOptions) (playwright.BrowserContext, playwright.Page, error) {
	lo := playwright.BrowserTypeLaunchPersistentContextOptions{
		Headless:          playwright.Bool(opts.Headless),
		Args:              DefaultArgs(),
		UserAgent:         playwright.String(opts.FP.UserAgent),
		Viewport:          &playwright.Size{Width: opts.FP.ViewportW, Height: opts.FP.ViewportH},
		Screen:            &playwright.Size{Width: 1920, Height: 1080},
		Locale:            playwright.String(opts.FP.Locale),
		TimezoneId:        playwright.String("Europe/London"),
		HasTouch:          playwright.Bool(false),
		DeviceScaleFactor: playwright.Float(1),
		ServiceWorkers:    playwright.ServiceWorkerPolicyBlock,
	}
	if opts.DarkScheme {
		lo.ColorScheme = playwright.ColorSchemeDark
	} else {
		lo.ColorScheme = playwright.ColorSchemeLight
	}
	if opts.Proxy != "" {
		lo.Proxy = &playwright.Proxy{Server: opts.Proxy}
	}

	ctx, err := pw.Chromium.LaunchPersistentContext(opts.ProfileDir, lo)
	if err != nil {
		return nil, nil, err
	}
	if err := installRoutes(ctx); err != nil {
		_ = ctx.Close()
		return nil, nil, err
	}
	for _, js := range opts.InitScripts {
		if err := ctx.AddInitScript(playwright.Script{Content: playwright.String(js)}); err != nil {
			_ = ctx.Close()
			return nil, nil, err
		}
	}
	page, err := ctx.NewPage()
	if err != nil {
		_ = ctx.Close()
		return nil, nil, err
	}
	traceRequests(ctx)
	return ctx, page, nil
}

// traceRequests logs the monitor traffic when WIPOS_TRACE is set. It exists to
// answer "did we even ask?" during live debugging: a search that never fires
// shows no terms.jsp and no select.jsp, which no amount of page inspection can
// distinguish from a submit that the site ignored.
func traceRequests(ctx playwright.BrowserContext) {
	if os.Getenv("WIPOS_TRACE") == "" {
		return
	}
	ctx.On("request", func(r playwright.Request) {
		u := r.URL()
		if strings.Contains(u, "madrid/monitor") {
			log.Printf("  -> %s %s", r.Method(), u)
		}
	})
}

// CurveMove steers the cursor to (x, y) through one or two jittered waypoints.
// Chained moves interpolate from the current position, so the path reads as one
// aimed motion instead of a jump.
func CurveMove(page playwright.Page, x, y float64) {
	for i := 0; i < 1+rand.Intn(2); i++ {
		f := 0.2 + rand.Float64()*0.5
		wx := clampPos(x*f + (rand.Float64()-0.5)*260)
		wy := clampPos(y*f + (rand.Float64()-0.5)*260)
		_ = page.Mouse().Move(wx, wy, playwright.MouseMoveOptions{Steps: playwright.Int(5 + rand.Intn(8))})
		page.WaitForTimeout(30 + rand.Float64()*90)
	}
	_ = page.Mouse().Move(clampPos(x), clampPos(y), playwright.MouseMoveOptions{Steps: playwright.Int(10 + rand.Intn(12))})
	page.WaitForTimeout(80 + rand.Float64()*220)
}

func clampPos(v float64) float64 {
	if v < 1 {
		return 1
	}
	return v
}

// HumanClick aims at an element and clicks it.
func HumanClick(page playwright.Page, selector string, timeoutMs float64) error {
	el, err := page.WaitForSelector(selector, playwright.PageWaitForSelectorOptions{
		State:   playwright.WaitForSelectorStateVisible,
		Timeout: playwright.Float(timeoutMs),
	})
	if err != nil {
		return err
	}
	return HumanClickHandle(page, el)
}

// HumanClickHandle curves to an already-found element and clicks it.
func HumanClickHandle(page playwright.Page, el playwright.ElementHandle) error {
	if box, err := el.BoundingBox(); err == nil && box != nil {
		HumanMoveTo(page, box)
	}
	return el.Click()
}

// HumanMoveTo curves onto the middle of an element, off-centre by up to 40% of
// its size — a click always exactly in the centre is its own kind of tell.
func HumanMoveTo(page playwright.Page, box *playwright.Rect) {
	x := box.X + box.Width/2 + (rand.Float64()-0.5)*box.Width*0.4
	y := box.Y + box.Height/2 + (rand.Float64()-0.5)*box.Height*0.4
	CurveMove(page, x, y)
}

// HumanType clicks the field and types character by character with per-keystroke
// delays, pausing longer on word breaks and punctuation. The caller is
// responsible for the field being empty — the site's own handlers fight a
// keyboard select-all on the reused results page and the clear never sticks.
func HumanType(page playwright.Page, selector, text string) error {
	if err := HumanClick(page, selector, 60000); err != nil {
		return err
	}
	page.WaitForTimeout(150 + rand.Float64()*350)
	for _, ch := range text {
		delay := 40 + rand.Float64()*80
		switch ch {
		case ' ', '-', '_':
			delay = 200 + rand.Float64()*400 // word break
		case '.', ',', ';', '!', '?', ':':
			delay = 150 + rand.Float64()*350
		}
		if err := page.Keyboard().Type(string(ch), playwright.KeyboardTypeOptions{Delay: playwright.Float(delay)}); err != nil {
			return err
		}
	}
	return nil
}

// HumanMouse moves the cursor around the viewport and scrolls a little, so the
// page is not navigated by a cursor that teleports from the address bar to the
// search box and never moves again.
func HumanMouse(page playwright.Page, viewportW, viewportH int) {
	if viewportW <= 0 || viewportH <= 0 {
		return
	}
	scrolls := 1 + rand.Intn(3)
	scrollDone := 0
	for i := 0; i < 8+rand.Intn(8); i++ {
		x := rand.Float64() * float64(viewportW)
		y := rand.Float64() * float64(viewportH)
		if err := page.Mouse().Move(x, y, playwright.MouseMoveOptions{Steps: playwright.Int(15 + rand.Intn(25))}); err != nil {
			return
		}
		if scrollDone < scrolls && rand.Float64() < 0.35 {
			_ = page.Mouse().Wheel(0, 120+rand.Float64()*380)
			scrollDone++
		}
		page.WaitForTimeout(120 + rand.Float64()*380)
	}
}

// RateGate enforces a minimum gap between events across all goroutines, plus
// jitter. One gate per proxy URL, so N proxies submit N searches per gap
// instead of the whole fleet funnelling through one.
type RateGate struct {
	mu     sync.Mutex
	last   time.Time
	gap    time.Duration
	jitter time.Duration
}

// GatePool hands out one RateGate per key (a proxy URL, "" for direct).
type GatePool struct {
	mu    sync.Mutex
	gap   time.Duration
	jit   time.Duration
	gates map[string]*RateGate
}

// NewGatePool builds a pool of per-key rate gates.
func NewGatePool(gap, jitter time.Duration) *GatePool {
	return &GatePool{gap: gap, jit: jitter, gates: map[string]*RateGate{}}
}

// Wait blocks until the gap since the previous event of the same key elapsed.
func (p *GatePool) Wait(key string) {
	if p.gap <= 0 && p.jit <= 0 {
		return
	}
	p.mu.Lock()
	g, ok := p.gates[key]
	if !ok {
		g = &RateGate{gap: p.gap, jitter: p.jit}
		p.gates[key] = g
	}
	p.mu.Unlock()
	g.wait()
}

func (g *RateGate) wait() {
	gap := g.gap + time.Duration(rand.Float64()*float64(g.jitter))
	for {
		g.mu.Lock()
		wait := gap - time.Since(g.last)
		if wait <= 0 {
			g.last = time.Now()
			g.mu.Unlock()
			return
		}
		g.mu.Unlock()
		time.Sleep(wait)
	}
}
