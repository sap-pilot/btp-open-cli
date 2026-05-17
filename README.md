# btp-open-cli

Open-source CLI for SAP Business Technology Platform (BTP) — batch-manage users, services, destinations, and common CF development tasks.

## Installation

### 1. Install Go (Ubuntu)

```bash
sudo apt update && sudo apt install -y golang-go
```

For a specific Go version (recommended: 1.22+):

```bash
wget https://go.dev/dl/go1.22.5.linux-amd64.tar.gz
sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf go1.22.5.linux-amd64.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
source ~/.bashrc
```

Verify:

```bash
go version
```

### 2. Build

Clone the repository and compile:

```bash
git clone <repo-url>
cd btp-open-cli
go build -o bo
```

Move the binary to your PATH (optional):

```bash
sudo mv bo /usr/local/bin/
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

## More help

Run `bo <command> --help` for full flag descriptions and usage examples for any command:

```bash
bo login --help
bo org-users --help
bo org-space-users --help
```
