package cmd

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

var countLinesCmd = &cobra.Command{
	Use:   "count-lines [folder]",
	Short: "Count total lines of source code files within a folder",
	Long: `Walk [folder] (default: current folder) and count the number of lines in
every source code file, broken down by file extension.

The ".git" directory is always skipped. If a ".gitignore" file is present at the
root of [folder], its rules are honoured and matching files and directories are
excluded from the count (a common subset of the gitignore syntax is supported:
comments, negation with "!", directory-only patterns, anchored patterns and the
"*", "?" and "**" wildcards).

Only files with a recognised source code extension (or a well-known name such as
"Makefile" or "Dockerfile") are counted; everything else is ignored.`,
	Args:    cobra.MaximumNArgs(1),
	GroupID: "utilities",
	RunE:    runCountLines,
}

func init() {
	rootCmd.AddCommand(countLinesCmd)
}

// countLinesSourceExts is the set of file extensions treated as source code.
var countLinesSourceExts = map[string]bool{
	".go": true, ".py": true, ".js": true, ".jsx": true, ".ts": true, ".tsx": true,
	".mjs": true, ".cjs": true, ".c": true, ".h": true, ".cc": true, ".cpp": true,
	".cxx": true, ".hpp": true, ".hh": true, ".java": true, ".kt": true, ".kts": true,
	".scala": true, ".rs": true, ".rb": true, ".php": true, ".pl": true, ".pm": true,
	".lua": true, ".swift": true, ".m": true, ".mm": true, ".cs": true, ".fs": true,
	".fsx": true, ".sh": true, ".bash": true, ".zsh": true, ".fish": true, ".ps1": true,
	".psm1": true, ".sql": true, ".r": true, ".jl": true, ".dart": true, ".ex": true,
	".exs": true, ".erl": true, ".hrl": true, ".clj": true, ".cljs": true, ".cljc": true,
	".hs": true, ".ml": true, ".mli": true, ".vue": true, ".svelte": true, ".astro": true,
	".html": true, ".htm": true, ".css": true, ".scss": true, ".sass": true, ".less": true,
	".json": true, ".jsonc": true, ".yaml": true, ".yml": true, ".toml": true, ".ini": true,
	".xml": true, ".proto": true, ".graphql": true, ".gql": true, ".md": true, ".mdx": true,
	".rst": true, ".tex": true, ".gradle": true, ".bat": true, ".cmd": true, ".vim": true,
	".el": true, ".tf": true, ".tfvars": true, ".hcl": true, ".zig": true, ".nim": true,
	".groovy": true, ".make": true, ".mk": true, ".cmake": true, ".dockerfile": true,
}

// countLinesSourceNames is the set of extension-less filenames treated as source code.
var countLinesSourceNames = map[string]bool{
	"Makefile": true, "makefile": true, "GNUmakefile": true, "Dockerfile": true,
	"Rakefile": true, "Gemfile": true, "Vagrantfile": true, "Jenkinsfile": true,
	"CMakeLists.txt": true, "Brewfile": true, "Procfile": true,
}

// countLinesIgnoreNames is the set of filenames that are never counted, even
// when their extension is a recognised source code extension (e.g. machine-
// generated lock files).
var countLinesIgnoreNames = map[string]bool{
	"package-lock.json": true,
}

func isCountLinesSourceFile(name string) bool {
	if countLinesIgnoreNames[name] {
		return false
	}
	if countLinesSourceNames[name] {
		return true
	}
	ext := strings.ToLower(filepath.Ext(name))
	return ext != "" && countLinesSourceExts[ext]
}

func runCountLines(cmd *cobra.Command, args []string) error {
	root := "."
	if len(args) == 1 {
		root = args[0]
	}

	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("cannot access %q: %w", root, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%q is not a directory", root)
	}

	var gi *gitIgnore
	if data, err := os.ReadFile(filepath.Join(root, ".gitignore")); err == nil {
		gi = parseGitIgnore(string(data))
	}

	type stat struct {
		files int
		lines int
	}
	byExt := map[string]*stat{}
	var totalFiles, totalLines int

	walkErr := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil
		}

		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			if gi != nil && gi.ignored(rel, true) {
				return filepath.SkipDir
			}
			return nil
		}

		if gi != nil && gi.ignored(rel, false) {
			return nil
		}
		if !isCountLinesSourceFile(d.Name()) {
			return nil
		}

		n, cErr := countFileLines(p)
		if cErr != nil {
			fmt.Fprintf(os.Stderr, "  ! %s: %v\n", rel, cErr)
			return nil
		}

		key := strings.ToLower(filepath.Ext(d.Name()))
		if key == "" {
			key = d.Name()
		}
		s := byExt[key]
		if s == nil {
			s = &stat{}
			byExt[key] = s
		}
		s.files++
		s.lines += n
		totalFiles++
		totalLines += n
		return nil
	})
	if walkErr != nil {
		return walkErr
	}

	exts := make([]string, 0, len(byExt))
	for k := range byExt {
		exts = append(exts, k)
	}
	sort.Slice(exts, func(i, j int) bool {
		if byExt[exts[i]].lines != byExt[exts[j]].lines {
			return byExt[exts[i]].lines > byExt[exts[j]].lines
		}
		return exts[i] < exts[j]
	})

	extW := len("EXTENSION")
	for _, e := range exts {
		if len(e) > extW {
			extW = len(e)
		}
	}
	filesW := len("FILES")
	if w := len(fmt.Sprintf("%d", totalFiles)); w > filesW {
		filesW = w
	}
	linesW := len("LINES")
	if w := len(fmt.Sprintf("%d", totalLines)); w > linesW {
		linesW = w
	}

	printRow := func(ext string, files, lines string) {
		fmt.Printf("%-*s  %*s  %*s\n", extW, ext, filesW, files, linesW, lines)
	}
	printRow("EXTENSION", "FILES", "LINES")
	printRow(strings.Repeat("-", extW), strings.Repeat("-", filesW), strings.Repeat("-", linesW))
	for _, e := range exts {
		printRow(e, fmt.Sprintf("%d", byExt[e].files), fmt.Sprintf("%d", byExt[e].lines))
	}
	printRow(strings.Repeat("-", extW), strings.Repeat("-", filesW), strings.Repeat("-", linesW))
	printRow("TOTAL", fmt.Sprintf("%d", totalFiles), fmt.Sprintf("%d", totalLines))
	return nil
}

// countFileLines returns the number of lines in the file at p. A trailing line
// without a newline is counted; a completely empty file has zero lines.
func countFileLines(p string) (int, error) {
	f, err := os.Open(p)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	r := bufio.NewReader(f)
	var lines int
	var sawData, pendingLine bool
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		for _, b := range buf[:n] {
			sawData = true
			if b == '\n' {
				lines++
				pendingLine = false
			} else {
				pendingLine = true
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, err
		}
	}
	if sawData && pendingLine {
		lines++
	}
	return lines, nil
}

// gitIgnore is a compiled .gitignore file supporting a common subset of the
// gitignore pattern syntax.
type gitIgnore struct {
	patterns []gitIgnorePattern
}

type gitIgnorePattern struct {
	negate   bool
	dirOnly  bool
	anchored bool
	re       *regexp.Regexp
}

func parseGitIgnore(content string) *gitIgnore {
	gi := &gitIgnore{}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		// Trailing spaces are not significant unless escaped.
		line = strings.TrimRight(line, " ")
		if line == "" {
			continue
		}

		p := gitIgnorePattern{}
		if strings.HasPrefix(line, "!") {
			p.negate = true
			line = line[1:]
		}
		if strings.HasPrefix(line, `\#`) || strings.HasPrefix(line, `\!`) {
			line = line[1:]
		}
		if strings.HasSuffix(line, "/") {
			p.dirOnly = true
			line = strings.TrimSuffix(line, "/")
		}
		if line == "" {
			continue
		}
		// A pattern is anchored to the .gitignore location if it contains a
		// slash anywhere other than a trailing one (already stripped).
		p.anchored = strings.Contains(line, "/")
		line = strings.TrimPrefix(line, "/")
		p.re = regexp.MustCompile("^" + translateGitIgnoreGlob(line) + "$")
		gi.patterns = append(gi.patterns, p)
	}
	return gi
}

// translateGitIgnoreGlob converts a gitignore glob into an anchored regexp body.
func translateGitIgnoreGlob(g string) string {
	var b strings.Builder
	n := len(g)
	for i := 0; i < n; i++ {
		c := g[i]
		switch c {
		case '*':
			if i+1 < n && g[i+1] == '*' {
				i++
				if i+1 < n && g[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '.', '+', '(', ')', '|', '^', '$', '{', '}', '[', ']', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// ignored reports whether the path rel (relative to the .gitignore location,
// slash- or OS-separated) is excluded. Later patterns override earlier ones.
func (gi *gitIgnore) ignored(rel string, isDir bool) bool {
	rel = filepath.ToSlash(rel)
	ignored := false
	for _, p := range gi.patterns {
		if p.dirOnly && !isDir {
			continue
		}
		target := rel
		if !p.anchored {
			target = path.Base(rel)
		}
		if p.re.MatchString(target) {
			ignored = !p.negate
		}
	}
	return ignored
}
