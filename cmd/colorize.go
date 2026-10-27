package cmd

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/term"
)

// columnColors are ANSI SGR foreground codes cycled across columns, giving
// toon/csv console output the "rainbow csv" effect: adjacent columns get
// different colors so wide rows stay easy to scan by eye.
var columnColors = []string{
	"\x1b[36m", // cyan
	"\x1b[33m", // yellow
	"\x1b[35m", // magenta
	"\x1b[32m", // green
	"\x1b[34m", // blue
	"\x1b[31m", // red
	"\x1b[96m", // bright cyan
	"\x1b[93m", // bright yellow
}

const colorReset = "\x1b[0m"

// validColorModes are the accepted values of the global --color flag.
var validColorModes = map[string]bool{"auto": true, "always": true, "never": true}

// consoleColorEnabled reports whether column coloring should be applied to a
// command's toon/csv output, based on the global --color flag:
//   - "always" forces color on, regardless of destination.
//   - "never" forces color off.
//   - "auto" (the default) enables color only for an interactive terminal on
//     stdout — never when the result is redirected to a file via
//     --output/-o, piped to another program, or NO_COLOR is set — so saved
//     or piped output stays plain and machine-readable.
//
// colorMode is expected to already be validated (see validColorModes); an
// unrecognized value is treated like "auto".
func consoleColorEnabled(outputFile, colorMode string) bool {
	switch strings.ToLower(colorMode) {
	case "always":
		return true
	case "never":
		return false
	}
	if outputFile != "" {
		return false
	}
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if term.IsTerminal(int(os.Stdout.Fd())) {
		return true
	}
	// isatty can come back false even with a real terminal (or an
	// ANSI-capable view) on the other end, when something between us and it
	// means our own stdout fd isn't itself a pty:
	//   - SSH_TTY is only set by sshd when the client allocated a pty for
	//     the session, so its presence means there is a terminal watching.
	//   - TERM_PROGRAM=vscode is set by every surface VS Code runs a
	//     process's output into (the integrated terminal, Tasks output,
	//     and the Debug Console) — including over a Remote-SSH/SSH tunnel
	//     connection, where the terminal shell itself runs on the remote
	//     host via the remote extension host rather than through a fresh
	//     sshd pty negotiation, so SSH_TTY alone isn't set there. All of
	//     those surfaces render through xterm.js and support ANSI color.
	if os.Getenv("SSH_TTY") != "" {
		return true
	}
	return os.Getenv("TERM_PROGRAM") == "vscode"
}

// colorizeFields wraps each field in a color cycled by column index. When
// enabled is false, fields are returned unchanged.
func colorizeFields(fields []string, enabled bool) []string {
	if !enabled {
		return fields
	}
	out := make([]string, len(fields))
	for i, f := range fields {
		out[i] = columnColors[i%len(columnColors)] + f + colorReset
	}
	return out
}

// rainbowCSVWriter is a drop-in replacement for csv.NewWriter that colorizes
// each record's columns before writing, when enabled. ANSI escape codes
// never contain a comma, quote, or newline, so they never trigger csv's own
// quoting logic.
type rainbowCSVWriter struct {
	w       *csv.Writer
	enabled bool
}

func newRainbowCSVWriter(w io.Writer, enabled bool) *rainbowCSVWriter {
	return &rainbowCSVWriter{w: csv.NewWriter(w), enabled: enabled}
}

func (r *rainbowCSVWriter) Write(record []string) error {
	return r.w.Write(colorizeFields(record, r.enabled))
}

func (r *rainbowCSVWriter) Flush() {
	r.w.Flush()
}

// toonTabularHeaderRe matches a TOON compact tabular array header, e.g.
//
//	spaces[1]{space_id,space_name}:
//	  - spaces[1]{space_id,space_name}:
//
// Capturing leading indentation, an optional list-item dash, the key name,
// row count, and the comma-separated field names.
var toonTabularHeaderRe = regexp.MustCompile(`^(\s*)(- )?([A-Za-z0-9_]+)\[(\d+)\]\{([^}]*)\}:\s*$`)

// colorizeToon post-processes TOON-encoded output to colorize the columns of
// every compact tabular array (the `key[N]{f1,f2,...}:` form the toon
// encoder uses for uniform arrays of flat objects, which is how all these
// commands render their row data) — both the field names in the header and
// the comma-separated values in the rows that follow, column by column,
// à la "rainbow csv". Non-tabular lines (plain `key: value` pairs, nested
// object lists) are left untouched. When enabled is false, data is returned
// unchanged.
func colorizeToon(data []byte, enabled bool) []byte {
	if !enabled {
		return data
	}
	lines := strings.Split(string(data), "\n")
	remaining := 0
	var dataIndent string
	for i, line := range lines {
		if remaining > 0 {
			if content, ok := strings.CutPrefix(line, dataIndent); ok {
				fields := splitToonRow(content)
				lines[i] = dataIndent + strings.Join(colorizeFields(fields, true), ",")
				remaining--
				continue
			}
			remaining = 0
		}
		if m := toonTabularHeaderRe.FindStringSubmatch(line); m != nil {
			indent, dash, name, countStr, fieldsCSV := m[1], m[2], m[3], m[4], m[5]
			n, _ := strconv.Atoi(countStr)
			fieldNames := strings.Split(fieldsCSV, ",")
			colored := colorizeFields(fieldNames, true)
			lines[i] = fmt.Sprintf("%s%s%s[%s]{%s}:", indent, dash, name, countStr, strings.Join(colored, ","))
			remaining = n
			dataIndent = indent + strings.Repeat(" ", len(dash)) + "  "
		}
	}
	return []byte(strings.Join(lines, "\n"))
}

// splitToonRow splits one TOON tabular data row into its comma-separated
// fields, respecting TOON's double-quoted, backslash-escaped string syntax
// so a comma inside a quoted field is not treated as a column separator.
func splitToonRow(s string) []string {
	var fields []string
	var cur strings.Builder
	inQuotes := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inQuotes {
			cur.WriteByte(c)
			if c == '\\' && i+1 < len(s) {
				i++
				cur.WriteByte(s[i])
				continue
			}
			if c == '"' {
				inQuotes = false
			}
			continue
		}
		switch c {
		case '"':
			inQuotes = true
			cur.WriteByte(c)
		case ',':
			fields = append(fields, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	fields = append(fields, cur.String())
	return fields
}
