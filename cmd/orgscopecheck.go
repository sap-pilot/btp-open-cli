package cmd

import (
	"fmt"
	"strings"

	"btp-open-cli/internal/store"
)

// fileOrgRef identifies one org: either one referenced by a --roles/
// --role-collections/--apps input file, or one org currently selected via
// 'bo orgs'. Region and OrgName are matched case-insensitively for
// filtering purposes; OrgID is carried along (when known) so a 'bo orgs'
// entry can also serve as a concrete retarget destination.
type fileOrgRef struct {
	Region  string
	OrgID   string
	OrgName string
}

// orgRefKey builds the case-insensitive (region, org name) identity key
// used throughout this file.
func orgRefKey(region, name string) string {
	return strings.ToLower(strings.TrimSpace(region)) + "\x00" + strings.ToLower(strings.TrimSpace(name))
}

// defaultOrgScopeOrgs returns the orgs currently selected via 'bo orgs'
// (creds.DefaultOrgScope), or nil if none are selected.
func defaultOrgScopeOrgs(creds *store.Credentials) []fileOrgRef {
	if len(creds.DefaultOrgScope) == 0 {
		return nil
	}
	scope := make([]fileOrgRef, len(creds.DefaultOrgScope))
	for i, r := range creds.DefaultOrgScope {
		scope[i] = fileOrgRef{Region: r.Region, OrgID: r.ID, OrgName: r.Name}
	}
	return scope
}

// resolveFileOrgRetarget returns the orgs to retarget every entry of a
// --roles/--role-collections input file onto — the orgs currently selected
// via 'bo orgs', if any are selected, discarding the file's own org
// assignment entirely (see retargetEntries) — or nil, meaning "use the
// file's own org assignment unchanged," if no default org scope is
// selected. There is no mismatch check and nothing to confirm: a 'bo orgs'
// selection, when present, always wins.
func resolveFileOrgRetarget(creds *store.Credentials) []fileOrgRef {
	return defaultOrgScopeOrgs(creds)
}

// resolveFileOrgFilterKeepSet returns the set of (region, org name) keys —
// per orgRefKey — to keep from a --apps input CSV: the orgs currently
// selected via 'bo orgs', if any are selected — or nil, meaning "keep
// every row in the CSV," if no default org scope is selected. An app is
// bound to a specific org/space by GUID, so (unlike roles/role-collections)
// it can only ever be filtered down to the current scope, never retargeted
// to a different one — see filterByOrgKeepSet.
func resolveFileOrgFilterKeepSet(creds *store.Credentials) map[string]bool {
	scope := defaultOrgScopeOrgs(creds)
	if scope == nil {
		return nil
	}
	keep := make(map[string]bool, len(scope))
	for _, s := range scope {
		keep[orgRefKey(s.Region, s.OrgName)] = true
	}
	return keep
}

// filterByOrgKeepSet keeps only the entries whose (region, org name) key —
// per orgOf — is in keep. A nil keep means "every org in the file is in
// scope" and returns entries unchanged.
func filterByOrgKeepSet[T any](entries []T, keep map[string]bool, orgOf func(T) (region, orgName string)) []T {
	if keep == nil {
		return entries
	}
	var filtered []T
	for _, e := range entries {
		region, name := orgOf(e)
		if keep[orgRefKey(region, name)] {
			filtered = append(filtered, e)
		}
	}
	return filtered
}

// retargetEntries replaces every entry's own org assignment with the cross
// product of destOrgs: for each destination org, a copy of every entry in
// entries with its org fields (region, org_id, org_name) set to that
// destination via setOrg. The result has len(entries)*len(destOrgs)
// elements. A nil/empty destOrgs returns entries unchanged.
//
// This is how the file's own org assignment is overridden for --roles/
// --role-collections whenever a 'bo orgs' default scope is selected: the
// file is treated as a template of role/role-collection definitions to
// apply to whichever org(s) are currently selected, regardless of which
// org they were originally exported from.
func retargetEntries[T any](entries []T, destOrgs []fileOrgRef, setOrg func(e T, region, orgID, orgName string) T) []T {
	if len(destOrgs) == 0 {
		return entries
	}
	out := make([]T, 0, len(entries)*len(destOrgs))
	for _, dest := range destOrgs {
		for _, e := range entries {
			out = append(out, setOrg(e, dest.Region, dest.OrgID, dest.OrgName))
		}
	}
	return out
}

// orgScopeNote renders the note shown in the confirmation preview whenever
// a --roles/--role-collections/--apps file's targeting was overridden by
// the current 'bo orgs' selection (destOrgs is the orgs being targeted),
// telling the user how to target the file's own orgs instead if that
// wasn't intended.
func orgScopeNote(destOrgs []fileOrgRef, fileLabel string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Note: targeting the org(s) currently selected via 'bo orgs':\n")
	for _, o := range destOrgs {
		fmt.Fprintf(&b, "  %s / %s\n", o.Region, o.OrgName)
	}
	fmt.Fprintf(&b, "If this isn't the org you meant, press Ctrl-C now, then either run "+
		"'bo orgs' to select the correct org(s), or run 'bo orgs' and press 'c' to clear "+
		"the selection — with no default org scope selected, this command will use the "+
		"org(s) specified in %s instead.\n", fileLabel)
	return b.String()
}
