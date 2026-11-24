package cmd

import "fmt"

// This file implements the line-level diff algorithm behind 'bo compare'.
// It's a plain two-file text diff — like 'diff'/VS Code's compare view —
// completely agnostic of whether the files happen to be CSV or JSON: each
// file is just its sequence of lines, compared for equality line by line.

// cmpOpType classifies one step of an edit script turning file A into file B.
type cmpOpType int

const (
	cmpOpEqual cmpOpType = iota
	cmpOpDelete
	cmpOpInsert
)

// cmpOp is one step of the edit script: a line kept as-is (Equal, valid on
// both AIdx/BIdx), removed from A (Delete, valid AIdx only), or added in B
// (Insert, valid BIdx only).
type cmpOp struct {
	Type cmpOpType
	AIdx int
	BIdx int
}

// cmpMaxDiffCells caps a*len(b) for the O(len(a)*len(b)) time/space LCS
// table lcsDiff builds below — chosen as a practical memory ceiling (a
// cmpMaxDiffCells-cell int table is a few hundred MB), not a hard
// correctness limit. Comparing files past this is still possible in
// principle, just not with this straightforward an algorithm; narrowing the
// files first (e.g. with 'bo sort --fields') is the practical workaround.
const cmpMaxDiffCells = 50_000_000

// lcsDiff returns the edit script turning a into b, built from a longest-
// common-subsequence table: the classic (if quadratic) textbook diff
// algorithm, chosen here over a sub-quadratic one (e.g. Myers' O(ND)) for
// how straightforward it is to get exactly right. ops always covers every
// index of a (via Equal or Delete) and of b (via Equal or Insert), in order.
func lcsDiff(a, b []string) ([]cmpOp, error) {
	n, m := len(a), len(b)
	if n > 0 && m > 0 && n*m > cmpMaxDiffCells {
		return nil, fmt.Errorf("too large to compare (%d x %d lines, %d cells) — narrow the files first (e.g. with 'bo sort --fields') and retry", n, m, n*m)
	}

	// table[i][j] = length of the LCS of a[i:] and b[j:].
	table := make([][]int, n+1)
	for i := range table {
		table[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				table[i][j] = table[i+1][j+1] + 1
			} else if table[i+1][j] >= table[i][j+1] {
				table[i][j] = table[i+1][j]
			} else {
				table[i][j] = table[i][j+1]
			}
		}
	}

	ops := make([]cmpOp, 0, n+m)
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, cmpOp{Type: cmpOpEqual, AIdx: i, BIdx: j})
			i++
			j++
		case table[i+1][j] >= table[i][j+1]:
			ops = append(ops, cmpOp{Type: cmpOpDelete, AIdx: i})
			i++
		default:
			ops = append(ops, cmpOp{Type: cmpOpInsert, BIdx: j})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, cmpOp{Type: cmpOpDelete, AIdx: i})
	}
	for ; j < m; j++ {
		ops = append(ops, cmpOp{Type: cmpOpInsert, BIdx: j})
	}
	return ops, nil
}

// cmpHunkKind classifies one contiguous run of the edit script, grouped by
// cmpHunks below.
type cmpHunkKind int

const (
	cmpHunkEqual cmpHunkKind = iota
	cmpHunkInsert
	cmpHunkDelete
	// cmpHunkReplace is an Insert run and a Delete run with nothing Equal
	// between them — the "this region turned into that region" case,
	// rendered (see cmpBuildRows) as paired "updated" lines up to the
	// shorter side's length, with any excess length still pure
	// insert/delete.
	cmpHunkReplace
)

// cmpHunk is one maximal run (or adjacent delete+insert pair, for Replace)
// of the edit script: AStart/ACount and BStart/BCount are the (start index,
// line count) ranges of a/b this hunk covers — whichever side applies to
// its Kind (both, for Equal and Replace).
type cmpHunk struct {
	Kind   cmpHunkKind
	AStart int
	ACount int
	BStart int
	BCount int
}

// cmpHunks groups ops (as produced by lcsDiff) into hunks: consecutive
// same-type steps are merged into one run, and an adjacent Delete run +
// Insert run (in either order, since lcsDiff's own tie-breaking order
// isn't meaningful here) with nothing Equal between them is merged further
// into one Replace hunk.
func cmpHunks(ops []cmpOp) []cmpHunk {
	type run struct {
		Type   cmpOpType
		AStart int
		ACount int
		BStart int
		BCount int
	}
	var runs []run
	for _, op := range ops {
		if len(runs) > 0 && runs[len(runs)-1].Type == op.Type {
			last := &runs[len(runs)-1]
			switch op.Type {
			case cmpOpEqual:
				last.ACount++
				last.BCount++
			case cmpOpDelete:
				last.ACount++
			case cmpOpInsert:
				last.BCount++
			}
			continue
		}
		switch op.Type {
		case cmpOpEqual:
			runs = append(runs, run{Type: cmpOpEqual, AStart: op.AIdx, ACount: 1, BStart: op.BIdx, BCount: 1})
		case cmpOpDelete:
			runs = append(runs, run{Type: cmpOpDelete, AStart: op.AIdx, ACount: 1})
		case cmpOpInsert:
			runs = append(runs, run{Type: cmpOpInsert, BStart: op.BIdx, BCount: 1})
		}
	}

	hunks := make([]cmpHunk, 0, len(runs))
	for i := 0; i < len(runs); i++ {
		r := runs[i]
		if r.Type == cmpOpEqual {
			hunks = append(hunks, cmpHunk{Kind: cmpHunkEqual, AStart: r.AStart, ACount: r.ACount, BStart: r.BStart, BCount: r.BCount})
			continue
		}
		if i+1 < len(runs) && runs[i+1].Type != cmpOpEqual && runs[i+1].Type != r.Type {
			next := runs[i+1]
			del, ins := r, next
			if r.Type == cmpOpInsert {
				del, ins = next, r
			}
			hunks = append(hunks, cmpHunk{Kind: cmpHunkReplace, AStart: del.AStart, ACount: del.ACount, BStart: ins.BStart, BCount: ins.BCount})
			i++
			continue
		}
		if r.Type == cmpOpDelete {
			hunks = append(hunks, cmpHunk{Kind: cmpHunkDelete, AStart: r.AStart, ACount: r.ACount})
		} else {
			hunks = append(hunks, cmpHunk{Kind: cmpHunkInsert, BStart: r.BStart, BCount: r.BCount})
		}
	}
	return hunks
}
