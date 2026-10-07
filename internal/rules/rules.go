// Package rules reacts to markers found in a page body.
//
// It exists for the moment the monitor starts pushing back: instead of every
// worker reloading the same block page, one marker can park a worker for a
// while (sleepN), hand the query back to the queue (skip) or freeze the whole
// fleet (pauseAll).
package rules

import (
	"bufio"
	"log"
	"os"
	"strconv"
	"strings"
)

const separator = "@::@"

// Rule maps one body marker to the action taken on a match.
// Actions: sleepN (N seconds, this worker only), skip, pauseAll.
type Rule struct {
	Action string
	Marker string
}

// Rules is an ORDERED list. Order matters: Match returns the first rule found
// in the body, in file order, so overlapping markers behave deterministically
// (iterating a map would pick at random).
type Rules struct {
	rules []Rule
}

// Load parses a text file with one "action@::@text" line per rule. Bad lines
// are logged and skipped — a typo in the rules file must not stop a run.
func Load(path string) (*Rules, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	r := &Rules{}
	seen := map[string]struct{}{}
	sc := bufio.NewScanner(f)
	for lineNo := 1; sc.Scan(); lineNo++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, separator, 2)
		if len(parts) != 2 {
			log.Printf("errors template %s:%d: bad line %q, want action@::@text", path, lineNo, line)
			continue
		}
		action := strings.TrimSpace(parts[0])
		marker := strings.TrimSpace(parts[1])
		if _, ok := ActionSeconds(action); !ok && action != "skip" && action != "pauseAll" {
			log.Printf("errors template %s:%d: unknown action %q (want sleepN, skip or pauseAll)", path, lineNo, action)
			continue
		}
		if marker == "" {
			log.Printf("errors template %s:%d: empty marker text", path, lineNo)
			continue
		}
		if _, dup := seen[marker]; dup {
			log.Printf("errors template %s:%d: duplicate marker %q, the first rule wins", path, lineNo, marker)
			continue
		}
		seen[marker] = struct{}{}
		r.rules = append(r.rules, Rule{Action: action, Marker: marker})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return r, nil
}

// Len returns the number of loaded rules.
func (r *Rules) Len() int { return len(r.rules) }

// Match returns the action and marker of the first rule found in body, in file
// order, or ("", "") when nothing matches.
func (r *Rules) Match(body string) (action, marker string) {
	for _, rule := range r.rules {
		if strings.Contains(body, rule.Marker) {
			return rule.Action, rule.Marker
		}
	}
	return "", ""
}

// ActionSeconds parses a "sleepN" action into N seconds. N outside 1..3600 is
// rejected so a fat-fingered "sleep0" cannot turn into a busy loop.
func ActionSeconds(action string) (int, bool) {
	if !strings.HasPrefix(action, "sleep") {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(action, "sleep"))
	if err != nil || n < 1 || n > 3600 {
		return 0, false
	}
	return n, true
}
