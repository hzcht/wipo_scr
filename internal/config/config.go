// Package config holds every runtime setting of the WIPO collector.
//
// Precedence: built-in defaults < config.toml < command-line flags.
package config

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// Config is the effective configuration. Every path is resolved relative to
// the working directory, so the binary is meant to be run from the project
// root (that is also where the runtime files live).
type Config struct {
	// Input is a text file with one search term per line (a domain, a brand —
	// whatever the monitor accepts).
	Input string `toml:"input"`
	// DataDir is where the collected result pages land.
	DataDir string `toml:"data_dir"`
	// ResultsFile collects one "date \t query \t count" line per collected
	// query — the trademark count the monitor reported.
	ResultsFile string `toml:"results_file"`
	// RowsFile collects one "date time \t query \t <grid columns>" line per
	// trademark record, tab separated.
	RowsFile string `toml:"rows_file"`

	// DoneFile is the append-only list of queries already collected: it is what
	// makes a run resumable, and it has to stay put between runs.
	DoneFile string `toml:"done_file"`
	// FailedFile is the append-only list of queries that never got a fair shot
	// (max_attempts reached). Those are retried on the next run, so this file is
	// a record of what to look at, not a blacklist.
	FailedFile string `toml:"failed_file"`

	// Threads is the number of concurrent browsers. Each one owns a
	// persistent Chromium profile, so this is also the number of windows on
	// screen and roughly the number of Chromium processes. 2–4 is the stable
	// range; much more and Windows starts thrashing.
	Threads int `toml:"threads"`

	// SplitPages gives every thread its own block of each query's result pages
	// instead of handing the whole query to one of them. It is what makes
	// -threads matter for a single huge input such as "*": without it, three of
	// four browsers wait while the fourth walks all 15,430 pages. The cost is
	// that every shard searches for itself, so a large list of small queries is
	// better left unsplit.
	SplitPages bool `toml:"split_pages"`
	// MaxAttempts caps re-tries per query. A query that keeps failing ends up
	// in failed.lst and is retried on the next run.
	MaxAttempts int `toml:"max_attempts"`

	// ProxyList is a file with one proxy per line (http://, https://,
	// socks4://, socks5://). Empty or missing means direct connections —
	// which does not work from this host, WIPO does not answer a bare IP.
	ProxyList string `toml:"proxy_list"`
	// ProxyProbeURL is the endpoint every proxy is health-checked against
	// before the first browser starts.
	ProxyProbeURL string `toml:"proxy_probe_url"`
	// ProfilesDir holds one persistent Chromium profile per worker, so
	// cookies and local storage survive attempts and restarts.
	ProfilesDir string `toml:"profiles_dir"`
	// UaFile adds user agent strings to the built-in list, one per line. The
	// list is fixed for the whole run: a UA that changes halfway through is a
	// different machine claiming the same cookies.
	UaFile string `toml:"ua_file"`

	// Mode selects how a query is executed. "replay" posts the monitor's own
	// select.jsp form over net/http — no browser, ~1–3s a page — and is the
	// validated default: the rows it produces are byte-identical to the grid.
	// "browser" drives Chromium through Playwright, the original path, which
	// is also what a replay falls back to when a response is not a result
	// page (site change, block, maintenance).
	Mode string `toml:"mode"`

	// Headless runs the browsers without windows. Faster and lighter, but a
	// headless browser is a louder bot signal — and page.PDF only works
	// headless, so SavePDF needs it.
	Headless bool `toml:"headless"`

	// SaveGrid writes the raw results grid HTML per page into DataDir
	// (<query>_p<N>.raw). This is the source the TSV rows are parsed from.
	SaveGrid bool `toml:"save_grid"`
	// SaveFull additionally writes the whole results page once per query
	// (<query>_full.html). Big (~0.5 MB per query) and mostly redundant once
	// the grid is stored, so it is off by default.
	SaveFull bool `toml:"save_full"`
	// SavePDF prints the first results page to DataDir/<query>.pdf. Chromium
	// only prints in headless mode; in a headed run the call is skipped with
	// a log line instead of failing per query.
	SavePDF bool `toml:"save_pdf"`

	// RowsPerPage is the grid page size (the site offers 10/30/60/100).
	// Bigger means fewer page loads for a large result set.
	RowsPerPage int `toml:"rows_per_page"`
	// MaxPages caps how many result pages one query walks. 0 = every page.
	MaxPages int `toml:"max_pages"`

	// StartPage / EndPage bound which result pages one query walks, in grid
	// page numbers. 0 on either side means "open" — the default pair walks the
	// whole result set. They exist so several processes can split one huge
	// crawl: the "*" query is 15,430 pages, about 11 hours for one browser,
	// and every process has to start its own search anyway, so the only way to
	// use a second core is to let it skip ahead through the "Go to page" box
	// and take a different slice. Each shard needs its own done file, or the
	// first one to finish marks the query collected and the rest do nothing.
	StartPage int `toml:"start_page"`
	EndPage   int `toml:"end_page"`

	// SubmitGapMS / SubmitJitterMS hold the minimum pause between two search
	// submits from the same IP. WIPO scores the source IP, so this is the
	// main brake on getting blocked. Set the gap to 0 to crawl at full speed
	// and accept the block risk.
	SubmitGapMS       int `toml:"submit_gap_ms"`
	SubmitJitterMS    int `toml:"submit_jitter_ms"`
	TaskPauseMS       int `toml:"task_pause_ms"`
	TaskPauseJitterMS int `toml:"task_pause_jitter_ms"`

	// NavTimeoutMS caps a single page load, ResultsTimeoutMS caps the wait for
	// a results grid to appear or change. Both are what a healthy link needs; a
	// slow proxy gets them scaled by the pool's measured latency, against
	// ProxyLatencyRefMS and bounded by TimeoutScaleMin/Max.
	NavTimeoutMS     int `toml:"nav_timeout_ms"`
	ResultsTimeoutMS int `toml:"results_timeout_ms"`

	// WatchdogMS is how long one step may make no progress before the attempt is
	// abandoned. It is a silence budget rather than a total budget, so it does
	// not cap the page walk: a 15,430-page crawl beats on every page and never
	// goes quiet, while a CDP call that hangs behind a stalled renderer does.
	// 0 derives it from the two timeouts above.
	WatchdogMS int `toml:"watchdog_ms"`

	// ProxyFailBudget retires a proxy after this many hard failures.
	ProxyFailBudget int `toml:"proxy_fail_budget"`
	// ProxyCooldownMS parks a proxy that served a block page.
	ProxyCooldownMS int `toml:"proxy_cooldown_ms"`
	// ProxyProbeTimeoutMS caps one up-front liveness probe.
	ProxyProbeTimeoutMS int `toml:"proxy_probe_timeout_ms"`
	// ProxyProbeConcurrency is how many probes run at once.
	ProxyProbeConcurrency int `toml:"proxy_probe_concurrency"`
	// ProxyLatencyRefMS is what one request step costs on a healthy link to
	// this site. It is the yardstick: a proxy measured at 12s against a 3s
	// reference gets four times the timeout, up to TimeoutScaleMax.
	ProxyLatencyRefMS int `toml:"proxy_latency_ref_ms"`
	// TimeoutScaleMin / TimeoutScaleMax clamp that stretch. The floor is there
	// because Playwright timeouts also cover the site's own render time, which
	// has nothing to do with the proxy, and the ceiling because one dying
	// endpoint must not hold the queue for an hour.
	TimeoutScaleMin float64 `toml:"timeout_scale_min"`
	TimeoutScaleMax float64 `toml:"timeout_scale_max"`

	// ErrorsTemplate is a text file with one "action@::@text" marker per
	// line: sleepN (this worker pauses), skip (requeue), pauseAll (every
	// worker pauses). Missing file disables marker handling.
	ErrorsTemplate string `toml:"errors_template"`
	// AllThreadsPauseDuration is how long a pauseAll marker freezes the fleet.
	AllThreadsPauseDuration int `toml:"all_threads_pause_duration"`
}

// Replay reports whether queries run through the HTTP replay engine. Validate
// guarantees Mode is exactly "replay" or "browser".
func (c Config) Replay() bool { return c.Mode == "replay" }

// Watchdog returns how long a single step may make no progress before the
// attempt is abandoned, deriving it from the step budgets when the config does
// not set one.
//
// It is a silence budget, not a total budget: the worker beats on every step and
// every collected page, and the watchdog only asks "has anything moved lately".
// So the number it needs to clear is the longest budget any one step is allowed
// to take — scaled all the way up, since a slow endpoint is the case the
// watchdog exists for. Deriving it any lower would kill attempts that were
// still inside their own timeouts.
func (c Config) Watchdog() time.Duration {
	if c.WatchdogMS > 0 {
		return time.Duration(c.WatchdogMS) * time.Millisecond
	}
	step := time.Duration(c.NavTimeoutMS) * time.Millisecond
	if r := time.Duration(c.ResultsTimeoutMS) * time.Millisecond; r > step {
		step = r
	}
	if c.TimeoutScaleMax > 1 {
		step = time.Duration(float64(step) * c.TimeoutScaleMax)
	}
	return step + 30*time.Second
}

// DefaultConfig returns the built-in defaults.
func DefaultConfig() Config {
	return Config{
		Input:       "task.txt",
		DataDir:     "./data",
		ResultsFile: "results.txt",
		RowsFile:    "trademarks.tsv",
		DoneFile:    "done.lst",
		FailedFile:  "failed.lst",

		Threads:     3,
		SplitPages:  false,
		MaxAttempts: 5,

		ProxyList:     "proxy.lst",
		ProxyProbeURL: "https://www3.wipo.int/madrid/monitor/en/",
		ProfilesDir:   "profiles",
		UaFile:        "ua.txt",

		Mode: "replay",

		Headless: false,

		SaveGrid: true,
		SaveFull: false,
		SavePDF:  false,

		RowsPerPage: 100,
		MaxPages:    0,
		StartPage:   0,
		EndPage:     0,

		SubmitGapMS:       3000,
		SubmitJitterMS:    2500,
		TaskPauseMS:       2000,
		TaskPauseJitterMS: 4000,

		NavTimeoutMS:     120000,
		ResultsTimeoutMS: 180000,
		WatchdogMS:       0,

		ProxyFailBudget:       5,
		ProxyCooldownMS:       900000,
		ProxyProbeTimeoutMS:   12000,
		ProxyProbeConcurrency: 8,
		ProxyLatencyRefMS:     3000,
		TimeoutScaleMin:       1.0,
		TimeoutScaleMax:       4.0,

		ErrorsTemplate:          "errors.lst",
		AllThreadsPauseDuration: 300,
	}
}

// LoadFile overlays values from a TOML file. A missing file is not an error —
// the defaults plus the flags are a complete configuration on their own.
func (c *Config) LoadFile(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return toml.Unmarshal(raw, c)
}

// ParseFlags applies CLI flags on top of the TOML file. The flag set is a
// parameter on purpose: the global one can only be defined once per process, so
// a test could not parse anything twice.
//
// -gen-config writes a default template to the -config path and exits before any
// browser work.
func (c *Config) ParseFlags(fs *flag.FlagSet) {
	cfgPath := fs.String("config", "config.toml", "path to the TOML config file (optional)")
	genConfig := fs.Bool("gen-config", false, "write a default config template to the -config path and exit")
	input := fs.String("input", "", "input file with search terms, one per line")
	dataDir := fs.String("data-dir", "", "directory for the collected result pages")
	resultsFile := fs.String("results-file", "", "file collecting 'date \\t query \\t count' lines")
	rowsFile := fs.String("rows-file", "", "file collecting 'date time \\t query \\t <grid columns>' lines")
	doneFile := fs.String("done-file", "", "file with the queries already collected (makes a run resumable)")
	failedFile := fs.String("failed-file", "", "file with the queries that ran out of attempts")
	threads := fs.Int("threads", 0, "number of concurrent browsers")
	splitPages := fs.Bool("split-pages", false, "split every query's pages across the threads (for one huge query)")
	maxAttempts := fs.Int("max-attempts", 0, "max attempts per query before it goes to failed.lst")
	proxyList := fs.String("proxy", "", "file with proxies, one per line (empty = direct)")
	profilesDir := fs.String("profiles-dir", "", "directory for the persistent browser profiles")
	uaFile := fs.String("ua-file", "", "file with extra user agent strings, one per line")
	mode := fs.String("mode", "", "query engine: replay (http post, no browser) or browser (playwright)")
	headless := fs.Bool("headless", false, "run the browsers headless (required for save_pdf)")
	saveGrid := fs.Bool("save-grid", false, "save the raw results grid per page")
	saveFull := fs.Bool("save-full", false, "also save the whole results page per query")
	savePDF := fs.Bool("save-pdf", false, "print the first results page to PDF (headless only)")
	rowsPerPage := fs.Int("rows-per-page", 0, "grid page size: 10, 30, 60 or 100")
	maxPages := fs.Int("max-pages", 0, "max result pages per query (0 = all)")
	startPage := fs.Int("start-page", 0, "first result page to collect (0 = 1), for splitting a crawl across processes")
	endPage := fs.Int("end-page", 0, "last result page to collect (0 = the site's own last page)")
	submitGap := fs.Int("submit-gap", 0, "minimum ms between two search submits from the same IP")
	taskPause := fs.Int("task-pause", 0, "base ms pause between two queries in one worker")
	navTimeout := fs.Int("nav-timeout", 0, "ms cap for a single page load (scaled up for a slow proxy)")
	resultsTimeout := fs.Int("results-timeout", 0, "ms cap for waiting on a results grid (scaled up for a slow proxy)")
	watchdog := fs.Int("watchdog", 0, "ms a single step may go without progress (0 = derive from the timeouts)")
	proxyFailBudget := fs.Int("proxy-fail-budget", 0, "hard failures before a proxy is retired")
	proxyCooldown := fs.Int("proxy-cooldown", 0, "ms a proxy that served a block page is parked")
	proxyProbeTimeout := fs.Int("proxy-probe-timeout", 0, "ms cap on one proxy liveness probe")
	proxyProbeConcurrency := fs.Int("proxy-probe-concurrency", 0, "probes in flight at once")
	proxyLatencyRef := fs.Int("proxy-latency-ref", 0, "ms one request step costs on a healthy link, the yardstick for the timeout scale")
	timeoutScaleMin := fs.Float64("timeout-scale-min", 0, "floor for the timeout scale a slow proxy gets")
	timeoutScaleMax := fs.Float64("timeout-scale-max", 0, "ceiling for the timeout scale a slow proxy gets")
	proxyProbeURL := fs.String("proxy-probe-url", "", "URL fetched through every proxy during the precheck")
	submitJitter := fs.Int("submit-jitter", 0, "random extra ms on top of the submit gap")
	taskPauseJitter := fs.Int("task-pause-jitter", 0, "random extra ms on top of the pause between queries")
	errorsTemplate := fs.String("errors-template", "", "file with error markers 'action@::@text', one per line")
	pauseAll := fs.Int("pause-all-duration", 0, "seconds every worker pauses on a pauseAll marker")
	fs.Parse(os.Args[1:])

	if *genConfig {
		template := DefaultConfig()
		if err := template.Write(*cfgPath); err != nil {
			log.Fatalf("gen-config: %v", err)
		}
		log.Printf("config template written to %s", *cfgPath)
		os.Exit(0)
	}

	if err := c.LoadFile(*cfgPath); err != nil {
		log.Fatalf("config %s: %v", *cfgPath, err)
	}

	// Only the flags that were actually typed get applied, and they get applied
	// as given. That is what makes "-headless=false" and "-submit-gap 0" work:
	// asking for a value the file has is a legitimate override, and zero is a
	// real value for a pause or a page cap.
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "input":
			c.Input = *input
		case "data-dir":
			c.DataDir = *dataDir
		case "results-file":
			c.ResultsFile = *resultsFile
		case "rows-file":
			c.RowsFile = *rowsFile
		case "done-file":
			c.DoneFile = *doneFile
		case "failed-file":
			c.FailedFile = *failedFile
		case "proxy":
			c.ProxyList = *proxyList
		case "profiles-dir":
			c.ProfilesDir = *profilesDir
		case "ua-file":
			c.UaFile = *uaFile
		case "mode":
			c.Mode = *mode
		case "errors-template":
			c.ErrorsTemplate = *errorsTemplate
		case "threads":
			c.Threads = *threads
		case "split-pages":
			c.SplitPages = *splitPages
		case "max-attempts":
			c.MaxAttempts = *maxAttempts
		case "rows-per-page":
			c.RowsPerPage = *rowsPerPage
		case "max-pages":
			c.MaxPages = *maxPages
		case "start-page":
			c.StartPage = *startPage
		case "end-page":
			c.EndPage = *endPage
		case "submit-gap":
			c.SubmitGapMS = *submitGap
		case "task-pause":
			c.TaskPauseMS = *taskPause
		case "nav-timeout":
			c.NavTimeoutMS = *navTimeout
		case "results-timeout":
			c.ResultsTimeoutMS = *resultsTimeout
		case "watchdog":
			c.WatchdogMS = *watchdog
		case "proxy-fail-budget":
			c.ProxyFailBudget = *proxyFailBudget
		case "proxy-cooldown":
			c.ProxyCooldownMS = *proxyCooldown
		case "proxy-probe-timeout":
			c.ProxyProbeTimeoutMS = *proxyProbeTimeout
		case "proxy-probe-concurrency":
			c.ProxyProbeConcurrency = *proxyProbeConcurrency
		case "proxy-latency-ref":
			c.ProxyLatencyRefMS = *proxyLatencyRef
		case "timeout-scale-min":
			c.TimeoutScaleMin = *timeoutScaleMin
		case "timeout-scale-max":
			c.TimeoutScaleMax = *timeoutScaleMax
		case "proxy-probe-url":
			c.ProxyProbeURL = *proxyProbeURL
		case "submit-jitter":
			c.SubmitJitterMS = *submitJitter
		case "task-pause-jitter":
			c.TaskPauseJitterMS = *taskPauseJitter
		case "pause-all-duration":
			c.AllThreadsPauseDuration = *pauseAll
		case "headless":
			c.Headless = *headless
		case "save-grid":
			c.SaveGrid = *saveGrid
		case "save-full":
			c.SaveFull = *saveFull
		case "save-pdf":
			c.SavePDF = *savePDF
		}
	})
}

// Validate checks that the resulting configuration is usable.
func (c *Config) Validate() error {
	switch {
	case c.Input == "":
		return errors.New("input is required")
	case c.DataDir == "":
		return errors.New("data_dir is required")
	case c.ResultsFile == "":
		return errors.New("results_file is required")
	case c.RowsFile == "":
		return errors.New("rows_file is required")
	case c.DoneFile == "":
		return errors.New("done_file is required")
	case c.FailedFile == "":
		return errors.New("failed_file is required")
	case c.Threads < 1:
		return errors.New("threads must be >= 1")
	case c.Mode != "replay" && c.Mode != "browser":
		return errors.New("mode must be \"replay\" or \"browser\"")
	case c.MaxAttempts < 1:
		return errors.New("max_attempts must be >= 1")
	case c.RowsPerPage != 10 && c.RowsPerPage != 30 && c.RowsPerPage != 60 && c.RowsPerPage != 100:
		return errors.New("rows_per_page must be one of 10, 30, 60, 100 (the sizes the site offers)")
	case c.MaxPages < 0:
		return errors.New("max_pages must be >= 0 (0 = every page)")
	case c.StartPage < 0:
		return errors.New("start_page must be >= 0 (0 = the first page)")
	case c.EndPage < 0:
		return errors.New("end_page must be >= 0 (0 = the site's last page)")
	case c.StartPage > 1 && c.EndPage > 0 && c.EndPage < c.StartPage:
		return fmt.Errorf("end_page (%d) is before start_page (%d): the shard is empty", c.EndPage, c.StartPage)
	case c.NavTimeoutMS < 1000:
		return errors.New("nav_timeout_ms must be >= 1000")
	case c.ResultsTimeoutMS < 1000:
		return errors.New("results_timeout_ms must be >= 1000")
	case c.WatchdogMS < 0:
		return errors.New("watchdog_ms must be >= 0 (0 = derive it from the timeouts)")
	case c.ProxyFailBudget < 1:
		return errors.New("proxy_fail_budget must be >= 1")
	case c.ProxyCooldownMS < 0:
		return errors.New("proxy_cooldown_ms must be >= 0")
	case c.ProxyProbeTimeoutMS < 1000:
		return errors.New("proxy_probe_timeout_ms must be >= 1000")
	case c.ProxyProbeConcurrency < 1:
		return errors.New("proxy_probe_concurrency must be >= 1")
	case c.ProxyLatencyRefMS < 1:
		return errors.New("proxy_latency_ref_ms must be >= 1 — it is the yardstick every proxy is measured against")
	case c.TimeoutScaleMin <= 0:
		return errors.New("timeout_scale_min must be > 0")
	case c.TimeoutScaleMax < c.TimeoutScaleMin:
		return fmt.Errorf("timeout_scale_max (%g) is below timeout_scale_min (%g): no budget would ever grow",
			c.TimeoutScaleMax, c.TimeoutScaleMin)
	case c.AllThreadsPauseDuration < 0:
		return errors.New("all_threads_pause_duration must be >= 0")
	}
	return nil
}

// Write serializes the config to a TOML file.
func (c *Config) Write(path string) error {
	raw, err := toml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0644)
}

// Log prints the effective configuration so a run leaves a trace of what it
// was told to do.
func (c *Config) Log() {
	log.Printf("config: input=%s data_dir=%s results=%s rows=%s mode=%s threads=%d max_attempts=%d",
		c.Input, c.DataDir, c.ResultsFile, c.RowsFile, c.Mode, c.Threads, c.MaxAttempts)
	log.Printf("config: proxy_list=%s profiles=%s headless=%v save[grid=%v full=%v pdf=%v]",
		c.ProxyList, c.ProfilesDir, c.Headless, c.SaveGrid, c.SaveFull, c.SavePDF)
	log.Printf("config: rows_per_page=%d max_pages=%d submit_gap=%dms(+%d) task_pause=%dms(+%d) timeouts[nav=%dms results=%dms watchdog=%s]",
		c.RowsPerPage, c.MaxPages, c.SubmitGapMS, c.SubmitJitterMS,
		c.TaskPauseMS, c.TaskPauseJitterMS, c.NavTimeoutMS, c.ResultsTimeoutMS, c.Watchdog().Round(time.Millisecond))
	log.Printf("config: proxy[tuning fail_budget=%d cooldown=%ds probe=%dms/%d ref=%dms scale=%.1f..%.1f] probe_url=%s",
		c.ProxyFailBudget, c.ProxyCooldownMS/1000, c.ProxyProbeTimeoutMS, c.ProxyProbeConcurrency,
		c.ProxyLatencyRefMS, c.TimeoutScaleMin, c.TimeoutScaleMax, c.ProxyProbeURL)
	log.Printf("config: errors_template=%s pause_all=%ds", c.ErrorsTemplate, c.AllThreadsPauseDuration)
	log.Printf("config: pages %s..%s of every query%s%s", pageBound(c.StartPage), pageBound(c.EndPage),
		pageNote(c.StartPage, c.EndPage), splitNote(c.SplitPages, c.Threads))
}

// pageBound renders a page bound for the startup log: 0 means "open".
func pageBound(n int) string {
	if n <= 0 {
		return "1"
	}
	return strconv.Itoa(n)
}

// pageNote spells out what an open bound actually means, because a run that
// prints only "1..1" reads like a one-page job.
func pageNote(start, end int) string {
	switch {
	case start <= 1 && end == 0:
		return " (whole result set)"
	case end == 0:
		return " (shard: from here to the site's last page; give this process its own done file)"
	default:
		return " (shard: give this process its own done file)"
	}
}

// splitNote reports the in-process split, spelled out because "split" alone does
// not say into how many pieces or what happens when threads are 1.
func splitNote(split bool, threads int) string {
	if !split {
		return ""
	}
	if threads <= 1 {
		return ", split_pages on but threads=1 (no split)"
	}
	return fmt.Sprintf(", split over %d threads", threads)
}
