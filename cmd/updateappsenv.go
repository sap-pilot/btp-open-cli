package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"

	toonenc "github.com/toon-format/toon-go"

	"btp-open-cli/internal/cf"
	"btp-open-cli/internal/store"

	"github.com/spf13/cobra"
)

// uaeConcurrency bounds how many /v3/apps/:guid/environment_variables PATCH
// calls run at once across the whole batch.
const uaeConcurrency = 8

// uaeSensitiveKeySuffixes holds the (upper-cased) env var name suffixes
// whose value is masked in the confirmation preview rather than shown in
// plain text.
var uaeSensitiveKeySuffixes = []string{"KEY", "SECRET", "PASSWORD", "PASSWD"}

// uaeIsSensitiveKey reports whether name should be masked in the preview,
// matched case-insensitively against uaeSensitiveKeySuffixes.
func uaeIsSensitiveKey(name string) bool {
	upper := strings.ToUpper(name)
	for _, suf := range uaeSensitiveKeySuffixes {
		if strings.HasSuffix(upper, suf) {
			return true
		}
	}
	return false
}

// uaeMaskValue returns value as-is, or a fixed mask if key is sensitive —
// for display only; the real value is always sent to the CF API.
func uaeMaskValue(key, value string) string {
	if uaeIsSensitiveKey(key) {
		return "********"
	}
	return value
}

// uaeFileEntry is one app's environment variables, scoped to a region/org/
// space, as read from an --apps-env file (the JSON/TOON shape 'bo apps
// --include-env' writes).
type uaeFileEntry struct {
	Region    string
	OrgID     string
	OrgName   string
	SpaceName string
	AppName   string
	Env       map[string]string
}

// parseAppsEnvFile reads path — JSON or TOON, as written by
// 'bo apps --include-env', or hand-authored with the same
// regions -> orgs -> spaces -> apps shape — and flattens it into one entry
// per app that actually has environment variables set. Apps with no
// "app_env" (nothing to update) are silently skipped.
func parseAppsEnvFile(path string) ([]uaeFileEntry, error) {
	if path == "" {
		return nil, fmt.Errorf("no file given")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("opening file: %w", err)
	}

	var doc appsOutDoc
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		if err := json.Unmarshal(trimmed, &doc); err != nil {
			return nil, fmt.Errorf("parsing JSON: %w", err)
		}
	} else {
		if err := toonenc.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("parsing TOON: %w", err)
		}
	}

	var entries []uaeFileEntry
	for _, r := range doc.Regions {
		for _, o := range r.Orgs {
			for _, sp := range o.Spaces {
				for _, a := range sp.Apps {
					if len(a.Env) == 0 {
						continue
					}
					env := a.Env
					for k := range env {
						if appsSkippedEnvKeys[strings.ToUpper(k)] {
							delete(env, k)
						}
					}
					if len(env) == 0 {
						continue
					}
					entries = append(entries, uaeFileEntry{
						Region:    r.ID,
						OrgID:     o.ID,
						OrgName:   o.Name,
						SpaceName: sp.Name,
						AppName:   a.Name,
						Env:       env,
					})
				}
			}
		}
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("file contains no apps with environment variables set (run 'bo apps --include-env' to produce one)")
	}
	return entries, nil
}

// uaeTarget is one app, resolved to its actual GUID and client in whichever
// org is actually being targeted (the file's own org, or a 'bo orgs'
// retarget destination — see resolveFileOrgRetarget), ready to update.
type uaeTarget struct {
	Client    *cf.Client
	Region    string
	OrgName   string
	SpaceName string
	AppName   string
	AppGUID   string
	Env       map[string]string
}

// resolveUaeTargets groups entries by (region, org) and, for each group,
// lists that org's spaces and apps exactly once, then resolves each entry's
// own space/app name (case-insensitive exact match) to a concrete app GUID
// in that org — since an app's GUID from the source file is almost
// certainly wrong once the org differs (or even if it doesn't, the file may
// be stale). An entry whose org has no token, or whose space/app name isn't
// found in that org, is reported as a warning on stderr and skipped rather
// than aborting the whole run.
func resolveUaeTargets(ctx context.Context, creds *store.Credentials, entries []uaeFileEntry) []uaeTarget {
	type groupKey struct{ region, orgID string }
	groups := make(map[groupKey][]uaeFileEntry)
	var order []groupKey
	for _, e := range entries {
		k := groupKey{e.Region, e.OrgID}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], e)
	}

	// Map each region name back to its authenticated CF API URL by deriving
	// the region from every cached token's own apiURL key (the exact inverse
	// of how 'bo apps' wrote the region into the file in the first place),
	// rather than reconstructing the URL from the region name — the two are
	// equivalent for standard SAP BTP endpoints, but only this direction
	// also works against non-standard ones (e.g. in tests).
	apiURLByRegion := make(map[string]string, len(creds.Tokens))
	for apiURL := range creds.Tokens {
		apiURLByRegion[store.APIURLToRegion(apiURL)] = apiURL
	}

	var targets []uaeTarget
	for _, k := range order {
		groupEntries := groups[k]
		apiURL, ok := apiURLByRegion[k.region]
		if !ok {
			fmt.Fprintf(os.Stderr, "warning: [%s] no token — run: bo login --regions %s — skipping %d app(s)\n",
				k.region, k.region, len(groupEntries))
			continue
		}
		tok := creds.Tokens[apiURL]
		client := cf.NewClient(apiURL, tok.AccessToken)
		client.SetTokenRefresher(makeTokenRefresher(apiURL, tok.AccessToken))

		orgName := groupEntries[0].OrgName

		spaces, err := client.ListOrganizationSpaces(ctx, k.orgID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: [%s] %s: listing spaces: %v — skipping %d app(s)\n",
				k.region, orgName, err, len(groupEntries))
			continue
		}
		spaceByName := make(map[string]cf.Space, len(spaces))
		spaceGUIDs := make([]string, len(spaces))
		for i, sp := range spaces {
			spaceByName[strings.ToLower(sp.Name)] = sp
			spaceGUIDs[i] = sp.GUID
		}

		var appsBySpace map[string][]cf.App
		if len(spaceGUIDs) > 0 {
			apps, err := client.ListAppsBySpaces(ctx, spaceGUIDs)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: [%s] %s: listing apps: %v — skipping %d app(s)\n",
					k.region, orgName, err, len(groupEntries))
				continue
			}
			appsBySpace = make(map[string][]cf.App)
			for _, a := range apps {
				appsBySpace[a.Relationships.Space.Data.GUID] = append(appsBySpace[a.Relationships.Space.Data.GUID], a)
			}
		}

		for _, e := range groupEntries {
			sp, ok := spaceByName[strings.ToLower(e.SpaceName)]
			if !ok {
				fmt.Fprintf(os.Stderr, "warning: [%s] %s: space %q not found — skipping app %q\n",
					k.region, orgName, e.SpaceName, e.AppName)
				continue
			}
			var appGUID string
			for _, a := range appsBySpace[sp.GUID] {
				if strings.EqualFold(a.Name, e.AppName) {
					appGUID = a.GUID
					break
				}
			}
			if appGUID == "" {
				fmt.Fprintf(os.Stderr, "warning: [%s] %s/%s: app %q not found — skipping\n",
					k.region, orgName, e.SpaceName, e.AppName)
				continue
			}
			targets = append(targets, uaeTarget{
				Client:    client,
				Region:    k.region,
				OrgName:   orgName,
				SpaceName: e.SpaceName,
				AppName:   e.AppName,
				AppGUID:   appGUID,
				Env:       e.Env,
			})
		}
	}
	return targets
}

// printUaeTable prints the planned updates as an aligned table (region, org,
// space, app, env), masking sensitive values (see uaeIsSensitiveKey) in the
// env column.
func printUaeTable(targets []uaeTarget) {
	headers := []string{"REGION", "ORG", "SPACE", "APP", "ENV"}
	rows := make([][]string, len(targets))
	for i, t := range targets {
		keys := make([]string, 0, len(t.Env))
		for k := range t.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		pairs := make([]string, len(keys))
		for j, k := range keys {
			pairs[j] = k + "=" + uaeMaskValue(k, t.Env[k])
		}
		rows[i] = []string{t.Region, t.OrgName, t.SpaceName, t.AppName, strings.Join(pairs, ", ")}
	}

	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i, v := range row[:len(row)-1] { // last column (ENV) isn't padded
			if len(v) > widths[i] {
				widths[i] = len(v)
			}
		}
	}
	printRow := func(cols []string) {
		parts := make([]string, len(cols))
		for i, v := range cols {
			if i == len(cols)-1 {
				parts[i] = v
				continue
			}
			parts[i] = fmt.Sprintf("%-*s", widths[i], v)
		}
		fmt.Fprintln(os.Stdout, strings.TrimRight(strings.Join(parts, "  "), " "))
	}
	printRow(headers)
	for _, row := range rows {
		printRow(row)
	}
}

var updateAppsEnvCmd = &cobra.Command{
	Use:   "update-apps-env <apps-env-file>",
	Short: "Create/update apps' user-provided environment variables from a 'bo apps --include-env' export",
	Long: `Read <apps-env-file> — a JSON or TOON document in the regions -> orgs ->
spaces -> apps shape written by 'bo apps --include-env' (or hand-authored
with the same shape) — and create/update each app's "app_env" entries as its
own user-provided environment variables (the same ones 'cf set-env'
manages), via the CF v3 API's key-level merge: existing keys not present in
the file are left untouched. DEPLOY_ATTRIBUTES, if present in the file, is
always skipped (same as 'bo apps --include-env' itself never includes it) —
it's injected by deploy tooling, not something meaningful to replay onto
another org/space.

The source org/space recorded in the file and the target org/space this
command actually writes to are not necessarily the same. For each app, the
target space is located by exact name (case-insensitive) within the target
org, and the target app is then located by exact name (case-insensitive)
within that space — never by the file's own app_id/space_id/org_id GUIDs,
which belong to wherever the file was originally exported from and would
almost certainly be wrong for a different org. An app whose target org has
no space by that name, or whose target space has no app by that name, is
skipped with a warning rather than aborting the run.

Which org(s) are actually targeted depends on whether a default org scope is
currently selected via 'bo orgs': if one is, every app in the file is
targeted at those org(s) instead of the file's own — discarding the file's
own org assignment entirely, the same as 'bo update-roles'. If no default
org scope is selected, each app's own org (exactly as specified in the file)
is used.

Before changing anything, a preview table (region, org, space, app, env) is
shown — with any env var whose name ends in KEY, SECRET, PASSWORD, or
PASSWD masked as '********', never shown in plain text — and confirmation
is required unless -y is given.

Updating an app's environment variables does not by itself make a running
instance pick up the new values — restart or restage it afterwards (see
'bo restart-apps'/'bo restage-apps').`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		skipConfirm, _ := cmd.Flags().GetBool("yes")

		entries, err := parseAppsEnvFile(args[0])
		if err != nil {
			return fmt.Errorf("invalid apps-env file: %w", err)
		}

		creds, err := store.Load()
		if err != nil {
			return fmt.Errorf("not logged in — run: bo login --regions <region>")
		}

		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
		defer cancel()

		destOrgs := resolveFileOrgRetarget(creds)
		entries = retargetEntries(entries, destOrgs, func(e uaeFileEntry, region, orgID, orgName string) uaeFileEntry {
			e.Region, e.OrgID, e.OrgName = region, orgID, orgName
			return e
		})
		var scopeNote string
		if destOrgs != nil {
			scopeNote = orgScopeNote(destOrgs, "the apps-env file")
		}

		targets := resolveUaeTargets(ctx, creds, entries)
		if len(targets) == 0 {
			fmt.Fprintln(os.Stdout, "No matching apps found.")
			return nil
		}
		sort.Slice(targets, func(i, j int) bool {
			if targets[i].Region != targets[j].Region {
				return targets[i].Region < targets[j].Region
			}
			if targets[i].OrgName != targets[j].OrgName {
				return targets[i].OrgName < targets[j].OrgName
			}
			if targets[i].SpaceName != targets[j].SpaceName {
				return targets[i].SpaceName < targets[j].SpaceName
			}
			return targets[i].AppName < targets[j].AppName
		})

		out := cmd.OutOrStdout()
		fmt.Fprintln(out, "Apps to update environment variables for:")
		printUaeTable(targets)
		fmt.Fprintf(out, "\n%d app(s) across %d region(s).\n", len(targets), countDistinctUaeRegions(targets))
		if scopeNote != "" {
			fmt.Fprintln(out)
			fmt.Fprint(out, scopeNote)
		}

		if !skipConfirm {
			fmt.Fprint(out, "\nProceed with updating these apps' environment variables? [y/N] ")
			text, ok := readLine(ctx)
			if !ok || strings.ToLower(text) != "y" {
				fmt.Fprintln(out, "Aborted.")
				return nil
			}
		}

		fmt.Fprintln(out, "\nUpdating...")
		var (
			wg              sync.WaitGroup
			mu              sync.Mutex
			updated, failed int
		)
		sem := make(chan struct{}, uaeConcurrency)
		for _, t := range targets {
			wg.Add(1)
			sem <- struct{}{}
			go func(t uaeTarget) {
				defer wg.Done()
				defer func() { <-sem }()

				vars := make(map[string]interface{}, len(t.Env))
				for k, v := range t.Env {
					vars[k] = v
				}
				if _, err := t.Client.UpdateAppEnvVars(ctx, t.AppGUID, vars); err != nil {
					mu.Lock()
					failed++
					mu.Unlock()
					fmt.Fprintf(os.Stderr, "  ! ERROR [%s] %s/%s/%s: %v\n", t.Region, t.OrgName, t.SpaceName, t.AppName, err)
					return
				}
				mu.Lock()
				updated++
				mu.Unlock()
				fmt.Fprintf(out, "  ~ updated: [%s] %s/%s/%s (%d var(s))\n", t.Region, t.OrgName, t.SpaceName, t.AppName, len(t.Env))
			}(t)
		}
		wg.Wait()

		fmt.Fprintf(out, "\nDone: %d updated, %d failed.\n", updated, failed)
		return nil
	},
}

// countDistinctUaeRegions counts the number of distinct regions across targets.
func countDistinctUaeRegions(targets []uaeTarget) int {
	seen := make(map[string]bool)
	for _, t := range targets {
		seen[t.Region] = true
	}
	return len(seen)
}

func init() {
	updateAppsEnvCmd.GroupID = "cf-org"
	rootCmd.AddCommand(updateAppsEnvCmd)
	updateAppsEnvCmd.Flags().BoolP("yes", "y", false, "Skip the confirmation prompt")
}
