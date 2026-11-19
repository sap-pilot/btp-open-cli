package cmd

import (
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// This file gives a specific set of commands — the ones that all share the
// same --include/--exclude/--fields/--sort-by flags (added uniformly across
// them in earlier work) — a three-section --help layout instead of cobra's
// usual two: each command's own flags under "Flags:", those shared flags
// pulled out into their own "Common Flags:" section just above "Global
// Flags:", and --help itself moved out of "Flags:" into "Global Flags:"
// alongside --color/--verbose. Every other command (create-*, delete-*,
// update-*, login, etc.) is unaffected — SetUsageTemplate only changes the
// command it's called on (and any of its own subcommands that don't set
// their own), never its siblings or parent.

// commonHelpFlagNames are the flags grouped under "Common Flags:" instead
// of a command's own "Flags:" section — not every command using this
// template has all four (e.g. 'orgs' has --include/--exclude but not
// --fields/--sort-by); only the ones it actually has are shown.
var commonHelpFlagNames = map[string]bool{
	"include": true,
	"exclude": true,
	"fields":  true,
	"sort-by": true,
}

// splitLocalFlags copies cmd's local flags (its own, not inherited from a
// parent) into a fresh FlagSet: those named in commonHelpFlagNames if want
// is true, every other one if want is false — except "help" either way,
// which cobraGlobalFlags below adds to its own set instead.
func splitLocalFlags(cmd *cobra.Command, want bool) *pflag.FlagSet {
	out := pflag.NewFlagSet(cmd.Name(), pflag.ContinueOnError)
	cmd.LocalFlags().VisitAll(func(f *pflag.Flag) {
		if f.Name == "help" {
			return
		}
		if commonHelpFlagNames[f.Name] == want {
			out.AddFlag(f)
		}
	})
	return out
}

// cobraOwnFlags returns cmd's own local flags for the "Flags:" section,
// excluding both the common ones (see commonHelpFlagNames) and --help.
func cobraOwnFlags(cmd *cobra.Command) *pflag.FlagSet { return splitLocalFlags(cmd, false) }

// cobraCommonFlags returns cmd's local flags that belong in the
// "Common Flags:" section (see commonHelpFlagNames).
func cobraCommonFlags(cmd *cobra.Command) *pflag.FlagSet { return splitLocalFlags(cmd, true) }

// cobraGlobalFlags returns cmd's inherited flags (--color/--verbose, from
// the root command) plus its own --help, for the "Global Flags:" section.
func cobraGlobalFlags(cmd *cobra.Command) *pflag.FlagSet {
	out := pflag.NewFlagSet(cmd.Name(), pflag.ContinueOnError)
	cmd.InheritedFlags().VisitAll(func(f *pflag.Flag) {
		out.AddFlag(f)
	})
	if hf := cmd.Flags().Lookup("help"); hf != nil {
		out.AddFlag(hf)
	}
	return out
}

func init() {
	cobra.AddTemplateFunc("cobraOwnFlags", cobraOwnFlags)
	cobra.AddTemplateFunc("cobraCommonFlags", cobraCommonFlags)
	cobra.AddTemplateFunc("cobraGlobalFlags", cobraGlobalFlags)
}

// commonFlagsUsageTemplate is cobra's own defaultUsageTemplate with its
// single "{{if .HasAvailableLocalFlags}}Flags:...{{if
// .HasAvailableInheritedFlags}}Global Flags:..." block split into three —
// see this file's own doc comment above.
const commonFlagsUsageTemplate = `Usage:{{if .Runnable}}
  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{.CommandPath}} [command]{{end}}{{if gt (len .Aliases) 0}}

Aliases:
  {{.NameAndAliases}}{{end}}{{if .HasExample}}

Examples:
{{.Example}}{{end}}{{if .HasAvailableSubCommands}}{{$cmds := .Commands}}{{if eq (len .Groups) 0}}

Available Commands:{{range $cmds}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{else}}{{range $group := .Groups}}

{{.Title}}{{range $cmds}}{{if (and (eq .GroupID $group.ID) (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{if not .AllChildCommandsHaveGroup}}

Additional Commands:{{range $cmds}}{{if (and (eq .GroupID "") (or .IsAvailableCommand (eq .Name "help")))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{end}}{{end}}{{if (cobraOwnFlags .).HasAvailableFlags}}

Flags:
{{(cobraOwnFlags .).FlagUsages | trimTrailingWhitespaces}}{{end}}{{if (cobraCommonFlags .).HasAvailableFlags}}

Common Flags:
{{(cobraCommonFlags .).FlagUsages | trimTrailingWhitespaces}}{{end}}{{if (cobraGlobalFlags .).HasAvailableFlags}}

Global Flags:
{{(cobraGlobalFlags .).FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasHelpSubCommands}}

Additional help topics:{{range .Commands}}{{if .IsAdditionalHelpTopicCommand}}
  {{rpad .CommandPath .CommandPathPadding}} {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableSubCommands}}

Use "{{.CommandPath}} [command] --help" for more information about a command.{{end}}
`

// setCommonFlagsUsageTemplate applies commonFlagsUsageTemplate to every
// command given.
func setCommonFlagsUsageTemplate(cmds ...*cobra.Command) {
	for _, c := range cmds {
		c.SetUsageTemplate(commonFlagsUsageTemplate)
	}
}

// The commands sharing --include/--exclude/--fields/--sort-by (or, for
// 'orgs', just --include/--exclude) get the three-section --help layout
// above instead of cobra's usual two.
func init() {
	setCommonFlagsUsageTemplate(
		appsCmd,
		usersCmd,
		roleCollectionsCmd,
		rolesCmd,
		subaccountDestinationsCmd,
		spaceDestinationsCmd,
		serviceKeysCmd,
		orgUsersCmd,
		orgSpaceUsersCmd,
		orgSpacesCmd,
		spacesCmd,
		html5AppsCmd,
		orgsCmd,
	)
}
