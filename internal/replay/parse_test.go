package replay

import (
	"errors"
	"testing"

	"wipos/internal/site"
)

// The application reports some of its failures as a JSON envelope rather than
// an HTML page. Either shape has to come back as site.ErrAppError so the
// worker requeues the query — and NOT as ErrNotJSON, which would send it
// through the browser fallback for the same doomed response.
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
