package site

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gridFixture reproduces the parts of the live jqGrid markup that a parser can
// get wrong. It is trimmed from a saved page of the real site, so every cell
// here is one the collector has to survive:
//
//   - the hidden sizing row (tr.jqgfirstrow, no id) that must not become a
//     record;
//   - hidden data columns (real_id, real_type, real_status, result_number) whose
//     text is identical to their title;
//   - a BRAND cell where the title is clean and the text carries the search
//     highlight: R<em>014</em> PEARLY GATES;
//   - a STATUS cell whose text is broken by a <br>: "Inactive: \n expired";
//   - empty cells holding a non-breaking space, which must come out as "" and
//     not as a stray tab in the TSV;
//   - a result_link column holding a wall of JavaScript.
const gridFixture = `<tbody role="rowgroup">
<tr class="jqgfirstrow" role="row" style="height:auto">
<td role="gridcell" style="height:0px;width:0px;display:none;"></td>
<td role="gridcell" style="height: 0px; width: 118px;"></td>
</tr>
<tr role="row" id="0" tabindex="-1" class="ui-widget-content jqgrow ui-row-ltr">
<td role="gridcell" style="display:none;" title="ROM.1246127" aria-hidden="true" aria-describedby="gridForsearch_pane_real_id">ROM.1246127</td>
<td role="gridcell" style="display:none;" title="ROM" aria-hidden="true" aria-describedby="gridForsearch_pane_real_type">ROM</td>
<td role="gridcell" style="display:none;" title="DEL_REN2" aria-hidden="true" aria-describedby="gridForsearch_pane_real_status">DEL_REN2</td>
<td role="gridcell" style="display:none;" title="0" aria-hidden="true" aria-describedby="gridForsearch_pane_result_number">0</td>
<td role="gridcell" style="display:none;" title="function(e){return e.ID}" aria-hidden="true" aria-describedby="gridForsearch_pane_result_link">function(e){return e.ID}</td>
<td role="gridcell" style="" title="R014 PEARLY GATES" aria-describedby="gridForsearch_pane_BRAND">R<em>014</em> PEARLY GATES</td>
<td role="gridcell" style="text-align:center;" title="" aria-describedby="gridForsearch_pane_IMG">&nbsp;</td>
<td role="gridcell" style="" title="Inactive: expired" aria-describedby="gridForsearch_pane_STATUS"><div class="status_icon"><div>Inactive: <br>expired</div></div></td>
<td role="gridcell" style="" title="CH" aria-describedby="gridForsearch_pane_OO">CH</td>
<td role="gridcell" style="" title="Kronoplus Limited" aria-describedby="gridForsearch_pane_HOL">Kronoplus Limited</td>
<td role="gridcell" style="text-align:right;" title="2015-03-20" aria-describedby="gridForsearch_pane_RD">2015-03-20</td>
<td role="gridcell" style="" title="1246127" aria-describedby="gridForsearch_pane_IRN">1246127</td>
<td role="gridcell" style="text-align:right;" title="17, 19, 27" aria-describedby="gridForsearch_pane_NC">17, 19, 27</td>
<td role="gridcell" style="text-align:right;" title="" aria-describedby="gridForsearch_pane_VCS">&nbsp;</td>
</tr>
<tr role="row" id="1" tabindex="-1" class="ui-widget-content jqgrow ui-row-ltr">
<td role="gridcell" style="display:none;" title="ROM.9999999" aria-hidden="true" aria-describedby="gridForsearch_pane_real_id">ROM.9999999</td>
<td role="gridcell" style="display:none;" title="1" aria-hidden="true" aria-describedby="gridForsearch_pane_result_number">1</td>
<td role="gridcell" style="" title="PEARLY GATES &amp; CO" aria-describedby="gridForsearch_pane_BRAND">PEARLY GATES &amp; CO</td>
<td role="gridcell" style="" title="Active" aria-describedby="gridForsearch_pane_STATUS"><div>Active</div></td>
<td role="gridcell" style="" title="" aria-describedby="gridForsearch_pane_VCS">&nbsp;</td>
</tr>
<tr role="row" id="2" class="ui-widget-content jqgrow"><td role="gridcell" style="display:none;" title="ROM.1" aria-describedby="gridForsearch_pane_real_id">ROM.1</td><td role="gridcell" style="" title="" aria-describedby="gridForsearch_pane_HOL">&nbsp;</td></tr>
</tbody>`

func TestParseGridRealisticMarkup(t *testing.T) {
	rows, err := ParseGrid(gridFixture)
	if err != nil {
		t.Fatalf("ParseGrid: %v", err)
	}
	// Two real records: the sizing row has no id, and the third row has an id
	// but no brand, so it is a layout leftover rather than a record.
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 (the sizing row and the partial row must be dropped)", len(rows))
	}

	first := rows[0]
	want := Row{
		Brand:        "R014 PEARLY GATES",
		Holder:       "Kronoplus Limited",
		Status:       "Inactive: expired",
		Origin:       "CH",
		RegDate:      "2015-03-20",
		RegNumber:    "1246127",
		Nice:         "17, 19, 27",
		Vienna:       "",
		RecordID:     "ROM.1246127",
		RecordStatus: "DEL_REN2",
		Number:       0,
	}
	if first != want {
		t.Errorf("row 0\n got %+v\nwant %+v", first, want)
	}

	// The second row has no holder/reg-date columns at all. Missing columns must
	// stay empty rather than inherit the previous row's values — that is what
	// a parser carrying state between rows would do.
	second := rows[1]
	if second.Brand != "PEARLY GATES & CO" {
		t.Errorf("row 1 brand = %q, want the title value with the entity decoded", second.Brand)
	}
	if second.Holder != "" || second.RegDate != "" || second.RegNumber != "" || second.Nice != "" {
		t.Errorf("row 1 kept values from row 0: %+v", second)
	}
	if second.Status != "Active" {
		t.Errorf("row 1 status = %q, want its own value, not row 0's", second.Status)
	}
	if second.RecordID != "ROM.9999999" || second.Number != 1 {
		t.Errorf("row 1 ids = %q/%d", second.RecordID, second.Number)
	}
}

func TestParseGridStaysTSVSafe(t *testing.T) {
	rows, err := ParseGrid(gridFixture)
	if err != nil {
		t.Fatalf("ParseGrid: %v", err)
	}
	for _, r := range rows {
		for _, v := range r.Values() {
			if strings.ContainsAny(v, "\t\r\n") {
				t.Errorf("value %q carries a field separator", v)
			}
			if strings.ContainsAny(v, "<>") {
				t.Errorf("value %q still has markup in it", v)
			}
		}
	}
}

func TestRowKeyPrefersRecordID(t *testing.T) {
	// The grid's result number restarts per page, so it cannot identify a record
	// on its own. Two pages of the same query must produce different keys for
	// the same record number, and the record id must win when it is there.
	a := Row{Brand: "x", RecordID: "ROM.1", Number: 5}
	b := Row{Brand: "y", RecordID: "ROM.2", Number: 5}
	if a.Key() == b.Key() {
		t.Fatalf("two different records share the key %q", a.Key())
	}
	anon1 := Row{Brand: "x", Number: 5}
	anon2 := Row{Brand: "y", Number: 5}
	if anon1.Key() == anon2.Key() {
		t.Errorf("rows without a record id collapsed onto one key: %q", anon1.Key())
	}
	if (Row{Brand: "x", Number: 5}).Key() == (Row{Brand: "x", Number: 6}).Key() {
		t.Error("the result number is not part of the fallback key, so a re-read page looks like new data")
	}
}

func TestParsePager(t *testing.T) {
	tests := []struct {
		in              string
		from, to, total int
		last            bool
		wantErr         bool
	}{
		{in: "1 - 100 / 955", from: 1, to: 100, total: 955},
		// The grid groups thousands, and a bare (\d+) used to stop at the first
		// comma: this line was read as a total of 1, i.e. the whole
		// 1.5-million-record database reported as a single hit.
		{in: "1 - 100 / 1,813", from: 1, to: 100, total: 1813},
		{in: "1 - 100 / 1,528,768", from: 1, to: 100, total: 1528768},
		{in: "1,528,669 - 1,528,768 / 1,528,768", from: 1528669, to: 1528768, total: 1528768, last: true},
		{in: "1 - 100 / 1 528 768", from: 1, to: 100, total: 1528768},
		{in: "1 - 100 / 12,34", wantErr: true},
		{in: "1 - 100 / 50", wantErr: true},
		{in: "956 - 955 / 955", from: 956, to: 955, total: 955, last: true},
		{in: "1-7/7", from: 1, to: 7, total: 7, last: true},
		{in: "1 - 60 / 60", from: 1, to: 60, total: 60, last: true},
		{in: "1 – 30 / 30", from: 1, to: 30, total: 30, last: true},
		{in: "no results", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, tc := range tests {
		p, err := ParsePager(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParsePager(%q) = %+v, want an error", tc.in, p)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParsePager(%q): %v", tc.in, err)
			continue
		}
		if p.From != tc.from || p.To != tc.to || p.Total != tc.total {
			t.Errorf("ParsePager(%q) = %d-%d/%d, want %d-%d/%d",
				tc.in, p.From, p.To, p.Total, tc.from, tc.to, tc.total)
		}
		if p.Last() != tc.last {
			t.Errorf("ParsePager(%q).Last() = %v, want %v", tc.in, p.Last(), tc.last)
		}
		if p.PagerText != strings.TrimSpace(tc.in) {
			t.Errorf("ParsePager(%q).PagerText = %q", tc.in, p.PagerText)
		}
	}
}

func TestSanitizeQuery(t *testing.T) {
	tests := map[string]string{
		"ACME Corp":    "acme corp",
		"example.com":  "example-com",
		"R_014/PEARLY": "r-014-pearly",
		"0-14":         "0-14",
		"":             "query",
		"###":          "query",
		// Windows forbids both characters in a name, and both are meaningful
		// to the monitor: "*" is the whole database. They must not collapse
		// onto the shared "query" fallback or every wildcard crawl overwrites
		// the previous one's saved pages.
		"*":                     "star",
		"?":                     "qmark",
		"a*?":                   "a-star-qmark",
		"a**":                   "a-star-star",
		"*the*":                 "star-the-star",
		strings.Repeat("z", 80): strings.Repeat("z", 60),
	}
	for in, want := range tests {
		if got := SanitizeQuery(in); got != want {
			t.Errorf("SanitizeQuery(%q) = %q, want %q", in, got, want)
		}
	}
	// The name has to survive being a file name at all.
	for _, in := range []string{"*", "?", "a*?", "*the*"} {
		if got := SanitizeQuery(in); strings.ContainsAny(got, `*?:"<>|`) {
			t.Errorf("SanitizeQuery(%q) = %q, which Windows will not accept", in, got)
		}
	}
}

// TestParseGridOnCollectedData runs the parser over a page the previous
// collector really saved, which is the only fixture that cannot go stale on us.
// It is skipped when data_to_proc is not there.
func TestParseGridOnCollectedData(t *testing.T) {
	path := filepath.Join("..", "..", "data_to_proc", "0-14.raw")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("no collected page at %s: %v", path, err)
	}
	rows, err := ParseGrid(string(raw))
	if err != nil {
		t.Fatalf("ParseGrid(%s): %v", path, err)
	}
	if len(rows) == 0 {
		t.Fatalf("ParseGrid(%s) found no records in a real page", path)
	}
	for _, r := range rows {
		if r.Brand == "" {
			t.Error("a record came out without a brand")
		}
		if r.RecordID == "" {
			t.Error("a record came out without real_id, so it cannot be deduplicated")
		}
		for _, v := range r.Values() {
			if strings.ContainsAny(v, "\t\r\n") {
				t.Errorf("value %q carries a field separator", v)
			}
			if strings.ContainsAny(v, "<>") {
				t.Errorf("value %q still has markup: %s", path, v)
			}
		}
	}
	t.Logf("%s: %d records, first = %q / %q", filepath.Base(path), len(rows), rows[0].Brand, rows[0].Holder)
}

// RowFromValues is how a restart recovers the dedupe key from a written line.
// If it drifts from Values, a resumed run re-appends rows it already has — the
// one failure mode that is silent until the output file has doubled.
func TestRowFromValuesRoundtrip(t *testing.T) {
	rows := []Row{
		{Brand: "R\u00e9sum\u00e9 Co", Holder: "Holder\twith tab? no", Status: "Registered",
			Origin: "FR", RegDate: "2019-05-06", RegNumber: "987654", Nice: "25",
			Vienna: "07.01.01", RecordID: "1234567", RecordStatus: "A", Number: 42},
		{Brand: "NO-ID", Number: 7},
		{Number: -1},
	}
	for i, want := range rows {
		got, ok := RowFromValues(want.Values())
		if !ok {
			t.Errorf("row %d: RowFromValues ok = false", i)
			continue
		}
		if got != want {
			t.Errorf("row %d roundtrip = %+v, want %+v", i, got, want)
		}
		if got.Key() != want.Key() {
			t.Errorf("row %d key = %q after roundtrip, want %q", i, got.Key(), want.Key())
		}
	}

	// A torn or truncated line must be rejected, not silently turned into a row
	// with empty columns (whose key would match other empty rows).
	for _, short := range [][]string{
		nil,
		{"only", "three", "fields"},
		rows[0].Values()[:10],
	} {
		if _, ok := RowFromValues(short); ok {
			t.Errorf("RowFromValues(%d fields): ok = true, want false", len(short))
		}
	}

	// A non-numeric result number cannot happen in written data, but a corrupt
	// line must not panic — the -1 sentinel marks it unreadable.
	got, ok := RowFromValues(append(rows[1].Values()[:10], "x"))
	if !ok || got.Number != -1 {
		t.Errorf("non-numeric number: row = %+v ok = %v, want Number -1", got, ok)
	}
}
