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

func TestMatchesProfile(t *testing.T) {
	const profile = `C:\wipos\profiles\worker-1`
	cases := []struct {
		name string
		cmd  string
		want bool
	}{
		{"quoted", `"C:\x\chrome.exe" --user-data-dir="C:\wipos\profiles\worker-1" --headless`, true},
		{"bare", `chrome.exe --user-data-dir=C:\wipos\profiles\worker-1`, true},
		{"case and separators", `chrome.exe --user-data-dir=c:/WIPOS/Profiles/WORKER-1/`, true},
		{"quoted trailing separator", `chrome.exe --user-data-dir="C:\wipos\profiles\worker-1\"`, true},
		{"bare trailing separator", `chrome.exe --user-data-dir=C:\wipos\profiles\worker-1\ --headless`, true},
		{"other worker", `chrome.exe --user-data-dir=C:\wipos\profiles\worker-2`, false},
		{"worker-10 not worker-1", `chrome.exe --user-data-dir=C:\wipos\profiles\worker-10`, false},
		{"different root", `chrome.exe --user-data-dir=D:\wipos\profiles\worker-1`, false},
		{"no marker", `chrome.exe --type=renderer`, false},
	}
	for _, c := range cases {
		if got := matchesProfile(c.cmd, profile); got != c.want {
			t.Errorf("%s: matchesProfile(%q) = %v, want %v", c.name, c.cmd, got, c.want)
		}
	}
}
