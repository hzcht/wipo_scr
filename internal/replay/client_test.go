package replay

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

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
