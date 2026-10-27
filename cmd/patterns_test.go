package cmd

import "testing"

func TestMatchesNegatablePattern(t *testing.T) {
	cases := []struct {
		patterns string
		name     string
		want     bool
	}{
		{"*", "anything", true},
		{"btp*,temp*", "btp-key-1", true},
		{"btp*,temp*", "temp-foo", true},
		{"btp*,temp*", "other", false},
		{"!btp*", "btp-key-1", false},
		{"!btp*", "other-key", true},
		{"btp*,!temp*", "temp-foo", false},
		{"btp*,!temp*", "btp-foo", true},
		{"btp*,!temp*", "other", true}, // "!temp*" matches since "other" doesn't match "temp*"
		{"exactname", "ExactName", true},
		{"exactname", "no-match", false},
	}
	for _, c := range cases {
		got := matchesNegatablePattern(c.patterns, c.name)
		if got != c.want {
			t.Errorf("matchesNegatablePattern(%q, %q) = %v, want %v", c.patterns, c.name, got, c.want)
		}
	}
}
