//go:build !windows

package browser

// killWindows and sweepWindows are no-ops off Windows. The collector's process
// management is aimed at the Windows builds of Chromium it launches in
// production; elsewhere the graceful context close in worker.close is the only
// path.
func killWindows(wanted map[string]struct{}, label string) {}

func sweepWindows(wanted map[string]struct{}) {}
