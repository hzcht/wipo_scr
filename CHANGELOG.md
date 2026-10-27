# Changelog

All notable changes to the Wisconsin IP collector are listed here, newest first.
Each entry points at the commit that carried it.

## 2026-10-26 — data integrity and the memory behind it

### A 403 is read from the status line, never from the body
The replay client decided a refusal by looking for the substrings `403` and
`forbidden` anywhere in the response — and a *successful* answer is the result
documents themselves, which legitimately contain both: a mark called
`403 FORBIDDEN`, a holder named `FORBIDDEN CITY CULTURE DEVELOPMENT`, an IRN or
a postal code that happens to be 403. In the run this was measured on, **141 of
201 attempts (70%)** were thrown away as blocks while their pages were
perfectly good — each one also cooling down a working endpoint and rotating the
proxy, so the run collected 19 pages and finished 0 queries in 40 minutes.

- `gradeResponse` now takes the status code from `doRequest`: 403 → blocked,
  anything else non-200 → transport error, 200 → handed to the JSON parser,
  which grades it by structure (`response`, `error`) instead of by wording.
- A body check only runs on something that is **not** JSON — an HTML stub from a
  proxy, a wall, a maintenance screen — where wording is all there is.
- `site.IsForbidden` requires the two halves to stand next to each other
  (`403 Forbidden`, `403: Forbidden`) and refuses JSON outright.
- Tests carry a realistic 47 KB docs payload with both words inside the data;
  the old tests only had three-word strings, which is why they passed.

### Every page is held to a contract, and a walk is held against its count
A hole used to be able to open one page at a time: an empty or clamped answer
advanced the checkpoint exactly like a full one, while `results.txt` went on
claiming rows that were never written. Measured on the existing output: 6
queries short by **28,600 rows**, every gap a multiple of 100 — whole pages.

- replay: the answer must be at the offset that was asked for, must hold rows,
  and must hold exactly `rows_per_page` unless it is the last page. Any other
  answer is an error, so the checkpoint cannot move and the attempt is retried.
- browser: a page whose position line promises rows but parses into none is an
  error; a page that parses into fewer (the parser drops a record with neither
  brand nor id) is logged.
- `finishQuery` refuses to mark a query collected when the walk is short by a
  page or more: the count is written, the query is **not** added to `done.lst`,
  its checkpoint is dropped, and `short.lst` records
  `query<TAB>count<TAB>rows<TAB>date`. The next run re-walks it and dedupe
  fills only what is missing. A second shortfall in a row is accepted — by then
  the gap is in the data, not in the walk — and keeps its line as the marker.
- `max_pages` no longer records a partial walk as collected: the checkpoint
  stays and a later run continues, which is what the cap is for.
- the checkpoint now carries the row total beside the page number
  (`<page>\t<rows>`), so a run that resumes at page 500 still accounts for the
  499 pages before it. An older page-only checkpoint still reads; the row total
  is then unknown and the check is skipped rather than guessed.
- sub-page gaps are accepted on purpose: the site repeats records across a page
  boundary, and chasing those would be a walk that never ends.

### The dedupe index holds only the queries that are open
The index was one 8-byte hash per collected row for *everything ever
collected*: 1.33 GB on disk (~166 M hashes) and ~5.5 GB of RSS a fifth of the
way into the input, growing linearly — that run cannot finish.

- hashes are now grouped per query in memory and dropped the moment the query
  is recorded (`Sink.Forget`), so the heap holds the queries in flight.
- the file is a magic header plus `[query length][query][n][hashes]` blocks. A
  start reads it end to end, keeps the open queries and rewrites the rest away:
  a free compaction, because the file had to be read anyway. Startup goes from
  loading a gigabyte to milliseconds, and the file never grows past the open
  queries again.
- a torn tail stops the pass at the last whole block and is truncated away.
- a file without the header is the old flat layout, which cannot say which query
  a hash belongs to — it falls back to one full scan of the rows files (progress
  in `trademarks.tsv.idx.progress-log`), now skipping the rows of finished
  queries. **The first start after this change performs that scan once.**
- `-reindex` does the same scan deliberately, and honours `done.lst` too.

### The proxy precheck actually uses the proxy
- HTTP(S) endpoints must answer **200** (429 accepted: the site is rate
  limiting, the tunnel works). A 502/407/403 stub used to count as alive and
  then fail every request of the run.
- SOCKS5 is checked through a real dial plus an HTTP request over the tunnel,
  with the deadline the old bare port probe lacked. A port that spoke nothing
  used to pass in "1 ms" and then refuse every tunneled connection at runtime.
- SOCKS4 has no client in the toolkit, so it keeps the port probe but is
  flagged `PortOnly`: the log says the tunnel was not verified, and its
  handshake never seeds the pool's latency.
- the probe reads the body, so the seeded latency is a page load rather than a
  first byte.

### Also
- `save_full` is off in `config.toml` and `config.toml.example` (the comment
  already said "off"; the value said `true`) — ~0.5 MB per query over this
  input is hundreds of GB of HTML nobody reads.
- `short_file` (`-short-file`, default `short.lst`) joins the config.
- `Store.AppendRecord` writes a record line while membership stays on the name.

## 2026-10-23 — row dedupe index

### Startup no longer re-scans the rows files
The dedupe set is now kept beside the rows output as `trademarks.tsv.idx` —
one 8-byte FNV-1a hash per collected row (~80 MB for ten million rows) instead
of the row keys as strings (multiple GB of heap). On startup the index is
loaded directly; the multi-gigabyte TSV scan runs only when the index is
missing or odd-sized (first run, crash mid-append) and rebuilds it, so every
run after the first starts in seconds. Rotation is unaffected: parts are only
read by the rebuild.

- `-reindex` (also `reindex` in the config) forces the rebuild from the rows
  files and exits right after — the verification/repair option for when the
  rows output was edited or deleted by hand. The rows files stay the source of
  truth, the index is a cache of them.
- an index append that fails is logged and never fails a batch whose rows are
  already on disk; the next start just falls back to a scan;
- batch-internal duplicates (the same record twice in one page) are now
  dropped too.

### Progress in its own file
A full rebuild reports every 10 s — percent, GB done/total, current file, ETA —
into `trademarks.tsv.idx.progress-log`, not the run log: the ticks are noise
that would otherwise accumulate in `run.log` forever. The main log keeps one
line about the sizes at the start and one summary at the end.

## 2026-10-21 — instant sweeps

### Native process lookup (8166341 and forward)
The sweep used to run one `Get-CimInstance` per call through powershell.exe —
a couple of seconds of module loading and a console host each time. It is now a
native scan: a Toolhelp snapshot for pids and names, then the command line of
every Chromium read straight out of its PEB (`ReadProcessMemory`; the
NtQueryInformationProcess command-line class answers zeroed out on hardened
builds, so the PEB path is the one that works). A sweep takes milliseconds, and
no console flashes on the failure path anymore.

### Sweeps on a schedule (same commit)
- `SweepAll` runs once **before** the first browser launches: leftovers of a
  crashed run die at startup instead of living until the next close.
- `sweep_secs` (default 180, `-sweep-secs`, 0 = off) starts a background sweep
  that repeats while the run lasts — a window that slips past the per-close
  sweeps is now reaped within minutes on its own.
- Rotated rows parts (`trademarks.tsv.N`) are gitignored.

## 2026-10 — hanging browser windows

### Post-close browser sweep (8166341)
The graceful context close reported success while the Chromium tree occasionally
stayed behind, leaving empty windows on the desktop and eating memory. Three
layers were added:

- after a successful close, one CIM query verifies that no process is left on
  the worker's profile directory; any leftover is `taskkill`ed with a log line
  (`process N stayed behind after the close, killing`);
- the browser's own launch page (about:blank) is reused instead of opening a
  second tab, so every window holds exactly one tab and never a blank one;
  init scripts still apply — Playwright evaluates them on every navigation;
- any popup page the site spawns is closed as it appears (it was the exact
  shape of the hanging windows), and `main` sweeps every worker profile after
  the driver is stopped — a killed driver orphans its Chromium children.

### Close diagnostics and timeouts (ef24f15)
Every browser exit is logged with its stage, stall time and duration; the
process lookup logs the `--user-data-dir` of every process it sees, the kill
result of every `taskkill`, and a warning when a window's process carries a
profile that the matcher did not recognize. PowerShell and taskkill calls run
under a 15 s timeout so a wedged WMI subsystem cannot hold the failure path.

### Force-kill backstop (9b9e973)
The worker's close runs against a 5 s deadline: past it, the browser is located
by its profile directory and terminated outright (`internal/browser` package:
`Kill`, CIM process matching by `--user-data-dir`, `taskkill /F /T`). Driver-side
`ErrTargetClosed` responses are treated as success; a panic inside the close
call cannot take the collector down.

## 2026-10-13 — output rotation (1d07315)
`trademarks.tsv` is renamed to `trademarks.tsv.N` once it passes a size cap
(default 256 MB, `rows_max_bytes` in the config or `-rows-max-bytes` on the
command line). At startup every part is loaded oldest-to-newest, so dedupe
still works across rotations. The per-line scan cap is 64 MB.

## 2026-10-12 — proxy and ban handling (d5d1b31, cfca157)
- all TLS verification is skipped: self-signed *proxy* certificates are a fact
  of life and the origin certificate is not verifiable through a MITM anyway;
- WIPO's `403: Forbidden` counts as a block: cool down, rotate the proxy;
- the site's "An error occurred processing your request… Contact Madrid"
  window also counts as a ban.

## 2026-10-07 — initial collector (e1f0aef)
Persistent per-worker Chromium profiles behind rotating proxies, keyword and
owner search flows, grid/row collection with rotation, fallback HTTP replay,
watchdog that kills a wedged attempt, block-streak profile wipes, proxy health
probing and rate gates per proxy.
