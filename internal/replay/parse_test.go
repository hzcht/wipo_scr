package replay

import (
	"errors"
	"testing"

	"wipos/internal/site"
)

// The application reports some of its failures as a JSON envelope rather than
// an HTML page. Either shape has to come back as site.ErrAppError so the
// worker bans the endpoint (rotating the proxy) — and NOT as ErrNotJSON, which
// would send it through the browser fallback for the same doomed response.
func TestParsePageAppErrorEnvelope(t *testing.T) {
	raw := []byte(`{"error":{"code":500,"msg":"An error occurred processing your request. Please report the error, along with details about what browser you are using, to Contact Madrid"}}`)
	_, err := ParsePage(raw)
	if !errors.Is(err, site.ErrAppError) {
		t.Fatalf("ParsePage = %v, want site.ErrAppError", err)
	}
	if errors.Is(err, ErrNotJSON) {
		t.Error("an application error must not read as ErrNotJSON (it would trigger the browser fallback)")
	}
}

// Any other error envelope keeps its old verdict: not a result page, browser
// fallback decides what it really was.
func TestParsePageOtherErrorStaysNotJSON(t *testing.T) {
	raw := []byte(`{"error":{"msg":"something the classifier has never seen"}}`)
	_, err := ParsePage(raw)
	if !errors.Is(err, ErrNotJSON) {
		t.Fatalf("ParsePage = %v, want ErrNotJSON", err)
	}
}

// stripHTML's byte-scan fast paths must be invisible in the output: every
// branch still has to produce the title bytes the grid renders.
func TestStripHTMLFastPaths(t *testing.T) {
	cases := map[string]string{
		"":                           "",
		"PLAIN MARK":                 "PLAIN MARK",
		`GRAND"MÈRE`:                 `GRAND'MÈRE`,
		"<b>BOLD</b> MARK":           "BOLD MARK",
		"<b></b>":                    "",
		"&nbsp;":                     "",
		"&#160;":                     "",
		"&nbsp;<i>x</i>":             "&nbsp;x",
		`<i title="a">quoted</i>`:    "quoted",
		"<i>unclosed":                "unclosed", // the tag matched, the trailing text is not a tag
		"no tags but \"quotes\": ok": "no tags but 'quotes': ok",
	}
	for in, want := range cases {
		if got := stripHTML(in); got != want {
			t.Errorf("stripHTML(%q) = %q, want %q", in, got, want)
		}
	}
}

// viennaClass cuts codes with JS substring semantics; the allocation-free scan
// has to agree with it at the boundary.
func TestClipAndUTF16Len(t *testing.T) {
	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{
		{"21.052.01", 5, "21.05"},
		{"21", 5, "21"},
		{"", 5, ""},
		{"abc", 0, ""},
		{"€€€", 2, "€€"},
		{"a€b", 2, "a€"},
		{"a😀b", 2, "a"}, // a two-unit rune cannot be cut in half in a Go string
	} {
		if got := clip(tc.in, tc.n); got != tc.want {
			t.Errorf("clip(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"", 0},
		{"abc", 3},
		{"€€", 2},
		{"a😀b", 4}, // one BMP unit each side, two for the emoji
	} {
		if got := utf16Len(tc.in); got != tc.want {
			t.Errorf("utf16Len(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
