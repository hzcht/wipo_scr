package replay

import (
	"context"
	"crypto/tls"
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

// ErrBlocked marks a 403 refusal: the endpoint's IP was forbidden, even though
// the body carries WIPO's generic application-error sentence. It is separate
// from ErrAppError only so the logs can name the refusal; both rotate the proxy.
// It is kept apart from ErrNotJSON so it does not send the query through the
// browser fallback first.
var ErrBlocked = errors.New("replay: the request was forbidden (403)")

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
			Jar:       jar,
			Timeout:   0,
			Transport: newTransport(http.ProxyFromEnvironment),
		},
		ua: ua,
	}
}

// newTransport builds the transport the session and every rotation use. TLS
// verification is off: the pool is a list of intercepting HTTPS proxies whose
// certificates are self-signed, and Go uses one TLS config for both the tunnel
// and the site behind it. The precheck makes the same call, so an endpoint that
// passes it is not then rejected here.
func newTransport(proxy func(*http.Request) (*url.URL, error)) *http.Transport {
	return &http.Transport{
		Proxy:             proxy,
		ForceAttemptHTTP2: false,
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
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
	c.hc.Transport = newTransport(proxy)
	return nil
}

// doRequest executes req with the client's timeout context and returns the body
// with its status code. The context must stay alive until the body is fully
// read, so we read it here.
//
// Transport failures are the only errors it reports. What a status means is
// gradeResponse's decision, because a 403 has to come from the status line: the
// body of a *successful* answer is the result documents themselves, and those
// documents legitimately contain anything at all.
func (c *Client) doRequest(req *http.Request) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(req.Context(), c.Timeout)
	defer cancel()
	req = req.WithContext(ctx)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return raw, resp.StatusCode, nil
}

// gradeResponse decides what a completed answer is. It returns nil when raw is
// a result payload for ParsePage to decode, and an error the worker already
// knows how to grade otherwise.
//
// The status line decides a refusal — never the body. This is the fix for the
// failure that cost the run most of its throughput: the old check looked for
// the substrings "403" and "forbidden" anywhere in the response, and a healthy
// page of result documents carries both — a mark called "403 FORBIDDEN", a
// holder named "FORBIDDEN CITY CULTURE DEVELOPMENT", an IRN or a postal code
// that happens to be 403. About 70% of the attempts in one run were thrown
// away as blocks while their pages were perfectly good, and every one of them
// also cooled down a working endpoint.
//
// A JSON body is therefore handed straight to ParsePage, which grades it by
// structure (its error field, its response object) and not by wording. Only a
// body that is not JSON — an HTML stub from a proxy, a wall, a maintenance
// screen — is still read as text, because there the wording is all there is.
func gradeResponse(status int, raw []byte) ([]byte, error) {
	if status == http.StatusForbidden {
		return nil, fmt.Errorf("%w: HTTP %d, %d bytes %s", ErrBlocked, status, len(raw), site.TrimSnippet(string(raw), 160))
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("replay: HTTP %d (%d bytes)", status, len(raw))
	}
	peek := strings.TrimLeft(string(raw), "\xef\xbb\xbf \t\r\n")
	if strings.HasPrefix(peek, "{") || strings.HasPrefix(peek, "[") {
		return raw, nil
	}
	if site.IsForbidden(peek) {
		return nil, fmt.Errorf("%w: %d bytes %s", ErrBlocked, len(raw), site.TrimSnippet(peek, 160))
	}
	if site.IsAppError(peek) {
		return nil, fmt.Errorf("%w: %d bytes %s", site.ErrAppError, len(raw), site.TrimSnippet(peek, 160))
	}
	return nil, fmt.Errorf("%w: %d bytes %.120q", ErrNotJSON, len(raw), peek)
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
	raw, status, serr := c.doRequest(req)
	if serr != nil {
		return serr
	}
	if status != http.StatusOK {
		return fmt.Errorf("replay: session: HTTP %d (%d bytes)", status, len(raw))
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
	// Site's own application error, a 403 refusal, or a non-JSON response: do
	// not retry with a fresh session. A fresh session hits the same error, and
	// the first two are bans — the worker cools the proxy down and rotates it.
	if errors.Is(err, ErrNotJSON) || errors.Is(err, site.ErrAppError) || errors.Is(err, ErrBlocked) {
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
	raw, status, err := c.doRequest(req)
	if err != nil {
		return nil, err
	}
	return gradeResponse(status, raw)
}
