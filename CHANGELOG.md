# Changelog

## v0.5 — 2026-05-20

### Added
- **`describe-subaccount`** — new command to describe a single BTP subaccount in detail, identified by `--org` (exact GUID or name substring):
  - **Subaccount metadata** — fetched from the CIS Accounts Service (`/accounts/v1/subaccounts/{orgGUID}`) using a `cis/central-viewer` service key auto-discovered from any accessible org/space; credentials cached in `~/.bo/credentials.json`
  - **Destinations** — fetches subaccount-level destinations from the `destination/lite` service key found in the target org; sensitive fields (`Password`, `ClientSecret`, etc.) are redacted
  - **Role collections** — XSUAA role collections from the target org (same `btp-xsuaa` apiaccess key setup as `users` and `role-collections`)
  - Output: `{subaccount, spaces, destinations, rolecollections}` in TOON (default) or JSON (`--format json`)
  - **`--subaccount`** flag — overrides the BTP subaccount GUID used in the CIS API call (defaults to the CF org GUID)
  - **`spaces`** — lists all CF spaces in the org with their managed service instances; each service shows `id`, `name`, `service` (offering), `plan`, and `state`; resolved in a single API round-trip using `include=service_offering` on the service plans endpoint
  - `--regions`, `-y` flags; destinations and role collections degrade gracefully if the respective services are unavailable
- **`delete-users`** — new command to delete XSUAA users across all accessible organizations via the `apiaccess` service plan
  - CSV input (`--users`): columns `origin,userName`; users matched case-insensitively by origin + userName
  - TOON preview shows full user details (`user_id`, `user_externalId`, `user_origin`, `userName`, `email`, `lastLogonTime`, `groups`) for all matched users before any deletions are made
  - `Proceed with user deletion? [y/N]` confirmation prompt (skipped with `-y`)
  - `--regions` — comma-separated CF regions; falls back to stored login regions if omitted
  - `--orgs` / `--excludeOrgs` — CSV files (`region,org_id,org_name`) to include or skip specific orgs
  - Reuses the XSUAA `btp-xsuaa` service instance and `btp-open-cli-sk` service key setup and credential caching from `users` and `role-collections`

### Changed
- **`role-collections` — deterministic sort order** — output is now sorted for easier diff and comparison across orgs:
  - `roles` sorted by `roleTemplateAppId` then `roleTemplateName`
  - `roleCollections` sorted by `rolecollection_name`
  - Each role collection's `roleReferences` sorted by `roleTemplateAppId` then `roleTemplateName`

## v0.4 — 2026-05-19

### Added
- **`apps`** — new command to list Cloud Foundry applications across all accessible organizations and spaces; fetches organizations, spaces, apps, and web process metrics (instances, memory, disk) in parallel per region
  - Output formats: TOON (default), JSON (`--format json`), CSV (`--format csv`)
  - `--regions` — comma-separated CF regions; falls back to stored login regions if omitted
  - `--org` — restrict to a single org by exact GUID
  - `--orgs` / `--excludeOrgs` — CSV files (`region,id,name`) to include or skip specific orgs
  - `--filter` — case-insensitive substring match on `mta_id`, `app_id`, `app_name`, `app_state`, `app_created_at`, `app_updated_at`, `process_memory_in_mb`
  - TOON/JSON output is hierarchical: `regions → orgs → spaces → apps`; CSV output is flat with one row per app
  - App metadata annotations (`mta_id`) are sourced from `app.metadata.annotations.mta_id`
  - Process metrics (`instances`, `memory_in_mb`, `disk_in_mb`) are sourced from the CF v3 `web` process for each app

### Improvements
- **Automatic token refresh on HTTP 401** — all CF API commands (`orgs`, `org-users`, `org-space-users`, `create-org-space-users`, `delete-org-space-users`, `apps`, `users`, `role-collections`) now recover automatically from expired access tokens:
  1. Silent refresh via stored OAuth refresh token (no prompts)
  2. If the refresh token is also expired: interactive re-authentication using the same method as the last login (password or SSO passcode)
  3. Only one refresh attempt per region per command run; subsequent 401s propagate as warnings
- **Ctrl-C exits cleanly** in all interactive prompts — email, password, SSO passcode, and `[y/N]` confirmations now respect context cancellation
- **`login_type` stored per region** — the credential store now records whether each region was authenticated with `password` or `sso` so the correct prompt can be shown during re-authentication

### Changed
- **Field naming standardised across all commands** — output fields (TOON/JSON/CSV) and CSV input headers now use entity-prefixed names for clarity:
  - Region key: `region` (was `id` in TOON/JSON output)
  - Org fields: `org_id`, `org_name` (was `id`, `name`)
  - Space fields: `space_id`, `space_name` (was `id`, `name`)
  - Role name: `role_name` (was `name` in `role-collections` output)
  - Role collection name: `rolecollection_name` (was `name`)
  - CF user fields (`org-users`, `org-space-users`, create/delete commands): `cfuser_id`, `cfuser_name`, `cfuser_origin`, `cfuser_roles` (was `id`, `name`, `origin`, `roles`)
  - XSUAA user fields (`users` command): `user_id`, `user_externalId`, `user_origin` (was `id`, `externalId`, `origin`)
- **`--orgs` / `--excludeOrgs` CSV input** — header now `region,org_id,org_name` (was `region,id,name`); applies to `create-org-space-users`, `delete-org-space-users`, `apps`, `users`, and `role-collections`
- **`create-org-space-users --users` CSV input** — header now `cfuser_name,cfuser_origin,cfuser_roles` (was `name,origin,roles`)
- **`delete-org-space-users --users` CSV input** — header now `cfuser_name,cfuser_origin` (was `name,origin`)
- **`orgs` CSV output** — columns now `region,org_id,org_name` (was `region,id,name`)
- **`users --fields` / `--excludeFields`** — field names updated to `user_id`, `user_externalId`, `user_origin` to match new output keys

## v0.3 — 2026-05-19

### Added
- **`upgrade`** — new command to self-update `bo` to the latest GitHub release; compares local version against the latest tag, prompts `y/N` before downloading (bypass with `-y`); on Linux/macOS atomically replaces the running binary via temp-file rename; on Windows renames `bo.exe` to `bo-{version}.exe` first then downloads the new release as `bo.exe`

### Changed
- **`users`** — added `email` field to user output (sourced from `emails[0].value`); appears after `userName` and participates in `--filter`, `--fields`, and `--excludeFields`
- **`users`** — added `--org` flag to restrict fetching to a single org by exact GUID
- **`role-collections`** — added `--org` flag to restrict fetching to a single org; matches by case-insensitive name substring or exact GUID

## v0.2 — 2026-05-18

### Added
- **`orgs`** — new command to list all accessible CF organizations; outputs CSV (`region,id,name`) compatible with the `--orgs` / `--excludeOrgs` flags of `create-org-space-users`, `delete-org-space-users`, and `users`
- **`users`** — new command to list users from the XSUAA (Authorization and Trust Management) `apiaccess` service across all accessible CF organizations; automatically provisions the `btp-xsuaa` service instance and `btp-open-cli-sk` service key in each org's `util` space if they do not exist (TOON preview + `y/N` confirmation before any CF resource is created, bypass with `-y`); XSUAA credentials are cached in `~/.bo/credentials.json` and reused on subsequent runs; access tokens are refreshed when within 60 seconds of expiry
  - `--orgs` / `--excludeOrgs` CSV files (`region,id,name`) to target or skip specific orgs
  - `--filter` — case-insensitive substring match on any user field (`id`, `externalId`, `origin`, `userName`, `lastLogonTime`, `groups`)
  - `--fields` — comma-separated allowlist of fields to include in output
  - `--excludeFields` — comma-separated denylist of fields to omit from output
- **`role-collections`** — new command to list XSUAA roles and role collections (with their role references) across all accessible CF organizations; shares the same `btp-xsuaa` / `btp-open-cli-sk` service key setup and credential caching as `users` (TOON preview + `y/N` confirmation, bypass with `-y`); outputs TOON (default) or JSON (`--format json`)
  - `--orgs` / `--excludeOrgs` CSV files (`region,id,name`) to target or skip specific orgs
  - Roles include: `roleTemplateAppId`, `roleTemplateName`, `name`, `appName`, `description`, `isReadOnly`
  - Role collections include: `name`, `description`, `isReadOnly`, and `roleReferences` (each with `roleTemplateAppId`, `roleTemplateName`, `name`, `description`)

### Changed
- **`logoff`** — now also clears cached XSUAA credentials (`clientid`, `clientsecret`, `url`, `access_token`) while preserving stored regions

### Improvements
- CF API rate limiting: HTTP 429 responses are handled with automatic retry; `Retry-After` header is honoured when present; falls back to randomised exponential backoff (base 2 s, cap 60 s, up to 5 retries) when the header is absent
- All CF API list calls (`/v3/organizations`, `/v3/spaces`, `/v3/roles`, `/v3/organizations/{guid}/users`, `/v3/spaces/{guid}/users`) now use `per_page=5000` to minimise round trips
- `org-users` and `org-space-users` fetch all role assignments for a region in a single bulk call (`/v3/roles?per_page=5000`) instead of one call per org or space, significantly reducing API usage on large landscapes

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
