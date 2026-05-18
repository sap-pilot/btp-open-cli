# Changelog

## v0.2 — 2026-05-18

### Added
- **`users`** — new command to list users from the XSUAA (Authorization and Trust Management) `apiaccess` service across all accessible CF organizations; automatically provisions the `btp-xsuaa` service instance and `btp-open-cli-sk` service key in each org's `util` space if they do not exist (TOON preview + `y/N` confirmation before any CF resource is created, bypass with `-y`); XSUAA credentials are cached in `~/.bo/credentials.json` and reused on subsequent runs; access tokens are refreshed when within 60 seconds of expiry
  - `--orgs` / `--excludeOrgs` CSV files (`region,id,name`) to target or skip specific orgs
  - `--filter` — case-insensitive substring match on any user field (`id`, `externalId`, `origin`, `userName`, `lastLogonTime`, `groups`)
  - `--fields` — comma-separated allowlist of fields to include in output
  - `--excludeFields` — comma-separated denylist of fields to omit from output

### Changed
- **`logoff`** — now also clears cached XSUAA credentials (`clientid`, `clientsecret`, `url`, `access_token`) while preserving stored regions

### Improvements
- CF API rate limiting: HTTP 429 responses are handled with automatic retry; `Retry-After` header is honoured when present; falls back to randomised exponential backoff (base 2 s, cap 60 s, up to 5 retries) when the header is absent

## v0.1 — 2026-05-17

### Commands
- **`login`** — authenticate against SAP BTP Cloud Foundry via password or SSO passcode across one or more regions (`--regions us10,eu10`); regions persisted so subsequent commands need no `--regions` flag; `-u`/`-p` flags for non-interactive CI/CD use; SSO passcode input is silent (not echoed)
- **`logoff`** — clear stored OAuth tokens while preserving the regions list for the next login
- **`org-users`** — list all users across every accessible CF organization with their org-level roles; outputs TOON (default), JSON, or CSV; `--filter` for case-insensitive substring match on id, name, origin, or roles; `--regions` to scope to specific regions
- **`org-space-users`** — list users at both organization and space level with roles; same output formats and flags as `org-users`
- **`create-org-space-users`** — add users from a CSV (`name,origin,roles`) to CF organizations and their spaces; org-level roles (`organization_*`) assigned to the org, space-level roles (`space_*`) assigned to every space within the org; `--orgs` / `--excludeOrgs` CSV files (`region,id,name`) to target or skip specific orgs; shows a TOON preview of target scope and users then prompts `y/N` before executing (bypass with `-y`)
- **`delete-org-space-users`** — remove users from every space and organization from a CSV (`name,origin`); shows a TOON preview of all roles to be deleted then prompts `y/N` (bypass with `-y`); space roles are deleted first, followed by a 5-second wait for CF async processing, then org roles are removed

### Features
- Multi-region support: all commands accept `--regions` (comma-separated); data is fetched in parallel per region
- User roles included in all listing commands as a semicolon-separated string sourced from the CF v3 roles API
- TOON output format (`github.com/toon-format/toon-go`) for compact human-readable tables; JSON and CSV also supported
- `HTTPS_PROXY` / `HTTPS_PROXY_INSECURE` environment variables for routing traffic through a proxy (e.g. mitmproxy)
- Version embedded at build time via `-ldflags` (`Version`, `Commit`, `Date` vars in `cmd/version.go`)
- GitHub Actions workflow: builds cross-platform binaries (linux/darwin amd64+arm64, windows amd64) and publishes a GitHub release on merge to `main` or on a `v*` tag push
