# btp-open-cli

Open-source CLI for SAP BTP — bulk-manage users and services across multiple Cloud Foundry subaccounts and regions

## Installation

### Option A — Download pre-built binary (recommended)

Pre-built binaries for every platform are attached to each [release](https://github.com/da-chen/btp-open-cli/releases/latest). A `checksums.txt` file is included in the assets — use it to verify your download before running.

#### Windows

Open PowerShell and run:

```powershell
Invoke-WebRequest -Uri "https://github.com/da-chen/btp-open-cli/releases/latest/download/bo-windows-amd64.exe" -OutFile "bo.exe"
```

Verify the checksum (compare against `checksums.txt` in the release assets):

```powershell
Get-FileHash .\bo.exe -Algorithm SHA256
```

Start using the CLI:

```powershell
.\bo.exe login --regions us10
```

#### Linux

```bash
wget -O bo https://github.com/da-chen/btp-open-cli/releases/latest/download/bo-linux-amd64
chmod +x ./bo
./bo login --regions us10
```

Verify the checksum:

```bash
sha256sum ./bo
# compare against checksums.txt in the release assets
```

#### macOS (Apple Silicon)

```bash
wget -O bo https://github.com/da-chen/btp-open-cli/releases/latest/download/bo-darwin-arm64
chmod +x ./bo
./bo login --regions us10
```

Verify the checksum:

```bash
sha256sum ./bo
# compare against checksums.txt in the release assets
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
git clone https://github.com/da-chen/btp-open-cli.git
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

### `logoff`

Clear stored OAuth tokens (regions are preserved for the next login).

```bash
bo logoff
```

### `orgs`

List all accessible CF organizations across one or more regions and output them as CSV.
The output format (`region,id,name`) is compatible with the `--orgs` and `--excludeOrgs` flags
accepted by `create-org-space-users`, `delete-org-space-users`, and `users`.

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

CSV format (`name,origin,roles`):
```
name,origin,roles
user@example.com,sap.ids,organization_user;organization_manager;space_developer;space_manager
```

Org-level roles (`organization_*`) are assigned to each target org.
Space-level roles (`space_*`) are assigned to every space within each target org.

```bash
# Add to all orgs in stored regions (shows TOON preview, prompts y/N)
bo create-org-space-users --users users.csv

# Skip confirmation prompt
bo create-org-space-users --users users.csv -y

# Target specific orgs only (CSV: region,id,name)
bo create-org-space-users --users users.csv --orgs target-orgs.csv

# Exclude orgs such as production environments (CSV: region,id,name)
bo create-org-space-users --users users.csv --excludeOrgs prod-orgs.csv

# Specific regions
bo create-org-space-users --users users.csv --regions us10,eu10
```

Without `-y`, a TOON preview of target orgs/spaces and users is shown before any changes are made.

### `delete-org-space-users`

Remove users from every space (space roles first, then org roles after a 5-second pause) across all accessible CF orgs.

CSV format (`name,origin`):
```
name,origin
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

# Include only specific orgs (CSV: region,id,name)
bo users --orgs target-orgs.csv

# Exclude orgs such as production environments (CSV: region,id,name)
bo users --excludeOrgs prod-orgs.csv

# Filter output — only users matching a substring in any field
bo users --filter "@example.com"
bo users --filter "sap.ids"

# Include only specific fields in output
bo users --fields id,userName,origin

# Exclude specific fields from output
bo users --excludeFields lastLogonTime,groups

# Combine filtering and field selection
bo users --filter "sap.ids" --excludeFields groups --regions us10
```

Output format (TOON):
```
regions:
  - id: us10
    orgs:
      - id: <org-guid>
        name: my-org
        users:
          - id: <user-id>
            externalId: user@example.com
            origin: sap.ids
            userName: user@example.com
            lastLogonTime: 2026-01-15T08:30:00Z
            groups: <group-values>
```

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
```
