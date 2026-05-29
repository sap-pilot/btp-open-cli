package cmd

import (
	"strings"
	"testing"
)

func TestVersion_Output(t *testing.T) {
	stdout, _, err := runCmd(t, "version")
	if err != nil {
		t.Fatalf("version command failed: %v", err)
	}
	if !strings.Contains(stdout, "bo ") {
		t.Errorf("expected 'bo ' prefix in output, got: %q", stdout)
	}
}
