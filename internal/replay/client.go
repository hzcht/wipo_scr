package replay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"wipos/internal/site"
)

// SelectURL is the endpoint the site's JavaScript posts to. It answers with
// Solr JSON, not HTML — the HTML path is the browser's own rendering of it.
const SelectURL = "https://www3.wipo.int/madrid/monitor/jsp/select.jsp"

// ErrNotJSON marks a response that is not a result page at all: a block
// page, a proxy error, a login redirect. The caller falls back to the
// browser for these instead of retrying blindly.
var ErrNotJSON = errors.New("replay: response is not a result page")

// Client is one HTTP session against the monitor: a cookie jar, one user
// agent, and the select.jsp round trip. Sessions are cheap — one GET of the
// home page — and each worker owns exactly one, the way each browser profile
// owns its own cookies.
type Client struct {
	hc      *http.Client
	ua      string
	Timeout time.Duration
}

// NewClient builds a session client. The transport mirrors the browser's
// known-good path: environment proxy (empty means direct/VPN) and no HTTP/2,
// which the chromium launcher also disables. The client's Timeout field is
// kept at 0; per-request deadlines are enforced via context.WithTimeout in
// doRequest so they can be adjusted per attempt without recreating the transport.
func NewClient(ua string) *Client {
	jar, _ := cookiejar.New(&cookiejar.Options{})
	return &Client{
		hc: &http.Client{
			Jar:     jar,
			Timeout: 0,
			Transport: &http.Transport{
				Proxy:             http.ProxyFromEnvironment,
				ForceAttemptHTTP2: false,
			},
		},
		ua: ua,
	}
}

// SetProxy routes the session through raw — one endpoint from the pool, in the
// same form the browser launcher takes it. "" goes back to the environment
// (empty means direct, the normal case here). It replaces the transport, so it
// is meant to be called before the session is used, or on a rotation.
func (c *Client) SetProxy(raw string) error {
	var proxy func(*http.Request) (*url.URL, error)
	if raw == "" {
		proxy = http.ProxyFromEnvironment
	} else {
		u, err := url.Parse(raw)
		if err != nil {
			return fmt.Errorf("replay: proxy %q: %w", raw, err)
		}
		proxy = http.ProxyURL(u)
	}
	c.hc.Transport = &http.Transport{
		Proxy:             proxy,
		ForceAttemptHTTP2: false,
	}
	return nil
}

// doRequest executes req with the client's timeout context and returns the body.
// The context must stay alive until the body is fully read, so we read it here.
func (c *Client) doRequest(req *http.Request) ([]byte, error) {
	ctx, cancel := context.WithTimeout(req.Context(), c.Timeout)
	defer cancel()
	req = req.WithContext(ctx)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("replay: HTTP %s (%d bytes)", resp.Status, len(raw))
	}
	return raw, nil
}

// EnsureSession fetches the home page once so the jar holds a JSESSIONID.
// Calling it again rotates the session — used after a failed request.
func (c *Client) EnsureSession() error {
	req, err := http.NewRequest("GET", site.HomeURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	_, serr := c.doRequest(req)
	if serr != nil {
		return serr
	}
	return nil
}

// Search posts one page of query results. start is the row offset, rows the
// page size (0 keeps the site's default of 30). Two attempts: a stale
// session is the common failure, and rotating it once is cheaper than
// pushing the error into the worker's retry ladder.
func (c *Client) Search(query string, start, rows int) (*Page, error) {
	page, err := c.searchOnce(query, start, rows)
	if err == nil {
		return page, nil
	}
	// Site's own application error or non-JSON response: do not retry with a fresh session.
	// These are not stale-session issues; a fresh session will hit the same error.
	if errors.Is(err, ErrNotJSON) || errors.Is(err, site.ErrAppError) {
		return nil, err
	}
	// Transport trouble or a bad status: start over with a fresh session.
	if serr := c.EnsureSession(); serr != nil {
		return nil, err
	}
	return c.searchOnce(query, start, rows)
}

func (c *Client) searchOnce(query string, start, rows int) (*Page, error) {
	raw, err := c.post(EncodeForm(State(query, start, rows)))
	if err != nil {
		return nil, err
	}
	return ParsePage(raw)
}

// post sends one form body to select.jsp and returns the raw answer.
func (c *Client) post(form string) ([]byte, error) {
	req, err := http.NewRequest("POST", SelectURL, strings.NewReader(form))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", "https://www3.wipo.int")
	req.Header.Set("Referer", site.HomeURL)
	raw, err := c.doRequest(req)
	if err != nil {
		return nil, err
	}
	// WIPO's own application error page, whatever the status line says. It is
	// the site's fault, not the endpoint's, so it must not read as a transport
	// failure (the proxy would be charged) or as ErrNotJSON (the query would be
	// sent through a pointless browser fallback). The worker requeues it.
	if site.IsAppError(string(raw)) {
		return nil, fmt.Errorf("%w: %d bytes %s", site.ErrAppError, len(raw), site.TrimSnippet(string(raw), 160))
	}
	peek := strings.TrimLeft(string(raw), "\xef\xbb\xbf \t\r\n")
	if !strings.HasPrefix(peek, "{") {
		return nil, fmt.Errorf("%w: %d bytes %.120q", ErrNotJSON, len(raw), peek)
	}
	return raw, nil
}
