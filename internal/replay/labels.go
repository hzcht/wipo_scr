package replay

import (
	"embed"
	"encoding/json"
	"strings"

	"golang.org/x/net/html"
)

// labelsFS holds the message table extracted from branddb.en: the same
// key → HTML pairs the site's options.messages carries. The browser resolves
// STATUS_* keys through getMessage, which falls back to the key itself when
// a message is missing — message() does the same.
//
//go:embed labels.json
var labelsFS embed.FS

var messages map[string]string

func init() {
	raw, err := labelsFS.ReadFile("labels.json")
	if err != nil {
		panic("replay: labels.json: " + err.Error())
	}
	var htmls map[string]string
	if err := json.Unmarshal(raw, &htmls); err != nil {
		panic("replay: labels.json: " + err.Error())
	}
	messages = make(map[string]string, len(htmls))
	for k, v := range htmls {
		messages[k] = textContent(v)
	}
}

// message mirrors getMessage: a known key resolves to its text, an unknown
// key comes back unchanged.
func message(key string) string {
	if t, ok := messages[key]; ok {
		return t
	}
	return key
}

// statusLabel is statusName: the getStatus key, with the PEND branch swapped
// to PEND_SD/PEND_APP depending on whether the record already has an
// international registration number, then resolved through the message
// table. Icons and extended information are on — that is the configuration
// the collected trademarks.tsv was produced under.
func statusLabel(raw string, hasIRN bool) string {
	key := statusKey(raw)
	if strings.Contains(key, "PEND") {
		if hasIRN {
			key = strings.ReplaceAll(key, "PEND", "PEND_SD")
		} else {
			key = strings.ReplaceAll(key, "PEND", "PEND_APP")
		}
		return message("STATUS_" + key)
	}
	return message("STATUS_" + key)
}

// statusKey is getStatus under extendedStatusInformation + icons: the four
// chained replaces, each touching only its first occurrence.
func statusKey(raw string) string {
	if raw == "" {
		return ""
	}
	b := strings.ToUpper(raw)
	b = strings.Replace(b, "DEL", "INA", 1)
	b = strings.Replace(b, "ACT", "ROM_ACT", 1)
	b = strings.Replace(b, "INA", "ROM_INA", 1)
	b = strings.Replace(b, "DEL", "INA", 1)
	return b
}

// textContent flattens an HTML message to the text a cell shows: tags drop
// out (a <br> contributes nothing) and entities decode — the tokenizer
// handles both exactly like the DOM's textContent would.
func textContent(s string) string {
	z := html.NewTokenizer(strings.NewReader(s))
	var b strings.Builder
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			return b.String()
		}
		if tt == html.TextToken {
			b.Write(z.Text())
		}
	}
}
