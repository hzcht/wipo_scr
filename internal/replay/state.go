package replay

import (
	"encoding/json"
	"net/url"
)

// state mirrors the JSON the monitor's own JavaScript compresses into the
// qz form field. Field placement was verified live: start and rows only take
// effect inside p — a top-level start is silently ignored — while qi (absent
// here on purpose) is echoed but never required.
type state struct {
	La   string `json:"la"`
	P    stateP `json:"p"`
	Type string `json:"type"`
}

type stateP struct {
	Facets []string    `json:"facets"`
	Search stateSearch `json:"search"`
	Start  *int        `json:"start,omitempty"`
	Rows   *int        `json:"rows,omitempty"`
}

type stateSearch struct {
	Raw []string  `json:"raw"`
	Sq  []stateSq `json:"sq"`
}

type stateSq struct {
	Co string `json:"co"`
	Df string `json:"df"`
	Fi string `json:"fi"`
	Te string `json:"te"`
}

// State renders the request state for query at offset start with rows docs
// per page. rows <= 0 leaves the site's default (30) alone; start <= 0 omits
// the offset entirely, the way a first page does.
func State(query string, start, rows int) string {
	st := state{
		La: "en",
		P: stateP{
			Facets: []string{"DS", "HOLC"},
			Search: stateSearch{
				Raw: []string{query},
				Sq: []stateSq{{
					Co: "AND",
					Df: "MARK_ALL,HOL",
					Fi: "BRAND_ALL,HOL",
					Te: query,
				}},
			},
		},
		Type: "madrid",
	}
	if start > 0 {
		st.P.Start = &start
	}
	if rows > 0 {
		st.P.Rows = &rows
	}
	b, err := json.Marshal(st)
	if err != nil {
		return ""
	}
	return string(b)
}

// EncodeForm builds the complete select.jsp form body for a state: qz holds
// the LZString-compressed JSON, form-encoded the way the browser's own
// jQuery sends it.
func EncodeForm(stateJSON string) string {
	return "qz=" + url.QueryEscape(CompressToBase64(stateJSON))
}
