package cmd

import (
	"context"
	"fmt"
	"strings"

	"btp-open-cli/internal/xsuaa"

	"github.com/spf13/cobra"
)

var createRolesCmd = &cobra.Command{
	Use:   "create-roles [role_name_pattern]",
	Short: "Create XSUAA roles from an exported roles file",
	Long: `Create roles in the XSUAA (Authorization and Trust Management) apiaccess
service, read from --roles <file> — a JSON or TOON document produced by
'bo roles' (or hand-authored with the same regions -> orgs -> roles shape).
Each role entry carries its roleTemplateAppId, roleTemplateName, role_name,
description, and optional attributeList.

<role_name_pattern> is optional — a comma-separated list of patterns, e.g.
'Custom_*,temp-*' or '*' (the default when omitted: every role in the file).
Each pattern is a glob (via filepath.Match) if it contains * ? [, otherwise
a case-insensitive substring; a pattern prefixed with '!' reverses it, and a
role is selected if any pattern — negated or not — matches it. Only roles
in --roles matching the pattern are created; the rest of the file is
ignored.

For each org referenced in the (filtered) file an XSUAA token is resolved
the same way as 'bo roles'/'bo users' (via the xsuaa/apiaccess service key)
— only the orgs that actually appear are touched.

The orgs actually targeted depend on whether a default org scope is
currently selected via 'bo orgs': if one is, every role in the file is
created within those org(s) instead of the file's own — discarding the
file's own org assignment entirely, so the file can double as a role
template to apply against any subaccount just by changing your 'bo orgs'
selection. If no default org scope is selected, each role's own org
(exactly as specified in the file) is used. Either way, each role's
roleTemplateAppId is automatically re-resolved against the target org's
own role templates, matching by app name (ignoring the subaccount-specific
suffix after '!') and roleTemplateName, since the exported appId's suffix
is specific to the subaccount it came from.

If a role with the same identity already exists (HTTP 409), creation is
skipped for that role.

Without -y, a TOON preview of all roles to be created is shown and
confirmation is required before any changes are made.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		pattern := "*"
		if len(args) > 0 {
			pattern = strings.TrimSpace(args[0])
			if pattern == "" {
				return fmt.Errorf("role_name_pattern cannot be empty")
			}
		}
		return runRolesFileApply(cmd, pattern,
			"Roles to be created:",
			"Proceed with role creation? [y/N] ",
			"Creating roles...",
			"+",
			func(ctx context.Context, apiURL, token string, role xsuaa.Role) error {
				return xsuaa.CreateRole(ctx, apiURL, token, role)
			})
	},
}

func init() {
	createRolesCmd.GroupID = "xsuaa"
	rootCmd.AddCommand(createRolesCmd)
	createRolesCmd.Flags().String("roles", "", "Path to a roles JSON/TOON file produced by 'bo roles' (required)")
	createRolesCmd.MarkFlagRequired("roles") //nolint:errcheck
	createRolesCmd.Flags().BoolP("yes", "y", false, "Skip the confirmation prompt")
}
