package cmd

import "testing"

func TestSkipMatches(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		values  []string
		want    bool
	}{
		{"empty pattern never matches", "", []string{"anything"}, false},
		{"single keyword, case-insensitive substring", "PROD", []string{"my-prod-org"}, true},
		{"single keyword, no match", "prod", []string{"my-dev-org"}, false},
		{"multi-keyword, first matches", "prod,staging", []string{"my-staging-org"}, true},
		{"multi-keyword, second matches", "prod,staging", []string{"my-prod-org"}, true},
		{"multi-keyword, none match", "prod,staging", []string{"my-dev-org"}, false},
		{"multi-keyword, matches a different value", "prod,staging", []string{"my-dev-org", "some-staging-thing"}, true},
		{"keywords with surrounding whitespace are trimmed", " prod , staging ", []string{"my-staging-org"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := skipMatches(c.pattern, c.values...); got != c.want {
				t.Errorf("skipMatches(%q, %v) = %v, want %v", c.pattern, c.values, got, c.want)
			}
		})
	}
}
