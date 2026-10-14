//go:build windows

package browser

import (
	"encoding/base64"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf16"
)

// win32ProcessFilter selects every process name a Playwright Chromium can run
// under: the regular channel is chrome.exe, the bundled headless shell is one of
// the other two depending on the Playwright version.
const win32ProcessFilter = "Name='chrome.exe' OR Name='headless_shell.exe' OR Name='chrome-headless-shell.exe'"

// killBrowsers finds the Chromium processes started on profileDir and terminates
// them. It is the Windows half of Kill; see kill.go for why it exists.
func killBrowsers(profileDir string) {
	pids := browsersOn(profileDir)
	if len(pids) == 0 {
		return
	}
	log.Printf("browser: force-killing %d process(es) for %s", len(pids), profileDir)
	for _, pid := range pids {
		taskkill(pid)
	}
}

// browsersOn lists the pids whose command line carries profileDir as its
// --user-data-dir. The whole listing is one CIM query rather than a query per
// pid: the process table is large and each CIM call costs the better part of a
// second.
func browsersOn(profileDir string) []int {
	out, err := runPowerShell(listScript())
	if err != nil {
		log.Printf("browser: list processes: %v", err)
		return nil
	}
	var pids []int
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		pidField, cmd, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(pidField))
		if err != nil {
			continue
		}
		if matchesProfile(cmd, profileDir) {
			pids = append(pids, pid)
		}
	}
	return pids
}

// listScript emits one "pid<TAB>commandline" line per matching process. The tab
// separator is a literal tab in the single-quoted format string; Win32 command
// lines never contain one.
func listScript() string {
	return "Get-CimInstance Win32_Process -Filter \"" + win32ProcessFilter + "\"" +
		" | ForEach-Object { '{0}\t{1}' -f $_.ProcessId, $_.CommandLine }"
}

// taskkill ends a process and its children. /T matters because the browser is a
// process tree and killing only the root would orphan the renderers; /F because
// a wedged Chromium ignores the polite request.
func taskkill(pid int) {
	cmd := exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid))
	hide(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			log.Printf("browser: taskkill %d: %v: %s", pid, err, msg)
		}
	}
}

// runPowerShell runs script through powershell.exe and returns its stdout. The
// script travels as -EncodedCommand (base64 UTF-16LE) so no quoting layer can
// mangle it, and the window is hidden so a background collector never flashes a
// console.
func runPowerShell(script string) (string, error) {
	cmd := exec.Command("powershell.exe",
		"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-EncodedCommand", encodeCommand(script))
	hide(cmd)
	out, err := cmd.Output()
	return string(out), err
}

func hide(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
}

// encodeCommand encodes script for PowerShell's -EncodedCommand, which expects
// base64 over UTF-16LE.
func encodeCommand(script string) string {
	units := utf16.Encode([]rune(script))
	buf := make([]byte, len(units)*2)
	for i, u := range units {
		buf[i*2] = byte(u)
		buf[i*2+1] = byte(u >> 8)
	}
	return base64.StdEncoding.EncodeToString(buf)
}
