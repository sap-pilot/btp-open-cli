# Changelog

## v0.1+7d191a16d.2026-05-17

### New commands
- **`delete-org-space-users`** — remove users from all spaces (space-level roles first) then from the org across one or more regions; CSV format: `name,origin`; actual CF API errors surfaced to stderr

### Fixes
- `add-org-users` / `add-space-users`: fixed "user not found" error by passing `username`+`origin` directly in `POST /v3/roles` body — CF resolves IdP users itself, no separate `POST /v3/users` step needed
- `delete-org-space-users`: `DELETE /v3/roles` now accepts `202 Accepted` in addition to `204 No Content` for async CF role deletions
- Role deletion errors now print the actual CF API response to stderr instead of being silently swallowed

---

## v0.1+f06db5869.2026-05-16

### New commands
- **`add-space-users`** — add users from a CSV file (`name,origin,roles`) to every space in every accessible CF organization across one or more regions; users created via `POST /v3/users`, space roles assigned via `POST /v3/roles`; both operations are idempotent

### Features
- `add-space-users --file <path>` (required): same CSV format as `add-org-users` (`name,origin,roles`); roles should be space-level (e.g. `space_developer;space_manager`)
- `add-space-users --regions`: optional, defaults to regions from last login
- CSV parser (`parseUsersCSV`) extracted as a shared helper used by both `add-org-users` and `add-space-users`

### Internal
- `cf/roles`: added `CreateSpaceRole` (POST `/v3/roles` with space relationship, ignores 422 already-exists)

---

## v0.1+e4a576dd8.2026-05-16

### New commands
- **`add-org-users`** — add users from a CSV file (`name,origin,roles`) to all accessible CF organizations in one or more regions; users created via `POST /v3/users`, roles assigned via `POST /v3/roles`; both operations are idempotent (existing users/roles are left unchanged)

### Features
- `add-org-users --file <path>` (required): parses CSV with header `name,origin,roles`; roles are semicolon-separated
- `add-org-users --regions`: optional, defaults to regions from last login
- Version string now follows `v{version}+{9-char-commit}.{YYYY-MM-DD}` format, embedded at build time from `cmd/version.txt`
- `bo` root command consolidated to show version and description on a single line

### Internal
- `cf/client`: added `post()` method and `APIError` type for structured HTTP error handling
- `cf/users`: added `CreateUser` (POST `/v3/users` with 422 fallback to `FindUser`) and `FindUser` (GET `/v3/users?usernames=&origins=`)
- `cf/roles`: added `CreateOrganizationRole` (POST `/v3/roles`, ignores 422 already-exists)

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
