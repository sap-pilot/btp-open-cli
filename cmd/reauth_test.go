package cmd

import "testing"

func TestIsSkipInput(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"s", true},
		{"S", true},
		{"skip", true},
		{"SKIP", true},
		{"  s  ", true},
		{"", false},
		{"y", false},
		{"skip me", false},
	}
	for _, c := range cases {
		if got := isSkipInput(c.text); got != c.want {
			t.Errorf("isSkipInput(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}
