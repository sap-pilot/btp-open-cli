package cmd

import (
	"testing"
)

func TestDescribeSubaccount_MissingOrgFlag(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	setupTestEnv(t, "http://fake-cf.example.com")
	_, _, err := runCmd(t, "describe-subaccount")
	if err == nil {
		t.Fatal("expected error when --org is not provided")
	}
}

func TestDescribeSubaccount_NotLoggedIn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, _, err := runCmd(t, "describe-subaccount", "--org", "my-org")
	if err == nil {
		t.Fatal("expected error when not logged in")
	}
}
