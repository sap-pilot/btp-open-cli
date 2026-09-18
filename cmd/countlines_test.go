package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestCountLines_Basic(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "main.go"), "package main\n\nfunc main() {}\n") // 3 lines
	writeFile(t, filepath.Join(dir, "pkg", "util.go"), "package pkg\nvar X = 1")    // 2 lines (no trailing NL)
	writeFile(t, filepath.Join(dir, "README.md"), "# Title\n")                      // 1 line
	writeFile(t, filepath.Join(dir, "image.png"), "not source\n")                   // ignored: not source
	writeFile(t, filepath.Join(dir, "empty.go"), "")                                // 0 lines

	stdout, _, err := runCmd(t, "count-lines", dir)
	if err != nil {
		t.Fatalf("count-lines failed: %v", err)
	}

	if !strings.Contains(stdout, ".go") || !strings.Contains(stdout, ".md") {
		t.Fatalf("expected .go and .md rows, got:\n%s", stdout)
	}
	if strings.Contains(stdout, ".png") {
		t.Errorf("non-source .png file should not be counted:\n%s", stdout)
	}
	// 3 + 2 + 0 = 5 go lines across 3 files; 1 md line; total 6 lines / 4 files.
	if !strings.Contains(stdout, "TOTAL") {
		t.Fatalf("expected TOTAL row, got:\n%s", stdout)
	}
	lastLine := ""
	for _, l := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if strings.HasPrefix(l, "TOTAL") {
			lastLine = l
		}
	}
	fields := strings.Fields(lastLine)
	if len(fields) != 3 || fields[1] != "4" || fields[2] != "6" {
		t.Errorf("expected 'TOTAL 4 6', got %q", lastLine)
	}
}

func TestCountLines_DefaultFolder(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.go"), "package a\n")

	cwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := runCmd(t, "count-lines")
	if err != nil {
		t.Fatalf("count-lines with no arg failed: %v", err)
	}
	if !strings.Contains(stdout, ".go") {
		t.Errorf("expected .go row for default folder, got:\n%s", stdout)
	}
}

func TestCountLines_GitIgnore(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".gitignore"), "dist/\n*.gen.go\nsecret.py\n!keep.gen.go\n")
	writeFile(t, filepath.Join(dir, "app.go"), "package app\n")
	writeFile(t, filepath.Join(dir, "bundle.gen.go"), "package app\n// generated\n")
	writeFile(t, filepath.Join(dir, "keep.gen.go"), "package app\n")
	writeFile(t, filepath.Join(dir, "secret.py"), "TOKEN = 1\n")
	writeFile(t, filepath.Join(dir, "dist", "out.go"), "package dist\n")
	writeFile(t, filepath.Join(dir, "dist", "nested", "more.go"), "package nested\n")

	stdout, _, err := runCmd(t, "count-lines", dir)
	if err != nil {
		t.Fatalf("count-lines failed: %v", err)
	}

	// app.go (1) + keep.gen.go (1, re-included) => 2 go files, 2 lines. No python.
	lastLine := ""
	for _, l := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if strings.HasPrefix(l, "TOTAL") {
			lastLine = l
		}
	}
	fields := strings.Fields(lastLine)
	if len(fields) != 3 || fields[1] != "2" || fields[2] != "2" {
		t.Errorf("expected 'TOTAL 2 2' (gitignore honoured), got %q\nfull:\n%s", lastLine, stdout)
	}
	if strings.Contains(stdout, ".py") {
		t.Errorf("secret.py should be ignored:\n%s", stdout)
	}
}

func TestCountLines_GitDirAlwaysSkipped(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.go"), "package a\n")
	writeFile(t, filepath.Join(dir, ".git", "hooks", "x.go"), "package hooks\nvar A = 1\nvar B = 2\n")

	stdout, _, err := runCmd(t, "count-lines", dir)
	if err != nil {
		t.Fatalf("count-lines failed: %v", err)
	}
	lastLine := ""
	for _, l := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if strings.HasPrefix(l, "TOTAL") {
			lastLine = l
		}
	}
	if !strings.Contains(lastLine, " 1 ") && !strings.HasSuffix(lastLine, " 1") {
		t.Errorf("expected only 1 line counted (.git skipped), got %q", lastLine)
	}
}

func TestCountLines_IgnoresPackageLock(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "app.js"), "const x = 1\n")
	writeFile(t, filepath.Join(dir, "config.json"), "{}\n")
	writeFile(t, filepath.Join(dir, "package-lock.json"), strings.Repeat("\"dep\": {}\n", 5000))
	writeFile(t, filepath.Join(dir, "web", "package-lock.json"), strings.Repeat("x\n", 100))

	stdout, _, err := runCmd(t, "count-lines", dir)
	if err != nil {
		t.Fatalf("count-lines failed: %v", err)
	}

	lastLine := ""
	for _, l := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if strings.HasPrefix(l, "TOTAL") {
			lastLine = l
		}
	}
	fields := strings.Fields(lastLine)
	// app.js (1 line) + config.json (1 line) => 2 files, 2 lines. Lock files excluded.
	if len(fields) != 3 || fields[1] != "2" || fields[2] != "2" {
		t.Errorf("expected 'TOTAL 2 2' (package-lock.json excluded), got %q\nfull:\n%s", lastLine, stdout)
	}
}

func TestCountLines_NotADirectory(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "file.go")
	writeFile(t, f, "package x\n")

	_, _, err := runCmd(t, "count-lines", f)
	if err == nil {
		t.Fatal("expected error when target is not a directory")
	}
}
