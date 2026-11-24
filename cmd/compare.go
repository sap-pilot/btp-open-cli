package cmd

import (
	"fmt"
	"html/template"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// This file implements 'bo compare', a standalone file-comparison utility in
// the same spirit as 'bo sort' (cmd/sort.go): it never talks to BTP/CF, it
// just compares two files (or two equal-length lists of files, paired up by
// position) already on disk and renders a single, self-contained HTML
// report — a side-by-side, VS Code-style diff view, plus a summary table —
// so the comparison described in doc/compare-subaccounts.md can be shared
// or viewed without an editor. The diff itself (cmd/comparediff.go) is a
// plain line-level text diff: it doesn't parse CSV or JSON at all, so it
// works identically on either.

// ── report data model ───────────────────────────────────────────────────────

// cmpRowKind is a rendered diff row's classification, and also its CSS
// class name (see compareHTMLTemplate's <style>).
type cmpRowKind string

const (
	cmpRowEqual   cmpRowKind = "equal"
	cmpRowAdded   cmpRowKind = "added"
	cmpRowDeleted cmpRowKind = "deleted"
	cmpRowUpdated cmpRowKind = "updated"
)

// cmpRow is one rendered line of the side-by-side view. LeftNum/RightNum is
// 0 (template-falsy, so the gutter renders blank) whenever this row has
// nothing on that side — a pure addition has no LeftNum, a pure deletion no
// RightNum.
type cmpRow struct {
	LeftNum   int
	LeftText  string
	RightNum  int
	RightText string
	Kind      cmpRowKind
}

// cmpStats is one pair's summary counts — see cmpBuildPair for exactly how
// each is derived from the diff.
type cmpStats struct {
	Identical        int
	Added            int
	Deleted          int
	Updated          int
	TotalA           int
	PercentIdentical float64
}

// cmpPair is one file_A/file_B pairing: its rendered rows plus summary.
type cmpPair struct {
	NameA, NameB string
	AnchorID     string
	Stats        cmpStats
	Rows         []cmpRow
}

// cmpReport is the whole HTML page's data: one or more pairs, compared and
// rendered independently of one another.
type cmpReport struct {
	Pairs []cmpPair
}

// cmpBuildPair diffs a against b (lcsDiff/cmpHunks) and renders the result
// into one cmpPair: every hunk becomes some number of cmpRows, and feeds
// the running Stats tally. A Replace hunk (see cmpHunks) is rendered as
// "updated" pairs up to the shorter side's length, with anything left over
// on the longer side still pure deleted/added — so every line of a ends up
// counted as exactly one of Identical, Deleted, or Updated, and TotalA
// (len(a)) is always their sum.
func cmpBuildPair(nameA, nameB string, a, b []string) (cmpPair, error) {
	ops, err := lcsDiff(a, b)
	if err != nil {
		return cmpPair{}, fmt.Errorf("%s vs %s: %w", nameA, nameB, err)
	}

	var rows []cmpRow
	var stats cmpStats
	for _, h := range cmpHunks(ops) {
		switch h.Kind {
		case cmpHunkEqual:
			for k := 0; k < h.ACount; k++ {
				rows = append(rows, cmpRow{
					LeftNum: h.AStart + k + 1, LeftText: a[h.AStart+k],
					RightNum: h.BStart + k + 1, RightText: b[h.BStart+k],
					Kind: cmpRowEqual,
				})
			}
			stats.Identical += h.ACount

		case cmpHunkDelete:
			for k := 0; k < h.ACount; k++ {
				rows = append(rows, cmpRow{LeftNum: h.AStart + k + 1, LeftText: a[h.AStart+k], Kind: cmpRowDeleted})
			}
			stats.Deleted += h.ACount

		case cmpHunkInsert:
			for k := 0; k < h.BCount; k++ {
				rows = append(rows, cmpRow{RightNum: h.BStart + k + 1, RightText: b[h.BStart+k], Kind: cmpRowAdded})
			}
			stats.Added += h.BCount

		case cmpHunkReplace:
			paired := min(h.ACount, h.BCount)
			for k := 0; k < paired; k++ {
				rows = append(rows, cmpRow{
					LeftNum: h.AStart + k + 1, LeftText: a[h.AStart+k],
					RightNum: h.BStart + k + 1, RightText: b[h.BStart+k],
					Kind: cmpRowUpdated,
				})
			}
			stats.Updated += paired
			for k := paired; k < h.ACount; k++ {
				rows = append(rows, cmpRow{LeftNum: h.AStart + k + 1, LeftText: a[h.AStart+k], Kind: cmpRowDeleted})
			}
			stats.Deleted += h.ACount - paired
			for k := paired; k < h.BCount; k++ {
				rows = append(rows, cmpRow{RightNum: h.BStart + k + 1, RightText: b[h.BStart+k], Kind: cmpRowAdded})
			}
			stats.Added += h.BCount - paired
		}
	}

	stats.TotalA = len(a)
	switch {
	case stats.TotalA > 0:
		stats.PercentIdentical = float64(stats.Identical) / float64(stats.TotalA) * 100
	case len(b) == 0:
		stats.PercentIdentical = 100 // both empty: trivially identical
	default:
		stats.PercentIdentical = 0
	}

	return cmpPair{NameA: nameA, NameB: nameB, Stats: stats, Rows: rows}, nil
}

// ── file reading ─────────────────────────────────────────────────────────────

// readCompareLines reads path and splits it into lines for lcsDiff: line
// endings are normalized (\r\n and bare \r both become \n) so a difference
// in line-ending style alone doesn't register as a content difference, and
// a single trailing newline (if any) doesn't produce a spurious empty last
// line. The file's content is never otherwise parsed or interpreted — this
// is a plain text diff, equally applicable to a CSV or a JSON file.
func readCompareLines(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	if text == "" {
		return nil, nil
	}
	lines := strings.Split(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines, nil
}

// ── command ──────────────────────────────────────────────────────────────────

var compareCmd = &cobra.Command{
	Use:   "compare <file_A[,file_A2,...]> <file_B[,file_B2,...]> --output <report.html>",
	Short: "Compare two (or more paired) files into a single HTML diff report",
	Long: `Compares file_A against file_B — a plain line-by-line text diff, the same
idea as 'diff'/VS Code's compare view, and equally applicable to a CSV or a
JSON file: each file is just its sequence of lines, never otherwise parsed
or interpreted — and renders the result as one self-contained HTML page
(--output/-o), with no other files or network access needed to view it.

Each of file_A/file_B may instead be a comma-separated list of several
files, as long as both lists are the same length: file_A's Nth file is
paired with file_B's Nth file, each pair compared and rendered
independently, e.g. for comparing the same set of exports across more than
one pair of subaccounts in one report.

The page opens with a summary table, one row per pair, reporting:
  identical  lines equal on both sides
  added      lines only in file_B
  deleted    lines only in file_A
  updated    lines present on both sides but changed (paired up to the
             shorter side's length when a changed block's line count
             differs between the two files; any excess is still counted as
             plain added/deleted)
  identical% identical / file_A's own total line count

Below the summary, each pair gets its own collapsible section (open by
default) with the full side-by-side diff: line numbers and content for
file_A on the left, file_B on the right, color-coded to match the summary
(added green, deleted red, updated dark red, identical uncolored).

This command never talks to BTP/CF, and --output/-o is required (there's no
useful plain-text/stdout form of an HTML page).`,
	Args:    cobra.ExactArgs(2),
	GroupID: "utilities",
	RunE: func(cmd *cobra.Command, args []string) error {
		outputFlag, _ := cmd.Flags().GetString("output")
		if outputFlag == "" {
			return fmt.Errorf("--output/-o is required")
		}

		filesA := splitCSV(args[0])
		filesB := splitCSV(args[1])
		if len(filesA) == 0 || len(filesB) == 0 {
			return fmt.Errorf("file_A and file_B must each list at least one file")
		}
		if len(filesA) != len(filesB) {
			return fmt.Errorf("file_A and file_B must list the same number of files to pair them up, got %d and %d", len(filesA), len(filesB))
		}

		var report cmpReport
		for i := range filesA {
			a, err := readCompareLines(filesA[i])
			if err != nil {
				return fmt.Errorf("reading %s: %w", filesA[i], err)
			}
			b, err := readCompareLines(filesB[i])
			if err != nil {
				return fmt.Errorf("reading %s: %w", filesB[i], err)
			}
			pair, err := cmpBuildPair(filesA[i], filesB[i], a, b)
			if err != nil {
				return err
			}
			pair.AnchorID = fmt.Sprintf("pair-%d", i+1)
			report.Pairs = append(report.Pairs, pair)
		}

		f, err := os.Create(outputFlag)
		if err != nil {
			return fmt.Errorf("creating %s: %w", outputFlag, err)
		}
		defer f.Close()
		if err := compareHTMLTemplate.Execute(f, report); err != nil {
			return fmt.Errorf("rendering HTML: %w", err)
		}

		fmt.Fprintf(cmd.OutOrStdout(), "Compared %d file pair(s) -> %s.\n", len(report.Pairs), outputFlag)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(compareCmd)
	compareCmd.Flags().StringP("output", "o", "", "Write the HTML comparison report to this file (required)")
}

// ── HTML rendering ───────────────────────────────────────────────────────────

// compareHTMLTemplate renders a cmpReport into one self-contained HTML
// page: no external stylesheets/scripts, so the file opens and reads
// correctly on its own, offline, anywhere. Styling is a minimal dark theme
// in the spirit of VS Code's own diff view (monospace, dark background,
// line-number gutters), with the four row kinds colored consistently
// between the summary table and the diff view itself: added green, deleted
// red, updated a darker red, identical left uncolored.
var compareHTMLTemplate = template.Must(template.New("compare").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>bo compare report</title>
<style>
  :root {
    --bg: #1e1e1e;
    --fg: #d4d4d4;
    --muted: #8a8a8a;
    --border: #3c3c3c;
    --panel: #252526;
    --added-bg: rgba(46, 160, 67, 0.22);
    --added-fg: #3fb950;
    --deleted-bg: rgba(248, 81, 73, 0.16);
    --deleted-fg: #f85149;
    --updated-bg: rgba(140, 30, 30, 0.45);
    --updated-fg: #c0504d;
    --identical-fg: #79c0ff;
  }
  * { box-sizing: border-box; }
  body {
    background: var(--bg);
    color: var(--fg);
    font-family: Consolas, "Courier New", monospace;
    font-size: 13px;
    margin: 0;
    padding: 24px;
  }
  h1 { font-size: 18px; font-weight: 600; color: #fff; margin: 0 0 16px; }
  h2 { font-size: 14px; font-weight: 600; margin: 0; }
  a { color: var(--identical-fg); text-decoration: none; }
  a:hover { text-decoration: underline; }

  table.summary { border-collapse: collapse; width: 100%; margin-bottom: 28px; }
  table.summary th, table.summary td {
    border: 1px solid var(--border);
    padding: 6px 10px;
    text-align: right;
  }
  table.summary th:first-child, table.summary td:first-child { text-align: left; }
  table.summary th {
    background: var(--panel);
    color: #cccccc;
    font-weight: 600;
  }
  .badge-identical { color: var(--identical-fg); }
  .badge-added     { color: var(--added-fg); }
  .badge-deleted   { color: var(--deleted-fg); }
  .badge-updated   { color: var(--updated-fg); }

  details.pair {
    border: 1px solid var(--border);
    border-radius: 4px;
    margin-bottom: 20px;
    background: var(--panel);
  }
  details.pair > summary {
    cursor: pointer;
    padding: 8px 12px;
    list-style: none;
  }
  details.pair > summary::-webkit-details-marker { display: none; }
  details.pair > summary:before { content: "▸ "; }
  details.pair[open] > summary:before { content: "▾ "; }

  .diff-scroll { overflow-x: auto; }
  table.diff { border-collapse: collapse; width: 100%; table-layout: fixed; }
  table.diff col.gutter { width: 54px; }
  table.diff col.text { width: 46%; }
  table.diff th {
    background: var(--panel);
    color: #cccccc;
    text-align: left;
    padding: 4px 8px;
    border-bottom: 1px solid var(--border);
    position: sticky;
    top: 0;
  }
  table.diff td {
    padding: 1px 8px;
    white-space: pre;
    vertical-align: top;
    border-right: 1px solid var(--border);
  }
  table.diff td.gutter {
    color: var(--muted);
    text-align: right;
    user-select: none;
    border-right: 1px solid var(--border);
  }
  tr.row-added   td.cell-right, tr.row-added   td.gutter-right { background: var(--added-bg); }
  tr.row-deleted td.cell-left,  tr.row-deleted  td.gutter-left  { background: var(--deleted-bg); }
  tr.row-updated td.cell-left,  tr.row-updated  td.cell-right,
  tr.row-updated td.gutter-left, tr.row-updated td.gutter-right { background: var(--updated-bg); }
</style>
</head>
<body>
<h1>bo compare report</h1>

<table class="summary">
  <tr>
    <th>Pair</th>
    <th>Identical</th>
    <th>Added</th>
    <th>Deleted</th>
    <th>Updated</th>
    <th>Identical %</th>
  </tr>
  {{range .Pairs}}
  <tr>
    <td><a href="#{{.AnchorID}}">{{.NameA}} vs {{.NameB}}</a></td>
    <td class="badge-identical">{{.Stats.Identical}}</td>
    <td class="badge-added">{{.Stats.Added}}</td>
    <td class="badge-deleted">{{.Stats.Deleted}}</td>
    <td class="badge-updated">{{.Stats.Updated}}</td>
    <td class="badge-identical">{{printf "%.1f" .Stats.PercentIdentical}}%</td>
  </tr>
  {{end}}
</table>

{{range .Pairs}}
<details class="pair" id="{{.AnchorID}}" open>
  <summary><h2 style="display:inline">{{.NameA}} <span style="color:var(--muted)">vs</span> {{.NameB}}</h2></summary>
  <div class="diff-scroll">
  <table class="diff">
    <col class="gutter"><col class="text"><col class="gutter"><col class="text">
    <tr><th colspan="2">{{.NameA}}</th><th colspan="2">{{.NameB}}</th></tr>
    {{range .Rows}}
    <tr class="row-{{.Kind}}">
      <td class="gutter gutter-left">{{if .LeftNum}}{{.LeftNum}}{{end}}</td>
      <td class="cell-left">{{.LeftText}}</td>
      <td class="gutter gutter-right">{{if .RightNum}}{{.RightNum}}{{end}}</td>
      <td class="cell-right">{{.RightText}}</td>
    </tr>
    {{end}}
  </table>
  </div>
</details>
{{end}}
</body>
</html>
`))
