# Changelog

All notable changes to the Wisconsin IP collector are listed here, newest first.
Each entry points at the commit that carried it.

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
