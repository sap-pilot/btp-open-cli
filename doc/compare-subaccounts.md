# Comparing apps, destinations, HTML5 apps, roles, role collections, and users across subaccounts with `bo`

A common audit question is "what's different between these two subaccounts?" — e.g. after a clone, or to catch drift between a pre-prod and prod landscape. The approach is the same for every artifact type covered here (apps, destinations, HTML5 apps, roles, role collections, and users): export both subaccounts with the same comparable columns, in the same sorted order, then compare the resulting files — either with a plain-text diff/editor, or with [`bo compare`](../README.md#compare)'s own HTML report (step 8 below). Do all exports for one subaccount in a single pass before switching to the other, rather than switching back and forth once per topic.

## Prerequisites

1. `bo` installed and on `PATH` (`bo version` to check) — see the main [README](../README.md#installation).
2. The logged-in user needs the **Space Developer** role in **every space** of **both** the source and target subaccounts — `bo apps`/`bo destinations`/`bo html5-apps` only return results for spaces the user can actually see, so a space they're missing Space Developer on silently looks the same as a space with nothing in it, which can show up as a false difference. This is also what `html5-apps --no-runtime-option=create`/`--no-key-option=create` (see below) need to create a service instance/key in a space.
3. `bo roles`/`bo role-collections`/`bo users` each need an accessible `xsuaa`/`apiaccess` service instance **with a service key** in some space of the subaccount (any one space is enough, unlike the per-space requirement above) — without one, they prompt interactively for CF CLI instructions to create it (or skip with `--no-prompt`). See the README's [`users`](../README.md#users) section for the exact mechanism.

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

3. **Extract apps, destinations, HTML5 apps, roles, role collections, and users for this subaccount, in one pass:**

   > CAUTION: if destination service key or html5-app-runtime instance/service-key is missing then below commands will create those missing keys or service instance for the extraction to work (see below destinatons/apps section for more detail); Threfore - recommend to test in QA/PREPROD subaccounts first prior to running these command in PRODUCTION subaccounts. 

   ```bash
   bo apps --include-env --include-labels --output=us10-pre-apps-full.csv
   bo destinations --no-key-option=create --output=us10-pre-dest-full.json
   bo html5-apps --no-runtime-option=create --no-key-option=create --output=us10-pre-html5-full.csv
   bo roles --output=us10-pre-roles-full.csv
   bo role-collections --output=us10-pre-role-collections-full.csv
   bo users --output=us10-pre-users-full.csv
   ```

   > `.csv`/`.json` are auto-detected from each filename. See "Apps"/"Destinations"/"HTML5 apps"/"Roles"/"Role Collections"/"Users" below for what each command needs and what to watch out for.

4. **(Optional) open any of the `...-full` files** and fix up anything that would otherwise show as a false difference or impact sorting, eg: an irregularly-named space, a one-off app/destination/HTML5 app/role/user you know isn't meant to exist on the other side, etc. For a quick text substitution across a whole file, `sed` works just as well as opening it in an editor:

   ```bash
   sed -i 's/old_text/new_text/g' us10-pre-apps-full.csv
   sed -i 's/old_text/new_text/g' us10-pre-dest-full.json
   sed -i 's/old_text/new_text/g' us10-pre-html5-full.csv
   sed -i 's/old_text/new_text/g' us10-pre-roles-full.csv
   sed -i 's/old_text/new_text/g' us10-pre-role-collections-full.csv
   sed -i 's/old_text/new_text/g' us10-pre-users-full.csv
   ```

5. **Narrow each down to just the comparable columns:**

   ```bash
   bo sort --fields=space_name,app_mta_id,app_name,app_labels.mta_id,app_labels.mta_version,app_labels.commit_id us10-pre-apps-full.csv --output=us10-pre-apps-sorted.csv
   bo sort --fields=space_name,instance_name,Name,URL,Type,Authentication,User,Password us10-pre-dest-full.json --output=us10-pre-dest-sorted.json
   bo sort --fields=space_name,instance_name,sap_cloud_service,application_id,application_name,application_version us10-pre-html5-full.csv --output=us10-pre-html5-sorted.csv
   bo sort --fields appName,role_name,attributeList --sort-nested-fields=attributeList us10-pre-roles-full.csv --output us10-pre-roles-sorted.csv
   bo sort --fields rolecollection_name,role_name us10-pre-role-collections-full.csv -o us10-pre-role-collections-sorted.csv
   bo sort --fields user_name,email,user_origin,groups --sort-nested-fields=groups us10-pre-users-full.csv --output=us10-pre-users-sorted.csv
   ```

   > `app_labels.mta_id` is a standard label for BTP mta hash; comparing it will tell whether the deployed mta/app are the same. 
   > 
   > On the other hand custom labels - `app_labels.mta_version` and `app_labels.commit_id` can be defined via `cf set-label app <myapp> mta_version=<extract_version_from_mta.yaml>` and `cf set-label app <myapp> commit_id=<extract_repo_commit_id>` after mta/apps deployment

6. **Select the second subaccount and repeat steps 3–5:**

   ```bash
   bo orgs
   #   > 01 [ ] us10/us10-pre (a1b2c3d4-...)
   #     02 [x] us20/us20-pre (e5f6g7h8-...)
   #   Default org scope set to 1 org(s) for this login session.
   ```

   ```bash
   bo apps --include-env --include-labels --output=us20-pre-apps-full.csv
   bo destinations --no-key-option=create --output=us20-pre-dest-full.json
   bo html5-apps --no-runtime-option=create --no-key-option=create --output=us20-pre-html5-full.csv
   bo roles --output=us20-pre-roles-full.csv
   bo role-collections --output=us20-pre-role-collections-full.csv
   bo users --output=us20-pre-users-full.csv
   # (optional edits, same as step 4)
   bo sort --fields=space_name,app_mta_id,app_name,app_labels.mta_id,app_labels.mta_version,app_labels.commit_id us20-pre-apps-full.csv --output=us20-pre-apps-sorted.csv
   bo sort --fields=space_name,instance_name,Name,URL,Type,Authentication,User,Password us20-pre-dest-full.json --output=us20-pre-dest-sorted.json
   bo sort --fields=space_name,instance_name,sap_cloud_service,application_id,application_name,application_version us20-pre-html5-full.csv --output=us20-pre-html5-sorted.csv
   bo sort --fields appName,role_name,attributeList --sort-nested-fields=attributeList us20-pre-roles-full.csv --output=us20-pre-roles-sorted.csv
   bo sort --fields rolecollection_name,role_name us20-pre-role-collections-full.csv --output=us20-pre-role-collections-sorted.csv
   bo sort --fields user_name,email,user_origin,groups --sort-nested-fields=groups us20-pre-users-full.csv --output=us20-pre-users-sorted.csv
   ```

7. **Compare each pair of sorted files** — in VS Code (open both, right-click one → *Select for Compare*, right-click the other → *Compare with Selected*) or SAP Business Application Studio (same VS Code-based diff). Anything shown is either missing from one side or genuinely different between them.

8. **(Optional) Run the following command to generate a single HTML comparison report** (via [`bo compare`](../README.md#compare)) covering every pair of sorted files at once, for review and distribution without needing an editor:
   ```bash
   bo compare us10-pre-apps-sorted.csv,us10-pre-dest-sorted.json,us10-pre-html5-sorted.csv,us10-pre-roles-sorted.csv,us10-pre-role-collections-sorted.csv,us10-pre-users-sorted.csv us20-pre-apps-sorted.csv,us20-pre-dest-sorted.json,us20-pre-html5-sorted.csv,us20-pre-roles-sorted.csv,us20-pre-role-collections-sorted.csv,us20-pre-users-sorted.csv -o pre-compare.html
   ```

## Additional Notes

### Apps

`--include-env` adds the `app_env` column (each app's own user-provided environment variables, JSON-encoded) and requires at least Space Developer access to each app's space — this is sensitive output, see the README's note on `--include-env` before sharing this file. `--include-labels` adds the `app_labels` column (each app's current CF metadata labels, JSON-encoded, fetched via one extra API call per app) — not sensitive output, unlike `--include-env`. The `--fields` list in step 5 above keeps `app_labels` but drops `app_env` from the final sorted/compared file — add `app_env` back to that list too if you also want environment variables in the comparison.

Sample `...-apps-sorted.csv`:

```csv
space_name,app_mta_id,app_name,app_labels
dev,myapp,myapp-srv,"{""team"":""payments""}"
prod,myapp,myapp-srv,"{""team"":""payments""}"
prod,myapp,myapp-ui,"{}"
```

### Destinations

⚠️ **`bo destinations` always includes every destination property unredacted by default — including live secrets like `Password` and `ClientSecret`.** There's no `--full`/safe-mode flag to opt out of this (see the README). Treat every file in this section as a secret: don't commit it, don't paste its contents into a chat/ticket, and delete it once you're done comparing.

`space_name`/`instance_name` are empty for a subaccount-level destination (only instance-level ones have a space/instance) — that's expected, not a gap. Destination property names (`Name`, `URL`, `Type`, `Authentication`, `User`, `Password`) are matched exactly as the destination service itself stores them, case-sensitive.

Sample `...-dest-sorted.json` (passwords below are fictional placeholders, not real secrets):

```json
{
  "rows": [
    {
      "space_name": "",
      "instance_name": "",
      "Name": "API_HUB",
      "URL": "https://api.example.com",
      "Type": "HTTP",
      "Authentication": "BasicAuthentication",
      "User": "api-user",
      "Password": "N0tAReal-P@ss1"
    },
    {
      "space_name": "prod",
      "instance_name": "my-dest-svc",
      "Name": "S4_BACKEND",
      "URL": "https://s4.example.com",
      "Type": "HTTP",
      "Authentication": "OAuth2SAMLBearerAssertion",
      "User": "",
      "Password": ""
    }
  ]
}
```

### HTML5 apps

`--no-runtime-option=create --no-key-option=create` automatically create a temporary `html5-apps-repo`/`app-runtime` service instance and/or service key in any space that doesn't already have one, so no space gets silently skipped for lack of one — see the README's `html5-apps` section for the full set of options (`skip`/`create`/`transient`/`abort`) and what each does. Passing both explicitly like this also avoids the interactive `s/c/t/a` prompt that `bo html5-apps` would otherwise show for a missing instance/key.

**The count here can legitimately differ from the BTP cockpit's "HTML5 Applications" list.** The cockpit's list often includes duplicate-looking entries caused by a destination with an incorrectly-set `sap.cloud.service` property pointing at a host that isn't actually an HTML5 repo, or entries that are really SAP Build Process Automation ("SBPA") inbox workflow apps rather than genuine HTML5 apps. If the numbers don't match and you need the *actual* deployed-app list to compare against, check **SAP Build Work Zone → Content Manager → Content Explorer** instead of the cockpit's HTML5 Applications list.

Sample `...-html5-sorted.csv`:

```csv
space_name,instance_name,sap_cloud_service,application_id,application_name,application_version
prod,html5-apps-repo-host,com.acme.myapp,com.acme.myapp,My App,1.2.3
prod,html5-apps-repo-host,com.acme.myapp.admin,com.acme.myapp.admin,My App Admin,1.0.1
```

### Roles

Lists XSUAA role templates' roles (one row per role). `attributeList` (each role's attribute restrictions, JSON-encoded as an array of objects) can differ only in the order its attributes happen to come back in, even when the actual restrictions are identical — pass it to `--sort-nested-fields` (as step 5 does) so that ordering alone doesn't show up as a difference.

Sample `...-roles-sorted.csv`:

```csv
appName,role_name,attributeList
my-app,Admin,
my-app,Viewer,"[{""attributeName"":""cost_center"",""attributeValueOrigin"":""cost_center"",""attributeValues"":[""1000""]}]"
```

### Role Collections

Lists XSUAA role collections, flattened to one row per role reference — a role collection with several roles produces several rows, one per role, all sharing the same `rolecollection_name`. Since each row is already this granular, no `--sort-nested-fields` is needed here: sorting by `rolecollection_name` then `role_name` (the implicit sort from `--fields` in step 5) already produces a stable, comparable row order regardless of the order role references came back in from the API.

Sample `...-role-collections-sorted.csv`:

```csv
rolecollection_name,role_name
MyApp_Admin,Admin
MyApp_Admin,Viewer
```

### Users

Lists XSUAA users. `groups` (each user's group memberships) is **semicolon-delimited** (`;`), not comma-delimited — `bo users` itself writes it that way to avoid colliding with the CSV's own comma — which `--sort-nested-fields` (step 5) also understands: it falls back to `;` automatically whenever a field's value contains no `,` at all.

Sample `...-users-sorted.csv`:

```csv
user_name,email,user_origin,groups
alice@example.com,alice@example.com,sap.ids,Admins;Developers
bob@example.com,bob@example.com,sap.ids,Developers
```

### Generating a shareable HTML report

[`bo compare`](../README.md#compare) (step 8) turns any number of sorted-file pairs into one self-contained HTML page: a summary table (identical/added/deleted/updated line counts and identical % per pair) followed by a full side-by-side, color-coded diff per pair — the same information a manual VS Code compare (step 7) would show, but viewable without an editor and easy to attach to a ticket or email. It's a plain line-by-line text diff, so it works identically on every file type used in this guide (CSV or JSON alike) without needing to understand either format.

### Batch Job Processing

Use `bo orgs --select <org_name_or_id>` to switch orgs without prompt.

Sample script to extract the info for background job or pipeline execution:

```bash
TODAY=$(date +%Y%m%d)

# update below variables accordingly
CF_USERNAME="username"
CF_PASSWORD="password"
ENV_PREFIX="pre"
REGION_1="us10"
REGION_2="us20"
ORG_1="first-org"
ORG_2="second-org"

# create and enter a new folder for data extraction
mkdir "${ENV_PREFIX}-${TODAY}" && cd "${ENV_PREFIX}-${TODAY}"

# make sure to install BO first, see: https://github.com/sap-pilot/btp-open-cli#installation
bo login --regions "${REGION_1},${REGION_2}" --username "${CF_USERNAME}" --password "${CF_PASSWORD}"
REGION="${REGION_1}"
bo orgs --select "${ORG_1}"
bo apps --include-env --include-labels --output=${REGION}-${ENV_PREFIX}-apps-full.csv
bo destinations --no-key-option=create --output=${REGION}-${ENV_PREFIX}-dest-full.json
bo html5-apps --no-runtime-option=create --no-key-option=create --output=${REGION}-${ENV_PREFIX}-html5-full.csv
bo roles --output=${REGION}-${ENV_PREFIX}-roles-full.csv
bo role-collections --output=${REGION}-${ENV_PREFIX}-role-collections-full.csv
bo users --output=${REGION}-${ENV_PREFIX}-users-full.csv
bo sort --fields=space_name,app_mta_id,app_name,app_labels.mta_id,app_labels.mta_version,app_labels.commit_id ${REGION}-${ENV_PREFIX}-apps-full.csv --output=${REGION}-${ENV_PREFIX}-apps-sorted.csv
bo sort --fields=space_name,instance_name,Name,URL,Type,Authentication,User,Password ${REGION}-${ENV_PREFIX}-dest-full.json --output=${REGION}-${ENV_PREFIX}-dest-sorted.json
bo sort --fields=space_name,instance_name,sap_cloud_service,application_id,application_name,application_version ${REGION}-${ENV_PREFIX}-html5-full.csv --output=${REGION}-${ENV_PREFIX}-html5-sorted.csv
bo sort --fields=appName,role_name,attributeList --sort-nested-fields=attributeList ${REGION}-${ENV_PREFIX}-roles-full.csv --output=${REGION}-${ENV_PREFIX}-roles-sorted.csv
bo sort --fields=rolecollection_name,role_name ${REGION}-${ENV_PREFIX}-role-collections-full.csv --output=${REGION}-${ENV_PREFIX}-role-collections-sorted.csv
bo sort --fields=user_name,email,user_origin,groups --sort-nested-fields=groups ${REGION}-${ENV_PREFIX}-users-full.csv --output=${REGION}-${ENV_PREFIX}-users-sorted.csv
REGION="${REGION_2}"
bo orgs --select "${ORG_2}"
bo apps --include-env --include-labels --output=${REGION}-${ENV_PREFIX}-apps-full.csv
bo destinations --no-key-option=create --output=${REGION}-${ENV_PREFIX}-dest-full.json
bo html5-apps --no-runtime-option=create --no-key-option=create --output=${REGION}-${ENV_PREFIX}-html5-full.csv
bo roles --output=${REGION}-${ENV_PREFIX}-roles-full.csv
bo role-collections --output=${REGION}-${ENV_PREFIX}-role-collections-full.csv
bo users --output=${REGION}-${ENV_PREFIX}-users-full.csv
bo sort --fields=space_name,app_mta_id,app_name,app_labels.mta_id,app_labels.mta_version,app_labels.commit_id ${REGION}-${ENV_PREFIX}-apps-full.csv --output=${REGION}-${ENV_PREFIX}-apps-sorted.csv
bo sort --fields=space_name,instance_name,Name,URL,Type,Authentication,User,Password ${REGION}-${ENV_PREFIX}-dest-full.json --output=${REGION}-${ENV_PREFIX}-dest-sorted.json
bo sort --fields=space_name,instance_name,sap_cloud_service,application_id,application_name,application_version ${REGION}-${ENV_PREFIX}-html5-full.csv --output=${REGION}-${ENV_PREFIX}-html5-sorted.csv
bo sort --fields=appName,role_name,attributeList --sort-nested-fields=attributeList ${REGION}-${ENV_PREFIX}-roles-full.csv --output=${REGION}-${ENV_PREFIX}-roles-sorted.csv
bo sort --fields=rolecollection_name,role_name ${REGION}-${ENV_PREFIX}-role-collections-full.csv --output=${REGION}-${ENV_PREFIX}-role-collections-sorted.csv
bo sort --fields=user_name,email,user_origin,groups --sort-nested-fields=groups ${REGION}-${ENV_PREFIX}-users-full.csv --output=${REGION}-${ENV_PREFIX}-users-sorted.csv

# One HTML report covering every artifact type, both subaccounts
bo compare \
  "${REGION_1}-${ENV_PREFIX}-apps-sorted.csv,${REGION_1}-${ENV_PREFIX}-dest-sorted.json,${REGION_1}-${ENV_PREFIX}-html5-sorted.csv,${REGION_1}-${ENV_PREFIX}-roles-sorted.csv,${REGION_1}-${ENV_PREFIX}-role-collections-sorted.csv,${REGION_1}-${ENV_PREFIX}-users-sorted.csv" \
  "${REGION_2}-${ENV_PREFIX}-apps-sorted.csv,${REGION_2}-${ENV_PREFIX}-dest-sorted.json,${REGION_2}-${ENV_PREFIX}-html5-sorted.csv,${REGION_2}-${ENV_PREFIX}-roles-sorted.csv,${REGION_2}-${ENV_PREFIX}-role-collections-sorted.csv,${REGION_2}-${ENV_PREFIX}-users-sorted.csv" \
  -o "${ENV_PREFIX}-${TODAY}-compare.html"
```

## Command reference

- [`bo login`](../README.md#login), [`bo orgs`](../README.md#orgs) — authenticate and pick the subaccount in scope for the rest of the session.
- [`bo apps`](../README.md#apps) — list apps (`--include-env` for their environment variables).
- [`bo destinations`](../README.md#destinations) — list subaccount- and instance-level destinations together (sensitive — see above).
- [`bo html5-apps`](../README.md#html5-apps) — list HTML5 Application Repository app metadata.
- [`bo roles`](../README.md#roles) — list XSUAA role templates' roles, one row per role.
- [`bo role-collections`](../README.md#role-collections) — list XSUAA role collections, one row per role reference.
- [`bo users`](../README.md#users) — list XSUAA users and their group memberships.
- [`bo sort`](../README.md#sort) — narrow (`--fields`) and sort a CSV/JSON file already on disk, in place or to a new file — sorts by `--fields`' own fields when `--by` is omitted; `--sort-nested-fields` additionally sorts a named field's own list/object content, independent of row order.
- [`bo compare`](../README.md#compare) — render one or more pairs of sorted files into a single, self-contained HTML side-by-side diff report, with a summary table.
- [`bo orgs --select`](../README.md#orgs) — non-interactively switch the default org scope by name or GUID, for unattended/batch use.
