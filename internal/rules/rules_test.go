package rules

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "errors.lst")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestLoadAndMatchInFileOrder(t *testing.T) {
	path := write(t, `# marker rules, one per line
sleep45@::@Just a moment...
skip@::@Access Denied
pauseAll@::@Service temporarily unavailable

`)
	r, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if r.Len() != 3 {
		t.Fatalf("Len = %d, want 3", r.Len())
	}

	// The order of the file decides: a page carrying two markers takes the first
	// rule, so the behaviour does not depend on map iteration.
	action, marker := r.Match("Rate limited. Just a moment... Access Denied")
	if action != "sleep45" || marker != "Just a moment..." {
		t.Errorf("Match = %q/%q, want sleep45/Just a moment...", action, marker)
	}
	if action, _ := r.Match("Service temporarily unavailable"); action != "pauseAll" {
		t.Errorf("Match = %q, want pauseAll", action)
	}
	if action, _ := r.Match("a perfectly normal results page"); action != "" {
		t.Errorf("Match = %q, want no match", action)
	}
}

func TestLoadSkipsBadLines(t *testing.T) {
	// A typo in the rules file must not take a run down: the bad lines are
	// dropped and the good ones still work.
	path := write(t, "this line has no separator\n"+
		"explode@::@something\n"+
		"sleep0@::@too eager\n"+
		"skip@::@\n"+
		"skip@::@Access Denied\n"+
		"skip@::@Access Denied\n"+
		"pauseAll@::@maintenance\n")
	r, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if r.Len() != 2 {
		t.Fatalf("Len = %d, want 2 (only the two good, distinct rules)", r.Len())
	}
	if action, _ := r.Match("Access Denied"); action != "skip" {
		t.Errorf("Match(Access Denied) = %q, want skip", action)
	}
}

func TestActionSeconds(t *testing.T) {
	tests := []struct {
		action string
		want   int
		ok     bool
	}{
		{"sleep1", 1, true},
		{"sleep45", 45, true},
		{"sleep3600", 3600, true},
		{"sleep0", 0, false},
		{"sleep3601", 0, false},
		{"sleep", 0, false},
		{"sleep-5", 0, false},
		{"skip", 0, false},
		{"pauseAll", 0, false},
		{"", 0, false},
	}
	for _, tc := range tests {
		got, ok := ActionSeconds(tc.action)
		if got != tc.want || ok != tc.ok {
			t.Errorf("ActionSeconds(%q) = %d/%v, want %d/%v", tc.action, got, ok, tc.want, tc.ok)
		}
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.lst")); err == nil {
		t.Error("a missing rules file must be an error, so main can log it and carry on without rules")
	}
}
