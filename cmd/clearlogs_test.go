package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClearLogs_NoLogDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stdout, _, err := runCmd(t, "clear-logs", "--yes")
	if err != nil {
		t.Fatalf("clear-logs with no log dir should not fail: %v", err)
	}
	if !strings.Contains(stdout, "No log directory") {
		t.Errorf("expected 'No log directory' message, got: %q", stdout)
	}
}

func TestClearLogs_EmptyLogDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Create empty log dir
	logDir := filepath.Join(home, ".bo", "log")
	if err := os.MkdirAll(logDir, 0700); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := runCmd(t, "clear-logs", "--yes")
	if err != nil {
		t.Fatalf("clear-logs with empty log dir failed: %v", err)
	}
	if !strings.Contains(stdout, "No log files") {
		t.Errorf("expected 'No log files' message, got: %q", stdout)
	}
}

func TestClearLogs_AutoConfirm(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	logDir := filepath.Join(home, ".bo", "log")
	if err := os.MkdirAll(logDir, 0700); err != nil {
		t.Fatal(err)
	}

	// Create two fake log files.
	for _, name := range []string{"bo-2024-01-01.log", "bo-2024-01-02.log"} {
		if err := os.WriteFile(filepath.Join(logDir, name), []byte("log content"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	stdout, _, err := runCmd(t, "clear-logs", "--yes")
	if err != nil {
		t.Fatalf("clear-logs --yes failed: %v", err)
	}
	if !strings.Contains(stdout, "Deleted") {
		t.Errorf("expected 'Deleted' in output, got: %q", stdout)
	}

	// Verify files are gone.
	entries, _ := os.ReadDir(logDir)
	if len(entries) != 0 {
		t.Errorf("expected log dir to be empty after clear-logs, got %d files", len(entries))
	}
}
