//go:build !windows

package browser

// killBrowsers is a no-op off Windows. The collector's process management is
// aimed at the Windows builds of Chromium it launches in production; elsewhere
// the graceful context close in worker.close is the only path.
func killBrowsers(profileDir string) {}
