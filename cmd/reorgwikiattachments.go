package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/spf13/cobra"
)

var reorgWikiAttachmentsCmd = &cobra.Command{
	Use:   "reorg-wiki-attachments [path]",
	Short: "Move wiki attachments from .attachments/ next to their wiki pages",
	Long: `Reorganizes wiki attachments:
  1. Inventories all files in [path]/.attachments/
  2. Scans every .md wiki page for references to /.attachments/ files
  3. Moves each attachment to the same folder as the first page that references it
  4. Renames files with meaningless names (image*, UUID/GUID, ==image_*) to
     {wiki_page_name}-image-N.ext
  5. Updates all attachment references across every wiki page
  6. Prints a CSV summary: old_path,new_path`,
	Args: cobra.ExactArgs(1),
	RunE: runReorgWikiAttachments,
}

func init() {
	reorgWikiAttachmentsCmd.GroupID = "utilities"
	rootCmd.AddCommand(reorgWikiAttachmentsCmd)
}

// wikiAttachRefRe matches markdown image/link syntax that points into .attachments/.
// Sub-matches: [1] alt/link text  [2] full href  [3] filename inside .attachments/
var wikiAttachRefRe = regexp.MustCompile(`(!?\[[^\]]*\])\(([./]*\.attachments/([^)#?\s]+))\)`)

// wikiAttachGUIDRe matches bare UUID or GUID-style prefixes (with optional braces/parens).
var wikiAttachGUIDRe = regexp.MustCompile(`(?i)^[{(]?[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}[})]?`)

// wikiAttachImageRe matches names that start with "image" or "==image" (case-insensitive).
var wikiAttachImageRe = regexp.MustCompile(`(?i)^(==?image|image)`)

// isMeaninglessWikiAttachName returns true when the filename base has no human-readable value.
func isMeaninglessWikiAttachName(filename string) bool {
	base := strings.TrimSuffix(filename, filepath.Ext(filename))
	return wikiAttachGUIDRe.MatchString(base) || wikiAttachImageRe.MatchString(base)
}

// wikiAttachMove records a single file move/rename.
type wikiAttachMove struct {
	OldRelPath string // relative to wikiRoot, slash-separated
	NewRelPath string // relative to wikiRoot, slash-separated
	NewAbsPath string // absolute path after move (used for relative-ref computation)
}

func runReorgWikiAttachments(cmd *cobra.Command, args []string) error {
	wikiRoot, err := filepath.Abs(args[0])
	if err != nil {
		return fmt.Errorf("resolving path: %w", err)
	}
	attachmentsDir := filepath.Join(wikiRoot, ".attachments")

	// ── Phase 1: inventory .attachments/ ──────────────────────────────────────
	attachmentExists := map[string]bool{}
	_ = filepath.Walk(attachmentsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return filepath.SkipDir
			}
			return nil // keep walking on other errors
		}
		if !info.IsDir() {
			attachmentExists[info.Name()] = true
		}
		return nil
	})
	fmt.Fprintf(cmd.OutOrStdout(), "Attachments found in .attachments/: %d\n", len(attachmentExists))

	// ── Phase 2: collect .md files (skip .attachments subtree) ────────────────
	var mdFiles []string
	if err := filepath.Walk(wikiRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() && info.Name() == ".attachments" {
			return filepath.SkipDir
		}
		if !info.IsDir() && strings.EqualFold(filepath.Ext(path), ".md") {
			mdFiles = append(mdFiles, path)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("walking wiki directory: %w", err)
	}
	sort.Strings(mdFiles) // deterministic processing order
	fmt.Fprintf(cmd.OutOrStdout(), "Wiki pages found:              %d\n\n", len(mdFiles))

	// ── Phase 3: process each page — move files and update its own references ──
	// moves: keyed by original filename in .attachments/
	moves := map[string]*wikiAttachMove{}

	for _, mdPath := range mdFiles {
		if err := reorgProcessPage(cmd, wikiRoot, attachmentsDir, mdPath, moves); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s: %v\n", mdPath, err)
		}
	}

	// ── Phase 4: second pass — update any remaining cross-page references ──────
	for _, mdPath := range mdFiles {
		if err := reorgUpdateCrossRefs(mdPath, moves); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: updating cross-refs in %s: %v\n", mdPath, err)
		}
	}

	// ── Phase 5: print CSV summary ─────────────────────────────────────────────
	if len(moves) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No attachments were moved.")
		return nil
	}

	type csvRow struct{ old, new string }
	rows := make([]csvRow, 0, len(moves))
	for _, mv := range moves {
		rows = append(rows, csvRow{mv.OldRelPath, mv.NewRelPath})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].old < rows[j].old })

	fmt.Fprintln(cmd.OutOrStdout(), "old_path,new_path")
	for _, row := range rows {
		fmt.Fprintf(cmd.OutOrStdout(), "%s,%s\n", row.old, row.new)
	}
	return nil
}

// reorgProcessPage processes one wiki page: moves attachments it references and
// rewrites those references in-place.
func reorgProcessPage(
	cmd *cobra.Command,
	wikiRoot, attachmentsDir, mdPath string,
	moves map[string]*wikiAttachMove,
) error {
	data, err := os.ReadFile(mdPath)
	if err != nil {
		return err
	}
	src := string(data)

	pageDir := filepath.Dir(mdPath)
	pageBase := sanitizeWikiPageName(strings.TrimSuffix(filepath.Base(mdPath), filepath.Ext(mdPath)))

	localCounter := 0 // per-page image rename counter
	changed := false

	result := wikiAttachRefRe.ReplaceAllStringFunc(src, func(match string) string {
		sub := wikiAttachRefRe.FindStringSubmatch(match)
		if sub == nil {
			return match
		}
		altText := sub[1]      // e.g. "![alt text]" or "[link]"
		origFilename := sub[3] // e.g. "foo.png" (just the base name)

		// ── Already moved by an earlier page? Just update the reference. ──────
		if mv, ok := moves[origFilename]; ok {
			rel, err := filepath.Rel(pageDir, mv.NewAbsPath)
			if err != nil {
				return match
			}
			changed = true
			return altText + "(" + filepath.ToSlash(rel) + ")"
		}

		// ── Verify the file still lives in .attachments/ ──────────────────────
		origAbsPath := filepath.Join(attachmentsDir, origFilename)
		if _, statErr := os.Stat(origAbsPath); statErr != nil {
			return match // missing or inaccessible — leave reference unchanged
		}

		// ── Determine destination filename ────────────────────────────────────
		ext := filepath.Ext(origFilename)
		var newFilename string
		if isMeaninglessWikiAttachName(origFilename) {
			localCounter++
			newFilename = fmt.Sprintf("%s-image-%d%s", pageBase, localCounter, ext)
		} else {
			newFilename = origFilename
		}
		newFilename = resolveWikiAttachCollision(pageDir, newFilename)
		destAbsPath := filepath.Join(pageDir, newFilename)

		// ── Move the file ─────────────────────────────────────────────────────
		if err := os.Rename(origAbsPath, destAbsPath); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: move %s -> %s: %v\n", origAbsPath, destAbsPath, err)
			return match
		}

		oldRel, _ := filepath.Rel(wikiRoot, origAbsPath)
		newRel, _ := filepath.Rel(wikiRoot, destAbsPath)
		moves[origFilename] = &wikiAttachMove{
			OldRelPath: filepath.ToSlash(oldRel),
			NewRelPath: filepath.ToSlash(newRel),
			NewAbsPath: destAbsPath,
		}
		fmt.Fprintf(cmd.OutOrStdout(), "  moved: %s\n      -> %s\n",
			filepath.ToSlash(oldRel), filepath.ToSlash(newRel))

		rel, err := filepath.Rel(pageDir, destAbsPath)
		if err != nil {
			return match
		}
		changed = true
		return altText + "(" + filepath.ToSlash(rel) + ")"
	})

	if changed {
		return os.WriteFile(mdPath, []byte(result), 0644)
	}
	return nil
}

// reorgUpdateCrossRefs rewrites any remaining .attachments/ references in mdPath
// that point to files already recorded in moves.
func reorgUpdateCrossRefs(mdPath string, moves map[string]*wikiAttachMove) error {
	data, err := os.ReadFile(mdPath)
	if err != nil {
		return err
	}
	src := string(data)
	pageDir := filepath.Dir(mdPath)
	changed := false

	result := wikiAttachRefRe.ReplaceAllStringFunc(src, func(match string) string {
		sub := wikiAttachRefRe.FindStringSubmatch(match)
		if sub == nil {
			return match
		}
		altText := sub[1]
		origFilename := sub[3]

		mv, ok := moves[origFilename]
		if !ok {
			return match
		}
		rel, err := filepath.Rel(pageDir, mv.NewAbsPath)
		if err != nil {
			return match
		}
		changed = true
		return altText + "(" + filepath.ToSlash(rel) + ")"
	})

	if changed {
		return os.WriteFile(mdPath, []byte(result), 0644)
	}
	return nil
}

// sanitizeWikiPageName converts a wiki page name to a safe lowercase slug
// suitable for use as a filename prefix.
func sanitizeWikiPageName(name string) string {
	var sb strings.Builder
	for _, r := range name {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			sb.WriteRune(unicode.ToLower(r))
		default:
			sb.WriteRune('-')
		}
	}
	s := strings.Trim(sb.String(), "-")
	// collapse consecutive dashes
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	if s == "" {
		return "page"
	}
	return s
}

// resolveWikiAttachCollision returns a non-colliding filename in dir.
// If filename is already free, it is returned as-is. Otherwise, it appends
// -2, -3, … before the extension until a free name is found.
func resolveWikiAttachCollision(dir, filename string) string {
	if _, err := os.Stat(filepath.Join(dir, filename)); os.IsNotExist(err) {
		return filename
	}
	ext := filepath.Ext(filename)
	base := strings.TrimSuffix(filename, ext)
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d%s", base, i, ext)
		if _, err := os.Stat(filepath.Join(dir, candidate)); os.IsNotExist(err) {
			return candidate
		}
	}
}
