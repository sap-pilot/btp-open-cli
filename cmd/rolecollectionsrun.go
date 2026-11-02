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

// ── preview-only output shape (role collections, no roles field) ───────────────

type rcrPreviewOrg struct {
	ID              string                `json:"org_id"          toon:"org_id"`
	Name            string                `json:"org_name"        toon:"org_name"`
	RoleCollections []rcOutRoleCollection `json:"roleCollections" toon:"roleCollections"`
}

type rcrPreviewRegion struct {
	ID   string          `json:"region" toon:"region"`
	Orgs []rcrPreviewOrg `json:"orgs"   toon:"orgs"`
}

type rcrPreviewDoc struct {
	Regions []rcrPreviewRegion `json:"regions" toon:"regions"`
}

func xsuaaRCToOut(rc xsuaa.RoleCollection) rcOutRoleCollection {
	var refs []rcOutRoleRef
	for _, ref := range rc.RoleReferences {
		refs = append(refs, rcOutRoleRef{
			RoleTemplateAppID: ref.RoleTemplateAppID,
			RoleTemplateName:  ref.RoleTemplateName,
			Name:              ref.Name,
			Description:       ref.Description,
		})
	}
	return rcOutRoleCollection{
		Name:           rc.Name,
		Description:    rc.Description,
		IsReadOnly:     rc.IsReadOnly,
		RoleReferences: refs,
	}
}

func rcOutToXsuaaRC(rc rcOutRoleCollection) xsuaa.RoleCollection {
	var refs []xsuaa.RoleReference
	for _, ref := range rc.RoleReferences {
		refs = append(refs, xsuaa.RoleReference{
			RoleTemplateAppID: ref.RoleTemplateAppID,
			RoleTemplateName:  ref.RoleTemplateName,
			Name:              ref.Name,
			Description:       ref.Description,
		})
	}
	return xsuaa.RoleCollection{
		Name:           rc.Name,
		Description:    rc.Description,
		IsReadOnly:     rc.IsReadOnly,
		RoleReferences: refs,
	}
}

// rcFileEntry is one role collection, scoped to a region/org, as read from a
// --role-collections file (JSON or TOON, in the same shape 'bo
// role-collections' writes — its "roles" field, if present, is ignored).
type rcFileEntry struct {
	Region  string
	OrgID   string
	OrgName string
	RC      xsuaa.RoleCollection
}

// parseRoleCollectionsFile reads path — JSON or TOON, as written by
// 'bo role-collections', or hand-authored with the same
// regions -> orgs -> roleCollections shape — and flattens it into one entry
// per role collection.
func parseRoleCollectionsFile(path string) ([]rcFileEntry, error) {
	if path == "" {
		return nil, fmt.Errorf("no file given")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("opening file: %w", err)
	}

	var doc rcOutDoc
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

	var entries []rcFileEntry
	for _, r := range doc.Regions {
		for _, o := range r.Orgs {
			for _, rc := range o.RoleCollections {
				entries = append(entries, rcFileEntry{
					Region:  r.ID,
					OrgID:   o.ID,
					OrgName: o.Name,
					RC:      rcOutToXsuaaRC(rc),
				})
			}
		}
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("file contains no role collections")
	}
	return entries, nil
}

// rcTarget pairs one role collection with its already-resolved XSUAA client.
type rcTarget struct {
	RegionName string
	OrgGUID    string
	OrgName    string
	APIURL     string
	Token      string
	RC         xsuaa.RoleCollection
}

// resolveRCTargets resolves XSUAA clients for exactly the orgs referenced in
// entries (via the already-authenticated CF tokens — no broader discovery),
// and matches each entry to its client.
func resolveRCTargets(ctx context.Context, creds *store.Credentials, entries []rcFileEntry) ([]rcTarget, error) {
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

	var targets []rcTarget
	for _, w := range clients {
		for _, e := range entries {
			if !strings.EqualFold(e.OrgID, w.OrgGUID) || !strings.EqualFold(e.Region, w.RegionName) {
				continue
			}
			targets = append(targets, rcTarget{
				RegionName: w.RegionName,
				OrgGUID:    w.OrgGUID,
				OrgName:    w.OrgName,
				APIURL:     w.APIURL,
				Token:      w.Token,
				RC:         e.RC,
			})
		}
	}
	return targets, nil
}

// sortRCTargets sorts targets in place by region, then org, then role
// collection name, so preview output and execution order are deterministic.
func sortRCTargets(targets []rcTarget) {
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].RegionName != targets[j].RegionName {
			return targets[i].RegionName < targets[j].RegionName
		}
		if targets[i].OrgName != targets[j].OrgName {
			return targets[i].OrgName < targets[j].OrgName
		}
		return targets[i].RC.Name < targets[j].RC.Name
	})
}

// buildRCPreviewDoc renders already-sorted targets as an rcrPreviewDoc for
// the TOON preview.
func buildRCPreviewDoc(targets []rcTarget) rcrPreviewDoc {
	var doc rcrPreviewDoc
	var curRegion *rcrPreviewRegion
	var curOrg *rcrPreviewOrg

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
			curRegion = &rcrPreviewRegion{ID: t.RegionName}
		}
		if curOrg == nil || curOrg.ID != t.OrgGUID {
			flushOrg()
			curOrg = &rcrPreviewOrg{ID: t.OrgGUID, Name: t.OrgName}
		}
		curOrg.RoleCollections = append(curOrg.RoleCollections, xsuaaRCToOut(t.RC))
	}
	flushRegion()
	return doc
}

// applyRCPreviewAndRun shows a TOON preview of targets (unless skipConfirm),
// asks for confirmation, then calls fn for each target, reporting
// ErrAlreadyExists/ErrNotFound as informational rather than failures.
// successGlyph is the per-item prefix on success, e.g. "+", "~", "-".
func applyRCPreviewAndRun(
	ctx context.Context,
	targets []rcTarget,
	previewHeader, confirmPrompt, progressHeader, successGlyph string,
	skipConfirm bool,
	fn func(ctx context.Context, apiURL, token string, rc xsuaa.RoleCollection) error,
) error {
	if !skipConfirm {
		doc := buildRCPreviewDoc(targets)
		out, err := toonenc.Marshal(doc, toonenc.WithIndent(2))
		if err != nil {
			return fmt.Errorf("encoding preview: %w", err)
		}
		fmt.Fprintln(os.Stdout, previewHeader)
		os.Stdout.Write(out) //nolint:errcheck
		fmt.Fprintln(os.Stdout)
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
		err := fn(ctx, t.APIURL, t.Token, t.RC)
		switch {
		case err == nil:
			fmt.Fprintf(os.Stdout, "  %s [%s] %s / %s\n", successGlyph, t.RegionName, t.OrgName, t.RC.Name)
		case errors.Is(err, xsuaa.ErrAlreadyExists):
			fmt.Fprintf(os.Stdout, "  ~ [%s] %s / %s (already exists)\n", t.RegionName, t.OrgName, t.RC.Name)
		case errors.Is(err, xsuaa.ErrNotFound):
			fmt.Fprintf(os.Stderr, "  ! [%s] %s / %s: not found — skipping\n", t.RegionName, t.OrgName, t.RC.Name)
		default:
			fmt.Fprintf(os.Stderr, "  ! [%s] %s / %s: %v\n", t.RegionName, t.OrgName, t.RC.Name, err)
		}
	}
	return nil
}

// runRoleCollectionsFileApply implements the shared body of
// create-role-collections and update-role-collections: parse
// --role-collections, resolve targets, preview, confirm, and apply fn.
func runRoleCollectionsFileApply(
	cmd *cobra.Command,
	previewHeader, confirmPrompt, progressHeader, successGlyph string,
	fn func(ctx context.Context, apiURL, token string, rc xsuaa.RoleCollection) error,
) error {
	rcFile, _ := cmd.Flags().GetString("role-collections")
	skipConfirm, _ := cmd.Flags().GetBool("yes")

	entries, err := parseRoleCollectionsFile(rcFile)
	if err != nil {
		return fmt.Errorf("invalid --role-collections file: %w", err)
	}

	creds, err := store.Load()
	if err != nil {
		return fmt.Errorf("not logged in — run: bo login --regions <region>")
	}

	ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
	defer cancel()

	targets, err := resolveRCTargets(ctx, creds, entries)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		fmt.Fprintln(os.Stdout, "No matching orgs found.")
		return nil
	}
	sortRCTargets(targets)

	return applyRCPreviewAndRun(ctx, targets, previewHeader, confirmPrompt, progressHeader, successGlyph, skipConfirm, fn)
}
