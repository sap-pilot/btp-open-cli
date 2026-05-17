# Changelog

## v0.1+e4f0a435a.2026-05-17

### Changed
- **`delete-org-space-users`**: renamed flag `--file` → `--users`; added `-y`/`--yes` flag to skip confirmation; without `-y` the command now shows a TOON preview of all roles to be deleted (`{regions:[{id,orgs:[{id,name,users:[...],spaces:[{id,name,users:[...]}]}]}]}`) and prompts `y/N` before executing; roles are discovered up-front in parallel; space-level roles are deleted first, followed by a 5-second wait for CF's async processing, then org-level roles are removed

### New commands
- **`create-org-space-users`** — add users from a CSV (`name,origin,roles`) to CF organizations and their spaces across one or more regions; org-level roles (`organization_*`) assigned to the org, space-level roles (`space_*`) assigned to every space within the org; supports `--orgs` / `--excludeOrgs` CSV files (`region,id,name`) to include or skip specific orgs; prints a TOON preview of target scope and users then prompts `y/N` before executing (skipped with `-y`)

### Removed
- **`add-org-users`** and **`add-space-users`** commands removed — superseded by `create-org-space-users`, which handles both org and space role assignment in a single command with include/exclude org filtering and a confirmation preview

---

## v0.1+7d191a16d.2026-05-17

### New commands
- **`delete-org-space-users`** — remove users from all spaces (space-level roles first) then from the org across one or more regions; CSV format: `name,origin`; actual CF API errors surfaced to stderr

### Fixes
- `delete-org-space-users`: `DELETE /v3/roles` now accepts `202 Accepted` in addition to `204 No Content` for async CF role deletions
- Role deletion errors now print the actual CF API response to stderr instead of being silently swallowed
- Added `HTTPS_PROXY` / `HTTPS_PROXY_INSECURE` environment variable support for routing all HTTPS traffic through a proxy (e.g. mitmproxy)

---

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
