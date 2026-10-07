package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
)

// Logs go to ./logs/run.log and to stdout.
//
// A long run over tens of thousands of queries writes a lot: every attempt,
// every proxy rotation, every page transition. The file rotates on size during
// the run (run.log → run.log.1 → …, oldest dropped) and the live file is always
// run.log, so tailing it keeps working while the run goes on.
const (
	maxLogBytes  = 32 << 20 // rotate once run.log passes 32 MB
	keepLogFiles = 5        // run.log.1 … run.log.5 are kept
)

// rotatingWriter is a size-capped append-only file writer with N backups.
type rotatingWriter struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	keep     int
	f        *os.File
	written  int64
}

// newRotatingWriter opens path for appending, starting at its current size so
// the cap also holds across restarts.
func newRotatingWriter(path string, maxBytes int64, keep int) (*rotatingWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, err
	}
	var size int64
	if fi, serr := f.Stat(); serr == nil {
		size = fi.Size()
	}
	return &rotatingWriter{path: path, maxBytes: maxBytes, keep: keep, f: f, written: size}, nil
}

// Write appends p, rotating first when the cap would be crossed. A failed
// rotation never loses the line: the previous file is reopened for append and,
// failing even that, writes fall back to stderr.
func (rw *rotatingWriter) Write(p []byte) (int, error) {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	if rw.f == nil {
		return os.Stderr.Write(p)
	}
	if rw.written > 0 && rw.written+int64(len(p)) > rw.maxBytes {
		if err := rw.rotateLocked(); err != nil {
			fmt.Fprintf(os.Stderr, "log rotation failed: %v\n", err)
		}
	}
	n, err := rw.f.Write(p)
	rw.written += int64(n)
	return n, err
}

// Close releases the handle.
func (rw *rotatingWriter) Close() error {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	if rw.f == nil {
		return nil
	}
	err := rw.f.Close()
	rw.f = nil
	return err
}

// rotateLocked shifts run.log.N to run.log.N+1 (dropping the oldest), renames
// run.log to run.log.1 and opens a fresh run.log.
func (rw *rotatingWriter) rotateLocked() error {
	_ = rw.f.Close()
	rw.f = nil
	_ = os.Remove(fmt.Sprintf("%s.%d", rw.path, rw.keep))
	for i := rw.keep - 1; i >= 1; i-- {
		from := fmt.Sprintf("%s.%d", rw.path, i)
		if _, err := os.Stat(from); err == nil {
			_ = os.Rename(from, fmt.Sprintf("%s.%d", rw.path, i+1))
		}
	}
	if err := os.Rename(rw.path, rw.path+".1"); err != nil {
		// The old file is still in place: keep appending to it.
		if prev, perr := os.OpenFile(rw.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); perr == nil {
			rw.f = prev
			if fi, serr := prev.Stat(); serr == nil {
				rw.written = fi.Size()
			}
		}
		return err
	}
	f, err := os.OpenFile(rw.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	rw.f = f
	rw.written = 0
	fmt.Fprintf(os.Stderr, "log rotated: %s -> %s.1\n", rw.path, rw.path)
	return nil
}

// setupLogging mirrors the standard logger into ./logs/run.log. It runs before
// anything else so a crash during startup still leaves a trace.
func setupLogging() (*rotatingWriter, error) {
	const logDir = "logs"
	if err := os.MkdirAll(logDir, os.ModePerm); err != nil {
		return nil, err
	}
	rw, err := newRotatingWriter(filepath.Join(logDir, "run.log"), maxLogBytes, keepLogFiles)
	if err != nil {
		return nil, err
	}
	log.SetOutput(io.MultiWriter(os.Stdout, rw))
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	return rw, nil
}
