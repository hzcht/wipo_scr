// Command wipos collects Madrid Monitor trademark data for a list of search
// terms: how many trademarks each term has, and the rows of the results grid.
//
// The layout mirrors the oc_scr collector: main wires everything, the workers
// in worker.go/navigate.go do the crawling, and internal/site holds everything
// that is specific to WIPO.
package main

import (
	"flag"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	playwright "github.com/mxschmitt/playwright-go"

	"wipos/internal/browser"
	"wipos/internal/config"
	"wipos/internal/proxy"
	"wipos/internal/queue"
	"wipos/internal/rules"
	"wipos/internal/site"
)

// proxyTuning builds the pool's options from the config. A free proxy that
// answers in 400 ms and one that answers in 25 s are both "working" to a
// boolean check, but only the second needs its timeouts stretched, so the
// reference latency and the scale bounds are part of the pool's setup.
func proxyTuning(cfg config.Config) proxy.Options {
	return proxy.Options{
		MaxFails:   cfg.ProxyFailBudget,
		Cooldown:   time.Duration(cfg.ProxyCooldownMS) * time.Millisecond,
		LatencyRef: time.Duration(cfg.ProxyLatencyRefMS) * time.Millisecond,
		ScaleMin:   cfg.TimeoutScaleMin,
		ScaleMax:   cfg.TimeoutScaleMax,
	}
}

func main() {
	logFile, err := setupLogging()
	if err != nil {
		log.SetOutput(os.Stderr)
		log.Fatalf("logging: %v", err)
	}
	defer func() { _ = logFile.Close() }()

	// (1) effective configuration
	cfg := config.DefaultConfig()
	cfg.ParseFlags(flag.CommandLine)
	if err := cfg.Validate(); err != nil {
		log.Fatalf("config: %v", err)
	}
	cfg.Log()

	if _, err := os.Stat(cfg.Input); err != nil {
		log.Fatalf("input %s: %v", cfg.Input, err)
	}
	if err := os.MkdirAll(cfg.DataDir, os.ModePerm); err != nil {
		log.Fatalf("data dir %s: %v", cfg.DataDir, err)
	}
	if err := os.MkdirAll(cfg.ProfilesDir, os.ModePerm); err != nil {
		log.Fatalf("profiles dir %s: %v", cfg.ProfilesDir, err)
	}

	// (2) user agents. Fixed for the whole run: an identity that changes
	// halfway through is a different machine claiming the same cookies.
	if added := browser.LoadUserAgents(cfg.UaFile); added > 0 {
		log.Printf("ua file %s: %d extra user agents, %d in the pool", cfg.UaFile, added, len(browser.UserAgents))
	}

	// (3) proxies. WIPO does not answer this host's bare IP, so a list is
	// expected here; a missing or empty one means direct mode, and the log says
	// so in as many words.
	// Proxies. WIPO does not answer this host's bare IP, so a list is expected
	// here; a missing or empty one means direct mode, and the log says so in as
	// many words.
	//
	// The precheck runs a few probes at a time, so a list of 200 dead free
	// proxies is swept in well under a minute. Its latency is the only speed
	// measurement available before the first page load, so it is fed into the
	// pool rather than logged and dropped: without it the first attempt runs on
	// a budget sized for the fastest proxy in the list.
	var pool *proxy.Pool
	proxies, listErr := queue.LoadLines(cfg.ProxyList)
	if listErr != nil || len(proxies) == 0 {
		reason := "empty"
		if listErr != nil {
			reason = listErr.Error()
		}
		log.Printf("proxy list %s: %s — running WITHOUT proxies (direct)", cfg.ProxyList, reason)
		pool = proxy.New(nil, proxyTuning(cfg))
	} else {
		log.Printf("proxy list %s: %d entries, probing them before the first browser starts", cfg.ProxyList, len(proxies))
		probes := proxy.HealthCheck(proxies, proxy.ProbeOptions{
			URL:         cfg.ProxyProbeURL,
			Timeout:     time.Duration(cfg.ProxyProbeTimeoutMS) * time.Millisecond,
			Concurrency: cfg.ProxyProbeConcurrency,
		})
		proxy.LogProbes(probes)
		alive := proxy.Alive(probes)
		if dropped := len(proxies) - len(alive); dropped > 0 {
			log.Printf("proxy precheck: %d of %d endpoints dropped (dead port, refused tunnel)", dropped, len(proxies))
		}
		if len(alive) == 0 {
			log.Printf("proxy precheck: nothing survived — running WITHOUT proxies (direct)")
		}
		pool = proxy.New(alive, proxyTuning(cfg))
		pool.Seed(probes)
	}

	// (4) marker rules. A missing file is not fatal: without it a block page is
	// simply an attempt that failed.
	var rl *rules.Rules
	if r, err := rules.Load(cfg.ErrorsTemplate); err != nil {
		log.Printf("errors template %s: %v — no marker handling", cfg.ErrorsTemplate, err)
	} else {
		rl = r
		log.Printf("errors template %s: %d rules", cfg.ErrorsTemplate, r.Len())
	}

	// (5) browsers
	if err := playwright.Install(); err != nil {
		log.Fatalf("playwright install: %v", err)
	}
	pw, err := playwright.Run()
	if err != nil {
		log.Fatalf("playwright run: %v", err)
	}
	defer func() { _ = pw.Stop() }()

	// (6) what is already handled
	done, err := queue.LoadStore(cfg.DoneFile)
	if err != nil {
		log.Fatalf("%s: %v", cfg.DoneFile, err)
	}
	failed, err := queue.LoadStore(cfg.FailedFile)
	if err != nil {
		log.Fatalf("%s: %v", cfg.FailedFile, err)
	}
	if *migrateLegacy {
		migrateLegacyState(done)
	}
	log.Printf("%s: %d queries already collected, %s: %d queries that need another try",
		cfg.DoneFile, done.Len(), cfg.FailedFile, failed.Len())

	tasks, err := queue.BuildTasks(cfg.Input, done)
	if err != nil {
		log.Fatalf("input %s: %v", cfg.Input, err)
	}
	log.Printf("input %s: %d queries to collect (failed.lst entries are retried)", cfg.Input, len(tasks))
	if len(tasks) == 0 {
		return
	}
	queries := len(tasks)
	if cfg.SplitPages && cfg.Threads > 1 {
		// Expansion happens before the queue is built: each piece is an ordinary
		// task, so the channel spreads one huge query over every worker instead
		// of giving it to one and leaving the rest idle.
		tasks = queue.ShardTasks(tasks, cfg.Threads)
		log.Printf("split_pages: %d queries over %d page blocks each (%d tasks)", queries, cfg.Threads, len(tasks))
	} else if cfg.SplitPages {
		log.Printf("split_pages: on, but threads=1 — no split")
	}

	// (7) shared dependencies
	sink := site.NewSink(cfg.ResultsFile, cfg.RowsFile, cfg.DataDir, cfg.SaveGrid, cfg.SaveFull)
	if err := sink.Load(); err != nil {
		log.Fatalf("%s: %v", cfg.ResultsFile, err)
	}
	q := queue.NewQueue(tasks, cfg.Threads, time.Duration(cfg.AllThreadsPauseDuration)*time.Second)
	var wg sync.WaitGroup
	deps := &Deps{
		Cfg:       cfg,
		PW:        pw,
		Queue:     q,
		Rules:     rl,
		Proxy:     pool,
		Gate:      browser.NewGatePool(time.Duration(cfg.SubmitGapMS)*time.Millisecond, time.Duration(cfg.SubmitJitterMS)*time.Millisecond),
		Done:      done,
		Failed:    failed,
		Sink:      sink,
		Collected: &atomic.Int64{},
		Scripts:   Scripts(),
		WG:        &wg,
		Shards:    newShardGate(),
	}

	// (8) run
	wg.Add(cfg.Threads)
	for i := 0; i < cfg.Threads; i++ {
		go ProcPages(deps, i)
	}
	q.WaitAndClose()
	wg.Wait()

	// queries, not tasks: a split run has tasks = queries x threads, and the
	// collected counter only advances when a query is finished as a whole.
	log.Printf("finished: %d/%d queries collected", deps.Collected.Load(), queries)
	log.Printf("proxy pool summary:%s", pool.Summary())
}

// migrateLegacy seeds done.lst from the files the previous collector kept:
// "lost" (queries with no trademarks) and "counters" ("<query> \t 1 - 7 / 7").
// Without it a fresh run would collect thousands of queries all over again.
// It is a one-off: run it once, then forget the flag exists.
var migrateLegacy = flag.Bool("migrate-legacy", false, "seed done_file from the old lost/counters files, then continue")

func migrateLegacyState(done *queue.Store) {
	for _, path := range []string{"lost", "counters"} {
		lines, err := queue.LoadLines(path)
		if err != nil {
			log.Printf("migrate %s: %v", path, err)
			continue
		}
		for _, line := range lines {
			done.Append(strings.Split(line, "\t")[0]) // counters lines carry the pager text
		}
		log.Printf("migrate %s: %d entries considered", path, len(lines))
	}
}
