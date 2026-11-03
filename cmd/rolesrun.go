package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"

	toonenc "github.com/toon-format/toon-go"

	"btp-open-cli/internal/store"
	"btp-open-cli/internal/xsuaa"

	"github.com/spf13/cobra"
)

// roFileEntry is one role, scoped to a region/org, as read from a --roles
// file (JSON or TOON, in the same shape 'bo roles' writes).
type roFileEntry struct {
	Region  string
	OrgID   string
	OrgName string
	Role    xsuaa.Role
}

// parseRolesFile reads path — JSON or TOON, as written by 'bo roles', or
// hand-authored with the same regions -> orgs -> roles shape — and flattens
// it into one entry per role.
func parseRolesFile(path string) ([]roFileEntry, error) {
	if path == "" {
		return nil, fmt.Errorf("no file given")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("opening file: %w", err)
	}

	var doc roOutDoc
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

	var entries []roFileEntry
	for _, r := range doc.Regions {
		for _, o := range r.Orgs {
			for _, role := range o.Roles {
				entries = append(entries, roFileEntry{
					Region:  r.ID,
					OrgID:   o.ID,
					OrgName: o.Name,
					Role:    roOutToXsuaaRole(role),
				})
			}
		}
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("file contains no roles")
	}
	return entries, nil
}

// filterRoleEntriesByPattern returns only the entries whose role name
// matches pattern — a comma-separated, "!"-negatable pattern list (see
// matchesNegatablePattern); pattern "*" matches every entry.
func filterRoleEntriesByPattern(entries []roFileEntry, pattern string) []roFileEntry {
	var filtered []roFileEntry
	for _, e := range entries {
		if matchesNegatablePattern(pattern, e.Role.Name) {
			filtered = append(filtered, e)
		}
	}
	return filtered
}

// roleTarget pairs one role with its already-resolved XSUAA client.
type roleTarget struct {
	RegionName string
	OrgGUID    string
	OrgName    string
	APIURL     string
	Token      string
	Role       xsuaa.Role
}

// resolveRoleTargets resolves XSUAA clients for exactly the orgs referenced
// in entries (via the already-authenticated CF tokens — no broader
// discovery), and matches each entry to its client.
func resolveRoleTargets(ctx context.Context, creds *store.Credentials, entries []roFileEntry) ([]roleTarget, error) {
	apiURLSet := make(map[string]bool)
	orgFilter := make(cosOrgSet, 0, len(entries))
	seenOrg := make(map[string]bool)
	for _, e := range entries {
		apiURLSet[store.RegionToAPIURL(e.Region)] = true
		if !seenOrg[e.OrgID] {
			orgFilter = append(orgFilter, cosOrgRef{ID: e.OrgID})
			seenOrg[e.OrgID] = true
		}
	}
	apiURLs := make([]string, 0, len(apiURLSet))
	for u := range apiURLSet {
		apiURLs = append(apiURLs, u)
	}

	clients, _, err := resolveXsuaaClients(ctx, apiURLs, creds, orgFilter, nil, false)
	if err != nil {
		return nil, err
	}

	var targets []roleTarget
	for _, w := range clients {
		for _, e := range entries {
			if !strings.EqualFold(e.OrgID, w.OrgGUID) || !strings.EqualFold(e.Region, w.RegionName) {
				continue
			}
			targets = append(targets, roleTarget{
				RegionName: w.RegionName,
				OrgGUID:    w.OrgGUID,
				OrgName:    w.OrgName,
				APIURL:     w.APIURL,
				Token:      w.Token,
				Role:       e.Role,
			})
		}
	}
	return targets, nil
}

// sortRoleTargets sorts targets in place by region, then org, then role
// name, so preview output and execution order are deterministic.
func sortRoleTargets(targets []roleTarget) {
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].RegionName != targets[j].RegionName {
			return targets[i].RegionName < targets[j].RegionName
		}
		if targets[i].OrgName != targets[j].OrgName {
			return targets[i].OrgName < targets[j].OrgName
		}
		return targets[i].Role.Name < targets[j].Role.Name
	})
}

// buildRolePreviewDoc renders already-sorted targets as a roOutDoc for the
// TOON preview.
func buildRolePreviewDoc(targets []roleTarget) roOutDoc {
	var doc roOutDoc
	var curRegion *roOutRegion
	var curOrg *roOutOrg

	flushOrg := func() {
		if curOrg != nil && curRegion != nil {
			curRegion.Orgs = append(curRegion.Orgs, *curOrg)
		}
		curOrg = nil
	}
	flushRegion := func() {
		flushOrg()
		if curRegion != nil {
			doc.Regions = append(doc.Regions, *curRegion)
		}
		curRegion = nil
	}

	for _, t := range targets {
		if curRegion == nil || curRegion.ID != t.RegionName {
			flushRegion()
			curRegion = &roOutRegion{ID: t.RegionName}
		}
		if curOrg == nil || curOrg.ID != t.OrgGUID {
			flushOrg()
			curOrg = &roOutOrg{ID: t.OrgGUID, Name: t.OrgName}
		}
		curOrg.Roles = append(curOrg.Roles, xsuaaRoleToOut(t.Role))
	}
	flushRegion()
	return doc
}

// resolveRoleTemplateAppIDs corrects each target's Role.RoleTemplateAppID to
// the one actually registered, in that target's own org, for the same app
// name (ignoring the subaccount-specific suffix after '!' — see
// xsuaa.AppIDPrefix) and role template name. This matters whenever a role
// is being created/updated/deleted in a different subaccount than the one
// it was exported from: the exported appId's suffix is specific to the
// source subaccount's app registration and is very likely wrong for the
// destination. Targets are mutated in place. If no matching template is
// found for a target, or its org's template list can't be fetched, that
// target is left unchanged so the normal create/update/delete call still
// runs and surfaces whatever error (or success) the literal exported appId
// produces.
func resolveRoleTemplateAppIDs(ctx context.Context, targets []roleTarget) {
	templatesByAPIURL := make(map[string][]xsuaa.RoleTemplate)
	fetchFailed := make(map[string]bool)

	for i, t := range targets {
		if fetchFailed[t.APIURL] {
			continue
		}
		templates, ok := templatesByAPIURL[t.APIURL]
		if !ok {
			var err error
			templates, err = xsuaa.ListRoleTemplates(ctx, t.APIURL, t.Token)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: [%s] %s: listing role templates: %v — using roleTemplateAppId as given\n",
					t.RegionName, t.OrgName, err)
				fetchFailed[t.APIURL] = true
				continue
			}
			templatesByAPIURL[t.APIURL] = templates
		}
		if resolved, ok := xsuaa.ResolveRoleTemplateAppID(templates, t.Role.RoleTemplateAppID, t.Role.RoleTemplateName); ok {
			targets[i].Role.RoleTemplateAppID = resolved
		}
	}
}

// applyRolesPreviewAndRun shows a TOON preview of targets (unless
// skipConfirm), optionally followed by scopeNote (see orgScopeNote), asks
// for confirmation, then calls fn for each target, reporting
// ErrAlreadyExists/ErrNotFound as informational rather than failures.
// successGlyph is the per-item prefix on success, e.g. "+", "~", "-".
func applyRolesPreviewAndRun(
	ctx context.Context,
	targets []roleTarget,
	previewHeader, confirmPrompt, progressHeader, successGlyph, scopeNote string,
	skipConfirm bool,
	fn func(ctx context.Context, apiURL, token string, role xsuaa.Role) error,
) error {
	resolveRoleTemplateAppIDs(ctx, targets)

	if !skipConfirm {
		doc := buildRolePreviewDoc(targets)
		out, err := toonenc.Marshal(doc, toonenc.WithIndent(2))
		if err != nil {
			return fmt.Errorf("encoding preview: %w", err)
		}
		fmt.Fprintln(os.Stdout, previewHeader)
		os.Stdout.Write(out) //nolint:errcheck
		fmt.Fprintln(os.Stdout)
		if scopeNote != "" {
			fmt.Fprintln(os.Stdout, scopeNote)
		}
		fmt.Fprint(os.Stdout, confirmPrompt)
		text, ok := readLine(ctx)
		if !ok || strings.ToLower(text) != "y" {
			fmt.Fprintln(os.Stdout, "Aborted.")
			return nil
		}
		fmt.Fprintln(os.Stdout)
	}

	fmt.Fprintln(os.Stdout, progressHeader)
	for _, t := range targets {
		err := fn(ctx, t.APIURL, t.Token, t.Role)
		switch {
		case err == nil:
			fmt.Fprintf(os.Stdout, "  %s [%s] %s / %s\n", successGlyph, t.RegionName, t.OrgName, t.Role.Name)
		case errors.Is(err, xsuaa.ErrAlreadyExists):
			fmt.Fprintf(os.Stdout, "  ~ [%s] %s / %s (already exists)\n", t.RegionName, t.OrgName, t.Role.Name)
		case errors.Is(err, xsuaa.ErrNotFound):
			fmt.Fprintf(os.Stderr, "  ! [%s] %s / %s: not found — skipping\n", t.RegionName, t.OrgName, t.Role.Name)
		default:
			fmt.Fprintf(os.Stderr, "  ! [%s] %s / %s: %v\n", t.RegionName, t.OrgName, t.Role.Name, err)
		}
	}
	return nil
}

// runRolesFileApply implements the shared body of create-roles and
// update-roles: parse --roles, narrow to entries matching pattern (a
// comma-separated, "!"-negatable pattern list matched against each role's
// name — "*" matches every entry in the file), resolve targets, preview,
// confirm, and apply fn.
func runRolesFileApply(
	cmd *cobra.Command,
	pattern string,
	previewHeader, confirmPrompt, progressHeader, successGlyph string,
	fn func(ctx context.Context, apiURL, token string, role xsuaa.Role) error,
) error {
	rolesFile, _ := cmd.Flags().GetString("roles")
	skipConfirm, _ := cmd.Flags().GetBool("yes")

	entries, err := parseRolesFile(rolesFile)
	if err != nil {
		return fmt.Errorf("invalid --roles file: %w", err)
	}

	entries = filterRoleEntriesByPattern(entries, pattern)
	if len(entries) == 0 {
		return fmt.Errorf("no roles matched — check the --roles file and the name pattern")
	}

	creds, err := store.Load()
	if err != nil {
		return fmt.Errorf("not logged in — run: bo login --regions <region>")
	}

	ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
	defer cancel()

	destOrgs := resolveFileOrgRetarget(creds)
	entries = retargetEntries(entries, destOrgs, func(e roFileEntry, region, orgID, orgName string) roFileEntry {
		e.Region, e.OrgID, e.OrgName = region, orgID, orgName
		return e
	})
	var scopeNote string
	if destOrgs != nil {
		scopeNote = orgScopeNote(destOrgs, "--roles file")
	}

	targets, err := resolveRoleTargets(ctx, creds, entries)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		fmt.Fprintln(os.Stdout, "No matching orgs found.")
		return nil
	}
	sortRoleTargets(targets)

	return applyRolesPreviewAndRun(ctx, targets, previewHeader, confirmPrompt, progressHeader, successGlyph, scopeNote, skipConfirm, fn)
}
