package cmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/term"

	"btp-open-cli/internal/cf"
	"btp-open-cli/internal/store"
)

// orgChoice is one selectable org in the interactive multi-select prompt.
type orgChoice struct {
	Region string
	ID     string
	Name   string
	APIURL string // apiURL the org was listed from — for callers that need to make further CF calls against it
}

// listAccessibleOrgs lists every org visible across apiURLs, tagged with its
// region, sorted by region then org name (case-insensitive). Regions that
// fail to list (missing token, network error, etc.) are reported as warnings
// on stderr rather than aborting the whole listing.
func listAccessibleOrgs(ctx context.Context, creds *store.Credentials, apiURLs []string) ([]orgChoice, error) {
	var (
		mu   sync.Mutex
		all  []orgChoice
		errs []error
	)
	var wg sync.WaitGroup
	for _, apiURL := range apiURLs {
		wg.Add(1)
		go func(url string) {
			defer wg.Done()
			regionName := store.APIURLToRegion(url)
			tok, ok := creds.Tokens[url]
			if !ok {
				mu.Lock()
				errs = append(errs, fmt.Errorf("[%s] no token — run: bo login --regions %s", regionName, regionName))
				mu.Unlock()
				return
			}
			client := cf.NewClient(url, tok.AccessToken)
			client.SetTokenRefresher(makeTokenRefresher(url, tok.AccessToken))
			orgs, err := client.ListOrganizations(ctx)
			if err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("[%s] listing orgs: %w", regionName, err))
				mu.Unlock()
				return
			}
			mu.Lock()
			for _, o := range orgs {
				all = append(all, orgChoice{Region: regionName, ID: o.GUID, Name: o.Name, APIURL: url})
			}
			mu.Unlock()
		}(apiURL)
	}
	wg.Wait()

	sort.Slice(all, func(i, j int) bool {
		if !strings.EqualFold(all[i].Region, all[j].Region) {
			return strings.ToLower(all[i].Region) < strings.ToLower(all[j].Region)
		}
		return strings.ToLower(all[i].Name) < strings.ToLower(all[j].Name)
	})

	if len(all) == 0 {
		if len(errs) > 0 {
			return nil, errs[0]
		}
		return nil, fmt.Errorf("no accessible orgs found")
	}
	for _, e := range errs {
		fmt.Fprintf(os.Stderr, "warning: %v\n", e)
	}
	return all, nil
}

// orgNumWidth returns the zero-padded digit width used to number orgs in the
// interactive picker: 1 digit for fewer than 10 orgs, 2 for fewer than 100,
// 3 otherwise.
func orgNumWidth(n int) int {
	switch {
	case n >= 100:
		return 3
	case n >= 10:
		return 2
	default:
		return 1
	}
}

// selectOrgsInteractive renders a numbered checkbox list of orgs — one per
// line as "{number} [ ] {region}/{org.name} ({org.id})" — and lets the user
// navigate with the up/down arrow keys, toggle the highlighted entry with
// space, type an org's number to jump to and toggle it directly, press 'a'
// to select every org or 'c' to clear the selection, and confirm with enter.
// If enter is pressed with nothing checked, the currently highlighted entry
// is selected. Numbers are zero-padded to a fixed width (1 digit for fewer
// than 10 orgs, 2 for fewer than 100, 3 otherwise) so a digit sequence of
// that width unambiguously identifies one entry. Returns the selected orgs
// in list order.
func selectOrgsInteractive(ctx context.Context, choices []orgChoice) ([]orgChoice, error) {
	if len(choices) == 0 {
		return nil, fmt.Errorf("no accessible orgs found")
	}
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return nil, fmt.Errorf("interactive org selection requires a terminal — pass --all instead")
	}

	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return nil, fmt.Errorf("entering raw terminal mode: %w", err)
	}
	restored := false
	restore := func() {
		if !restored {
			_ = term.Restore(fd, oldState)
			restored = true
		}
	}
	defer restore()

	numWidth := orgNumWidth(len(choices))

	selected := make([]bool, len(choices))
	cursor := 0
	const headerLines = 2

	render := func(first bool) {
		if !first {
			// Move the cursor back to the top of the list and clear downward
			// before redrawing, so the list updates in place.
			fmt.Fprintf(os.Stdout, "\033[%dA\033[J", len(choices)+headerLines+1)
		}
		fmt.Fprint(os.Stdout, "Select orgs — up/down move, digits jump, space toggle, a=all, c=clear\r\n")
		fmt.Fprint(os.Stdout, "Enter to confirm (confirms the highlighted org if none are checked):\r\n")
		for i, c := range choices {
			mark := " "
			if selected[i] {
				mark = "x"
			}
			num := fmt.Sprintf("%0*d", numWidth, i+1)
			line := fmt.Sprintf("%s [%s] %s/%s (%s)", num, mark, c.Region, c.Name, c.ID)
			if i == cursor {
				fmt.Fprintf(os.Stdout, "\033[7m> %s\033[0m\r\n", line)
			} else {
				fmt.Fprintf(os.Stdout, "  %s\r\n", line)
			}
		}
		fmt.Fprint(os.Stdout, "\r\n")
	}
	render(true)

	reader := bufio.NewReader(os.Stdin)
	readByte := func() (byte, error) {
		type result struct {
			b   byte
			err error
		}
		ch := make(chan result, 1)
		go func() {
			b, err := reader.ReadByte()
			ch <- result{b, err}
		}()
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case r := <-ch:
			return r.b, r.err
		}
	}

	var digitBuf []byte

	for {
		b, err := readByte()
		if err != nil {
			return nil, fmt.Errorf("aborted")
		}
		switch {
		case b == 3: // Ctrl-C
			return nil, fmt.Errorf("aborted")
		case b == '\r' || b == '\n':
			var result []orgChoice
			for i, sel := range selected {
				if sel {
					result = append(result, choices[i])
				}
			}
			if len(result) == 0 {
				result = []orgChoice{choices[cursor]}
			}
			restore()
			fmt.Fprint(os.Stdout, "\r\n")
			return result, nil
		case b == ' ':
			digitBuf = digitBuf[:0]
			selected[cursor] = !selected[cursor]
			render(false)
		case b == 'a' || b == 'A':
			digitBuf = digitBuf[:0]
			for i := range selected {
				selected[i] = true
			}
			render(false)
		case b == 'c' || b == 'C':
			digitBuf = digitBuf[:0]
			for i := range selected {
				selected[i] = false
			}
			render(false)
		case b >= '0' && b <= '9':
			digitBuf = append(digitBuf, b)
			if len(digitBuf) >= numWidth {
				if n, convErr := strconv.Atoi(string(digitBuf)); convErr == nil && n >= 1 && n <= len(choices) {
					idx := n - 1
					selected[idx] = !selected[idx]
					cursor = idx
				}
				digitBuf = digitBuf[:0]
			}
			render(false)
		case b == 27: // ESC — start of an arrow-key escape sequence
			digitBuf = digitBuf[:0]
			b2, err := readByte()
			if err != nil || b2 != '[' {
				continue
			}
			b3, err := readByte()
			if err != nil {
				continue
			}
			switch b3 {
			case 'A': // up
				if cursor > 0 {
					cursor--
				}
			case 'B': // down
				if cursor < len(choices)-1 {
					cursor++
				}
			}
			render(false)
		}
	}
}

// resolveOutputWriter returns os.Stdout when outputFlag is empty, or creates
// (truncating if it already exists) the file at outputFlag otherwise.
//
// Commands with interactive org selection need this instead of shell '>'
// redirection: the picker itself is drawn to stdout, so redirecting stdout at
// the shell level would send the picker's own UI into the file right along
// with the interactive session never seeing the visible prompt. Passing
// --output/-o instead keeps the picker on the real terminal and only the
// final result goes to the file.
//
// The returned close function must be called (e.g. via defer) once done
// writing; it is a no-op when writing to stdout.
func resolveOutputWriter(outputFlag string) (io.Writer, func() error, error) {
	if outputFlag == "" {
		return os.Stdout, func() error { return nil }, nil
	}
	f, err := os.Create(outputFlag)
	if err != nil {
		return nil, nil, fmt.Errorf("creating output file %s: %w", outputFlag, err)
	}
	return f, f.Close, nil
}

// resolveDefaultOrgScope returns the org scope selected via `bo orgs` for the
// current login session, converted to a cosOrgSet. Callers must invoke this
// only after confirming that neither --org nor --orgs (or a command-specific
// equivalent) was given; it never prompts interactively itself — only the
// `orgs` command does that.
func resolveDefaultOrgScope(creds *store.Credentials) (cosOrgSet, error) {
	if len(creds.DefaultOrgScope) == 0 {
		return nil, fmt.Errorf("no orgs specified — run 'bo orgs' to select a default scope for this login session, or pass --org/--orgs")
	}
	set := make(cosOrgSet, len(creds.DefaultOrgScope))
	for i, r := range creds.DefaultOrgScope {
		set[i] = cosOrgRef{Region: r.Region, ID: r.ID, Name: r.Name, APIURL: r.APIURL}
	}
	return set, nil
}

// resolveOrgTargets returns the org identifiers to operate on for commands
// that resolve a client per org by GUID or name substring (destination-service
// commands): --org (a single GUID or name substring, resolved downstream by
// the caller), --orgs (a CSV of exact org refs), or the default scope set via
// `bo orgs`. Callers must invoke this only after confirming --org was empty.
func resolveOrgTargets(creds *store.Credentials, orgFlag, orgsFile string) ([]string, error) {
	if orgFlag != "" {
		return []string{orgFlag}, nil
	}
	var (
		refs cosOrgSet
		err  error
	)
	if orgsFile != "" {
		refs, err = parseCosOrgCSV(orgsFile)
		if err != nil {
			return nil, fmt.Errorf("invalid --orgs CSV: %w", err)
		}
	} else {
		refs, err = resolveDefaultOrgScope(creds)
		if err != nil {
			return nil, err
		}
	}
	targets := make([]string, 0, len(refs))
	for _, ref := range refs {
		if ref.ID == "" {
			return nil, fmt.Errorf("--orgs entries must include org_id for this command")
		}
		targets = append(targets, ref.ID)
	}
	return targets, nil
}
