// Package custom is a placeholder for project-specific commands.
// Add your own .go files here; they are gitignored by default so they
// never conflict with upstream changes when you pull from the upstream repo.
//
// To register a command, call cmd.RegisterCommand from your init():
//
//	func init() {
//	    cmd.RegisterCommand(&cobra.Command{
//	        Use:   "my-command",
//	        Short: "My custom command",
//	        RunE: func(c *cobra.Command, args []string) error {
//	            // ...
//	            return nil
//	        },
//	    })
//	}
//
// See example_custom_command.go.template for a ready-to-copy starting point.
package custom
