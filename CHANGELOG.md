# Changelog

## v0.1 — 2026-05-16

### New commands
- **`login`** — authenticate against SAP BTP Cloud Foundry via password or SSO passcode, single or multiple regions (`--regions us10,eu10`); regions persisted for reuse across sessions
- **`logoff`** — clear stored OAuth tokens while preserving the regions list so the next `bo login` requires no flags
- **`org-users`** — list all users across every accessible CF organization; outputs TOON (default), JSON, or CSV; includes org-level roles fetched from `/v3/roles`
- **`org-space-users`** — list users at both organization and space level; same output formats and role support as `org-users`

### Features
- Multi-region support: `--regions` flag accepts a comma-separated list; all commands fetch data in parallel per region and merge results
- User roles: each user entry includes a semicolon-separated `roles` string (e.g. `organization_manager;organization_user`) sourced from the CF v3 roles API
- `--filter` flag on `org-users` and `org-space-users`: case-insensitive substring match against user id, name, origin, and roles; orgs/spaces/regions with no matches are omitted from output
- `-u`/`--username` and `-p`/`--password` flags on `login` for non-interactive CI/CD use (e.g. GitHub Actions); falls back to interactive prompts for any missing value
- TOON output format powered by `github.com/toon-format/toon-go` for compact, human-readable tabular display
- JSON and CSV output formats for all listing commands

### Fixes
- `bo login --sso`: passcode input is now silent (not echoed to terminal), consistent with the password prompt
- TOON output no longer leaves a trailing `%` on the terminal line (missing newline added)

### Documentation
- Added `README.md` with Ubuntu Go install instructions, build steps (`go build -o bo`), command examples, and pointer to `bo <cmd> --help`
