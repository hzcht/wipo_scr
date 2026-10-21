//go:build windows

package browser

import (
	"context"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// spawnTimeout bounds the external command left in the failure path. taskkill
// takes a second or two on a busy machine; a wedged one must not stall the
// worker.
const spawnTimeout = 15 * time.Second

// killWindows force-kills the processes of the wanted profiles and logs
// everything it can: it runs on the failure path, where the log is the only
// record of why the window survived. It is the Windows half of Kill; see
// kill.go.
func killWindows(wanted map[string]struct{}, label string) {
	pids := matchedPids(wanted, true)
	if len(pids) == 0 {
		log.Printf("browser: no processes found on %s", label)
		return
	}
	log.Printf("browser: force-killing %d process(es) for %s", len(pids), label)
	for _, pid := range pids {
		taskkill(pid)
	}
}

// sweepWindows kills any process on one of the wanted profiles, and says
// nothing when there is none — Sweep runs on the happy path, and the periodic
// sweep runs it across every worker profile.
func sweepWindows(wanted map[string]struct{}) {
	for _, pid := range matchedPids(wanted, false) {
		log.Printf("browser: process %d stayed behind after the close, killing", pid)
		taskkill(pid)
	}
}

// matchedPids lists the pids whose command line carries a --user-data-dir that
// is in the wanted set. The lookup is one native scan of the process table; the
// command line of each Chromium is read straight out of the kernel. On the
// failure path (verbose) every process that carries a profile is logged,
// matched or not: when a window survives, the log shows the --user-data-dir it
// actually carries and the comparison can be fixed instead of guessed at.
func matchedPids(wanted map[string]struct{}, verbose bool) []int {
	procs, err := chromiumCmdLines()
	if err != nil {
		log.Printf("browser: list processes: %v", err)
		return nil
	}
	var pids []int
	for _, p := range procs {
		profile := profileFromCmdLine(p.cmd)
		if profile == "" {
			continue
		}
		_, want := wanted[normalizePath(profile)]
		if verbose {
			log.Printf("browser: pid %d carries --user-data-dir %q (expecting %s)",
				p.pid, profile, wantedLabels(wanted))
		}
		if want {
			pids = append(pids, p.pid)
		}
	}
	if len(pids) == 0 && verbose && len(procs) > 0 {
		log.Printf("browser: %d chromium process(es) running, none on %s",
			len(procs), wantedLabels(wanted))
	}
	return pids
}

func wantedLabels(wanted map[string]struct{}) string {
	if len(wanted) == 1 {
		for k := range wanted {
			return k
		}
	}
	out := make([]string, 0, len(wanted))
	for k := range wanted {
		out = append(out, k)
	}
	return strings.Join(out, " or ")
}

// taskkill ends a process and its children. /T matters because the browser is a
// process tree and killing only the root would orphan the renderers; /F because
// a wedged Chromium ignores the polite request.
func taskkill(pid int) {
	ctx, cancel := context.WithTimeout(context.Background(), spawnTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "taskkill", "/F", "/T", "/PID", strconv.Itoa(pid))
	hide(cmd)
	out, err := cmd.CombinedOutput()
	msg := strings.TrimSpace(string(out))
	switch {
	case err != nil && msg != "":
		log.Printf("browser: taskkill %d: %v: %s", pid, err, msg)
	case err != nil:
		log.Printf("browser: taskkill %d: %v", pid, err)
	case msg != "":
		log.Printf("browser: taskkill %d: %s", pid, msg)
	default:
		log.Printf("browser: taskkill %d: ok", pid)
	}
}

func hide(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
}
