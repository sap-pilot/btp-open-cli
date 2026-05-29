package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReorgWikiAttachments_MissingArg(t *testing.T) {
	_, _, err := runCmd(t, "reorg-wiki-attachments")
	if err == nil {
		t.Fatal("expected error when path argument is missing")
	}
}

func TestReorgWikiAttachments_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	stdout, _, err := runCmd(t, "reorg-wiki-attachments", dir)
	if err != nil {
		t.Fatalf("reorg-wiki-attachments failed for empty dir: %v", err)
	}
	if !strings.Contains(stdout, "Attachments found") {
		t.Errorf("expected attachment count in output, got: %q", stdout)
	}
	if !strings.Contains(stdout, "No attachments were moved") {
		t.Errorf("expected 'No attachments were moved', got: %q", stdout)
	}
}

func TestReorgWikiAttachments_MovesAttachment(t *testing.T) {
	dir := t.TempDir()

	// Create .attachments directory with one image.
	attachDir := filepath.Join(dir, ".attachments")
	if err := os.MkdirAll(attachDir, 0755); err != nil {
		t.Fatal(err)
	}
	imgPath := filepath.Join(attachDir, "image.png")
	if err := os.WriteFile(imgPath, []byte("fake-image"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create a wiki page that references the attachment.
	mdPath := filepath.Join(dir, "MyPage.md")
	mdContent := `# My Page

See the figure: ![alt text](.attachments/image.png)
`
	if err := os.WriteFile(mdPath, []byte(mdContent), 0644); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := runCmd(t, "reorg-wiki-attachments", dir)
	if err != nil {
		t.Fatalf("reorg-wiki-attachments failed: %v", err)
	}

	// The attachment should have been moved.
	if _, statErr := os.Stat(imgPath); !os.IsNotExist(statErr) {
		t.Error("original attachment should have been moved out of .attachments/")
	}
	// The summary CSV should appear in stdout.
	if !strings.Contains(stdout, "old_path,new_path") {
		t.Errorf("expected CSV summary in output, got: %q", stdout)
	}
}

func TestReorgWikiAttachments_MeaninglessNameRenamed(t *testing.T) {
	dir := t.TempDir()
	attachDir := filepath.Join(dir, ".attachments")
	if err := os.MkdirAll(attachDir, 0755); err != nil {
		t.Fatal(err)
	}

	// GUID-style filename should be renamed.
	guidFile := "12345678-1234-1234-1234-123456789abc.png"
	if err := os.WriteFile(filepath.Join(attachDir, guidFile), []byte("img"), 0644); err != nil {
		t.Fatal(err)
	}

	mdPath := filepath.Join(dir, "Notes.md")
	mdContent := "# Notes\n\n![](.attachments/" + guidFile + ")\n"
	if err := os.WriteFile(mdPath, []byte(mdContent), 0644); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := runCmd(t, "reorg-wiki-attachments", dir)
	if err != nil {
		t.Fatalf("reorg-wiki-attachments failed: %v", err)
	}

	// The renamed file should exist next to the wiki page.
	entries, _ := os.ReadDir(dir)
	var foundRenamed bool
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "notes-image-") {
			foundRenamed = true
		}
	}
	if !foundRenamed {
		t.Errorf("expected renamed image file in wiki dir, stdout: %q", stdout)
	}
}

// TestSanitizeWikiPageName verifies the slug helper.
func TestSanitizeWikiPageName(t *testing.T) {
	cases := []struct{ input, want string }{
		{"My Page", "my-page"},
		{"Foo  Bar", "foo--bar"},   // two spaces → two dashes (collapsed)
		{"Hello World!", "hello-world-"},
		{"", "page"},
	}
	for _, tc := range cases {
		got := sanitizeWikiPageName(tc.input)
		// "Foo  Bar" has two spaces → after sanitize → "foo--bar" → collapsed to "foo-bar"
		if tc.input == "Foo  Bar" {
			if got != "foo-bar" {
				t.Errorf("sanitizeWikiPageName(%q) = %q, want %q", tc.input, got, "foo-bar")
			}
			continue
		}
		// For "Hello World!", the ! becomes a dash → trailing dash trimmed
		if tc.input == "Hello World!" {
			if got != "hello-world" {
				t.Errorf("sanitizeWikiPageName(%q) = %q, want %q", tc.input, got, "hello-world")
			}
			continue
		}
		if got != tc.want {
			t.Errorf("sanitizeWikiPageName(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
