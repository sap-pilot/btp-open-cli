package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// initFileLog opens (or creates) the daily log file at ~/.bo/log/bo_YYYY-MM-DD.log,
// writes a header line with the current timestamp and command arguments, then
// replaces os.Stdout and os.Stderr with pipes whose read ends are tee'd into
// both the original terminal FDs and the log file concurrently.
//
// It returns a teardown function that must be called after rootCmd.Execute()
// returns. The teardown flushes all buffered output, restores os.Stdout /
// os.Stderr to their original values, and closes the log file.
//
// On any setup error the function degrades gracefully: logging is skipped and
// a no-op teardown is returned so the CLI continues to work normally.
func initFileLog() func() {
	home, err := os.UserHomeDir()
	if err != nil {
		return func() {}
	}
	logDir := filepath.Join(home, ".bo", "log")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return func() {}
	}

	date := time.Now().Format("2006-01-02")
	logPath := filepath.Join(logDir, "bo_"+date+".log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return func() {}
	}

	// Header: blank line + timestamp + full command line.
	ts := time.Now().Format("2006-01-02 15:04:05")
	fmt.Fprintf(f, "\n=== %s %s ===\n", ts, strings.Join(os.Args, " "))

	// Mutex-protected writer so concurrent stdout/stderr goroutines don't
	// interleave partial writes inside the log file.
	var mu sync.Mutex
	logWriter := &lockedWriter{mu: &mu, w: f}

	origStdout := os.Stdout
	origStderr := os.Stderr

	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		f.Close()
		return func() {}
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdoutR.Close()
		stdoutW.Close()
		f.Close()
		return func() {}
	}

	os.Stdout = stdoutW
	os.Stderr = stderrW

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(io.MultiWriter(origStdout, logWriter), stdoutR)
	}()
	go func() {
		defer wg.Done()
		io.Copy(io.MultiWriter(origStderr, logWriter), stderrR)
	}()

	return func() {
		// Closing the write ends signals EOF to the copy goroutines.
		stdoutW.Close()
		stderrW.Close()
		wg.Wait()
		os.Stdout = origStdout
		os.Stderr = origStderr
		f.Close()
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (lw *lockedWriter) Write(p []byte) (int, error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return lw.w.Write(p)
}
