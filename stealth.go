package main

import (
	_ "embed"
	"os"
	"strings"
)

// The three payloads below are injected with AddInitScript, so they run before
// any of the page's own scripts and before the bot detection that usually
// happens at load time.
//
// They are embedded by filename, which pins them into the binary: a run cannot
// be broken by a half-edited file next to the executable.

//go:embed "stealth.min.js"
var StealthJS string

//go:embed "chrome_stealth.js"
var ChromeStealthJS string

//go:embed "fingerprint.js"
var FingerprintJS string

// Scripts is the injection order: the broad puppeteer-extra evasions first,
// then the Chrome-specific patches (window.chrome, permissions, isTrusted),
// then the small values that must stay consistent with the randomized user
// agent and viewport the browser was launched with.
//
// WIPOS_STEALTH selects the payload set, because these are load-bearing in the
// wrong direction.
//
// chrome_stealth.js must not be injected, and the reason is specific: it
// replaces Element.prototype.addEventListener globally and passes every site
// handler a shallow copy of the event (Object.assign({}, event)) so it can set
// isTrusted on it. The copy is a plain object, so the monitor's own handlers
// stop seeing a real event. Measured on 2026-09-29 against romarin 16117: with
// chrome_stealth.js the simple-search autocomplete never binds — zero
// suggestions, no terms.jsp, Enter submits nothing, and the run sits on the home
// page's "Loading..." grid until it times out. Without it, the same run gets 45
// suggestions, a select.jsp POST and 163 results. The puppeteer-extra payload
// already covers window.chrome and the rest, so nothing of value is lost.
//
// WIPOS_STEALTH=off,fp,stealth,chrome,stealth,fp,... exist to re-test any of it.
func Scripts() []string {
	all := map[string]string{"stealth": StealthJS, "chrome": ChromeStealthJS, "fp": FingerprintJS}
	want := os.Getenv("WIPOS_STEALTH")
	if want == "" {
		want = "stealth,fp"
	}
	var out []string
	for _, name := range strings.Split(want, ",") {
		if js, ok := all[strings.TrimSpace(name)]; ok {
			out = append(out, js)
		}
	}
	return out
}
