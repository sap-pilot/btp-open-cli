# btp-open-cli

Open-source CLI for SAP BTP — bulk-manage users, apps and services across multiple Cloud Foundry subaccounts and regions

## Installation

### Option A — Download pre-built binary (recommended)

Pre-built binaries for every platform are attached to each [release](https://github.com/sap-pilot/btp-open-cli/releases/latest).

#### Windows

Open PowerShell and run:

```powershell
Invoke-WebRequest -Uri "https://github.com/sap-pilot/btp-open-cli/releases/latest/download/bo-windows-amd64.exe" -OutFile "bo.exe"
```

Start using the CLI:

```powershell
.\bo.exe login --regions us10
```

#### Linux

```bash
wget -O bo https://github.com/sap-pilot/btp-open-cli/releases/latest/download/bo-linux-amd64
chmod +x ./bo
./bo login --regions us10
```

#### macOS (Apple Silicon)

```bash
wget -O bo https://github.com/sap-pilot/btp-open-cli/releases/latest/download/bo-darwin-arm64
chmod +x ./bo
./bo login --regions us10
```

> **macOS Gatekeeper:** if macOS blocks the binary, run `xattr -d com.apple.quarantine ./bo` to remove the quarantine flag, then retry.

Move the binary to your PATH (optional, Linux/macOS):

```bash
sudo mv bo /usr/local/bin/
```

### Option B — Build from source

Requires Go 1.22+. Install Go on Ubuntu:

```bash
wget https://go.dev/dl/go1.22.5.linux-amd64.tar.gz
sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf go1.22.5.linux-amd64.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
source ~/.bashrc
go version
```

Clone and compile:

```bash
git clone https://github.com/sap-pilot/btp-open-cli.git
cd btp-open-cli
go build -o bo
sudo mv bo /usr/local/bin/   # optional
```

## Commands

### `login`

Authenticate against one or more SAP BTP Cloud Foundry regions.

```bash
# Interactive password login (single region)
bo login --region us10

# Interactive password login (multiple regions)
bo login --regions us10,eu10

# Non-interactive login for CI/CD (e.g. GitHub Actions)
bo login --regions us10,eu10 -u user@example.com -p secret

# SSO passcode login
bo login --sso --regions us10,eu10
```

Regions are persisted — subsequent commands reuse them if `--regions` is not specified.

#### Automatic token refresh

OAuth access tokens expire after a few hours. When any command receives an HTTP 401 from the CF API, `bo` automatically attempts to recover without interrupting the operation:

1. **Silent refresh** — tries the stored OAuth refresh token to obtain a new access token without any prompts. If this succeeds the command continues transparently.
2. **Interactive re-authentication** — if the refresh token is also expired or invalid, `bo` prints a message and prompts for credentials using the same method as the last login:
   - Password login: prompts for email and password
   - SSO login: shows the passcode URL and prompts for a new one-time passcode
3. If re-authentication succeeds the command resumes; if it fails (or you press Ctrl-C) the affected region is skipped with a warning.

All interactive prompts — email, password, SSO passcode, and `[y/N]` confirmations — respect **Ctrl-C** and exit cleanly.

### `logoff`

Clear stored OAuth tokens (regions are preserved for the next login).

```bash
bo logoff
```

### `orgs`

List all accessible CF organizations across one or more regions and output them as CSV.
The output format (`region,org_id,org_name`) is compatible with the `--orgs` and `--excludeOrgs` flags
accepted by `create-org-space-users`, `delete-org-space-users`, `apps`, `users`, `delete-users`, and `role-collections`.

```bash
# List orgs for stored regions
bo orgs

# Specific regions
bo orgs --regions us10,eu10

# Save to a file for use as --orgs / --excludeOrgs input
bo orgs --regions us10 > my-orgs.csv

# Exclude production orgs when creating users
bo orgs --regions us10 | grep prod > prod-orgs.csv
bo create-org-space-users --users users.csv --excludeOrgs prod-orgs.csv
```

### `org-users`

List all users across every accessible CF organization.

```bash
# Default TOON output
bo org-users

# JSON output
bo org-users --format json

# CSV output
bo org-users --format csv

# Filter by name, id, origin, or role
bo org-users --filter manager

# Specific regions
bo org-users --regions us10,eu10
```

### `org-space-users`

List users at both the organization and space level.

```bash
# Default TOON output
bo org-space-users

# JSON output
bo org-space-users --format json

# CSV output
bo org-space-users --format csv

# Filter by name, id, origin, or role
bo org-space-users --filter space_developer

# Specific regions
bo org-space-users --regions us10,eu10
```

### `create-org-space-users`

Add users with org and space roles to target CF organizations and their spaces from a CSV file.

CSV format (`cfuser_name,cfuser_origin,cfuser_roles`):
```
cfuser_name,cfuser_origin,cfuser_roles
user@example.com,sap.ids,organization_user;organization_manager;space_developer;space_manager
```

Org-level roles (`organization_*`) are assigned to each target org.
Space-level roles (`space_*`) are assigned to every space within each target org.

```bash
# Add to all orgs in stored regions (shows TOON preview, prompts y/N)
bo create-org-space-users --users users.csv

# Skip confirmation prompt
bo create-org-space-users --users users.csv -y

# Target specific orgs only (CSV: region,org_id,org_name)
bo create-org-space-users --users users.csv --orgs target-orgs.csv

# Exclude orgs such as production environments (CSV: region,org_id,org_name)
bo create-org-space-users --users users.csv --excludeOrgs prod-orgs.csv

# Specific regions
bo create-org-space-users --users users.csv --regions us10,eu10
```

Without `-y`, a TOON preview of target orgs/spaces and users is shown before any changes are made.

### `delete-org-space-users`

Remove users from every space (space roles first, then org roles after a 5-second pause) across all accessible CF orgs.

CSV format (`cfuser_name,cfuser_origin`):
```
cfuser_name,cfuser_origin
user@example.com,sap.ids
```

```bash
# Preview roles to be deleted, then confirm (y/N)
bo delete-org-space-users --users users.csv

# Skip confirmation prompt
bo delete-org-space-users --users users.csv -y

# Specific regions
bo delete-org-space-users --users users.csv --regions us10,eu10
```

Without `-y`, a TOON preview of all roles that will be deleted is shown before any changes are made.

### `users`

List users from the XSUAA (Authorization and Trust Management) `apiaccess` service across all accessible CF organizations.

For each organization the command checks whether the service instance `btp-xsuaa` (xsuaa / apiaccess plan) and service key `btp-open-cli-sk` exist in the `util` space. If they are missing, a TOON preview of what will be created is shown before any changes are made. Credentials retrieved from the service key are cached in `~/.bo/credentials.json` and reused on subsequent runs.

```bash
# List XSUAA users across all orgs in stored regions
bo users

# Skip service/key creation confirmation
bo users -y

# Scope to specific regions
bo users --regions us10,eu10

# Fetch users from a single org by GUID
bo users --org <org-guid>

# Include only specific orgs (CSV: region,org_id,org_name)
bo users --orgs target-orgs.csv

# Exclude orgs such as production environments (CSV: region,org_id,org_name)
bo users --excludeOrgs prod-orgs.csv

# Filter output — only users matching a substring in any field
bo users --filter "@example.com"
bo users --filter "sap.ids"

# Include only specific fields in output
bo users --fields user_id,userName,email,user_origin

# Exclude specific fields from output
bo users --excludeFields lastLogonTime,groups

# Combine filtering and field selection
bo users --filter "sap.ids" --excludeFields groups --regions us10
```

Output format (TOON):
```
regions:
  - region: us10
    orgs:
      - org_id: <org-guid>
        org_name: my-org
        users:
          - user_id: <user-id>
            user_externalId: user@example.com
            user_origin: sap.ids
            userName: user@example.com
            email: user@example.com
            lastLogonTime: 2026-01-15T08:30:00Z
            groups: <group-values>
```

### `delete-users`

Delete users from the XSUAA (Authorization and Trust Management) `apiaccess` service across all accessible CF organizations.

For each organization the command checks whether the service instance `btp-xsuaa` (xsuaa / apiaccess plan) and service key `btp-open-cli-sk` exist in the `util` space. If they are missing, a TOON preview of what will be created is shown before any changes are made. Credentials retrieved from the service key are cached in `~/.bo/credentials.json` and reused on subsequent runs.

CSV format (`origin,userName`):
```
origin,userName
sap.default,user@example.com
```

Users are matched by `origin` + `userName` (case-insensitive). A TOON preview of all matched users is shown before any deletions are made.

```bash
# Preview users to be deleted, then confirm (y/N)
bo delete-users --users delete-users.csv

# Skip all confirmation prompts
bo delete-users --users delete-users.csv -y

# Scope to specific regions
bo delete-users --users delete-users.csv --regions us10,eu10

# Include only specific orgs (CSV: region,org_id,org_name)
bo delete-users --users delete-users.csv --orgs target-orgs.csv

# Exclude orgs such as production environments (CSV: region,org_id,org_name)
bo delete-users --users delete-users.csv --excludeOrgs prod-orgs.csv
```

Preview output format (TOON):
```
Users to be deleted:
regions:
  - region: us10
    orgs:
      - org_id: <org-guid>
        org_name: my-org
        users:
          - user_id: <user-id>
            user_externalId: user@example.com
            user_origin: sap.default
            userName: user@example.com
            email: user@example.com
            lastLogonTime: 2026-01-15T08:30:00Z
            groups: <group-values>
```

Without `-y`, the preview is shown and `Proceed with user deletion? [y/N]` is prompted before any changes are made.

### `role-collections`

List XSUAA roles and role collections (with their role references) across all accessible CF organizations.

For each organization the command checks whether the service instance `btp-xsuaa` (xsuaa / apiaccess plan) and service key `btp-open-cli-sk` exist in the `util` space. If they are missing, a TOON preview of what will be created is shown before any changes are made. Credentials retrieved from the service key are cached in `~/.bo/credentials.json` and reused on subsequent runs.

```bash
# List roles and role collections across all orgs in stored regions
bo role-collections

# Skip service/key creation confirmation
bo role-collections -y

# Scope to specific regions
bo role-collections --regions us10,eu10

# Fetch roles and role collections from a single org by name or GUID
bo role-collections --org <org-name-or-guid>

# Include only specific orgs (CSV: region,org_id,org_name)
bo role-collections --orgs target-orgs.csv

# Exclude orgs such as production environments (CSV: region,org_id,org_name)
bo role-collections --excludeOrgs prod-orgs.csv

# JSON output
bo role-collections --format json
```

Output format (TOON):
```
regions:
  - region: us10
    orgs:
      - org_id: <org-guid>
        org_name: my-org
        roles:
          - roleTemplateAppId: xsuaa!t1
            roleTemplateName: xsuaa_admin
            role_name: User and Role Administrator
            appName: xsuaa
            description: Manage authorizations, trusted identity providers, and users.
            isReadOnly: true
        roleCollections:
          - rolecollection_name: Subaccount Administrator
            description: Administrative access to the subaccount
            isReadOnly: true
            roleReferences:
              - roleTemplateAppId: xsuaa!t1
                roleTemplateName: xsuaa_admin
                role_name: User and Role Administrator
                description: Manage authorizations, trusted identity providers, and users.
```

### `describe-subaccount`

Describe a single BTP subaccount in detail: CIS account metadata, subaccount-level destinations (passwords redacted), and XSUAA role collections.

The `--org` value is matched by exact GUID or case-insensitive substring on the org name. The CIS `central-viewer` service key is auto-discovered once from any accessible org/space and cached; subsequent runs reuse the cached credentials.

By default the CF org GUID is used as the BTP subaccount ID in the CIS API call. Use `--subaccount` to specify a different subaccount GUID when the CF org GUID and BTP subaccount GUID differ.

**Prerequisites:**
- A `cis` service instance with plan `central-viewer` and at least one service key must exist in any accessible org/space. If not found, the command prints instructions and exits.
- The `btp-xsuaa` (xsuaa / apiaccess) service key setup in the target org's `util` space (same requirement as `users` and `role-collections`).

```bash
# Describe a subaccount by org name (TOON output)
bo describe-subaccount --org my-org-name

# Describe a subaccount by org GUID
bo describe-subaccount --org <org-guid>

# Override the BTP subaccount GUID used in the CIS API call
bo describe-subaccount --org my-org-name --subaccount <btp-subaccount-guid>

# JSON output
bo describe-subaccount --org my-org-name --format json

# Skip XSUAA service/key creation confirmation
bo describe-subaccount --org my-org-name -y

# Scope region search
bo describe-subaccount --org my-org-name --regions us10,eu10
```

Output format (TOON):
```
subaccount:
  guid: <subaccount-guid>
  displayName: My Org
  globalAccountGUID: <global-account-guid>
  subdomain: my-org-subdomain
  region: eu10
  state: OK
  betaEnabled: false
  usedForProduction: NOT_USED_FOR_PRODUCTION
  createdDate: 2024-03-01T10:00:00Z
  modifiedDate: 2026-05-01T08:00:00Z
spaces:
  - space_id: <space-guid>
    space_name: dev
    services:
      - id: <instance-guid>
        name: my-xsuaa
        service: xsuaa
        plan: apiaccess
        state: succeeded
      - id: <instance-guid>
        name: my-destination
        service: destination
        plan: lite
        state: succeeded
destinations:
  - name: my-destination
    properties:
      - key: Authentication
        value: NoAuthentication
      - key: ProxyType
        value: Internet
      - key: Type
        value: HTTP
      - key: URL
        value: https://target.example.com
rolecollections:
  - rolecollection_name: Subaccount Administrator
    description: Administrative access to the subaccount
    isReadOnly: true
    roleReferences:
      - roleTemplateAppId: xsuaa!t1
        roleTemplateName: xsuaa_admin
        role_name: User and Role Administrator
```

### `get-space-destinations`

List all instance-level destinations across every destination service instance found in a given CF space.

Credentials (clientId, clientSecret, tokenURL, URI) are auto-fetched from the first available service key of each destination service instance and cached in `~/.bo/credentials.json`. Access tokens are refreshed automatically when within 60 seconds of expiry. If a destination service instance has no service key, a warning is printed with `cf create-service-key` instructions and an interactive prompt allows you to create one and press Enter to retry.

```bash
# List destinations in a space (TOON output)
bo get-space-destinations --space <space-guid>

# JSON output
bo get-space-destinations --space <space-guid> --format json

# Include all non-sensitive destination properties (not just the 5 standard fields)
bo get-space-destinations --space <space-guid> --all

# Scope region search
bo get-space-destinations --space <space-guid> --regions us10,eu10
```

Output format (TOON, default):
```
space_id: <space-guid>
space_name: dev
destination_service_instances:
  - destination_service_id: <instance-guid>
    destination_service_name: my-dest-service
    destinations:
      - Name: API_S4_HTTP_PP
        Type: HTTP
        Authentication: PrincipalPropagation
        URL: http://qr1:443
        sap-client: "100"
      - Name: API_MDG_HTTP_PP
        Type: HTTP
        Authentication: PrincipalPropagation
        URL: http://qrg:443
        sap-client: "100"
```

With `--all`, each destination gains a `properties` section containing all remaining non-sensitive keys (e.g. `ProxyType`, `WebIDESystem`, `HTML5.DynamicDestination`), sorted alphabetically.

### `create-space-destinations`

Create instance-level destinations in every destination service instance within a given CF space.

Reads a JSON array of destination objects from `--destinations` and POSTs them (`POST /v1/instanceDestinations`) to each destination service instance in the space. Credential caching and the no-key interactive prompt work the same way as `get-space-destinations`.

```bash
bo create-space-destinations --space <space-guid> --destinations ./destinations.json

# Scope region search
bo create-space-destinations --space <space-guid> --destinations ./destinations.json --regions us10,eu10
```

The JSON file format (`--destinations`):
```json
[
  {
    "Name": "API_S4_HTTP_PP",
    "Type": "HTTP",
    "URL": "http://qr1:443",
    "Authentication": "PrincipalPropagation",
    "ProxyType": "OnPremise",
    "sap-client": "100"
  }
]
```

### `update-space-destinations`

Update (overwrite) instance-level destinations in every destination service instance within a given CF space.

Reads a JSON array from `--destinations` and PUTs them (`PUT /v1/instanceDestinations`) to each destination service instance. Existing destinations with matching names are overwritten; others are left unchanged.

```bash
bo update-space-destinations --space <space-guid> --destinations ./destinations.json

# Scope region search
bo update-space-destinations --space <space-guid> --destinations ./destinations.json --regions us10,eu10
```

The `--destinations` JSON format is the same as `create-space-destinations`.

### `delete-space-destinations`

Delete named instance-level destinations from every destination service instance within a given CF space.

Reads the `Name` field from each entry in the `--destinations` JSON array and issues a `DELETE /v1/instanceDestinations/{name}` for each name against every destination service instance in the space. Non-existent destinations are silently ignored (idempotent).

```bash
bo delete-space-destinations --space <space-guid> --destinations ./destinations.json

# Scope region search
bo delete-space-destinations --space <space-guid> --destinations ./destinations.json --regions us10,eu10
```

Only the `Name` field is read from the JSON file; all other properties are ignored.

### `apps`

List Cloud Foundry applications across all accessible organizations and spaces.

For each region the command fetches organizations, spaces, apps, and web process metrics in parallel, then assembles the result.

```bash
# List all apps across stored regions (TOON output)
bo apps

# JSON output
bo apps --format json

# CSV output (flat, one row per app)
bo apps --format csv

# Scope to specific regions
bo apps --regions us10,eu10

# Scope to a single org by GUID
bo apps --org <org-guid>

# Include only specific orgs (CSV: region,org_id,org_name)
bo apps --orgs target-orgs.csv

# Exclude orgs such as production environments (CSV: region,org_id,org_name)
bo apps --excludeOrgs prod-orgs.csv

# Filter output — only apps matching a substring in any listed field
bo apps --filter myapp
bo apps --filter STARTED
bo apps --filter "my-mta-id"

# Combine flags
bo apps --regions us10 --orgs my-orgs.csv --format csv --filter STARTED
```

Output format (TOON):
```
regions:
  - region: us10
    orgs:
      - org_id: <org-guid>
        org_name: my-org
        spaces:
          - space_id: <space-guid>
            space_name: dev
            apps:
              - mta_id: my-mta
                app_id: <app-guid>
                app_name: my-app
                app_state: STARTED
                app_created_at: 2026-01-10T12:00:00Z
                app_updated_at: 2026-05-01T08:00:00Z
                process_instances: 2
                process_memory_in_mb: 512
                process_disk_in_mb: 1024
```

CSV columns: `region_id,org_id,org_name,space_id,space_name,app_mta_id,app_id,app_name,app_state,app_created_at,app_updated_at,process_instances,process_memory_in_mb,process_disk_in_mb`

The `--filter` flag matches case-insensitively against: `mta_id`, `app_id`, `app_name`, `app_state`, `app_created_at`, `app_updated_at`, and `process_memory_in_mb`.

### `reorg-wiki-attachments`

Reorganize Azure DevOps (or any similar) wiki attachments by moving files out of the flat `.attachments/` folder and placing them next to the wiki page that first references them. Meaningless filenames are renamed to `{wiki-page-name}-image-N.ext`.

```bash
bo reorg-wiki-attachments /path/to/wiki
```

The command:
1. Inventories all files in `/path/to/wiki/.attachments/`
2. Scans every `.md` file in the wiki tree (alphabetical order)
3. For each `/.attachments/filename` reference found in a page:
   - Moves the file to the same folder as that wiki page
   - Renames it to `{wiki-page-name}-image-N.ext` if the original name is meaningless (starts with `image` or `==image`, or is a UUID/GUID)
   - Updates the reference in the `.md` file to a relative path
4. Runs a second pass to update cross-page references pointing to moved files
5. Prints a CSV summary of all moves: `old_path,new_path`

```
Attachments found in .attachments/: 12
Wiki pages found:              8

  moved: .attachments/image.png
      -> design/overview-image-1.png
  moved: .attachments/ab12cd34-ef56-7890-ab12-cd34ef567890.png
      -> design/overview-image-2.png
  moved: .attachments/architecture-diagram.pdf
      -> design/architecture-diagram.pdf

old_path,new_path
.attachments/ab12cd34-ef56-7890-ab12-cd34ef567890.png,design/overview-image-2.png
.attachments/architecture-diagram.pdf,design/architecture-diagram.pdf
.attachments/image.png,design/overview-image-1.png
```

Cross-page references (a page referencing an attachment owned by another page) are updated to the correct relative path (e.g. `../design/architecture-diagram.pdf`). Collision-safe: if the target filename already exists, `-2`, `-3`, … is appended before the extension.

### `upgrade`

Check for the latest release on GitHub and upgrade the `bo` binary in place.

```bash
# Check for updates and confirm before downloading
bo upgrade

# Skip confirmation prompt
bo upgrade -y
```

The command compares the local version against the latest GitHub release. If a newer version is available it downloads the platform-matching binary (`bo-{os}-{arch}`) and replaces the running executable:

- **Linux / macOS** — downloads to a temp file in the same directory, then atomically renames it over the current binary.
- **Windows** — renames `bo.exe` to `bo-{version}.exe` first (Windows cannot overwrite a running executable), then downloads the new release as `bo.exe`.

### `version`

Print version information.

```bash
bo version
```

## Global flags

| Flag | Description |
|------|-------------|
| `-v`, `--verbose` | Enable debug/verbose output |
| `-h`, `--help` | Show help |

## Environment variables

| Variable | Description |
|----------|-------------|
| `HTTPS_PROXY` | Route all HTTPS requests through a proxy (e.g. `http://127.0.0.1:8080`) |
| `HTTPS_PROXY_INSECURE` | Set to `true` to skip TLS verification — required when using mitmproxy |

## Command history and logs

Every `bo` invocation appends its full output to a daily log file in the `log/` folder of the current working directory:

```
log/bo_YYYY-MM-DD.log
```

Each entry is separated by a header line that includes the timestamp and the exact command that was run:

```
=== 2026-05-18 14:30:00 bo org-users --regions us10 ===
... command output ...

=== 2026-05-18 14:31:05 bo users --filter sap.ids ===
... command output ...
```

The `log/` folder is created automatically on first use. Use it to review what was run, audit changes, or retrieve previous command output.

## Debugging with mitmproxy

[mitmproxy](https://mitmproxy.org) lets you inspect every HTTP request and response the CLI makes, which is useful for understanding the CF API or troubleshooting errors.

### Install mitmproxy (macOS)

```bash
brew install mitmproxy
```

### Install mitmproxy (Ubuntu)

```bash
pip3 install mitmproxy
```

### Intercept traffic

```bash
# Terminal 1 — start the proxy with a web UI at http://127.0.0.1:8081
mitmweb --listen-port 8080

# Terminal 2 — run any bo command through the proxy
HTTPS_PROXY=http://127.0.0.1:8080 HTTPS_PROXY_INSECURE=true bo org-users
HTTPS_PROXY=http://127.0.0.1:8080 HTTPS_PROXY_INSECURE=true bo login --regions us10
```

Open `http://127.0.0.1:8081` in a browser to browse captured requests interactively.

> **Note:** `HTTPS_PROXY_INSECURE=true` disables TLS certificate verification so mitmproxy's intercepted certificate is accepted. Do not use this in production.

## More help

Run `bo <command> --help` for full flag descriptions and usage examples for any command:

```bash
bo login --help
bo orgs --help
bo org-users --help
bo org-space-users --help
bo create-org-space-users --help
bo delete-org-space-users --help
bo users --help
bo delete-users --help
bo describe-subaccount --help
bo get-space-destinations --help
bo create-space-destinations --help
bo update-space-destinations --help
bo delete-space-destinations --help
bo apps --help
bo role-collections --help
bo reorg-wiki-attachments --help
bo upgrade --help
```
