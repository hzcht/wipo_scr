package replay

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf16"

	"wipos/internal/site"
)

// Page is one fetched result page: the total hit count the pager would
// show, the offset it was fetched at, and the rows exactly as the grid
// would have rendered them.
type Page struct {
	NumFound int
	Start    int
	Rows     []site.Row
	// Raw is the response body as select.jsp sent it. The grid HTML is an
	// archive in the browser path (SaveGrid); this is replay's equivalent of
	// it, and it is dropped with the page.
	Raw []byte
}

// wirePage is select.jsp's Solr-shaped answer.
type wirePage struct {
	Response *struct {
		NumFound int       `json:"numFound"`
		Start    int       `json:"start"`
		Docs     []wireDoc `json:"docs"`
	} `json:"response"`
	Highlighting map[string]map[string][]string `json:"highlighting"`
	Error        json.RawMessage                `json:"error"`
}

// wireDoc is one result document. Field types follow the index, not the
// grid: NC comes back as numbers, IRN as a string, and flex adapts both.
type wireDoc struct {
	ID       flex     `json:"ID"`
	SOURCE   flex     `json:"SOURCE"`
	STATUS   flex     `json:"STATUS"`
	OO       flex     `json:"OO"`
	RD       flex     `json:"RD"`
	IRN      flexList `json:"IRN"`
	NC       flexList `json:"NC"`
	VCS      flexList `json:"VCS"`
	USC      flexList `json:"USC"`
	BRAND    flexList `json:"BRAND"`
	BRAND_EN flexList `json:"BRAND_EN"`
	HOL      flexList `json:"HOL"`
	HOL_EN   flexList `json:"HOL_EN"`
}

// flex decodes a JSON string, number, or null into one string — the index
// is not consistent about which of these a field arrives as, and the grid
// renders them all as text.
type flex string

func (f *flex) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		*f = ""
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flex(s)
		return nil
	}
	*f = flex(b)
	return nil
}

// flexList decodes a JSON array or a bare scalar into one slice.
type flexList []string

func (l *flexList) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		*l = nil
		return nil
	}
	if b[0] == '[' {
		var fs []flex
		if err := json.Unmarshal(b, &fs); err != nil {
			return err
		}
		out := make([]string, len(fs))
		for i, f := range fs {
			out[i] = string(f)
		}
		*l = out
		return nil
	}
	var f flex
	if err := f.UnmarshalJSON(b); err != nil {
		return err
	}
	*l = flexList{string(f)}
	return nil
}

// ParsePage decodes a select.jsp result body into rows, applying the same
// transformations the grid's formatters apply — the browser's own values,
// not a fresh interpretation of the index.
func ParsePage(raw []byte) (*Page, error) {
	var w wirePage
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotJSON, err)
	}
	if w.Response == nil {
		if len(w.Error) > 0 {
			// The application's own failure, wrapped in a JSON envelope: same
			// requeue as the HTML page with the same sentence on it.
			if site.IsAppError(string(w.Error)) {
				return nil, fmt.Errorf("%w: %s", site.ErrAppError, site.TrimSnippet(string(w.Error), 160))
			}
			return nil, fmt.Errorf("%w: error response %s", ErrNotJSON, w.Error)
		}
		return nil, fmt.Errorf("%w: no response object", ErrNotJSON)
	}
	// Defensive: if both Response and Error are present, check the Error
	// for the app-error sentence (should not happen per Solr schema, but
	// belt-and-suspenders in case the API changes).
	if len(w.Error) > 0 && site.IsAppError(string(w.Error)) {
		return nil, fmt.Errorf("%w: %s", site.ErrAppError, site.TrimSnippet(string(w.Error), 160))
	}
	page := &Page{
		NumFound: w.Response.NumFound,
		Start:    w.Response.Start,
		Rows:     make([]site.Row, 0, len(w.Response.Docs)),
		Raw:      raw,
	}
	for i, d := range w.Response.Docs {
		page.Rows = append(page.Rows, rowFromDoc(d, w.Highlighting[string(d.ID)], page.Start+i))
	}
	return page, nil
}

// rowFromDoc maps one document onto the grid's columns. Every rule here is
// the port of a formatter from branddb-required — brandName, holderName,
// statusName, getDate, niceClass, viennaClass — followed by stripHtml, the
// exact transform jqGrid applies to build a cell's title attribute.
// trademarks.tsv holds those title bytes, so replay must write them too.
func rowFromDoc(d wireDoc, hl map[string][]string, index int) site.Row {
	brand := pickField(hl, "BRAND", d.BRAND_EN, d.BRAND, true, message("NO_VERBAL_ELEMENTS"))
	holder := pickField(hl, "HOL", d.HOL_EN, d.HOL, false, "")
	h := holder.text()
	// The 64-char cut only ever runs on a plain string: a highlighting
	// value is an array, and a normal-length array never reaches 64
	// elements — which is exactly why long highlighted holders in the
	// ground truth carry "+N" but no "...".
	if !holder.isArray && utf16Len(h) > 64 {
		h += "..." // the printOnly/noPrint spans both land in the title
	}
	if len(d.HOL) > 1 {
		h += "+" + fmt.Sprint(len(d.HOL)-1)
	}

	nice := uniqueJoin(d.NC, ", ")
	if utf16Len(nice) > 64 {
		nice += "..."
	}

	return site.Row{
		Brand:        cell(stripHTML(brand.text())),
		Holder:       cell(stripHTML(h)),
		Status:       cell(stripHTML(statusLabel(string(d.STATUS), hasIRN(d.IRN)))),
		Origin:       cell(stripHTML(string(d.OO))),
		RegDate:      cell(stripHTML(dateOnly(string(d.RD)))),
		RegNumber:    cell(stripHTML(strings.Join(d.IRN, ", "))),
		Nice:         cell(stripHTML(nice)),
		Vienna:       cell(stripHTML(viennaClass(d.VCS, d.USC))),
		RecordID:     cell(stripHTML(string(d.ID))),
		RecordStatus: cell(stripHTML(string(d.STATUS))),
		Number:       index,
	}
}

// fieldVal is one column value as JS sees it after getLanguageField: either
// the raw highlighting array (tags included) or a plain string.
type fieldVal struct {
	hl      []string
	s       string
	isArray bool
}

// text renders the value the way JS string concatenation would: an array
// joins with a bare comma.
func (v fieldVal) text() string {
	if v.isArray {
		return strings.Join(v.hl, ",")
	}
	return v.s
}

// pickField is getLanguageField: the language highlighting key, the plain
// highlighting key, the document's language value, the document's plain
// value, then the default. A present highlighting key always wins — an
// array is truthy in JS even when empty — and the document values use the
// caller's pick: brands from the end, holders from the front.
func pickField(hl map[string][]string, field string, langDoc, doc []string, pickLast bool, def string) fieldVal {
	fieldU := strings.ToUpper(field)
	if hv, ok := hl[fieldU+"_EN"]; ok {
		return fieldVal{hl: hv, isArray: true}
	}
	if hv, ok := hl[fieldU]; ok {
		return fieldVal{hl: hv, isArray: true}
	}
	if s := pick(langDoc, pickLast); s != "" {
		return fieldVal{s: s}
	}
	if s := pick(doc, pickLast); s != "" {
		return fieldVal{s: s}
	}
	return fieldVal{s: def}
}

// pick selects one element the way the bundle does: the last for "last",
// the first otherwise, "" for a missing or empty array.
func pick(arr []string, last bool) string {
	if len(arr) == 0 {
		return ""
	}
	if last {
		return arr[len(arr)-1]
	}
	return arr[0]
}

// stripHTML is jqGrid's stripHtml: drop tags, then swap every double quote
// for a single quote. That swap is why the ground truth spells
// GRAND'MÈRE with an apostrophe where the index stores a double quote.
func stripHTML(v string) string {
	v = tagRe.ReplaceAllString(v, "")
	if v == "" || v == "&nbsp;" || v == "&#160;" {
		return ""
	}
	return strings.ReplaceAll(v, `"`, `'`)
}

// tagRe is the bundle's /<("[^"]*"|'[^']*'|[^'">])*>/gi.
var tagRe = regexp.MustCompile(`<(?:"[^"]*"|'[^']*'|[^'">])*>`)

// hasIRN is the statusName test `c.IRN && c.IRN.length > 0`: absent and
// empty alike mean no registration number yet.
func hasIRN(irn []string) bool {
	for _, v := range irn {
		if v != "" {
			return true
		}
	}
	return false
}

// cell is the grid's cellValue: trim, then collapse every run of spaces —
// which also keeps tabs and newlines out of the TSV output.
func cell(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// dateOnly is getDate for the RD column: everything before the time part,
// and nothing at all for the sentinel dates the index uses for "no date".
func dateOnly(s string) string {
	if s == "" {
		return ""
	}
	i := strings.IndexByte(s, 'T')
	if i < 0 {
		return ""
	}
	d := s[:i]
	if d == "1000-01-01" || d == "3000-01-01" {
		return ""
	}
	return d
}

// uniqueJoin keeps first occurrences, in order — the bundle's Array.unique
// followed by join(", ").
func uniqueJoin(vals []string, sep string) string {
	seen := make(map[string]bool, len(vals))
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return strings.Join(out, sep)
}

// viennaClass is the VCS column: five characters per code (21.052.01 →
// 21.05), unique and comma-joined, with the US class fallback.
func viennaClass(vcs, usc []string) string {
	if len(vcs) == 0 {
		if len(usc) == 0 {
			return ""
		}
		out := make([]string, 0, len(usc))
		for _, u := range usc {
			out = append(out, "US"+clip(u, 5))
		}
		return uniqueJoin(out, ", ")
	}
	out := make([]string, 0, len(vcs))
	for _, v := range vcs {
		out = append(out, clip(v, 5))
	}
	return uniqueJoin(out, ", ")
}

// clip is JS substring(0, n): a short string comes back whole.
func clip(s string, n int) string {
	units := utf16.Encode([]rune(s))
	if len(units) <= n {
		return s
	}
	return string(utf16.Decode(units[:n]))
}

func utf16Len(s string) int {
	return len(utf16.Encode([]rune(s)))
}
