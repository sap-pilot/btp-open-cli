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

   > CAUTION: if destination service key or html5-app-runtime instance/service-key is missing then below commands will create those missing keys or service instance for the extraction to work (see below destinatons/apps section for more detail); Threfore - recommend to test in QA/PREPROD subaccounts first prior to running these command in PRODUCTION subaccounts. 

   ```bash
   bo apps --include-env --include-labels --output=us10-pre-apps-full.csv
   bo destinations --no-key-option=create --output=us10-pre-dest-full.json
   bo html5-apps --no-runtime-option=create --no-key-option=create --output=us10-pre-html5-full.csv
   ```

   > `.csv` is auto-detected from each filename. See "Apps"/"Destinations"/"HTML5 apps" below for what each command needs and what to watch out for.

4. **(Optional) open any of the three `...-full.csv` files** and fix up anything that would otherwise show as a false difference or impact sorting, eg: an irregularly-named space, a one-off app/destination/HTML5 app you know isn't meant to exist on the other side, etc. For a quick text substitution across a whole file, `sed` works just as well as opening it in an editor:

   ```bash
   sed -i 's/old_text/new_text/g' us10-pre-apps-full.csv
   ```

5. **Narrow each down to just the comparable columns:**

   ```bash
   bo sort --fields=space_name,app_mta_id,app_name,app_labels.mta_id,app_labels.mta_version,app_labels.commit_id us10-pre-apps-full.csv --output=us10-pre-apps-sorted.csv
   bo sort --fields=space_name,instance_name,Name,URL,Type,Authentication,User,Password us10-pre-dest-full.json --output=us10-pre-dest-sorted.json
   bo sort --fields=space_name,instance_name,sap_cloud_service,application_id,application_name,application_version us10-pre-html5-full.csv --output=us10-pre-html5-sorted.csv
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
   # (optional edits, same as step 4)
   bo sort --fields=space_name,app_mta_id,app_name,app_labels.mta_id,app_labels.mta_version,app_labels.commit_id us20-pre-apps-full.csv --output=us20-pre-apps-sorted.csv
   bo sort --fields=space_name,instance_name,Name,URL,Type,Authentication,User,Password us20-pre-dest-full.json --output=us20-pre-dest-sorted.json
   bo sort --fields=space_name,instance_name,sap_cloud_service,application_id,application_name,application_version us20-pre-html5-full.csv --output=us20-pre-html5-sorted.csv
   ```

7. **Compare each pair of sorted files** — in VS Code (open both, right-click one → *Select for Compare*, right-click the other → *Compare with Selected*) or SAP Business Application Studio (same VS Code-based diff). Anything shown is either missing from one side or genuinely different between them.

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
bo sort --fields=space_name,app_mta_id,app_name,app_labels.mta_id,app_labels.mta_version,app_labels.commit_id ${REGION}-${ENV_PREFIX}-apps-full.csv --output=${REGION}-${ENV_PREFIX}-apps-sorted.csv
bo sort --fields=space_name,instance_name,Name,URL,Type,Authentication,User,Password ${REGION}-${ENV_PREFIX}-dest-full.json --output=${REGION}-${ENV_PREFIX}-dest-sorted.json
bo sort --fields=space_name,instance_name,sap_cloud_service,application_id,application_name,application_version ${REGION}-${ENV_PREFIX}-html5-full.csv --output=${REGION}-${ENV_PREFIX}-html5-sorted.csv
REGION="${REGION_2}"
bo orgs --select "${ORG_2}"
bo apps --include-env --include-labels --output=${REGION}-${ENV_PREFIX}-apps-full.csv
bo destinations --no-key-option=create --output=${REGION}-${ENV_PREFIX}-dest-full.json
bo html5-apps --no-runtime-option=create --no-key-option=create --output=${REGION}-${ENV_PREFIX}-html5-full.csv
bo sort --fields=space_name,app_mta_id,app_name,app_labels.mta_id,app_labels.mta_version,app_labels.commit_id ${REGION}-${ENV_PREFIX}-apps-full.csv --output=${REGION}-${ENV_PREFIX}-apps-sorted.csv
bo sort --fields=space_name,instance_name,Name,URL,Type,Authentication,User,Password ${REGION}-${ENV_PREFIX}-dest-full.json --output=${REGION}-${ENV_PREFIX}-dest-sorted.json
bo sort --fields=space_name,instance_name,sap_cloud_service,application_id,application_name,application_version ${REGION}-${ENV_PREFIX}-html5-full.csv --output=${REGION}-${ENV_PREFIX}-html5-sorted.csv
```

## Command reference

- [`bo login`](../README.md#login), [`bo orgs`](../README.md#orgs) — authenticate and pick the subaccount in scope for the rest of the session.
- [`bo apps`](../README.md#apps) — list apps (`--include-env` for their environment variables).
- [`bo destinations`](../README.md#destinations) — list subaccount- and instance-level destinations together (sensitive — see above).
- [`bo html5-apps`](../README.md#html5-apps) — list HTML5 Application Repository app metadata.
- [`bo sort`](../README.md#sort) — narrow (`--fields`) and sort a CSV/JSON file already on disk, in place or to a new file — sorts by `--fields`' own fields when `--by` is omitted.
