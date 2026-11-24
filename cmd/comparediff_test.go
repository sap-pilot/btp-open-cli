package cmd

import (
	"fmt"
	"testing"
)

func mustLcsDiff(t *testing.T, a, b []string) []cmpOp {
	t.Helper()
	ops, err := lcsDiff(a, b)
	if err != nil {
		t.Fatalf("lcsDiff failed: %v", err)
	}
	return ops
}

func TestLcsDiff_Identical(t *testing.T) {
	a := []string{"one", "two", "three"}
	ops := mustLcsDiff(t, a, a)
	for _, op := range ops {
		if op.Type != cmpOpEqual {
			t.Fatalf("expected every op to be Equal for identical input, got: %+v", ops)
		}
	}
	if len(ops) != 3 {
		t.Fatalf("expected 3 ops, got %d: %+v", len(ops), ops)
	}
}

func TestLcsDiff_BothEmpty(t *testing.T) {
	ops := mustLcsDiff(t, nil, nil)
	if len(ops) != 0 {
		t.Errorf("expected no ops for two empty inputs, got: %+v", ops)
	}
}

func TestLcsDiff_PureInsert(t *testing.T) {
	a := []string{"one"}
	b := []string{"one", "two", "three"}
	ops := mustLcsDiff(t, a, b)
	hunks := cmpHunks(ops)
	if len(hunks) != 2 || hunks[0].Kind != cmpHunkEqual || hunks[1].Kind != cmpHunkInsert {
		t.Fatalf("expected [Equal, Insert] hunks, got: %+v", hunks)
	}
	if hunks[1].BCount != 2 {
		t.Errorf("expected 2 inserted lines, got %+v", hunks[1])
	}
}

func TestLcsDiff_PureDelete(t *testing.T) {
	a := []string{"one", "two", "three"}
	b := []string{"one"}
	ops := mustLcsDiff(t, a, b)
	hunks := cmpHunks(ops)
	if len(hunks) != 2 || hunks[0].Kind != cmpHunkEqual || hunks[1].Kind != cmpHunkDelete {
		t.Fatalf("expected [Equal, Delete] hunks, got: %+v", hunks)
	}
	if hunks[1].ACount != 2 {
		t.Errorf("expected 2 deleted lines, got %+v", hunks[1])
	}
}

func TestLcsDiff_EmptyAVsNonEmptyB(t *testing.T) {
	ops := mustLcsDiff(t, nil, []string{"a", "b"})
	hunks := cmpHunks(ops)
	if len(hunks) != 1 || hunks[0].Kind != cmpHunkInsert || hunks[0].BCount != 2 {
		t.Fatalf("expected a single Insert hunk of 2 lines, got: %+v", hunks)
	}
}

func TestLcsDiff_NonEmptyAVsEmptyB(t *testing.T) {
	ops := mustLcsDiff(t, []string{"a", "b"}, nil)
	hunks := cmpHunks(ops)
	if len(hunks) != 1 || hunks[0].Kind != cmpHunkDelete || hunks[0].ACount != 2 {
		t.Fatalf("expected a single Delete hunk of 2 lines, got: %+v", hunks)
	}
}

func TestCmpHunks_ReplaceMergesAdjacentDeleteInsert(t *testing.T) {
	a := []string{"same1", "old-line", "same2"}
	b := []string{"same1", "new-line", "same2"}
	ops := mustLcsDiff(t, a, b)
	hunks := cmpHunks(ops)
	if len(hunks) != 3 {
		t.Fatalf("expected [Equal, Replace, Equal], got: %+v", hunks)
	}
	if hunks[0].Kind != cmpHunkEqual || hunks[2].Kind != cmpHunkEqual {
		t.Fatalf("expected the outer hunks to be Equal, got: %+v", hunks)
	}
	mid := hunks[1]
	if mid.Kind != cmpHunkReplace || mid.ACount != 1 || mid.BCount != 1 {
		t.Fatalf("expected a 1-line Replace hunk, got: %+v", mid)
	}
}

func TestCmpHunks_ReplaceWithDifferentLengths(t *testing.T) {
	// 2 old lines replaced by 3 new lines: pairs up to the shorter length
	// (2), with 1 line left over as a pure insert.
	hunks := cmpHunks([]cmpOp{
		{Type: cmpOpDelete, AIdx: 0},
		{Type: cmpOpDelete, AIdx: 1},
		{Type: cmpOpInsert, BIdx: 0},
		{Type: cmpOpInsert, BIdx: 1},
		{Type: cmpOpInsert, BIdx: 2},
	})
	if len(hunks) != 1 || hunks[0].Kind != cmpHunkReplace {
		t.Fatalf("expected a single merged Replace hunk, got: %+v", hunks)
	}
	if hunks[0].ACount != 2 || hunks[0].BCount != 3 {
		t.Fatalf("expected ACount=2, BCount=3, got: %+v", hunks[0])
	}
}

func TestCmpHunks_InsertThenDeleteAlsoMergesToReplace(t *testing.T) {
	// The opposite adjacency order (Insert run immediately followed by a
	// Delete run) is just as much a "this turned into that" as the reverse
	// order — cmpHunks must merge it the same way.
	hunks := cmpHunks([]cmpOp{
		{Type: cmpOpInsert, BIdx: 0},
		{Type: cmpOpDelete, AIdx: 0},
	})
	if len(hunks) != 1 || hunks[0].Kind != cmpHunkReplace {
		t.Fatalf("expected a single merged Replace hunk, got: %+v", hunks)
	}
	if hunks[0].ACount != 1 || hunks[0].BCount != 1 {
		t.Errorf("expected ACount=1, BCount=1, got: %+v", hunks[0])
	}
}

func TestLcsDiff_TooLargeErrors(t *testing.T) {
	a := make([]string, 8000)
	b := make([]string, 8000)
	for i := range a {
		a[i] = fmt.Sprintf("a-%d", i)
	}
	for i := range b {
		b[i] = fmt.Sprintf("b-%d", i)
	}
	_, err := lcsDiff(a, b) // 8000*8000 = 64,000,000 > cmpMaxDiffCells
	if err == nil {
		t.Fatal("expected an error for an over-large comparison")
	}
}
