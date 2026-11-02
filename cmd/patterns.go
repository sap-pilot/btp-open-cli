package cmd

import (
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// optionalPatternArgsIfFlagSet returns a cobra Args validator for a command
// whose single positional name-pattern argument is normally required, but
// becomes optional when flagName was explicitly passed (because that flag
// supplies its own exact targets, e.g. a CSV/JSON file, making the pattern
// argument an optional further filter rather than a required target).
func optionalPatternArgsIfFlagSet(flagName string) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		if cmd.Flags().Changed(flagName) {
			return cobra.MaximumNArgs(1)(cmd, args)
		}
		return cobra.ExactArgs(1)(cmd, args)
	}
}

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
