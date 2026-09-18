---
name: btp-open-cli
description: Use when the task involves inventorying, auditing, or bulk-managing SAP BTP Cloud Foundry orgs, spaces, users, role collections, or destinations via the `bo` CLI (btp-open-cli) — e.g. "list all users across our BTP orgs", "export a UAR", "bulk-create/delete users", "sync destinations across orgs". Requires the `bo` binary on PATH and an active `bo login` session.
---

# BTP administration via `bo`

`bo` (btp-open-cli) is a Go CLI for bulk operations across many SAP BTP Cloud
Foundry orgs at once: listing/auditing users, org/space role assignments, apps,
and destinations, plus bulk create/delete of users, role-collection
memberships, and subaccount destinations, all via CSV/JSON in and out. This
skill exists so an agent uses it safely and non-interactively — read the
guardrails section before running any write command.

## Prerequisites

- `bo` must be on PATH — check with `bo version`. If that fails, install it
  yourself using "Bootstrapping `bo`" below rather than asking the user to go
  download it by hand.
- Logged in: `bo login --regions <region1,region2>`. The session (tokens,
  optional default org scope) lives in `~/.bo/credentials.json` until
  `bo logoff`. If a command fails with `not logged in`, see "Logging in"
  below — don't just tell the user to run `bo login` themselves and stop.
- `bo <command> --help` is the source of truth for exact flags; this skill
  summarizes shapes and safety rules, not every flag.

## Bootstrapping `bo` (if it's not already installed)

If `bo version` fails (command not found), fetch the pre-built binary for the
current platform straight from the project's GitHub Releases — don't clone
and compile the repo just for this, and don't ask the user to install it
manually unless this fails. Tell the user you're installing `bo` from the
official release before running this.

```bash
if ! command -v bo >/dev/null 2>&1; then
  os=$(uname -s | tr '[:upper:]' '[:lower:]')     # linux | darwin
  arch=$(uname -m)
  case "$arch" in
    x86_64|amd64)  arch=amd64 ;;
    arm64|aarch64) arch=arm64 ;;
    *) echo "unsupported arch: $arch — see https://github.com/sap-pilot/btp-open-cli/releases/latest" >&2; exit 1 ;;
  esac
  asset="bo-${os}-${arch}"

  install_dir="$HOME/.local/bin"
  mkdir -p "$install_dir"
  curl -fsSL -o "$install_dir/bo" \
    "https://github.com/sap-pilot/btp-open-cli/releases/latest/download/${asset}"
  chmod +x "$install_dir/bo"
  export PATH="$install_dir:$PATH"   # current session only
fi
bo version
```

Notes:
- This only covers Linux and macOS (the shells an agent normally runs in). On
  Windows, use the PowerShell download command from the project's README
  Installation section instead — don't try to translate this script to
  PowerShell yourself, follow that section verbatim.
- `export PATH=...` only affects the current shell/session — if `bo` needs to
  persist for the user across sessions, tell them to add `$HOME/.local/bin`
  to their shell profile, don't silently edit it for them.
- Optional integrity check before trusting the binary: download
  `.../releases/latest/download/checksums.txt`, find the line ending in the
  same `asset` name, and compare its hash to `sha256sum "$install_dir/bo"` —
  treat a mismatch as a hard stop, not a warning.

## Logging in

If a command fails with `not logged in`, log in yourself rather than telling
the user to go run `bo login` in their own terminal and come back.

**Password login** (no SSO required) is fully non-interactive — just ask the
user for their email/region(s), and get the password from them however your
environment handles secrets (never have them paste it as plain chat text if
you can avoid it):

```bash
bo login --regions <region1,region2> --username <email> --password <password>
```

**SSO login** has no password to ask for — it needs a one-time passcode that
only exists after a human authenticates in a browser. Do this in two steps
rather than running `bo login --sso` bare (which prints the passcode URL(s)
but then blocks on a masked terminal prompt per region that you cannot
answer):

1. Get the passcode URL(s) without hanging, by running with stdin closed —
   this prints the URL(s) and then fails fast instead of waiting for input
   you can't provide:
   ```bash
   bo login --sso --regions <region1,region2> < /dev/null
   ```
2. Show the user each printed URL (`<region> → <url>/passcode`) and ask them
   to open it in their browser — where their existing SSO session issues a
   one-time passcode — and paste each code back to you in the chat.
3. Once you have a code per region, complete the login non-interactively,
   passing the codes comma-separated in the same order as `--regions`:
   ```bash
   bo login --sso --regions <region1,region2> --passcode <code-for-region1>,<code-for-region2>
   ```
   Confirm you see `Authenticated. N region(s) active.` before continuing.

If `--passcode` comes back as an unrecognized flag, the installed `bo` predates
this flag — fall back to asking the user to run `bo login --sso` themselves in
their own terminal (it needs a real TTY either way) and let you know once
they're logged in.

## Running non-interactively (read this first)

- `bo orgs` (the interactive org picker) requires a real terminal and fails
  fast otherwise. Never run it bare in an agent shell. Instead:
  - `bo orgs --all` (optionally with `--include`/`--exclude`) selects a scope
    without prompting, and can persist it via `--format csv -o some.csv` for
    reuse as `--orgs some.csv` on other commands, or
  - skip `bo orgs` entirely and pass `--org <guid-or-name>` / `--orgs <csv>`
    directly to whichever command you're running.
- Commands with a `[y/N]` confirmation or a "press Enter to retry / type 's'
  to skip / Ctrl-C to abort" prompt read one line from stdin. On closed/EOF
  stdin they abort safely (no changes made) — but if the shell you're given
  leaves stdin open and idle, the process blocks waiting for input instead of
  failing. Always redirect stdin explicitly so it fails safe immediately:
  ```bash
  bo create-users users.csv < /dev/null   # aborts right after printing the preview
  ```

## Core building blocks

- **Read-only** (safe to run freely): `orgs`, `org-spaces`, `org-users`,
  `org-space-users`, `users`, `apps`, `role-collections`,
  `space-destinations`, `subaccount-destinations`, `describe-subaccount`,
  `count-lines`.
- **Write** (change BTP state — see guardrails below): `create-users`,
  `delete-users`, `create-org-space-users`, `delete-org-space-users`,
  `create-subaccount-destinations`, `update-subaccount-destinations`,
  `delete-subaccount-destinations`.
- Common flags across most commands: `--regions`, `--org`/`--orgs`/
  `--excludeOrgs` (falls back to the `bo orgs` session default scope if
  omitted), `--format toon|json|csv` (plus `uar.csv` on `users` and
  `org-space-users`), `--output`/`-o <file>`, and `--include <kw1,kw2,...>` /
  `--exclude <kw1,kw2,...>` — comma-separated, case-insensitive, OR-matched
  substrings (the destination commands also accept a glob per keyword, e.g.
  `API*PP`).

## Guardrails for write operations

Read all of these before running any command from the "Write" list above.

1. **Preview before writing.** Run the matching read command with the same
   `--org`/`--orgs` and `--include`/`--exclude` filters first, and account for
   exactly which rows/orgs will be affected before running the write command.
2. **Never add `-y`/`--yes` on your own initiative.** `create-users`,
   `delete-users`, `create-org-space-users`, and `delete-org-space-users`
   print a full TOON preview and ask `Proceed? [y/N]` before touching
   anything. Get that preview in front of the user (or reproduce it and get
   explicit approval) before adding `-y` — don't skip straight to `-y`
   because it's "obviously" what the user asked for.
3. **`create-subaccount-destinations`, `update-subaccount-destinations`, and
   `delete-subaccount-destinations` have no confirmation step and no `-y`
   flag at all — they act immediately on invocation.** Treat these as
   irreversible the instant you run them: always run `subaccount-destinations`
   (read) first to see current state, and show the user the exact
   `--destinations` JSON payload before calling any of the three.
4. **Scope explicitly — don't lean on the session default.** Pass `--org`/
   `--orgs` yourself rather than assuming `bo orgs`'s saved default scope is
   what the user means right now; a human may have set it earlier for a
   different task, and it's cleared silently by `bo login`/`bo logoff`.
5. **Multi-org writes apply the identical payload to every org in scope.**
   The three `subaccount-destinations` write commands and
   `create-org-space-users`/`delete-org-space-users` fan out unchanged across
   every org/row you target — if more than one org is in scope, confirm with
   the user that fan-out across all of them is intended.
6. **Filter with `--include`/`--exclude`, not by hand-editing CSVs after the
   fact** — it's the exact mechanism the write command itself uses to decide
   what's affected, so a preview run with the same filters is a faithful
   preview.
7. **Redirect stdin (`< /dev/null`)** on any write command you're running
   without a human present to answer its prompt — see "Running
   non-interactively" above.
8. **Everything is logged** to `~/.bo/log/bo_YYYY-MM-DD.log` (full output,
   timestamped, one entry per invocation) — point the user there for an audit
   trail after a bulk operation, and check it yourself if an outcome is
   ambiguous.

## Example workflows

### Inventory / audit (read-only, safe to run freely)

```bash
# Pick a scope non-interactively and save it for reuse
bo orgs --all --include prod --format csv -o prod-orgs.csv

# User Access Review export across those orgs
bo users --orgs prod-orgs.csv --format uar.csv -o uar-export.csv

# Destinations matching a keyword or glob, across the same scope
bo subaccount-destinations --orgs prod-orgs.csv --include "API*PP" --format csv
```

### Bulk-create users (guarded)

```bash
# 1. Preview who's already there / who a keyword would target
bo users --org <org-guid> --include sap.custom --format csv

# 2. Dry preview of the create — this prints the TOON preview and then
#    aborts at the [y/N] prompt since stdin is closed
bo create-users new-users.csv < /dev/null

# 3. Only after the preview above has been shown to and approved by the user:
bo create-users new-users.csv -y
```

### Bulk-remove role collection assignments (guarded)

```bash
bo org-space-users --org <org-guid> --include sap.default --format csv     # preview
bo delete-org-space-users targets.csv --include sap.default < /dev/null    # dry-run
bo delete-org-space-users targets.csv --include sap.default -y             # after approval
```

### Sync destinations across orgs (highest caution — no confirm step exists)

```bash
bo subaccount-destinations --orgs target-orgs.csv --format json   # current state
# show dests.json's contents to the user before proceeding
bo update-subaccount-destinations --orgs target-orgs.csv --destinations dests.json --no-prompt
```

## When something fails

- `not logged in` → see "Logging in" above; don't just tell the user to run
  `bo login` and wait.
- `no orgs specified` → pass `--org`/`--orgs`, or run `bo orgs --all ...` first.
- `interactive ... requires a terminal` → `bo orgs` was run without `--all` in
  a non-interactive shell; add `--all` or pass `--org`/`--orgs` to the actual
  command instead of going through the picker.
- For anything else, `bo <command> --help` and `~/.bo/log/` are the sources of
  truth — don't guess at flags.
