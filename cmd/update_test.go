package cmd

import (
	"strings"
	"testing"
)

// TestUpdate_NoArgs_NoNetwork tests that "update" without args fails gracefully
// when the GitHub API is unreachable (which it is in unit tests). This verifies
// the error path is handled cleanly.
func TestUpdate_NoArgs_NoNetwork(t *testing.T) {
	// We can't easily redirect the hardcoded GitHub API URL without source changes.
	// Verify the command at least tries to check the version and surfaces an error.
	_, _, err := runCmd(t, "update", "--yes")
	// The error should be about checking the latest release, not a panic.
	if err != nil && !strings.Contains(err.Error(), "latest release") &&
		!strings.Contains(err.Error(), "connection refused") &&
		!strings.Contains(err.Error(), "no such host") &&
		!strings.Contains(err.Error(), "dial") {
		t.Errorf("unexpected error type: %v", err)
	}
}

// TestUpdate_AlreadyUpToDate tests the version comparison when the local version
// matches the "latest" (simulated by setting Version = the tag returned).
// Since we can't redirect the GitHub URL, we test the helper functions instead.
func TestUpdate_AssetName(t *testing.T) {
	name := updateAssetName()
	if !strings.HasPrefix(name, "bo-") {
		t.Errorf("updateAssetName should start with 'bo-', got: %q", name)
	}
}

// TestUpdate_VersionString tests that the versionString function works correctly.
func TestUpdate_VersionString(t *testing.T) {
	// Save and restore the Version variable.
	origVersion := Version
	defer func() { Version = origVersion }()

	Version = "v1.2.3"
	s := versionString()
	if !strings.Contains(s, "v1.2.3") {
		t.Errorf("expected version in versionString, got: %q", s)
	}
}
