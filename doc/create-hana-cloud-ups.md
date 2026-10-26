# Copying HANA Cloud credentials between subaccounts with `bo`

This page walks through creating dedicated service keys for HANA Cloud (or any `-db`/`-hdi`-style) service instances in a source subaccount, and recreating them as user-provided services in one or more target subaccounts, using the `bo` CLI's `create-service-keys` and `create-ups` commands. No manual copy/paste of hostnames, passwords, or certificates required.

## When to use this

Typical cases:

- An app in subaccount B needs to reach the same HANA Cloud instance that's already used by an app in subaccount A, without creating a second database or exporting a service key by hand.
- Standing up a staging/DR subaccount that should point at the same (or a cloned) HANA instance as prod.
- Migrating an app to a new subaccount without losing its database connection.
- Fanning the same database instance's credentials out into several target subaccounts/spaces at once.

The target app binds to the **user-provided service** created in its own subaccount, which carries the exact same `credentials` (host, port, JDBC URL, schema, user/password, certificate, ...) as a real binding to the source HANA Cloud instance — it works the same as binding to a real HANA Cloud service instance, from the app's point of view.

## Prerequisites

- `bo` installed and on `PATH` — see the main [README](../README.md#installation). Run `bo version` to check.
- Cloud Foundry access to:
  - the **source** org/space(s), where the HANA Cloud (or other `-db`/`-hdi`) service instances already exist (Space Developer or above, to create a service key on them), and
  - the **target** org/space(s), where the user-provided services will be created.
- Know (or be able to look up with `bo spaces`) the exact **target space name(s)** the user-provided services should land in.

⚠️ **Both `hana-creds.json` and the key it holds contain live secrets** — the HANA Cloud password, JDBC credentials, and certificate, in plain JSON. Treat it like any other secret: don't commit it, don't paste it into a chat/ticket, and delete it once you're done (see [Cleanup](#cleanup) at the end).

## Step 1 — log in to the region(s) involved

```bash
bo login --regions='us10,us20'
```

Log in to every region that has either the source or the target subaccount. If source and target are both in the same region, one region is enough — pass a comma-separated list when they're not.

## Step 2 — select the source org

```bash
bo orgs
#   Select orgs — up/down move, digits jump, space toggle, a=all, c=clear
#   Enter to confirm (confirms the highlighted org if none are checked):
#   > 01 [x] us10/my-source-subaccount (a1b2c3d4-...)
#     02 [ ] us10/my-target-subaccount (e5f6g7h8-...)
#
#   Default org scope set to 1 org(s) for this login session.
```

This sets the default org scope that `create-service-keys` (step 3) will use, since neither `--org` nor `--orgs` is passed to it. If a previous selection is still checked from an earlier session, press `c` to clear it first.

## Step 3 — create dedicated service keys on the source instances

```bash
bo create-service-keys btp-ups --service-instances '*-db,*-hdi' --output hana-creds.json
```

- `btp-ups` is the **service key name** — every matched instance gets a key with this exact name.
- `--service-instances '*-db,*-hdi'` matches managed service instances by name: each comma-separated entry is a glob (via Go's `filepath.Match`) because it contains `*`, so this matches any instance whose name ends in `-db` or `-hdi` (HANA HDI containers and HANA Cloud/other database instances both tend to follow one of these naming conventions). Narrow or widen this pattern to match your own naming.
- `--output hana-creds.json` writes the created keys' details (including credentials) as a JSON array to that file instead of stdout.
- `--org`/`--orgs` aren't passed, so this runs against the org scope selected in step 2; `--spaces` also isn't passed, so every accessible space in that scope is searched.

Without `-y`, you'll see a TOON preview of every matched instance and the key to be created, then a `Proceed? [y/N]` prompt (or, if a key named `btp-ups` already exists on an instance — e.g. you're re-running this later — a `[u]pdate / [c]reate / [a]bort` prompt instead; `u` deletes and recreates it, `c` leaves it alone). If you answer `c` (or pass `--create-new-keys-only`), the instances that already had a `btp-ups` key aren't touched, but `hana-creds.json` still ends up with an entry for them too — their existing key's credentials are fetched (not regenerated) and included alongside the newly-created ones, so the file remains a complete set for step 4/6 regardless of which instances were brand new.

**Why a dedicated service key instead of reading an app's bound credentials:** a service key lives on the service instance itself, independent of any app binding. Reusing the same key name (`btp-ups`) and periodically re-running this command keeps `hana-creds.json` in sync with a single, stable credential that isn't tied to — and won't be invalidated by — any particular app's lifecycle. Redeploying, restaging, or even deleting the apps that happen to also be bound to the same instance has no effect on this key, unlike extracting credentials from `VCAP_SERVICES` of one specific app binding (see [`bo service-credentials`](../README.md#service-credentials), which is the alternative if you specifically want an app's own view of its bound services instead of a dedicated key).

`hana-creds.json` now looks like:

```json
[
  {
    "org": "my-source-subaccount",
    "space": "dev",
    "instance_name": "FI-EMPMaintain-hdi",
    "instance_guid": "847d683e-e6d5-4d61-8e69-23242ea65404",
    "label": "hana",
    "plan": "hdi-shared",
    "tags": ["hana"],
    "binding_guid": "9c2e1a3b-...",
    "binding_name": "btp-ups",
    "credentials": {
      "host": "...hanacloud.ondemand.com",
      "port": "443",
      "driver": "com.sap.db.jdbc.Driver",
      "url": "jdbc:sap://...",
      "schema": "...",
      "certificate": "-----BEGIN CERTIFICATE-----...",
      "user": "...",
      "password": "..."
    }
  }
]
```

## Step 4 — review/update the target space names in `hana-creds.json`

Each entry's `org`/`space` fields currently record **where the service key was just created** — the source subaccount and space. `create-ups` (step 6) is run without `--space`, so it uses each entry's own `space` field to decide **where in the target org** to create the user-provided service. That means you must edit `space` (and, if entries should land in different target spaces than they came from, `org` is informational only and can be left as-is — only `space` drives targeting) in `hana-creds.json` to the correct **target** space name(s) before running step 6.

Open the file and, for every entry, set `space` to the exact (case-insensitive) name of the space in the target org that should receive this service — e.g. change `"space": "dev"` to `"space": "prod"` if that's where it belongs in the target subaccount. If a target org doesn't have a space with that name, that entry is silently skipped in step 6 rather than erroring, so a typo here means it quietly goes nowhere — double-check the names, e.g. with `bo spaces` against the target org.

## Step 5 — switch to the target org

```bash
bo orgs
#   > 01 [ ] us10/my-source-subaccount (a1b2c3d4-...)
#     02 [x] us10/my-target-subaccount (e5f6g7h8-...)
#
#   Default org scope set to 1 org(s) for this login session.
```

Clear the source org's checkbox (`c`) before checking the target, so step 6 doesn't also create services back in the source org. You can select more than one target org here — `create-ups` will create each entry in every target org that has a matching space.

## Step 6 — create the user-provided services in the target org

```bash
bo create-ups hana-creds.json --postfix '-ups' --tags 'hana,database,relational'
```

- No `--space` — each entry is targeted at the space you set in its `space` field in step 4.
- `--postfix '-ups'` appends `-ups` to each service's name (e.g. `FI-EMPMaintain-hdi` → `FI-EMPMaintain-hdi-ups`), so it can't collide with a real managed instance of the same base name in the target space.
- `--tags 'hana,database,relational'` sets these tags on every created/updated service, overriding whatever (if any) `tags` came through from the source instance — useful since a service key's own tags are often empty or unhelpful for identifying the resulting user-provided service later.

`create-ups` always shows a preview before touching anything:

```
Services to create/update:
ORG                    SPACE  SERVICE-NAME            LABEL  PLAN
my-target-subaccount   prod   FI-EMPMaintain-hdi-ups  hana   hdi-shared

1 to create, 0 to update (already exist).

Proceed? [y/N] y
  + created: my-target-subaccount / prod / FI-EMPMaintain-hdi-ups

Done: 1 created, 0 updated, 0 failed.
```

Answer `y` to proceed. If a service with the same (post-`--postfix`) name already exists in its target space, you'll get a **second**, separate prompt asking whether to overwrite it — declining that one skips the overwrite but still creates anything that's brand new (it doesn't abort the whole run). Add `-y` to skip both prompts for scripts/CI.

```bash
# Non-interactive (for scripts/CI)
bo create-ups hana-creds.json --postfix '-ups' --tags 'hana,database,relational' -y
```

## Step 7 — deploy apps bound to the new service

Point your target app's manifest/binding at the `-ups`-suffixed service name instead of a real managed instance, e.g. for `FI-EMPMaintain-hdi-ups`:

```yaml
---
applications:
  - name: my-app
    services:
      - FI-EMPMaintain-hdi-ups
```

or bind it directly and restage:

```bash
cf bind-service my-app FI-EMPMaintain-hdi-ups
cf restage my-app
```

The app's `VCAP_SERVICES` will now contain the same HANA credentials as the source instance's dedicated `btp-ups` service key.

## Verify

```bash
cf service FI-EMPMaintain-hdi-ups   # in the target space
```

should show a `user-provided` service. If you rebind or restage the app, `VCAP_SERVICES` is refreshed from this same user-provided service, which `create-ups` (re-run with the same arguments) can update in place whenever the underlying credentials rotate — it doesn't need to be recreated.

## Cleanup

Once the user-provided service is created (and verified), delete the exported credentials file — it's no longer needed and it's still holding live secrets:

```bash
rm hana-creds.json
```

If you wrote it somewhere logged or synced (shared drive, CI artifact, etc.), remove it from there too. `bo`'s own `~/.bo/log/bo_YYYY-MM-DD.log` also contains a full copy of both commands' output (including the credentials from `create-service-keys`) — clear it with `bo clear-logs` if that log shouldn't retain the secret.

If the source service key (`btp-ups`) is no longer needed either (e.g. this was a one-off migration, not an ongoing sync), delete it from the source instance via `cf delete-service-key <instance-name> btp-ups` — `bo` doesn't have a dedicated delete-key command.

## Troubleshooting

| Problem | Likely cause / fix |
|---|---|
| `create-service-keys` output has no entries | `--service-instances '*-db,*-hdi'` didn't match any managed instance name in scope — rerun with a wider pattern (or check `bo apps`/`cf services` for the actual instance names) to confirm the naming convention. |
| `create-service-keys` asks `[u/c/a]` unexpectedly | A key named `btp-ups` already exists on that instance (e.g. from a previous run) — `u` updates it in place (delete + recreate), `c` leaves the existing key alone and only creates on instances that don't have one yet (its credentials are still included in `hana-creds.json`, just unchanged), `a` aborts. |
| `create-ups` created (or updated) services in the wrong subaccount | The `bo orgs` picker still had the source subaccount checked from step 2 — press `c` to clear before selecting the target in step 5, or pass `--org <target-org-guid>` explicitly instead of relying on the picker. |
| `create-ups` entry doesn't show up anywhere, no error | Its `space` field (from step 4) doesn't match any accessible space name in the target org(s) in scope — this is skipped silently, not an error. Double-check the name (e.g. with `bo spaces`) and re-edit `hana-creds.json`. |
| Binding the app still shows old credentials after `cf bind-service` | Run `cf restage` (or `cf restart`) — `VCAP_SERVICES` is only re-injected on (re)start. |

## Command reference

- [`bo login`](../README.md#login), [`bo orgs`](../README.md#orgs) — authenticate and pick the org(s) in scope for the rest of this session.
- [`bo create-service-keys`](../README.md#create-service-keys) — create (or update) a dedicated, named service key on matching service instances, across orgs/spaces, and export the credentials as JSON.
- [`bo create-ups`](../README.md#create-ups) — create/update user-provided services from that JSON array in target space(s).
- [`bo spaces`](../README.md#spaces) — list spaces in the currently selected orgs, handy for confirming exact space names.
