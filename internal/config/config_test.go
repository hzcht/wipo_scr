package config

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// parse runs the same path main does — defaults, then a TOML file, then flags —
// with the arguments the test wants instead of os.Args.
func parse(t *testing.T, toml string, args ...string) Config {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if toml != "" {
		if err := os.WriteFile(cfgPath, []byte(toml), 0644); err != nil {
			t.Fatalf("write config: %v", err)
		}
	}
	c := DefaultConfig()
	fs := flag.NewFlagSet("wipos", flag.ContinueOnError)
	fs.SetOutput(&strings.Builder{})
	// os.Args[1:] is what ParseFlags reads, so the test has to put its
	// arguments there for the duration of the call.
	old := os.Args
	os.Args = append([]string{"wipos", "-config", cfgPath}, args...)
	defer func() { os.Args = old }()
	c.ParseFlags(fs)
	return c
}

func TestDefaultsAreUsable(t *testing.T) {
	c := DefaultConfig()
	if err := c.Validate(); err != nil {
		t.Errorf("the built-in defaults do not validate: %v", err)
	}
	if c.ProxyList != "proxy.lst" || c.Input != "task.txt" {
		t.Errorf("unexpected defaults: input=%q proxy=%q", c.Input, c.ProxyList)
	}
}

func TestPrecedenceDefaultsTomlFlags(t *testing.T) {
	toml := `
input = "from_toml.txt"
threads = 7
rows_per_page = 30
headless = true
max_pages = 3
save_pdf = true
split_pages = true
`
	c := parse(t, toml, "-threads", "2", "-rows-per-page", "60", "-headless=false", "-input", "from_flag.txt")
	if c.Input != "from_flag.txt" {
		t.Errorf("input = %q, want the flag value", c.Input)
	}
	if c.Threads != 2 {
		t.Errorf("threads = %d, want 2 from the flag", c.Threads)
	}
	if c.RowsPerPage != 60 {
		t.Errorf("rows_per_page = %d, want 60 from the flag", c.RowsPerPage)
	}
	// A false flag must be able to switch a true TOML value back off, which is
	// the whole reason these flags are parsed with flag.ContinueOnError semantics
	// and not as "set if present".
	if c.Headless {
		t.Error("headless = true, want -headless=false to override the TOML value")
	}
	if !c.SavePDF {
		t.Error("save_pdf = false, want the TOML value to survive a run without that flag")
	}
	if c.MaxPages != 3 {
		t.Errorf("max_pages = %d, want 3 from the TOML", c.MaxPages)
	}
	if !c.SplitPages {
		t.Error("split_pages = false, want the TOML value to survive a run without that flag")
	}
}

func TestMissingConfigFileIsNotAnError(t *testing.T) {
	// Defaults plus flags are a complete configuration on their own.
	c := parse(t, "", "-config", filepath.Join(t.TempDir(), "nope.toml"))
	if c.Input != DefaultConfig().Input {
		t.Errorf("input = %q, want the default", c.Input)
	}
}

func TestZeroAndFalseFlagsDoOverride(t *testing.T) {
	// Zero and false are real values here: "no page cap" and "run with windows",
	// which is what you need when a run behaves differently from the file's
	// settings. A flag that was not typed at all must leave the file alone.
	toml := "max_pages = 5\nsubmit_gap_ms = 3000\nheadless = true\nsplit_pages = true\n"
	c := parse(t, toml, "-max-pages", "0", "-submit-gap", "0", "-headless=false", "-split-pages=false")
	if c.MaxPages != 0 {
		t.Errorf("max_pages = %d, want 0 — every page", c.MaxPages)
	}
	if c.SubmitGapMS != 0 {
		t.Errorf("submit_gap_ms = %d, want 0 — no gap", c.SubmitGapMS)
	}
	if c.Headless {
		t.Error("headless = true, want -headless=false to switch it off")
	}
	if c.SplitPages {
		t.Error("split_pages = true, want -split-pages=false to switch it off")
	}
	// Untouched keys keep the file's values.
	if c.RowsPerPage != DefaultConfig().RowsPerPage {
		t.Errorf("rows_per_page = %d, want the default", c.RowsPerPage)
	}
}

func TestModePrecedence(t *testing.T) {
	// The default is the validated replay engine; the file may pin the browser
	// path, and the flag has the last word either way.
	if c := parse(t, ""); c.Mode != "replay" {
		t.Errorf("mode = %q, want the default \"replay\"", c.Mode)
	}
	c := parse(t, "mode = \"browser\"\n")
	if c.Mode != "browser" || c.Replay() {
		t.Errorf("mode = %q (replay=%v), want \"browser\" from the TOML", c.Mode, c.Replay())
	}
	c = parse(t, "mode = \"browser\"\n", "-mode", "replay")
	if c.Mode != "replay" || !c.Replay() {
		t.Errorf("mode = %q (replay=%v), want \"replay\" from the flag", c.Mode, c.Replay())
	}
	// An unknown engine would silently collect nothing, so it is a config error.
	c = parse(t, "")
	c.Mode = "headless-browser"
	if err := c.Validate(); err == nil {
		t.Error("Validate accepted mode=\"headless-browser\"")
	}
}

func TestValidateRejectsUnusableSettings(t *testing.T) {
	tests := map[string]func(*Config){
		"no input":           func(c *Config) { c.Input = "" },
		"no data dir":        func(c *Config) { c.DataDir = "" },
		"no results file":    func(c *Config) { c.ResultsFile = "" },
		"no rows file":       func(c *Config) { c.RowsFile = "" },
		"zero threads":       func(c *Config) { c.Threads = 0 },
		"zero max attempts":  func(c *Config) { c.MaxAttempts = 0 },
		"odd page size":      func(c *Config) { c.RowsPerPage = 42 },
		"negative max pages": func(c *Config) { c.MaxPages = -1 },
		"negative start":     func(c *Config) { c.StartPage = -1 },
		"negative end":       func(c *Config) { c.EndPage = -1 },
		// An empty shard is always a mistake, and a silent one: the process
		// starts a browser, runs a search and stops, reporting success.
		"end before start":   func(c *Config) { c.StartPage = 500; c.EndPage = 499 },
		"tiny nav timeout":   func(c *Config) { c.NavTimeoutMS = 10 },
		"tiny results wait":  func(c *Config) { c.ResultsTimeoutMS = 10 },
		"negative pause all": func(c *Config) { c.AllThreadsPauseDuration = -1 },
	}
	for name, break_ := range tests {
		c := DefaultConfig()
		break_(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: Validate accepted it", name)
		}
	}
}

func TestShardPagesPrecedence(t *testing.T) {
	// A shard is set once and then left alone for hours, so where the numbers
	// came from has to be as predictable as everything else: 0 means "open",
	// and a flag has to be able to pull a shard back to the whole result set.
	toml := "start_page = 100\nend_page = 200\n"
	c := parse(t, toml)
	if c.StartPage != 100 || c.EndPage != 200 {
		t.Fatalf("from TOML: pages %d-%d, want 100-200", c.StartPage, c.EndPage)
	}
	c = parse(t, toml, "-start-page", "4000", "-end-page", "7999")
	if c.StartPage != 4000 || c.EndPage != 7999 {
		t.Errorf("from flags: pages %d-%d, want 4000-7999", c.StartPage, c.EndPage)
	}
	c = parse(t, toml, "-start-page", "0", "-end-page", "0")
	if c.StartPage != 0 || c.EndPage != 0 {
		t.Errorf("from flags: pages %d-%d, want 0-0 — the whole result set", c.StartPage, c.EndPage)
	}
}

func TestWriteRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "generated.toml")
	c := DefaultConfig()
	if err := c.Write(path); err != nil {
		t.Fatalf("Write: %v", err)
	}
	var back Config
	if err := back.LoadFile(path); err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if back.Input != c.Input || back.Threads != c.Threads || back.RowsPerPage != c.RowsPerPage {
		t.Errorf("round trip changed the config:\n got %+v\nwant %+v", back, c)
	}
	// A file this tool wrote must be readable by the tool that wrote it.
	if err := back.Validate(); err != nil {
		t.Errorf("the generated file does not validate: %v", err)
	}
}
