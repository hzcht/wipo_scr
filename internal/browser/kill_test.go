package browser

import "testing"

func TestProfileFromCmdLine(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want string
	}{
		{"quoted", `chrome.exe --user-data-dir="C:\p\worker-1" --x`, `C:\p\worker-1`},
		{"bare", `chrome.exe --user-data-dir=C:\p\worker-1 --x`, `C:\p\worker-1`},
		{"last", `chrome.exe --x --user-data-dir=C:\p\worker-1`, `C:\p\worker-1`},
		{"no marker", `chrome.exe --type=renderer`, ""},
		{"empty cmd", "", ""},
	}
	for _, c := range cases {
		if got := profileFromCmdLine(c.cmd); got != c.want {
			t.Errorf("%s: profileFromCmdLine(%q) = %q, want %q", c.name, c.cmd, got, c.want)
		}
	}
}

func TestNormalizePathKeys(t *testing.T) {
	// The same profile written by different writers (Playwright's launch,
	// Get-CimInstance, our own filepath.Join) must land on one normalized key,
	// and worker-1 must never share a key with worker-10.
	wanted := map[string]struct{}{normalizePath(`C:\wipos\profiles\worker-1`): {}}
	same := []string{
		`C:\wipos\profiles\worker-1`,
		`c:/wipos/profiles/worker-1/`,
		`C:\WIPOS\Profiles\WORKER-1`,
		`C:\wipos\profiles\worker-1\`,
	}
	for _, p := range same {
		if _, ok := wanted[normalizePath(p)]; !ok {
			t.Errorf("normalizePath(%q) is not in the worker-1 key set", p)
		}
	}
	other := []string{
		`C:\wipos\profiles\worker-10`,
		`C:\wipos\profiles\worker-2`,
		`D:\wipos\profiles\worker-1`,
	}
	for _, p := range other {
		if _, ok := wanted[normalizePath(p)]; ok {
			t.Errorf("normalizePath(%q) wrongly matches worker-1", p)
		}
	}
}
