# Comparing apps, destinations, and HTML5 apps across subaccounts with `bo`

A common audit question is "what's different between these two subaccounts?" — e.g. after a clone, or to catch drift between a pre-prod and prod landscape. The approach is the same for apps, destinations, and HTML5 apps: export both subaccounts with the same comparable columns, in the same sorted order, then compare the two files. Do all three exports for one subaccount in a single pass before switching to the other, rather than switching back and forth once per topic.

## Prerequisites

1. `bo` installed and on `PATH` (`bo version` to check) — see the main [README](../README.md#installation).
2. The logged-in user needs the **Space Developer** role in **every space** of **both** the source and target subaccounts — `bo apps`/`bo destinations`/`bo html5-apps` only return results for spaces the user can actually see, so a space they're missing Space Developer on silently looks the same as a space with nothing in it, which can show up as a false difference. This is also what `html5-apps --no-runtime-option=create`/`--no-key-option=create` (see below) need to create a service instance/key in a space.

## Steps

1. **Log in to both regions (once):**

   ```bash
   bo login --regions=us10,us20 --sso
   ```

2. **Select the first subaccount:**

   ```bash
   bo orgs
   #   > 01 [x] us10/us10-pre (a1b2c3d4-...)
   #     02 [ ] us20/us20-pre (e5f6g7h8-...)
   #   Default org scope set to 1 org(s) for this login session.
   ```

3. **Extract apps, destinations, and HTML5 apps for this subaccount, in one pass:**

   ```bash
   bo apps --include-env --output=us10-pre-apps-full.csv
   bo destinations --output=us10-pre-dest-full.csv
   bo html5-apps --no-runtime-option=create --no-key-option=create --output=us10-pre-html5-full.csv
   ```

   `.csv` is auto-detected from each filename. See "Apps"/"Destinations"/"HTML5 apps" below for what each command needs and what to watch out for.

4. **(Optional) open any of the three `...-full.csv` files** and fix up anything that would otherwise show as a false difference or impact sorting, eg: an irregularly-named space, a one-off app/destination/HTML5 app you know isn't meant to exist on the other side, etc. For a quick text substitution across a whole file, `sed` works just as well as opening it in an editor:

   ```bash
   sed -i 's/old_text/new_text/g' us10-pre-apps-full.csv
   ```

5. **Narrow each down to just the comparable columns:**

   ```bash
   bo sort --fields=space_name,app_mta_id,app_name,app_env us10-pre-apps-full.csv --output=us10-pre-apps-sorted.csv
   bo sort --fields=space_name,instance_name,Name,URL,Type,Authentication,User,Password us10-pre-dest-full.csv --output=us10-pre-dest-sorted.csv
   bo sort --fields=space_name,instance_name,sap_cloud_service,application_id,application_name,application_version us10-pre-html5-full.csv --output=us10-pre-html5-sorted.csv
   ```

   `--by` isn't needed here: with `--fields` given and no `--by`, [`bo sort`](../README.md#sort) sorts by `--fields`' own fields, in that order — exactly the columns you just narrowed down to.

6. **Select the second subaccount and repeat steps 3–5:**

   ```bash
   bo orgs
   #   > 01 [ ] us10/us10-pre (a1b2c3d4-...)
   #     02 [x] us20/us20-pre (e5f6g7h8-...)
   #   Default org scope set to 1 org(s) for this login session.

   bo apps --include-env --output=us20-pre-apps-full.csv
   bo destinations --output=us20-pre-dest-full.csv
   bo html5-apps --no-runtime-option=create --no-key-option=create --output=us20-pre-html5-full.csv
   # (optional edits, same as step 4)
   bo sort --fields=space_name,app_mta_id,app_name,app_env us20-pre-apps-full.csv --output=us20-pre-apps-sorted.csv
   bo sort --fields=space_name,instance_name,Name,URL,Type,Authentication,User,Password us20-pre-dest-full.csv --output=us20-pre-dest-sorted.csv
   bo sort --fields=space_name,instance_name,sap_cloud_service,application_id,application_name,application_version us20-pre-html5-full.csv --output=us20-pre-html5-sorted.csv
   ```

7. **Compare each pair of sorted files** — in VS Code (open both, right-click one → *Select for Compare*, right-click the other → *Compare with Selected*) or SAP Business Application Studio (same VS Code-based diff). Anything shown is either missing from one side or genuinely different between them.

## Apps

`--include-env` adds the `app_env` column (each app's own user-provided environment variables, JSON-encoded) and requires at least Space Developer access to each app's space — this is sensitive output, see the README's note on `--include-env` before sharing this file.

Sample `...-apps-sorted.csv`:

```csv
space_name,app_mta_id,app_name,app_env
dev,myapp,myapp-srv,"{""PROFILE"":""dev""}"
prod,myapp,myapp-srv,"{""PROFILE"":""prod""}"
prod,myapp,myapp-ui,"{}"
```

## Destinations

⚠️ **`bo destinations` always includes every destination property unredacted by default — including live secrets like `Password` and `ClientSecret`.** There's no `--full`/safe-mode flag to opt out of this (see the README). Treat every file in this section as a secret: don't commit it, don't paste its contents into a chat/ticket, and delete it once you're done comparing.

`space_name`/`instance_name` are empty for a subaccount-level destination (only instance-level ones have a space/instance) — that's expected, not a gap. Destination property names (`Name`, `URL`, `Type`, `Authentication`, `User`, `Password`) are matched exactly as the destination service itself stores them, case-sensitive.

Sample `...-dest-sorted.csv` (passwords below are fictional placeholders, not real secrets):

```csv
space_name,instance_name,Name,URL,Type,Authentication,User,Password
,,API_HUB,https://api.example.com,HTTP,BasicAuthentication,api-user,N0tAReal-P@ss1
prod,my-dest-svc,S4_BACKEND,https://s4.example.com,HTTP,OAuth2SAMLBearerAssertion,,
```

## HTML5 apps

`--no-runtime-option=create --no-key-option=create` automatically create a temporary `html5-apps-repo`/`app-runtime` service instance and/or service key in any space that doesn't already have one, so no space gets silently skipped for lack of one — see the README's `html5-apps` section for the full set of options (`skip`/`create`/`create-delete`/`abort`) and what each does.

**The count here can legitimately differ from the BTP cockpit's "HTML5 Applications" list.** The cockpit's list often includes duplicate-looking entries caused by a destination with an incorrectly-set `sap.cloud.service` property pointing at a host that isn't actually an HTML5 repo, or entries that are really SAP Build Process Automation ("SBPA") inbox workflow apps rather than genuine HTML5 apps. If the numbers don't match and you need the *actual* deployed-app list to compare against, check **SAP Build Work Zone → Content Manager → Content Explorer** instead of the cockpit's HTML5 Applications list.

Sample `...-html5-sorted.csv`:

```csv
space_name,instance_name,sap_cloud_service,application_id,application_name,application_version
prod,html5-apps-repo-host,com.acme.myapp,com.acme.myapp,My App,1.2.3
prod,html5-apps-repo-host,com.acme.myapp.admin,com.acme.myapp.admin,My App Admin,1.0.1
```

## Command reference

- [`bo login`](../README.md#login), [`bo orgs`](../README.md#orgs) — authenticate and pick the subaccount in scope for the rest of the session.
- [`bo apps`](../README.md#apps) — list apps (`--include-env` for their environment variables).
- [`bo destinations`](../README.md#destinations) — list subaccount- and instance-level destinations together (sensitive — see above).
- [`bo html5-apps`](../README.md#html5-apps) — list HTML5 Application Repository app metadata.
- [`bo sort`](../README.md#sort) — narrow (`--fields`) and sort a CSV/JSON file already on disk, in place or to a new file — sorts by `--fields`' own fields when `--by` is omitted.
