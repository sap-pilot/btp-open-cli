package cmd

import (
	"path/filepath"
	"strings"
)

// matchesNegatablePattern reports whether name is selected by patternsCSV, a
// comma-separated list of patterns. Each pattern is treated as a glob (via
// filepath.Match) if it contains any of * ? [, otherwise as a case-insensitive
// substring. A pattern prefixed with '!' is a reverse match: '!btp*' selects
// any name that does NOT match 'btp*'. name is selected if any pattern in the
// list — negated or not — matches; an empty patternsCSV matches every name.
func matchesNegatablePattern(patternsCSV, name string) bool {
	if patternsCSV == "" {
		return true
	}
	nameLower := strings.ToLower(name)
	for _, p := range splitCSV(patternsCSV) {
		negate := strings.HasPrefix(p, "!")
		if negate {
			p = p[1:]
		}
		m := singlePatternMatch(p, nameLower)
		if negate {
			m = !m
		}
		if m {
			return true
		}
	}
	return false
}

// singlePatternMatch matches a single pattern (no leading '!') against an
// already-lowercased name: a glob via filepath.Match if it contains any of
// * ? [, otherwise a case-insensitive substring.
func singlePatternMatch(pattern, nameLower string) bool {
	pl := strings.ToLower(pattern)
	if strings.ContainsAny(pattern, "*?[") {
		m, _ := filepath.Match(pl, nameLower)
		return m
	}
	return strings.Contains(nameLower, pl)
}
