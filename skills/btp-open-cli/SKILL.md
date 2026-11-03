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
  - `bo orgs --output some.csv --format csv` (optionally with
    `--include`/`--exclude`) skips the picker entirely — no prompting, no
    default scope change — and dumps the full accessible org list straight to
    the file, for reuse as `--orgs some.csv` on other commands, or
  - skip `bo orgs` entirely and pass `--org <guid-or-name>` / `--orgs <csv>`
    directly to whichever command you're running.
  - Note: `--format` is only accepted alongside `--output`, and `--all` has no
    effect when `--output` is given (nothing is being selected in that mode).
- Commands with a `[y/N]` confirmation or a "press Enter to retry / type 's'
  to skip / Ctrl-C to abort" prompt read one line from stdin. On closed/EOF
  stdin they abort safely (no changes made) — but if the shell you're given
  leaves stdin open and idle, the process blocks waiting for input instead of
  failing. Always redirect stdin explicitly so it fails safe immediately:
  ```bash
  bo create-users users.csv < /dev/null   # aborts right after printing the preview
  ```

## Core building blocks

- **Read-only** (safe to run freely): `orgs`, `org-spaces`, `spaces`,
  `org-users`, `org-space-users`, `users`, `apps`, `roles`, `role-collections`,
  `space-destinations`, `subaccount-destinations`, `describe-subaccount`,
  `count-lines`, `service-credentials` (this one outputs raw secrets — see
  "Handling commands that output secrets" below before using it),
  `service-instances`, `service-keys` (both are read-only by default, but
  `--full` fetches and prints live credentials on each — see "Handling
  commands that output secrets" below before using `--full`).
- **Write** (change BTP state — see guardrails below): `create-users`,
  `delete-users`, `create-org-space-users`, `delete-org-space-users`,
  `create-subaccount-destinations`, `update-subaccount-destinations`,
  `delete-subaccount-destinations`, `create-ups`, `create-service-keys`
  (this one also outputs raw secrets — see "Handling commands that output
  secrets" below before using it), `delete-service-instances` (irreversibly
  deletes service instances and all their bindings/keys — see guardrail #11
  below), `delete-service-keys` (irreversibly deletes service keys — see
  guardrail #12 below), `start-apps`, `stop-apps`, `restart-apps`,
  `restage-apps` (app lifecycle actions, scoped by name pattern — see
  guardrail #13 below), `create-roles`, `update-roles`, `delete-roles`,
  `create-role-collections`, `update-role-collections`,
  `delete-role-collections` (XSUAA role/role-collection management from an
  exported file or by name pattern — see guardrail #14 below),
  `update-apps-env` (creates/updates apps' user-provided environment
  variables from a `bo apps --include-env` export — see guardrail #15
  below).
- Common flags across most commands: `--regions`, `--org`/`--orgs`/
  `--excludeOrgs` (falls back to the `bo orgs` session default scope if
  omitted), `--format toon|json|csv` (plus `uar.csv` on `users` and
  `org-space-users`), `--output`/`-o <file>`, and `--include <kw1,kw2,...>` /
  `--exclude <kw1,kw2,...>` — comma-separated, case-insensitive, OR-matched
  substrings (the destination commands also accept a glob per keyword, e.g.
  `API*PP`).
- **Global `--color auto|always|never` flag (default `auto`).** In `auto`
  mode, `toon`/`csv` output on `spaces`, `apps`, `service-instances`,
  `service-keys`, `role-collections`, `users`, `org-users`, and
  `org-space-users` can come back with embedded ANSI color codes — not just
  in a real terminal, but also whenever an SSH or VS Code session signal is
  present, even if you redirect or pipe the output. If you're going to parse
  that output programmatically (rather than just showing it to the user),
  always pass `--color never` to guarantee plain text.

## Guardrails for write operations

Read all of these before running any command from the "Write" list above.

1. **Preview before writing.** Run the matching read command with the same
   `--org`/`--orgs` and `--include`/`--exclude` filters first, and account for
   exactly which rows/orgs will be affected before running the write command.
2. **Never add `-y`/`--yes` on your own initiative.** `create-users`,
   `delete-users`, `create-org-space-users`, `delete-org-space-users`,
   `create-service-keys`, `delete-service-instances`, `create-roles`,
   `update-roles`, `delete-roles`, `create-role-collections`,
   `update-role-collections`, `delete-role-collections`, and
   `update-apps-env` print a full TOON preview and ask `Proceed? [y/N]`
   (or the `[u/c/a]` variant, for
   `create-service-keys`) before touching anything. Get that preview in
   front of the user (or reproduce it and get explicit approval) before
   adding `-y` — don't skip straight to `-y` because it's "obviously" what
   the user asked for.
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
9. **`create-ups` broadcasts into every target org/space, and reads secrets
   as input.** It takes a `service-credentials.json`-shaped file (see
   "Handling commands that output secrets" below — the input is just as
   sensitive as that command's output) and creates every entry in it in
   *every* org currently in scope — not just the org(s) the file's services
   came from. With `--space`, every entry broadcasts into that one named
   space in each target org; without it, each entry is instead targeted at
   the space named in **its own** `space` field in the file — a typo there
   means that entry is silently skipped (not an error), so tell the user to
   confirm each entry's `space` field is the intended *target* space before
   running without `--space`. `--tags` overrides every entry's own `tags`
   with a fixed comma-separated list. It has two independent confirmations:
   the create/update preview (declining aborts the whole run), and a
   separate one only for services that already exist and would be
   overwritten (declining *that* one just skips the overwrites and still
   creates the brand-new ones — it does not abort). `-y` skips both, so
   only pass it once a human has seen the preview table and the org scope
   is deliberate.
10. **`create-service-keys` creates/deletes live service bindings and
    outputs their secrets.** Without `-y`, it shows a TOON preview and — if
    any matched instance already has a key by that name — asks `[u]pdate`
    (deletes and recreates the key), `[c]reate` new only (leaves the
    conflicting ones untouched), or `[a]bort`. **`-y` without `--create-new-keys-only`
    resolves every conflict as `update` automatically**, which deletes and
    replaces an existing key — don't pass `-y` alone when a conflict is
    possible unless the user has confirmed that recreate-on-conflict is
    fine; prefer `--create-new-keys-only` (or a human-approved `-y`) when
    you only want brand-new keys and must never touch an existing one.
    Even with `[c]reate`/`--create-new-keys-only`, the output still
    includes the untouched instances' existing credentials (fetched, not
    regenerated) alongside the newly-created ones — so it carries just as
    much secret material as the `update` path, only without having
    deleted/recreated anything. See "Handling commands that output secrets"
    below for its output.
11. **`delete-service-instances` is the most destructive write command —
    there's no undo, and it also deletes every binding/key on each matched
    instance first, not just the instance.** Its `<service_instance_name_patterns>`
    argument matches by glob/substring across *every* org/space in scope,
    the same as `--service-instances` on `create-service-keys` — a loose
    pattern like `*-db` can match far more than intended. Always run with
    no `--yes` first (or `< /dev/null` to force a dry-run abort) and show
    the user the exact TOON preview — which names every matched instance
    and how many existing bindings/keys would be deleted along with it —
    before ever adding `-y`. Never guess at a pattern on the user's behalf;
    confirm the exact pattern they mean, especially a broad one like `*`.
12. **`delete-service-keys` deletes service keys (not whole instances) —
    still no undo.** `<service_key_name_patterns>` matches by glob/substring
    across every org/space in scope, `!`-negatable the same way as
    `delete-service-instances`. Show the TOON preview (org -> space ->
    service_instance -> service_keys) and get approval before adding `-y`.
13. **`start-apps`/`stop-apps`/`restart-apps`/`restage-apps` act on every app
    matching a name pattern, across the whole org/space scope.** All four
    show a TOON preview (including each app's current state) and require
    `[y/N]` before `-y`. A few extra things to flag to the user before
    running these:
    - `restart-apps`/`restage-apps` cause a brief **downtime** window unless
      `--strategy rolling` or `--strategy canary` is given, in which case the
      app is redeployed with zero downtime via a CF v3 Deployment instead.
      `canary` is experimental on CF's side and needs a fairly recent CF API
      version — confirm with the user before relying on it in a scripted
      rollout.
    - `restage-apps` rebuilds a fresh droplet from the app's current package
      before redeploying it (regardless of `--strategy`), so it takes
      meaningfully longer per app than `start-apps`/`stop-apps`/
      `restart-apps` and can fail outright if the current package fails to
      stage — treat a staging failure as a real error to report, not
      something to retry blindly.
    - Loose patterns (e.g. `*`) can match far more apps than intended, same
      caution as `delete-service-instances`/`delete-service-keys`.
    - All four also accept `--apps <file>` to target exact apps by GUID from
      a CSV (same columns as `bo apps --format csv`: `region_id`, `org_name`,
      `space_id`, `space_name`, `app_id`, `app_name`), bypassing all
      org/space scope flags (`--org`/`--orgs`/`--excludeOrgs`/`--spaces`/
      `--space`/`--regions`) entirely — useful for "restart/restage only the
      apps that were already running", e.g.
      `bo apps --include STARTED --format csv -o started.csv` followed by
      `bo restage-apps --apps started.csv -y`. The name-pattern argument
      becomes optional in this mode (default `*`). **If a default org scope
      *is* currently selected via `bo orgs`, it still always wins**: the CSV
      is filtered down to just the rows whose org is in that selection (no
      prompt, no `--orgs` override — only `bo orgs` matters here). Only with
      no `bo orgs` selection is every row in the CSV used as-is. Check
      whether the user has a `bo orgs` selection active before assuming
      `-y` will run the CSV unmodified.
14. **`create-roles`/`update-roles`/`delete-roles` and
    `create-role-collections`/`update-role-collections`/
    `delete-role-collections` take a `--roles`/`--role-collections <file>` —
    a JSON/TOON document in the exact `regions -> orgs -> roles` /
    `regions -> orgs -> roleCollections` shape that `bo roles` /
    `bo role-collections` write (get one via `bo roles --format json -o
    roles.json`, edit it, then pass it back).** Always get that export, show
    the user the exact entries that will be created/updated/deleted (its
    `regions`/`orgs`/`role`(`Collection`) breakdown), and have them confirm
    before running the write command — don't hand-author one of these files
    from memory. A few specifics:
    - `create-*` silently skips (no error) a role/role collection that
      already exists (HTTP 409) rather than failing the whole run.
    - `update-*` warns and skips (doesn't fail the run) a role/role
      collection that doesn't exist (HTTP 404) — tell the user to run the
      matching `create-*` first if that happens unexpectedly.
    - `create-roles`/`update-roles`/`create-role-collections`/
      `update-role-collections` all take an optional leading
      `[name_pattern]` argument (same glob/substring/`!`-negation syntax as
      `delete-service-keys`) that narrows which entries of the
      `--roles`/`--role-collections` file are acted on — omitted or `*`
      means every entry in the file. Confirm with the user whether they
      want the whole file applied or only a subset before adding a pattern
      (or before letting it default to "everything").
    - **`update-role-collections` fully replaces each collection's
      `roleReferences`** with exactly what the file specifies — a role
      reference that's on the server but missing from the file is removed,
      not left alone. Confirm the file's `roleReferences` list for each
      collection is complete, not just the roles the user wants to add.
    - `delete-roles`/`delete-role-collections` work two ways: a required
      `<name_pattern>` that discovers roles/collections across the normal
      org/space scope (same glob/substring/`!`-negation as
      `delete-service-keys`), or `--roles`/`--role-collections <file>` which
      instead targets *exactly* the orgs/entries the file lists — ignoring
      `--org`/`--excludeOrgs`/`--regions` entirely — with the name pattern
      becoming optional (default `*`) in that mode. Confirm with the user
      which of the two modes they mean; a bare pattern with no
      `--roles`/`--role-collections` can match far more than intended, the
      same caution as `delete-service-keys`.
    - **All six (the three roles commands and the three role-collections
      commands) always target whatever org(s) are currently selected via
      `bo orgs`, if any are selected — there's no prompt, no `--orgs`
      override, and no comparison against the file's own orgs.** With a
      `bo orgs` selection active, every entry in the file is **retargeted**
      onto every org in that selection, discarding the file's own org
      assignment entirely. Only when *no* default org scope is selected is
      each entry's own org (exactly as specified in the file) used. Unlike
      the `--apps` case above, this is a genuine *relocation*, not a
      filter: with a `bo orgs` selection active, these commands create (or
      update/delete) **every** role/role-collection in the file within
      whatever org is currently selected, even if the file was exported
      from a completely different subaccount — that's the intended way to
      replicate a role setup across subaccounts, but it also means a stale
      `bo orgs` selection silently sends the file's definitions to the
      *wrong* subaccount. **Before running any of these six commands,
      always check (or ask) what the user's current `bo orgs` selection
      is** and confirm that's the subaccount they actually mean to target —
      if it's wrong, the fix is `bo orgs` to pick the right one, or `bo
      orgs` then 'c' to clear the selection so the file's own orgs are used
      instead (the confirmation preview, shown unless `-y`, states this).
    - When retargeting across subaccounts (or whenever the exported value
      doesn't match what's actually registered), each role's
      `roleTemplateAppId` is automatically re-resolved against the target
      org's own role templates (role-collections: against its existing
      roles instead, since a reference names a specific role) by matching
      the app name — everything before `!` — and `roleTemplateName` (and,
      for role-collections, `role_name` too). This is what makes
      cross-subaccount replication actually work, since the suffix after
      `!` is specific to the subaccount the role was exported from.
    - These commands exist because they modify who has administrative
      access to a subaccount — treat every call as security-sensitive, not
      just "another bulk write."
15. **`update-apps-env` reads a `bo apps --include-env` export — which is
    just as sensitive as `service-credentials`' output — and writes app
    environment variables (which commonly hold secrets) into whatever
    org(s) end up targeted.** Get the export (`bo apps --org <org-guid>
    --include-env --format json -o apps-env.json`) and show the user its
    `app_env` entries (mask or redact actual secret values when relaying
    them back, same as "Handling commands that output secrets" below)
    before running the write command — don't hand-author one of these files
    from memory. A few specifics:
    - Like the six commands in guardrail #14, **it always targets whatever
      org(s) are currently selected via `bo orgs`, if any are selected** —
      retargeting every app in the file onto that selection and discarding
      the file's own org entirely; only with no `bo orgs` selection is each
      app's own org (as recorded in the file) used. Check the current `bo
      orgs` selection before running this, the same as for guardrail #14.
    - Unlike those six, there's no GUID-based identity to carry over even
      within the *same* org: the target space and app are always re-located
      by exact name (case-insensitive) in whatever org ends up targeted,
      never by the file's own `app_id`/`space_id`/`org_id`. A space or app
      that doesn't exist by that name in the target org is skipped with a
      warning, not an error — mention this if a run comes back with fewer
      updates than apps in the file.
    - It's a key-level *merge*, not a replace: existing environment
      variables on the target app that aren't in the file's `app_env` are
      left alone, unlike `update-role-collections`' full-replace semantics
      for `roleReferences`.
    - `DEPLOY_ATTRIBUTES`, if present in the file, is always skipped and
      never written to the target app — it's deploy-tooling noise, not a
      user-set value. Every other variable, secret-named or not, is sent to
      the target app exactly as written in the file: nothing is masked or
      dropped from the *actual update* — masking only ever applies to the
      confirmation preview's display, next.
    - The confirmation preview masks any variable whose name ends in `KEY`,
      `SECRET`, `PASSWORD`, or `PASSWD` as `********` for display only — but
      every other variable's value is shown in plain text, so treat the full
      preview output the same as any other secret-bearing command output
      (don't paste it verbatim into a transcript the user didn't ask to
      persist).
    - Updating environment variables doesn't restart the app — a human (or
      a follow-up `bo restart-apps`/`bo restage-apps`, itself gated by
      guardrail #13) still needs to apply it.

## Handling commands that output secrets

`service-credentials` fetches each matching app's live `VCAP_SERVICES`,
`apps --include-env` fetches each matching app's own user-provided
environment variables (as `app_env`) — including secret/password/key-named
ones in full, unmasked, since that's exactly what `update-apps-env` needs
to recreate them on a target app (the only variable it ever excludes is
`DEPLOY_ATTRIBUTES`, deploy-tooling noise rather than a real secret) —
`create-service-keys` creates service keys and fetches their credentials,
and `service-instances --full` /
`service-keys --full` fetch and include each matched (user-provided
instance's or key's) existing credentials — passwords, client secrets,
certificates, the lot — printing them as plain JSON/TOON/CSV.
`service-credentials`, `apps --include-env`, and `service-instances`/
`service-keys` even with `--full`, are read-only (nothing in BTP changes),
while `create-service-keys` is a write command that also happens to output
secrets; the same `service-credentials.json` shape is also what `create-ups`
later reads as input, and `apps --include-env`'s output is what
`update-apps-env` (see guardrail #15 above) later reads as input. Treat all
of this as sensitive:

- Prefer `--output <file>` over letting it print to a transcript or chat the
  user didn't ask to persist, and tell the user where the file landed.
- Never echo, summarize with values intact, or paste raw credentials back
  into the conversation — describe *what* was found (e.g. "3 hana bindings
  across 2 spaces") rather than the secret values themselves, unless the user
  explicitly asks for a specific value.
- It's still written in full to `~/.bo/log/bo_YYYY-MM-DD.log` like every other
  command's output (see "Everything is logged" above) — mention this to the
  user so they know that log file now contains secrets too, don't just stay
  silent about it.
- Narrow with `--services`/`--spaces`/`--apps` (on `service-credentials`),
  `--spaces`/`--service-instances` (on `create-service-keys` and
  `service-instances`), or `--spaces`/`--service-instances`/`--service-keys`
  (on `service-keys`) to the minimum needed, and only pass `--full` on
  `service-instances`/`service-keys` (or `--include-env` on `apps`) when you
  actually need the credentials — omit it for a plain metadata inventory.

## Example workflows

### Inventory / audit (read-only, safe to run freely)

```bash
# Dump the accessible org list non-interactively and save it for reuse
bo orgs --include prod --format csv -o prod-orgs.csv

# User Access Review export across those orgs
bo users --orgs prod-orgs.csv --format uar.csv -o uar-export.csv

# Destinations matching a keyword or glob, across the same scope
bo subaccount-destinations --orgs prod-orgs.csv --include "API*PP" --format csv

# Extract hana credentials for *-srv apps in the prod space, to a file —
# see "Handling service-credentials output" above before running this
bo service-credentials --orgs prod-orgs.csv --spaces prod --apps "*-srv" --services hana -o hana-creds.json
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

### Broadcast credentials to other orgs as user-provided services (guarded, sensitive input)

```bash
# Extract once from a source org (see secrets handling above)
bo service-credentials --org <source-org-guid> --spaces prod --services hana -o hana-creds.json

# Preview the broadcast — this is a dry look at the plan, not a confirmed run
bo create-ups hana-creds.json --orgs target-orgs.csv --space prod --postfix -ups < /dev/null

# Only after the preview above has been shown to and approved by the user:
bo create-ups hana-creds.json --orgs target-orgs.csv --space prod --postfix -ups -y
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
