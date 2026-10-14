package browser

import (
	"path/filepath"
	"strings"
)

// Kill force-stops the Chromium processes that belong to profileDir.
//
// The graceful context close is the normal way to end a browser, but it travels
// through the Playwright driver: when the renderer or the proxy behind it is
// wedged the close call can hang forever, and the error it does return is easy
// to swallow. What is left then is a Chromium window with no pages still holding
// several hundred megabytes. This is the backstop — it locates the browser
// processes by their --user-data-dir, which is unique per worker, and
// terminates them outright. It is best-effort: a caller that only wanted a
// browser gone must never be failed by it.
//
// killBrowsers is the platform half; see kill_windows.go.
func Kill(profileDir string) {
	if profileDir == "" {
		return
	}
	if abs, err := filepath.Abs(profileDir); err == nil {
		profileDir = abs
	}
	killBrowsers(profileDir)
}

// profileFromCmdLine extracts the --user-data-dir value out of a Chromium
// command line. Chromium may quote the value when the path holds spaces, so
// both the quoted and the bare form are handled. It returns "" when the switch
// is absent — a renderer process carries --type=renderer but not the profile.
func profileFromCmdLine(cmd string) string {
	const marker = "--user-data-dir="
	i := indexFold(cmd, marker)
	if i < 0 {
		return ""
	}
	v := cmd[i+len(marker):]
	if strings.HasPrefix(v, `"`) {
		v = v[1:]
		if j := strings.Index(v, `"`); j >= 0 {
			return v[:j]
		}
		return v
	}
	if j := strings.IndexAny(v, " \t"); j >= 0 {
		return v[:j]
	}
	return v
}

// matchesProfile reports whether cmd is the Chromium command line that was
// started on profileDir. Matching is case-insensitive and separator-agnostic,
// and both sides are trimmed of a trailing separator, because Windows paths do
// not compare cleanly by byte. The comparison is exact rather than a prefix:
// worker-1 must not match worker-10.
func matchesProfile(cmd, profileDir string) bool {
	got := normalizePath(profileFromCmdLine(cmd))
	return got != "" && got == normalizePath(profileDir)
}

func normalizePath(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	p = strings.ReplaceAll(p, "/", "\\")
	return strings.TrimRight(p, "\\")
}

// indexFold is a case-insensitive strings.Index. It exists because lowercasing
// the haystack first and indexing into the original would misalign if a rune's
// lowercase form had a different byte length.
func indexFold(s, sub string) int {
	if sub == "" {
		return 0
	}
	ls, lsub := strings.ToLower(s), strings.ToLower(sub)
	for i := 0; i+len(lsub) <= len(ls); i++ {
		if ls[i:i+len(lsub)] == lsub {
			return i
		}
	}
	return -1
}
