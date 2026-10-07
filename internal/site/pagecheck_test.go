package site

import (
	"testing"
)

// The snippets below are the real thing, trimmed: the monitor's front page and
// position line come from pages the collector actually loaded, the wall and the
// gateway page are the shapes proxies and WAFs return. A classifier that has
// only been shown the happy path is not a classifier.

const realHomeText = `Keep up to date with your international registrations. Follow the status of ` +
	`your international application or trademark registration; access detailed information on ` +
	`all trademarks registered through the Madrid System; and keep an eye on competitors' ` +
	`trademarks. International Registration Number WIPO reference Office reference Applicant ` +
	`reference Basic application Basic registration Mark name Search Reset`

const realHomeTitle = "WIPO\u00a0Madrid Monitor" // the site uses a no-break space

const realResultsPager = "1 - 100 / 955"

const realNoResultsText = "No documents match your query. Please check your search criteria."

const cloudflareWall = `Attention Required! | Cloudflare
Access denied
Sorry, you have been blocked
Ray ID: 8f2c1a4b0e1d9c3f
www.cloudflare.com`

const proxyGatewayPage = `<html><head><title>502 Bad Gateway</title></head>
<body><h1>502 Bad Gateway</h1><p>The proxy server got an invalid response from an upstream server.</p>
</body></html>`

const proxyAuthPage = `407 Proxy Authentication Required
The proxy server requires Basic authentication.`

const maintenancePage = `WIPO Global Brand Database
The service is temporarily unavailable due to scheduled maintenance. Please try again later.`

// The message as WIPO serves it mid-collection, verbatim. It replaces the
// results pane, so the position line never moves — the collector must spot the
// sentence instead of waiting out the results timeout on it.
const appErrorPage = `An error occurred processing your request. Please report the error, along with details about what browser you are using, to Contact Madrid`

// wipoURL is the page's own address. Anything served from outside WIPO's space
// is a lookalike by definition.
const wipoURL = "https://www3.wipo.int/madrid/monitor/en/"

func TestClassifyRealPages(t *testing.T) {
	tests := []struct {
		name string
		in   PageSnapshot
		want Kind
	}{
		{
			name: "monitor front page",
			in:   PageSnapshot{URL: wipoURL, Title: realHomeTitle, Text: realHomeText},
			want: KindMonitor,
		},
		{
			name: "results with a grid and a position line",
			in: PageSnapshot{
				URL:      "https://www3.wipo.int/madrid/monitor/en/",
				Title:    realHomeTitle,
				Text:     realHomeText,
				GridRows: 100,
				Pager:    realResultsPager,
			},
			want: KindResults,
		},
		{
			name: "real empty answer",
			in: PageSnapshot{
				URL:   wipoURL,
				Title: realHomeTitle,
				Text:  realNoResultsText,
			},
			want: KindNoResults,
		},
		{
			name: "bot wall served under WIPO's own domain",
			in:   PageSnapshot{URL: wipoURL, Title: "Attention Required! | Cloudflare", Text: cloudflareWall},
			want: KindBlock,
		},
		{
			name: "gateway page from the proxy",
			in:   PageSnapshot{URL: "http://127.0.0.1:8888/error", Title: "502 Bad Gateway", Text: proxyGatewayPage},
			want: KindProxyError,
		},
		{
			name: "proxy demanding credentials",
			in:   PageSnapshot{URL: "https://www3.wipo.int/madrid/monitor/en/", Text: proxyAuthPage},
			want: KindProxyError,
		},
		{
			name: "site down",
			in:   PageSnapshot{URL: wipoURL, Title: "WIPO Global Brand Database", Text: maintenancePage},
			want: KindMaintenance,
		},
		{
			name: "the application's own error page",
			in:   PageSnapshot{URL: wipoURL, Title: realHomeTitle, Text: appErrorPage},
			want: KindAppError,
		},
		{
			name: "error page wins over a stale grid still on screen",
			in: PageSnapshot{
				URL:      wipoURL,
				Text:     realHomeText + " " + appErrorPage,
				GridRows: 100,
				Pager:    realResultsPager,
			},
			want: KindAppError,
		},
		{
			name: "still loading: nothing to decide yet",
			in:   PageSnapshot{URL: wipoURL, Text: ""},
			want: KindUnknown,
		},
		{
			name: "a page with only one control string is not the monitor",
			in:   PageSnapshot{URL: wipoURL, Text: "Mark name"},
			want: KindUnknown,
		},
		{
			name: "the monitor's title served from somewhere else is a lookalike",
			in:   PageSnapshot{URL: "https://collector.example/madrid/monitor/en/", Title: realHomeTitle},
			want: KindUnknown,
		},
		{
			name: "the monitor's title on a WIPO URL is accepted",
			in:   PageSnapshot{URL: wipoURL, Title: realHomeTitle},
			want: KindMonitor,
		},
		{
			name: "grid without a parsable position line is not a result",
			in: PageSnapshot{
				URL:      wipoURL,
				Text:     realHomeText,
				GridRows: 100,
				Pager:    "loading",
			},
			want: KindMonitor,
		},
		{
			name: "wall wins over the monitor's own text",
			in: PageSnapshot{
				URL:   wipoURL,
				Title: realHomeTitle,
				Text:  realHomeText + " " + cloudflareWall,
			},
			want: KindBlock,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, marker := Classify(tt.in)
			if got != tt.want {
				t.Fatalf("Classify = %v (marker %q), want %v", got, marker, tt.want)
			}
			if got != KindUnknown && marker == "" {
				t.Errorf("Classify = %v but reported no deciding marker", got)
			}
		})
	}
}

// A wall that says "no documents match" must still be a wall. Otherwise one
// block page writes a zero into results.txt and the query is marked done, which
// is the failure mode this whole check exists to prevent.
func TestClassifyWallBeatsNoResultsText(t *testing.T) {
	kind, marker := Classify(PageSnapshot{
		URL:   wipoURL,
		Text:  realNoResultsText + " Checking your browser before accessing WIPO.",
		Pager: "1 - 0 / 0",
	})
	if kind != KindBlock {
		t.Fatalf("Classify = %v (marker %q), want %v", kind, marker, KindBlock)
	}
}

func TestKindPredicates(t *testing.T) {
	if !KindResults.Terminal() || !KindNoResults.Terminal() {
		t.Error("results and no results must both be terminal")
	}
	for _, k := range []Kind{KindUnknown, KindMonitor, KindBlock, KindProxyError, KindMaintenance, KindAppError} {
		if k.Terminal() {
			t.Errorf("%v must not be terminal", k)
		}
	}
	if !KindBlock.Faulty() || !KindProxyError.Faulty() || !KindMaintenance.Faulty() || !KindAppError.Faulty() {
		t.Error("walls, gateway errors, downtime and the app error page are all faulty pages")
	}
	if KindResults.Faulty() || KindMonitor.Faulty() || KindNoResults.Faulty() {
		t.Error("a real page is not a faulty page")
	}
	if KindUnknown.String() != "unknown" || KindProxyError.String() != "proxy error" ||
		KindAppError.String() != "application error" {
		t.Error("Kind.String is used in the logs and must read well")
	}
}

// IsAppError is what the replay client runs on raw bodies: it has no snapshot,
// only bytes, and it has to tell WIPO's failure apart from an ordinary
// non-JSON answer (which would send the query to the browser fallback).
func TestIsAppError(t *testing.T) {
	if !IsAppError(appErrorPage) {
		t.Error("the verbatim message was not recognised")
	}
	if !IsAppError(`<html><body><p>An error occurred processing your request.</p></body></html>`) {
		t.Error("a trimmed message inside HTML was not recognised")
	}
	if !IsAppError("An\u00a0error occurred processing your request") {
		t.Error("the marker must survive the site's own no-break spaces")
	}
	for _, healthy := range []string{realHomeText, realNoResultsText, cloudflareWall, maintenancePage} {
		if IsAppError(healthy) {
			t.Errorf("healthy or already-classified content mistaken for the app error: %.60q", healthy)
		}
	}
}

func TestTrimSnippetNormalisesSpaces(t *testing.T) {
	got := TrimSnippet("1\u00a0-\u00a0100\u2009/\u2009955 and some more text after it", 12)
	want := "1 - 100 /..."
	if got != want {
		t.Fatalf("TrimSnippet = %q, want %q", got, want)
	}
	if got := TrimSnippet("short", 12); got != "short" {
		t.Fatalf("TrimSnippet = %q, want %q", got, "short")
	}
}

// The monitor's own title uses a no-break space, so a classifier that compared
// against "Madrid Monitor" with a plain space would find nothing. This is the
// exact trap the normalisation exists for.
func TestClassifySurvivesNoBreakSpaces(t *testing.T) {
	kind, _ := Classify(PageSnapshot{
		URL:   wipoURL,
		Title: "WIPO\u00a0Madrid\u00a0Monitor",
		Text:  "International Registration Number\tWIPO reference Mark name",
	})
	if kind != KindMonitor {
		t.Fatalf("Classify = %v, want %v", kind, KindMonitor)
	}
}

func TestIsMonitorURL(t *testing.T) {
	for _, u := range []string{
		"https://www3.wipo.int/madrid/monitor/en/",
		"https://www.wipo.int/madrid/monitor/en/showData.jsp?ID=BAN:AM%202018/2004",
	} {
		if !isMonitorURL(u) {
			t.Errorf("isMonitorURL(%q) = false, want true", u)
		}
	}
	for _, u := range []string{"", "https://collector.example/madrid/monitor/", "http://127.0.0.1:8888/"} {
		if isMonitorURL(u) {
			t.Errorf("isMonitorURL(%q) = true, want false", u)
		}
	}
}

// ParsePager is the other half of the control check: a position line that does
// not parse must not be reported as a result.
func TestClassifyRejectsUnparsablePager(t *testing.T) {
	kind, marker := Classify(PageSnapshot{
		URL:      wipoURL,
		Text:     realHomeText,
		GridRows: 100,
		Pager:    "1 - 100 /",
	})
	if kind == KindResults {
		t.Fatalf("Classify = %v (marker %q) on an unparsable pager, want not a result", kind, marker)
	}
	if _, err := ParsePager("1 - 100 /"); err == nil {
		t.Fatal("ParsePager accepted a truncated position line")
	} else if err.Error() == "" {
		t.Error("the pager error must carry a message")
	}
}
