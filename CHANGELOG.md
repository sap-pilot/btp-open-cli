# Changelog

## v0.7 — 2026-05-27

### Added

- **`subaccount-destinations`** — list subaccount-level destinations (`GET /v1/subaccountDestinations`) using any destination service instance found in any space of the target org (`--org`)
  - `--full`: all non-sensitive destination properties as a flat object (default: Name, URL, sap-client only)
  - `--filter <string|glob>`: case-insensitive substring or glob pattern matched against any destination property key or value; applied before field trimming
  - `--format toon|json|csv` (csv not supported with `--full`); CSV columns: `org_name,destination_name,destination_url,destination_sap_client`
  - `--no-prompt`: skip interactive prompts — fail immediately if no destination service instance or key is found
  - TOON output: `{org_id, org_name, destinations: [{Name, URL, sap-client}]}`; with `--full` destinations include all non-sensitive properties

- **`create-subaccount-destinations`** — POST a JSON array to `POST /v1/subaccountDestinations` via a destination service instance in the target org
  - `--org` (required): org GUID or name substring
  - `--destinations` (required): path to JSON file (array of destination objects)
  - `--no-prompt`, `--regions` flags

- **`update-subaccount-destinations`** — PUT a JSON array to `PUT /v1/subaccountDestinations` via a destination service instance in the target org; existing destinations with matching names are overwritten, others unchanged
  - Same flags as `create-subaccount-destinations`

- **`delete-subaccount-destinations`** — delete named destinations via `DELETE /v1/subaccountDestinations/{name}` for each `Name` in the JSON file; non-existent destinations are silently ignored (idempotent)
  - Same flags as `create-subaccount-destinations`

- **`org-spaces`** — new command to list all accessible CF organizations and their spaces
  - Fetches orgs and all spaces per region in parallel (one batched `GET /v3/spaces?per_page=5000` per region rather than one per org)
  - TOON output (default): `{regions:[{region, orgs:[{org_id, org_name, spaces:[{space_id, space_name}]}]}]}`
  - JSON output: `--format json`
  - CSV output: `--format csv` — columns `region,org_id,org_name,space_id,space_name`; one row per space; `region`, `org_id`, `org_name` are repeated for each space of the same org; orgs with no spaces are omitted
  - Spaces within each org are sorted alphabetically by name

- **`clear-logs`** — new command to delete all daily log files under `~/.bo/log/`
  - Shows a count and asks `Proceed? [y/N]` before deleting
  - `-y` skips the confirmation prompt

### Fixed

- **`users`, `role-collections` — `--org` flag ignored during XSUAA discovery**
  - Previously both commands called `resolveXsuaaClients` across all regions first, then applied the `--org` filter to the returned client list. This caused every region to be scanned for XSUAA instances even when the target org was known (e.g. specifying an eu20 org GUID would still scan and prompt on us20 orgs).
  - `users`: `--org` (GUID) is now folded into the `includeOrgs` filter passed to `resolveXsuaaClients` before the call, so only the matching org is processed. Returns an explicit error if the org GUID is not found in any accessible region.
  - `role-collections`: `--org` (GUID or case-insensitive name substring) now triggers a lightweight org pre-scan across all regions, resolves matching org GUIDs, then passes them as `includeOrgs` to `resolveXsuaaClients`. Returns an explicit error if no matching org is found.

### Changed

- **`users`, `delete-users`, `role-collections`, `describe-subaccount` — XSUAA credential handling rewritten**
  - Previously each command looked for a hardcoded service instance (`btp-xsuaa`) and service key (`btp-open-cli-sk`) in a hardcoded `util` space and offered to create them if missing
  - Now each command searches **all spaces** in the target org for any `xsuaa` service instance with the `apiaccess` plan, and uses the **first available service key** — no hardcoded names, no service creation
  - If no instance or key is found, the command prints CF CLI instructions (`cf create-service xsuaa apiaccess <name>` / `cf create-service-key`) and prompts the user to create them manually, then retries once on Enter; Ctrl-C skips the org
  - **`--no-prompt`** flag — skip the interactive prompt entirely; orgs without a service instance or key are silently skipped
  - **XSUAA service key credentials are no longer stored locally.** Only the access token (plus APIURL and expiry) is cached in `~/.bo/credentials.json` under `org_xsuaa[orgGUID]`. Client ID, client secret, and token URL are fetched from CF on demand each time a token refresh is needed and discarded immediately after.
  - `-y` / `--yes` flag removed from `users`, `role-collections`, and `describe-subaccount` (it was only used to skip service/key creation confirmation, which no longer happens); `-y` is retained in `delete-users` for the user deletion confirmation prompt

- **`get-space-destinations` renamed to `space-destinations`**; output and flags reworked:
  - Replaced `--all` flag with `--full`: without `--full` only `Name`, `URL`, and `sap-client` are shown per destination; with `--full` all non-sensitive destination properties are emitted as a flat object
  - Default and JSON output (without `--full`): `{space_id, space_name, destination_service_instances:[{destination_service_id, destination_service_name, destinations:[{Name, URL, sap-client}]}]}`
  - Added `--format csv` (without `--full`): flat CSV with columns `space_name,destination_service_name,destination_name,destination_url,destination_sap_client` — one row per destination; CSV is not supported with `--full`
  - Added `--filter <string|pattern>`: case-insensitive substring or glob pattern (e.g. `MDG`, `API*PP`) matched against every destination property key and value; only matching destinations are included; filter is applied to the full raw property set before any field trimming

- **`space-destinations`/`create-space-destinations`/`update-space-destinations`/`delete-space-destinations` — destination service key credentials no longer stored locally**
  - Previously the destination service `clientId`, `clientSecret`, `tokenURL`, and `URI` were all cached in `~/.bo/credentials.json` under `space_dest_services`
  - Now only the access token, `tokenURL`, and `URI` are persisted; `clientId` and `clientSecret` are fetched from CF on demand whenever a new token is needed and discarded immediately after — they never touch the local disk
  - Token refresh behaviour (60-second expiry window) and the no-key interactive prompt are unchanged

- **`create-space-destinations`, `update-space-destinations`, `create-subaccount-destinations`, `update-subaccount-destinations`** — success output changed from a generic `done` to per-destination lines (`created: {name}` / `updated: {name}`), consistent with the `deleted: {name}` output of the delete commands; when the destination service returns a bulk response (HTTP 207), per-item status is used; otherwise the names from the input file are used

- **`logoff`** — now also clears cached destination service access tokens (`space_dest_services`) in addition to CF region tokens and XSUAA tokens

### Internals

- `cmd/xsuaasetup.go`: removed `discoverXsuaaPlans`, `ensureXsuaaCredentials`, `xsuaaRefreshToken`, `xsuaaPrintSetupPreview` and the `xsuaaOrgPlan`/`xsuaaRegionPlan` types; replaced with `resolveXsuaaClients` (returns `[]xsuaaOrgClient` with ready-to-use token + APIURL) and the `xsuaaPromptRetryInstance`/`xsuaaPromptRetryKey` prompt helpers
- `internal/store/token.go`: removed `ClientID`/`ClientSecret`/`URL` fields from `XsuaaData` (only `APIURL`, `AccessToken`, `TokenExpiry` remain); removed `ClientID`/`ClientSecret` from `DestInstanceCache` (only `InstanceName`, `TokenURL`, `URI`, `AccessToken`, `TokenExpiry` remain); `ClearTokens()` now also zeroes `SpaceDestServices`
- `cmd/subaccountdestinations.go`: new file; `resolveOrgDestClient` helper scans all regions to locate the target org (by GUID or name), lists all spaces, finds any destination/lite instance, refreshes token on demand (caching only access token + tokenURL + URI in `SpaceDestServices`); credentials never stored
- `internal/destination/client.go`: refactored `ListSubaccountDestinations` into a shared `listSubaccountDestinations(redact bool)` helper; added `ListSubaccountDestinationsFull`, `CreateSubaccountDestinations`, `UpdateSubaccountDestinations`, `DeleteSubaccountDestination`; switched local `client` to module-level `httpClient`
- `cmd/spacedestinations.go`: replaced `printBulkResults` with `printActionResults(cmd, action, names, items)` — when the API returns a bulk response, prints `created/updated: {name}` per success and `ERROR({status}): {name}` per failure; when the API returns no body (simple 201/200), falls back to printing `created/updated: {name}` for each name in the input file

## v0.6 — 2026-05-26

### Added

- **`space-destinations`** — list all instance-level destinations across every destination service instance in a CF space (identified by `--space <GUID>`)
  - Calls `GET /destination-configuration/v1/instanceDestinations` for each instance
  - Default output (TOON or JSON with `--format json`): `{space_id, space_name, destination_service_instances: [{id, name, destinations: [{Name, Type, Authentication, URL, sap-client}]}]}`
  - `--all` flag: adds a `properties` section to each destination with all remaining non-sensitive keys, sorted alphabetically
  - Sensitive credential fields (`Password`, `ClientSecret`, `ProxyPassword`, etc.) are always redacted from responses
  - Destination service credentials are fetched from the first available service key of each instance and cached in `~/.bo/credentials.json` under `space_dest_services[spaceGUID][instanceGUID]`; access tokens are refreshed automatically when within 60 seconds of expiry
  - When a destination service instance has no service key, a warning is printed with `cf create-service-key` instructions and an interactive prompt lets you create one and press Enter to retry; the instance is skipped on Ctrl-C

- **`create-space-destinations`** — create instance-level destinations in every destination service instance of a CF space
  - Reads a JSON array from `--destinations` and calls `POST /v1/instanceDestinations` for each instance
  - Prints per-destination status when the API returns a bulk response (HTTP 207)
  - Same credential caching and no-key prompt as `space-destinations`

- **`update-space-destinations`** — update (overwrite) instance-level destinations in every destination service instance of a CF space
  - Reads a JSON array from `--destinations` and calls `PUT /v1/instanceDestinations` for each instance
  - Overwrites existing destinations with matching names; others are left unchanged
  - Same credential caching and no-key prompt as `space-destinations`

- **`delete-space-destinations`** — delete named instance-level destinations from every destination service instance of a CF space
  - Reads `Name` fields from the `--destinations` JSON array and calls `DELETE /v1/instanceDestinations/{name}` per name per instance
  - Idempotent: non-existent destinations are silently skipped (HTTP 404 treated as success)
  - Same credential caching and no-key prompt as `space-destinations`

- **`reorg-wiki-attachments [path]`** — reorganize wiki attachments from a flat `.attachments/` folder into per-page subdirectories
  - Inventories all files in `[path]/.attachments/`; scans all `.md` pages in alphabetical order
  - Moves each attachment to the same folder as the first page that references it
  - Renames files with meaningless names (starting with `image` / `==image`, or matching UUID/GUID format) to `{wiki-page-name}-image-N.ext`; collision-safe (`-2`, `-3`, … appended before extension if destination exists)
  - Updates all `/.attachments/` references in-place with correct relative paths; a second pass handles cross-page references (paths computed relative to each referencing page)
  - Prints a CSV summary at the end: `old_path,new_path`, sorted alphabetically by old path

### Fixed

- **XSUAA API URL for non-standard regions** — `users`, `delete-users`, and `role-collections` previously constructed the XSUAA admin API URL from the CF region name (e.g. `api.authentication.us10-001.hana.ondemand.com`) which does not exist for split regions. The `apiurl` field from the XSUAA service key is now stored in `~/.bo/credentials.json` under `XsuaaData.APIURL` and used directly; falls back to the constructed URL for existing cached credentials without the field. Fixes HTTP 404 errors on regions like `us10-001`.

### Changed

- **Log file location** — daily logs are now written to `~/.bo/log/bo_YYYY-MM-DD.log` instead of `./log/bo_YYYY-MM-DD.log` in the current working directory. Logs accumulate in one predictable place regardless of where `bo` is invoked from.

### Internals

- `internal/destination/client.go`: added `BulkResponseItem` type and four new functions for the instance-level destinations API: `ListInstanceDestinations`, `CreateInstanceDestinations`, `UpdateInstanceDestinations`, `DeleteInstanceDestination`
- `internal/cf/spaces.go`: added `FindSpaceByGUID` (`GET /v3/spaces?guids=`)
- `internal/cf/services.go`: added `ListServiceInstancesInSpace` (`GET /v3/service_instances?space_guids=&type=managed`)
- `internal/store/token.go`: added `DestInstanceCache` struct and `SpaceDestServices map[spaceGUID]map[instanceGUID]*DestInstanceCache` field to `Credentials` for persistent destination token caching

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
