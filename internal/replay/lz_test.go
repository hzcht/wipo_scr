package replay

import (
	"strings"
	"testing"
)

// capturedQZ is a live select.jsp payload (URL-decoded): the POST body the
// site's own JavaScript built for a search, captured by replayprobe.
const capturedQZ = "N4IgDiBcoM4KYEMBOBjAFlUSEHcoG0QAXEAXQBoQYBHA0IuKYkSgMwEsmAhAJQEEAcgBEA+nwAy48gAkA8uJYgAJqyYBZPjwDSYyTPmKUAeyaChIAL6kLbBCjhEYBEEIDKiueIDCZG8QCeYIyQIAC2CEpI7EqKADYITHAAdorUnCEADAC0AGpergJqSABeSAAsAKoA1DBeAK4AYgAcfACKAKIA4uIA9ACsABqxXuydrjhVTQjsakIAnADqXACMrlplALyWQA"

func TestDecompressCapturedQZ(t *testing.T) {
	state, err := DecompressFromBase64(capturedQZ)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(state, `"type":"madrid"`) {
		t.Fatalf("unexpected state: %s", state)
	}
	if !strings.Contains(state, `"sq"`) {
		t.Fatalf("no query block: %s", state)
	}
	t.Logf("state: %s", state)
}

// TestCompressMatchesSite proves the port byte for byte: recompressing what
// decompressed must give back exactly what the site sent.
func TestCompressMatchesSite(t *testing.T) {
	state, err := DecompressFromBase64(capturedQZ)
	if err != nil {
		t.Fatal(err)
	}
	got := CompressToBase64(state)
	if got != capturedQZ {
		t.Fatalf("compress drifted\n got: %s\nwant: %s", got, capturedQZ)
	}
}

func TestRoundTrip(t *testing.T) {
	for _, s := range []string{
		"",
		"a",
		"the-royal",
		`{"la":"en","p":{"search":{"sq":[{"te":"café ☕"}]}},"type":"madrid"}`,
		strings.Repeat("madrid-monitor-wipo/", 50),
		"non-bmp: \U0001F600 \U0001F601 end",
	} {
		got, err := DecompressFromBase64(CompressToBase64(s))
		if err != nil {
			t.Fatalf("%q: %v", s, err)
		}
		if got != s {
			t.Fatalf("round trip\n got: %q\nwant: %q", got, s)
		}
	}
}
