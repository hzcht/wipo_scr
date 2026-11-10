package replay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wipos/internal/site"
)

// docsPayload is select.jsp's real answer shape, with the two words that broke
// the old body-sniffing check inside the *data*: a mark called "403 FORBIDDEN"
// and a holder whose name contains "FORBIDDEN". A run lost about 70% of its
// attempts to exactly this payload being read as a refusal.
func docsPayload() []byte {
	return []byte(fmt.Sprintf(`{
		"lastUpdated": 1793027170514,
		"sv": "www3.wipo.int",
		"response": {
			"numFound": 2,
			"start": 0,
			"docs": [
				{"ID":"ROM.1","SOURCE":"IR","STATUS":"A","OO":"US","RD":"2020-01-01",
				 "IRN":["403"],"NC":["33"],"BRAND":["403 FORBIDDEN"],"HOL":["No. 403 Street"]},
				{"ID":"ROM.2","SOURCE":"IR","STATUS":"A","OO":"CN","RD":"2021-02-02",
				 "IRN":["9"],"NC":["5"],"BRAND":["PALACE MUSEUM"],"HOL":["FORBIDDEN CITY CULTURE DEVELOPMENT CO., LTD."]}
			]
		}
	}`))
}

// A healthy result page must survive grading and then parse into rows. This is
// the regression test for the false 403.
func TestGradeResponseKeepsAHealthyResultPage(t *testing.T) {
	raw := docsPayload()
	got, err := gradeResponse(http.StatusOK, raw)
	if err != nil {
		t.Fatalf("gradeResponse(200, result docs) = %v, want nil — result data is not an error page", err)
	}
	if string(got) != string(raw) {
		t.Error("gradeResponse did not hand the payload back unchanged")
	}
	page, err := ParsePage(got)
	if err != nil {
		t.Fatalf("ParsePage: %v", err)
	}
	if page.NumFound != 2 || len(page.Rows) != 2 {
		t.Errorf("parsed %d rows of %d, want 2 of 2", len(page.Rows), page.NumFound)
	}
	if page.Rows[0].Brand != "403 FORBIDDEN" {
		t.Errorf("row 0 brand = %q, want %q", page.Rows[0].Brand, "403 FORBIDDEN")
	}
}

// The status line is what makes a refusal, body or no body.
func TestGradeResponseReadsARefusalFromTheStatus(t *testing.T) {
	for name, body := range map[string][]byte{
		"json":  docsPayload(),
		"html":  []byte("<html><body>Denied</body></html>"),
		"empty": nil,
	} {
		if _, err := gradeResponse(http.StatusForbidden, body); !errors.Is(err, ErrBlocked) {
			t.Errorf("%s: gradeResponse(403) = %v, want ErrBlocked", name, err)
		}
	}
}

func TestGradeResponseClassifiesNonJSONBodies(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"proxy 403 stub", 200, "<h1>403 Forbidden</h1>", ErrBlocked},
		{"app error page", 200, "<p>An error occurred processing your request.</p>", site.ErrAppError},
		{"maintenance junk", 200, "<html>got something else</html>", ErrNotJSON},
		{"gateway status", http.StatusBadGateway, "<html>bad gateway</html>", nil},
	} {
		_, err := gradeResponse(tc.status, []byte(tc.body))
		if tc.want == nil {
			if err == nil {
				t.Errorf("%s: gradeResponse = nil, want an HTTP error", tc.name)
			} else if !strings.Contains(err.Error(), "HTTP 502") {
				t.Errorf("%s: error = %v, want the status named", tc.name, err)
			}
			continue
		}
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: gradeResponse = %v, want %v", tc.name, err, tc.want)
		}
	}
}

// A JSON error envelope is still an error: it is graded by its structure in
// ParsePage, not by being handed back as data.
func TestGradeResponseHandsJSONToTheParser(t *testing.T) {
	envelope := []byte(`{"lastUpdated":1,"response":null,"error":"An error occurred processing your request"}`)
	raw, err := gradeResponse(http.StatusOK, envelope)
	if err != nil {
		t.Fatalf("gradeResponse = %v, want nil (the parser grades JSON)", err)
	}
	if _, err := ParsePage(raw); !errors.Is(err, site.ErrAppError) {
		t.Errorf("ParsePage = %v, want the app error", err)
	}
}

// roundTripFunc lets a test drive the HTTP path without a network.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// A canceled search is a stopping walk, not a stale session. Renewing the
// session would send a home-page GET after the worker has already abandoned the
// attempt — exactly the traffic the cancel exists to avoid.
func TestSearchContextDoesNotRenewAfterCancellation(t *testing.T) {
	c := NewClient("test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls atomic.Int32
	c.hc.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		cancel()
		return nil, errors.New("dial failure")
	})

	if _, err := c.SearchContext(ctx, "acme", 0, 100); err == nil {
		t.Fatal("SearchContext = nil error, want the transport failure")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("requests = %d, want 1 — a canceled search renewed the session", got)
	}
}

// The deadline is read by every in-flight request and updated as the endpoint's
// measured latency changes; this is the concurrent access a prefetching walk
// creates, so it is pinned under the race detector.
func TestSetTimeoutIsSafeWhileRequestsRun(t *testing.T) {
	c := NewClient("test")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 1000; i++ {
			c.SetTimeout(time.Duration(i) * time.Millisecond)
		}
	}()
	for i := 0; i < 1000; i++ {
		_ = c.Timeout()
	}
	<-done
}

// releaseClient builds a session client with no proxy in front of it, so the
// tests can point it at a local server instead of the monitor.
func releaseClient() *Client {
	return &Client{hc: &http.Client{Transport: newTransport(nil)}}
}

// A dropped session is never reused, but its transport still parks idle
// sockets for IdleConnTimeout — and through a proxy a parked socket is a live
// tunnel to an endpoint the pool may already have cooled down. Close is what
// makes a rotation actually release them instead of leaving them to time out.
func TestCloseReleasesIdleConnections(t *testing.T) {
	var closed atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateClosed {
			closed.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()

	c := releaseClient()
	for i := 0; i < 2; i++ {
		res, err := c.hc.Get(srv.URL)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
	}
	// Both requests reused one connection, and it is sitting idle now: that is
	// the state Close has to clear.
	time.Sleep(50 * time.Millisecond)
	if got := closed.Load(); got != 0 {
		t.Fatalf("connections closed before Close = %d, want 0 — the test never parked one", got)
	}

	c.Close()

	deadline := time.Now().Add(3 * time.Second)
	for closed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := closed.Load(); got == 0 {
		t.Error("the idle connection survived Close; a rotation would keep the old tunnel open")
	}
}

// dropSession runs on paths where there may be no session at all — a first
// attempt that never got one, a second block in a row — so Close has to be
// safe on a nil receiver.
func TestCloseOnNilClient(t *testing.T) {
	var c *Client
	c.Close()
}

func okResponse(req *http.Request, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

// A transport failure that is not a ban gets one fresh session and one retry;
// that part must keep working when the retry path is shared by concurrent
// prefetches.
func TestSearchContextRenewsAndRetriesOnce(t *testing.T) {
	c := NewClient("test")
	var calls atomic.Int32
	c.hc.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch calls.Add(1) {
		case 1:
			return nil, errors.New("temporary transport failure")
		case 2:
			return okResponse(req, "<html>home</html>"), nil
		default:
			return okResponse(req, string(docsPayload())), nil
		}
	})

	page, err := c.SearchContext(context.Background(), "acme", 0, 100)
	if err != nil {
		t.Fatalf("SearchContext: %v", err)
	}
	if page.NumFound != 2 || len(page.Rows) != 2 {
		t.Errorf("parsed %d rows of %d, want 2 of 2", len(page.Rows), page.NumFound)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("requests = %d, want 3 (page, home, retried page)", got)
	}
}

// Prefetched pages fail together when an endpoint dies. Each one holds the
// session epoch it started with; only the first failure may fetch a new home
// page, or the walk would rotate the session once per in-flight request.
func TestRenewSessionCoalescesByGeneration(t *testing.T) {
	c := NewClient("test")
	var calls atomic.Int32
	c.hc.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return okResponse(req, "<html>home</html>"), nil
	})

	seen := c.sessionGeneration()
	if err := c.renewSession(context.Background(), seen); err != nil {
		t.Fatalf("first renew: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("first renew fetched %d times, want 1", got)
	}
	if err := c.renewSession(context.Background(), seen); err != nil {
		t.Fatalf("stale renew: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("a stale epoch fetched the session again (%d calls); want the first renewal to win", got)
	}
	if c.sessionGeneration() == seen {
		t.Error("a successful renewal did not advance the session generation")
	}
}
