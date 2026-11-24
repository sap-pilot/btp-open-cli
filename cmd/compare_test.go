package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCmpBuildPair_AllIdentical(t *testing.T) {
	a := []string{"one", "two", "three"}
	pair, err := cmpBuildPair("a.csv", "b.csv", a, a)
	if err != nil {
		t.Fatalf("cmpBuildPair failed: %v", err)
	}
	if pair.Stats.Identical != 3 || pair.Stats.Added != 0 || pair.Stats.Deleted != 0 || pair.Stats.Updated != 0 {
		t.Fatalf("expected all-identical stats, got: %+v", pair.Stats)
	}
	if pair.Stats.PercentIdentical != 100 {
		t.Errorf("expected 100%% identical, got %v", pair.Stats.PercentIdentical)
	}
	for _, r := range pair.Rows {
		if r.Kind != cmpRowEqual {
			t.Errorf("expected every row Equal, got: %+v", r)
		}
	}
}

func TestCmpBuildPair_ReplaceWithExcessBecomesAddedOrDeleted(t *testing.T) {
	a := []string{"same", "old1", "old2"}
	b := []string{"same", "new1", "new2", "new3"}
	pair, err := cmpBuildPair("a", "b", a, b)
	if err != nil {
		t.Fatalf("cmpBuildPair failed: %v", err)
	}
	// 1 equal, then a 2-vs-3 replace: 2 updated pairs + 1 excess added line.
	if pair.Stats.Identical != 1 {
		t.Errorf("expected 1 identical line, got %d", pair.Stats.Identical)
	}
	if pair.Stats.Updated != 2 {
		t.Errorf("expected 2 updated lines, got %d", pair.Stats.Updated)
	}
	if pair.Stats.Added != 1 {
		t.Errorf("expected 1 added (excess) line, got %d", pair.Stats.Added)
	}
	if pair.Stats.Deleted != 0 {
		t.Errorf("expected 0 deleted lines, got %d", pair.Stats.Deleted)
	}
	// TotalA must always be identical+deleted+updated.
	if pair.Stats.TotalA != len(a) {
		t.Errorf("expected TotalA=%d, got %d", len(a), pair.Stats.TotalA)
	}
	if pair.Stats.Identical+pair.Stats.Deleted+pair.Stats.Updated != pair.Stats.TotalA {
		t.Errorf("identical+deleted+updated must equal TotalA, got stats: %+v", pair.Stats)
	}
}

func TestCmpBuildPair_EmptyBothSides(t *testing.T) {
	pair, err := cmpBuildPair("a", "b", nil, nil)
	if err != nil {
		t.Fatalf("cmpBuildPair failed: %v", err)
	}
	if pair.Stats.PercentIdentical != 100 {
		t.Errorf("expected 100%% for two empty files, got %v", pair.Stats.PercentIdentical)
	}
	if len(pair.Rows) != 0 {
		t.Errorf("expected no rows, got: %+v", pair.Rows)
	}
}

func TestCmpBuildPair_EmptyAVsNonEmptyB(t *testing.T) {
	pair, err := cmpBuildPair("a", "b", nil, []string{"x", "y"})
	if err != nil {
		t.Fatalf("cmpBuildPair failed: %v", err)
	}
	if pair.Stats.PercentIdentical != 0 {
		t.Errorf("expected 0%% identical when file_A is empty but file_B isn't, got %v", pair.Stats.PercentIdentical)
	}
	if pair.Stats.Added != 2 {
		t.Errorf("expected 2 added lines, got %d", pair.Stats.Added)
	}
}

func TestReadCompareLines_NormalizesLineEndingsAndTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("one\r\ntwo\r\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, err := readCompareLines(path)
	if err != nil {
		t.Fatalf("readCompareLines failed: %v", err)
	}
	want := []string{"one", "two", "three"}
	if len(lines) != len(want) {
		t.Fatalf("expected %d lines, got %d: %v", len(want), len(lines), lines)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line %d: expected %q, got %q", i, want[i], lines[i])
		}
	}
}

func TestReadCompareLines_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(path, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, err := readCompareLines(path)
	if err != nil {
		t.Fatalf("readCompareLines failed: %v", err)
	}
	if len(lines) != 0 {
		t.Errorf("expected no lines for an empty file, got: %v", lines)
	}
}

// ── end-to-end 'bo compare' ──────────────────────────────────────────────────

func writeCompareFixture(t *testing.T, dir, name string, lines ...string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCompare_RequiresOutput(t *testing.T) {
	dir := t.TempDir()
	a := writeCompareFixture(t, dir, "a.csv", "x")
	b := writeCompareFixture(t, dir, "b.csv", "x")

	_, _, err := runCmd(t, "compare", a, b)
	if err == nil {
		t.Fatal("expected an error when --output is not given")
	}
	if !strings.Contains(err.Error(), "--output") {
		t.Errorf("expected error to mention --output, got: %v", err)
	}
}

func TestCompare_MismatchedListLengthsErrors(t *testing.T) {
	dir := t.TempDir()
	a1 := writeCompareFixture(t, dir, "a1.csv", "x")
	b1 := writeCompareFixture(t, dir, "b1.csv", "x")
	b2 := writeCompareFixture(t, dir, "b2.csv", "x")
	out := filepath.Join(dir, "out.html")

	_, _, err := runCmd(t, "compare", a1, b1+","+b2, "-o", out)
	if err == nil {
		t.Fatal("expected an error when file_A/file_B lists have different lengths")
	}
}

func TestCompare_MissingFileErrors(t *testing.T) {
	dir := t.TempDir()
	b := writeCompareFixture(t, dir, "b.csv", "x")
	out := filepath.Join(dir, "out.html")

	_, _, err := runCmd(t, "compare", filepath.Join(dir, "nonexistent.csv"), b, "-o", out)
	if err == nil {
		t.Fatal("expected an error for a missing input file")
	}
}

func TestCompare_SinglePairEndToEnd(t *testing.T) {
	dir := t.TempDir()
	a := writeCompareFixture(t, dir, "a.csv",
		"space_name,app_name,app_state",
		"dev,myapp-srv,STARTED",
		"prod,myapp-ui,STARTED",
	)
	b := writeCompareFixture(t, dir, "b.csv",
		"space_name,app_name,app_state",
		"dev,myapp-srv,STOPPED",
		"prod,myapp-ui,STARTED",
	)
	out := filepath.Join(dir, "report.html")

	stdout, stderr, err := runCmd(t, "compare", a, b, "-o", out)
	if err != nil {
		t.Fatalf("compare failed: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stdout, "Compared 1 file pair(s)") {
		t.Errorf("expected a completion message, got: %q", stdout)
	}

	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading report: %v", err)
	}
	got := string(body)

	if !strings.Contains(got, "<!doctype html>") {
		t.Error("expected a full HTML document")
	}
	if !strings.Contains(got, `class="badge-identical">2<`) {
		t.Errorf("expected 2 identical lines in the summary, got: %q", got)
	}
	if !strings.Contains(got, `class="badge-updated">1<`) {
		t.Errorf("expected 1 updated line in the summary, got: %q", got)
	}
	if !strings.Contains(got, "66.7%") {
		t.Errorf("expected 66.7%% identical (2 of 3 lines including header), got: %q", got)
	}
	if !strings.Contains(got, `class="row-updated"`) {
		t.Error("expected at least one row-updated diff row")
	}
}

func TestCompare_MultiplePairs(t *testing.T) {
	dir := t.TempDir()
	a1 := writeCompareFixture(t, dir, "a1.csv", "same", "only-in-a1")
	b1 := writeCompareFixture(t, dir, "b1.csv", "same")
	a2 := writeCompareFixture(t, dir, "a2.csv", "same")
	b2 := writeCompareFixture(t, dir, "b2.csv", "same", "only-in-b2")
	out := filepath.Join(dir, "report.html")

	_, stderr, err := runCmd(t, "compare", a1+","+a2, b1+","+b2, "-o", out)
	if err != nil {
		t.Fatalf("compare failed: %v\nstderr: %s", err, stderr)
	}

	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if !strings.Contains(got, `id="pair-1"`) || !strings.Contains(got, `id="pair-2"`) {
		t.Errorf("expected two anchored pair sections, got: %q", got)
	}
	if !strings.Contains(got, "a1.csv vs") || !strings.Contains(got, "a2.csv vs") {
		t.Errorf("expected both pairs' filenames in the report, got: %q", got)
	}
}

func TestCompare_EscapesHTMLSpecialCharacters(t *testing.T) {
	dir := t.TempDir()
	a := writeCompareFixture(t, dir, "a.txt", `line <b>&"quoted"</b>`)
	b := writeCompareFixture(t, dir, "b.txt", "different")
	out := filepath.Join(dir, "report.html")

	_, stderr, err := runCmd(t, "compare", a, b, "-o", out)
	if err != nil {
		t.Fatalf("compare failed: %v\nstderr: %s", err, stderr)
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if strings.Contains(got, "<b>&\"quoted\"</b>") {
		t.Error("expected file content to be HTML-escaped, found raw markup")
	}
	if !strings.Contains(got, "&lt;b&gt;") {
		t.Errorf("expected escaped content in output, got: %q", got)
	}
}
