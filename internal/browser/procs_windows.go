//go:build windows

package browser

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// procline is one Chromium process worth checking: pid, image name and
// command line.
type procline struct {
	pid  int
	name string
	cmd  string
}

// chromiumImageNames is the set of image names a Playwright Chromium can run
// under: the regular channel is chrome.exe, the bundled headless shell is one
// of the other two depending on the Playwright version. They are compared
// case-insensitively.
var chromiumImageNames = []string{"chrome.exe", "headless_shell.exe", "chrome-headless-shell.exe"}

// unicodeString16 is the NTDLL UNICODE_STRING: two USHORT lengths, a pad to 8
// bytes on x64, then the pointer to the payload. It is read out of the target
// process byte by byte, so it holds raw numbers, not Go pointers.
type unicodeString16 struct {
	Length uint16
	Max    uint16
	_      uint32
	Buffer uint64
}

// x64 layout of the pieces the command line hangs off of:
// PEB.ProcessParameters is PEB+0x20 and RTL_USER_PROCESS_PARAMETERS.CommandLine
// is parameters+0x70. Both have been fixed on every x64 Windows ever released,
// and unlike NtQueryInformationProcess's ProcessCommandLineInformation this
// path is not gated by the app-control hardening of recent builds: it needs
// PROCESS_VM_READ, which our own running browser grants.
const (
	pebToParams         = 0x20
	paramsToCommandLine = 0x70
)

// chromiumCmdLines lists every Chromium process alive with its command line.
//
// This used to be one Get-CimInstance call through powershell.exe per sweep —
// module loading alone cost a couple of seconds and a console host was spawned
// for it. The native path is a Toolhelp snapshot for pids and names plus one
// short read per Chromium pid: milliseconds all in, fast enough to sweep
// every minute.
func chromiumCmdLines() ([]procline, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap)

	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	if err := windows.Process32First(snap, &entry); err != nil {
		return nil, err
	}
	var out []procline
	var lastErr error
	failures, queries := 0, 0
	for {
		name := windows.UTF16ToString(entry.ExeFile[:])
		if isChromiumImage(name) {
			queries++
			cmd, err := cmdLineOf(uint32(entry.ProcessID))
			switch {
			case err == nil:
				out = append(out, procline{pid: int(entry.ProcessID), name: name, cmd: cmd})
				lastErr = nil
			case isGone(err):
				// The process exited between the snapshot and the query; the
				// list only matters for the living.
				lastErr = nil
			default:
				failures++
				lastErr = err
			}
		}
		if err := windows.Process32Next(snap, &entry); err != nil {
			break
		}
	}
	if queries > 0 && failures == queries && lastErr != nil {
		// Not one process could be read: the sweep must not pretend all is
		// fine when the rights that granted it are gone.
		return nil, lastErr
	}
	return out, nil
}

func isChromiumImage(name string) bool {
	for _, want := range chromiumImageNames {
		if strings.EqualFold(name, want) {
			return true
		}
	}
	return false
}

// isGone reports the error class "already finishing": the pid no longer has a
// command line to give, because the process exited between the snapshot and
// the query.
func isGone(err error) bool {
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) ||
		errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return true
	}
	var q *queryError
	if errors.As(err, &q) {
		return q.status == statusInvalidCid
	}
	return false
}

// statusInvalidCid is what NtQueryInformationProcess answers when the pid was
// taken by the time the query ran.
const statusInvalidCid = 0xC000000B

// cmdLineOf reads one process's command line: PEB → parameters → the
// UNICODE_STRING at parameters+0x70, whose payload is then read as UTF-16.
func cmdLineOf(pid uint32) (string, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ, false, pid)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)

	var pbi [6]uintptr
	var retLen uint32
	status, _, _ := ntQuery.Call(uintptr(h), 0, /* ProcessBasicInformation */
		uintptr(unsafe.Pointer(&pbi[0])), uintptr(unsafe.Sizeof(pbi)),
		uintptr(unsafe.Pointer(&retLen)))
	if status != 0 {
		return "", &queryError{status: status}
	}
	peb := pbi[1]
	if peb == 0 {
		return "", &queryError{status: status, what: "PEB is nil"}
	}
	params, err := readPointer(h, uint64(peb)+pebToParams)
	if err != nil {
		return "", err
	}
	cmd, err := readUnicodeString(h, params+paramsToCommandLine)
	if err != nil {
		return "", err
	}
	return cmd, nil
}

var ntQuery = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtQueryInformationProcess")

// readPointer reads one 8-byte pointer out of the target process.
func readPointer(h windows.Handle, addr uint64) (uint64, error) {
	var buf [8]byte
	if err := readMemory(h, addr, buf[:]); err != nil {
		return 0, err
	}
	return *(*uint64)(unsafe.Pointer(&buf[0])), nil
}

// readUnicodeString reads the UNICODE_STRING at a fixed address and returns
// the string it points at.
func readUnicodeString(h windows.Handle, addr uint64) (string, error) {
	var head [16]byte
	if err := readMemory(h, addr, head[:]); err != nil {
		return "", err
	}
	us := (*unicodeString16)(unsafe.Pointer(&head[0]))
	if us.Buffer == 0 || us.Length == 0 || us.Length > 1<<15 {
		return "", nil
	}
	raw := make([]byte, int(us.Length)+2) // the trailing NUL comes along
	addr = us.Buffer
	if err := readMemory(h, addr, raw); err != nil {
		return "", err
	}
	w := unsafe.Slice((*uint16)(unsafe.Pointer(&raw[0])), cap(raw)/2)
	return strings.TrimSuffix(string(utf16.Decode(w)), "\x00"), nil
}

// errPartialCopy is what ReadProcessMemory answers when the process vanished
// under our hands: the read window was no longer mapped.
var errPartialCopy = errors.New("ReadProcessMemory: partial copy")

// readMemory copies src bytes from another process into dst.
func readMemory(h windows.Handle, addr uint64, dst []byte) error {
	err := windows.ReadProcessMemory(h, uintptr(addr), &dst[0], uintptr(len(dst)), nil)
	if err != nil {
		if errors.Is(err, windows.ERROR_PARTIAL_COPY) {
			return errPartialCopy
		}
		return fmt.Errorf("ReadProcessMemory 0x%x: %w", addr, err)
	}
	return nil
}

// queryError renders an NTSTATUS a log can read.
type queryError struct {
	status uintptr
	what   string
}

func (e *queryError) Error() string {
	msg := fmt.Sprintf("NtQueryInformationProcess status 0x%08X", e.status)
	if e.what != "" {
		msg = msg + ": " + e.what
	}
	return msg
}
