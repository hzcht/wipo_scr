package site

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Row is one trademark from the results grid.
type Row struct {
	Brand        string
	Holder       string
	Status       string
	Origin       string
	RegDate      string
	RegNumber    string
	Nice         string
	Vienna       string
	RecordID     string
	RecordStatus string
	Number       int // the grid's own result number, 0-based; -1 when absent
}

// Values returns the row in the output order documented on Columns.
func (r Row) Values() []string {
	return []string{
		r.Brand, r.Holder, r.Status, r.Origin, r.RegDate, r.RegNumber,
		r.Nice, r.Vienna, r.RecordID, r.RecordStatus,
		strconv.Itoa(r.Number),
	}
}

// RowFromValues rebuilds a row from the columns Values wrote — the inverse that
// lets a restart re-read trademarks.tsv and recover the dedupe keys instead of
// appending every already-collected page a second time. ok is false when the
// line does not hold the full set of columns.
func RowFromValues(f []string) (Row, bool) {
	if len(f) < 11 {
		return Row{}, false
	}
	n, err := strconv.Atoi(f[10])
	if err != nil {
		n = -1
	}
	return Row{
		Brand:        f[0],
		Holder:       f[1],
		Status:       f[2],
		Origin:       f[3],
		RegDate:      f[4],
		RegNumber:    f[5],
		Nice:         f[6],
		Vienna:       f[7],
		RecordID:     f[8],
		RecordStatus: f[9],
		Number:       n,
	}, true
}

// Key identifies a row inside one result set, so walking the same page twice
// cannot write it twice.
//
// The record id is exact when the grid carries it. The grid's own result number
// is only good within a page — page 1 runs 1..100, page 2 runs 101..200 — so it
// is used as part of the fallback key, never alone: a per-page number compared
// across pages would drop every row after the first.
func (r Row) Key() string {
	if r.RecordID != "" {
		return "id:" + r.RecordID
	}
	return "c:" + strings.Join([]string{
		r.Brand, r.Holder, r.RegNumber, r.RegDate, strconv.Itoa(r.Number),
	}, "\x1f")
}

// pagerRe reads the position line: "1 - 100 / 955".
//
// The monitor pads that line with non-breaking spaces (&nbsp;), and Go's \s is
// ASCII-only, so the spacing class has to be \p{Zs} as well. With plain \s the
// count of every single page came back as a parse error — the regex simply did
// not match the real text.
//
// The grid groups thousands in all three numbers: the last page of a large
// result set reads "1,528,669 - 1,528,768 / 1,528,768". A bare (\d+) stops at
// the first comma, so every query above a thousand was reported as its first
// three digits — a count of 18 for a page that held 100 rows, and 1 for the
// whole 1.5-million-record database. pagerNum is therefore the number as a
// single alternative group, and ParsePager joins the digits back together.
//
// The grouping must be well formed: "12,34" is not a number, so the
// alternative falls through to the plain run of digits and leaves the parser
// with a value the bounds check below rejects.
const pagerNum = `\d{1,3}(?:[,\p{Zs}]\d{3})*|\d+`

var pagerRe = regexp.MustCompile(`(` + pagerNum + `)[\s\p{Zs}]*[-–][\s\p{Zs}]*(` + pagerNum + `)[\s\p{Zs}]*/[\s\p{Zs}]*(` + pagerNum + `)`)

// stripGrouping removes the thousands separators from a grouped number.
func stripGrouping(s string) string {
	return strings.Map(func(r rune) rune {
		if r == ',' || unicode.Is(unicode.Zs, r) {
			return -1
		}
		return r
	}, s)
}

// Pager is the position of the results grid on the current page.
type Pager struct {
	From  int // first result number on this page
	To    int // last result number on this page
	Total int // results for the whole query

	// PagerText is the position line as it was read. The collector compares it
	// before and after a pager click: only a line that actually changed proves
	// the grid moved, since the arrow is present on the last page too.
	PagerText string
}

// Last reports whether the current page is the final one.
func (p Pager) Last() bool { return p.To >= p.Total }

// ParsePager reads "1 - 100 / 955", and "1 - 100 / 1,528,768" just as happily.
func ParsePager(s string) (Pager, error) {
	m := pagerRe.FindStringSubmatch(s)
	if m == nil {
		return Pager{}, fmt.Errorf("pager %q: want '<from> - <to> / <total>'", strings.TrimSpace(s))
	}
	from, err1 := strconv.Atoi(stripGrouping(m[1]))
	to, err2 := strconv.Atoi(stripGrouping(m[2]))
	total, err3 := strconv.Atoi(stripGrouping(m[3]))
	if err1 != nil || err2 != nil || err3 != nil {
		return Pager{}, fmt.Errorf("pager %q: non-numeric bounds", strings.TrimSpace(s))
	}
	// A page cannot end past the total. That single check is what catches a
	// misparse where the regex latched onto the wrong digits — the failure this
	// parser actually had. (from > to is left alone: the monitor emits such a
	// line on an empty trailing page, and a pinned test depends on it parsing.)
	if to > total {
		return Pager{}, fmt.Errorf("pager %q: page ends at %d but only %d results exist", strings.TrimSpace(s), to, total)
	}
	return Pager{From: from, To: to, Total: total, PagerText: strings.TrimSpace(s)}, nil
}

// ParseGrid turns the saved inner HTML of the results grid into rows.
//
// The markup is a jqGrid table: every data cell names its column in
// aria-describedby, and the clean value sits in the title attribute — the text
// node is the same value plus highlight markup, so "R<em>014</em> PEARLY
// GATES" is what you get if you read the text instead of the title. The old
// standalone parser did exactly that and shipped <em> tags into the data.
func ParseGrid(gridHTML string) ([]Row, error) {
	doc, err := parseGridFragment(gridHTML)
	if err != nil {
		return nil, fmt.Errorf("parse grid: %w", err)
	}
	var rows []Row
	walk(doc, func(n *html.Node) {
		if n.DataAtom != atom.Tbody {
			return
		}
		for tr := n.FirstChild; tr != nil; tr = tr.NextSibling {
			if tr.DataAtom != atom.Tr {
				continue
			}
			// The grid opens with a hidden sizing row (tr.jqgfirstrow) whose
			// cells are all display:none and carry no id. Reading it would
			// produce one empty phantom record per page.
			if attr(tr, "id") == "" {
				continue
			}
			if row, ok := parseRow(tr); ok {
				rows = append(rows, row)
			}
		}
	})
	return rows, nil
}

// parseGridFragment parses the grid markup.
//
// It matters a lot which entry point is used. The saved grid is a bare
// <tbody>…</tbody>, and html.Parse() follows the HTML5 tree construction
// algorithm, where a tbody outside a table is a parse error: the tag is
// ignored and every <tr> and <td> in it disappears. Feeding the fragment to
// html.ParseFragment() with a <table> context puts it in "in table" insertion
// mode, where the table parts are exactly what they should be. A full
// <table>…</table> input still goes through html.Parse().
func parseGridFragment(gridHTML string) (*html.Node, error) {
	trimmed := strings.TrimSpace(gridHTML)
	if trimmed == "" {
		return nil, fmt.Errorf("empty grid markup")
	}
	if strings.HasPrefix(strings.ToLower(trimmed), "<table") {
		return html.Parse(strings.NewReader(trimmed))
	}
	nodes, err := html.ParseFragment(
		strings.NewReader(trimmed),
		&html.Node{Type: html.ElementNode, DataAtom: atom.Table, Data: "table"},
	)
	if err != nil {
		return nil, err
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("no table markup in %d bytes", len(trimmed))
	}
	// ParseFragment hands back the top-level nodes; walk() wants something with
	// children, so hang them off an empty document node.
	root := &html.Node{Type: html.DocumentNode}
	for _, n := range nodes {
		root.AppendChild(n)
	}
	return root, nil
}

// parseRow reads one <tr>. A row without a brand is not a record (layout
// leftovers, hidden helper rows) and is dropped.
func parseRow(tr *html.Node) (Row, bool) {
	row := Row{Number: -1}
	for td := tr.FirstChild; td != nil; td = td.NextSibling {
		if td.DataAtom != atom.Td {
			continue
		}
		described := attr(td, "aria-describedby")
		if !strings.HasPrefix(described, ariaPrefix) {
			continue // not a grid cell
		}
		col := strings.TrimPrefix(described, ariaPrefix)
		v := cellValue(td)
		switch col {
		case "BRAND":
			row.Brand = v
		case "HOL":
			row.Holder = v
		case "STATUS":
			row.Status = v
		case "OO":
			row.Origin = v
		case "RD":
			row.RegDate = v
		case "IRN":
			row.RegNumber = v
		case "NC":
			row.Nice = v
		case "VCS":
			row.Vienna = v
		case "real_id":
			row.RecordID = v
		case "real_status":
			row.RecordStatus = v
		case "result_number":
			if n, err := strconv.Atoi(v); err == nil {
				row.Number = n
			}
		}
	}
	return row, row.Brand != ""
}

// cellValue prefers the title attribute over the cell text. Empty titles fall
// back to the text, and every value is whitespace-collapsed — which also keeps
// tabs and newlines out of the TSV output, where they would break the format.
func cellValue(td *html.Node) string {
	if v := strings.TrimSpace(attr(td, "title")); v != "" {
		return strings.Join(strings.Fields(v), " ")
	}
	return strings.Join(strings.Fields(textOf(td)), " ")
}

// walk visits every node in the tree.
func walk(n *html.Node, fn func(*html.Node)) {
	fn(n)
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walk(c, fn)
	}
}

// textOf concatenates the text under n. Markup contributes nothing, so the
// result is already free of tags.
func textOf(n *html.Node) string {
	var b strings.Builder
	var w func(*html.Node)
	w = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			w(c)
		}
	}
	w(n)
	return b.String()
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}
