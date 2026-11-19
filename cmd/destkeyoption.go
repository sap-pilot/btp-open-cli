package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"btp-open-cli/internal/cf"
	"btp-open-cli/internal/destination"
	"btp-open-cli/internal/store"

	"github.com/spf13/cobra"
)

// This file provides the --no-key-option mechanism shared by 'destinations',
// 'subaccount-destinations', and 'space-destinations' (via
// resolveDestInstanceClient, called from resolveOrgDestClient in
// cmd/subaccountdestinations.go and resolveSpaceDestClientsForPlan in
// cmd/spacedestinations.go) for a destination service instance found with no
// service key — the same s/skip, c/create, t/transient, a/abort shape as
// html5-apps' own --no-key-option (see cmd/html5apps.go), kept as a
// separate, destination-specific type since the two commands' instances,
// key names, and callers are otherwise unrelated.
//
// create-subaccount-destinations/update-subaccount-destinations/
// delete-subaccount-destinations and create-space-destinations/
// update-space-destinations/delete-space-destinations have no
// --no-key-option flag of their own (only the three read commands do) — see
// destKeyOptionFromNoPrompt.

// destKeyOption governs what happens when a destination service instance is
// found with no service key. destKeyOptionPrompt is never a valid flag
// value — it's assigned internally (see each read command's RunE) when
// --no-key-option was omitted and -y wasn't given, so each such instance is
// asked about interactively instead of defaulting straight to skip.
type destKeyOption string

const (
	destKeyOptionPrompt    destKeyOption = "prompt"    // ask interactively, per instance (default when --no-key-option is omitted and -y isn't given)
	destKeyOptionSkip      destKeyOption = "skip"      // warn and skip (default when -y is given without --no-key-option)
	destKeyOptionCreate    destKeyOption = "create"    // create a service key automatically, and keep it
	destKeyOptionTransient destKeyOption = "transient" // create a service key, use it, then delete it
	destKeyOptionAbort     destKeyOption = "abort"     // stop the whole command immediately
)

// destAutoKeyName is the service key name used by --no-key-option's (or the
// interactive prompt's) c/create and t/transient choices.
const destAutoKeyName = "bo-dest-key"

// normalizeDestKeyOption validates and normalizes --no-key-option's raw
// value (accepting both the short and long form of each option). Never
// returns destKeyOptionPrompt — that's only assigned internally when the
// flag is omitted entirely.
func normalizeDestKeyOption(raw string) (destKeyOption, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "s", "skip":
		return destKeyOptionSkip, nil
	case "c", "create":
		return destKeyOptionCreate, nil
	case "t", "transient":
		return destKeyOptionTransient, nil
	case "a", "abort":
		return destKeyOptionAbort, nil
	default:
		return "", fmt.Errorf("invalid --no-key-option %q: must be one of s/skip, c/create, t/transient, a/abort", raw)
	}
}

// destKeyOptionFromNoPrompt maps a --no-prompt boolean onto destKeyOption,
// for every caller that has no --no-key-option flag of its own:
// create/update/delete-subaccount-destinations and
// create/update/delete-space-destinations (always destKeyOptionPrompt, no
// --no-prompt flag at all), and html5-apps' own --from-destinations
// destination-key resolution (tied to its existing --no-prompt flag rather
// than exposing a second, differently-scoped --no-key-option).
func destKeyOptionFromNoPrompt(noPrompt bool) destKeyOption {
	if noPrompt {
		return destKeyOptionSkip
	}
	return destKeyOptionPrompt
}

// promptDestKeyOption asks what to do about one destination service
// instance found with no service key, when --no-key-option wasn't given
// (and -y wasn't either): s/skip (default), c/create, t/transient
// (create-then-delete), or a/abort. Loops on unrecognized input; a
// closed/cancelled stdin is treated as abort, the same convention
// promptUpdateCreateAbort/promptHTML5KeyOption use. spaceName is included
// in the prompt when non-empty (a subaccount-level lookup, as in
// resolveOrgDestClient, has no single space to name).
func promptDestKeyOption(ctx context.Context, out io.Writer, instName, instGUID, spaceName string) destKeyOption {
	where := instGUID
	if spaceName != "" {
		where = fmt.Sprintf("%s, space %s", instGUID, spaceName)
	}
	for {
		fmt.Fprintf(out, "\nNo service key found for destination service instance %q (%s).\n"+
			"  s/skip (default), c/create, t/transient (create, use, then delete), or a/abort? ",
			instName, where)
		text, ok := readLine(ctx)
		if !ok {
			return destKeyOptionAbort
		}
		switch strings.ToLower(strings.TrimSpace(text)) {
		case "", "s", "skip":
			return destKeyOptionSkip
		case "c", "create":
			return destKeyOptionCreate
		case "t", "transient":
			return destKeyOptionTransient
		case "a", "abort":
			return destKeyOptionAbort
		default:
			fmt.Fprintln(out, "Please answer s, c, t, or a.")
		}
	}
}

// resolveDestInstanceClient obtains (or reuses a cached) OAuth token for one
// destination service instance, handling a missing service key per
// keyOption — shared by resolveOrgDestClient (subaccount level, scanning
// spaces for any one usable instance) and resolveSpaceDestClientsForPlan
// (space level, one client per instance in a single space). spaceCache is
// creds.SpaceDestServices[spaceGUID] (created by the caller if needed).
// spaceName is used only for the interactive prompt's message — pass "" if
// not meaningful in context.
//
// Returns ok=false (with a warning already printed) when the instance has
// to be skipped, or a non-nil error if the whole command must stop (chose,
// or interactively answered, abort).
//
// The returned client's Cleanup is non-nil only under destKeyOptionTransient,
// when this call itself created the service key being returned: the caller
// must call it exactly once, after it is done using the returned client, to
// delete that temporary key (and drop it from the on-disk cache, since it
// won't exist for a later run to reuse) — see cleanupDestClients.
func resolveDestInstanceClient(ctx context.Context, cmd *cobra.Command, cfClient *cf.Client, spaceName string, inst cf.ServiceInstance, spaceCache map[string]*store.DestInstanceCache, keyOption destKeyOption) (sdDestClient, bool, error) {
	cached := spaceCache[inst.GUID]
	needToken := cached == nil || cached.AccessToken == "" ||
		time.Now().Add(60*time.Second).After(cached.TokenExpiry)

	var createdBindingGUID string // set only when this call created the key under transient

	if needToken {
		key, keyErr := cfClient.FindAnyServiceCredentialBinding(ctx, inst.GUID)
		if keyErr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: finding service key for %q: %v\n", inst.Name, keyErr)
			return sdDestClient{}, false, nil
		}
		if key == nil {
			effective := keyOption
			if effective == destKeyOptionPrompt {
				effective = promptDestKeyOption(ctx, cmd.ErrOrStderr(), inst.Name, inst.GUID, spaceName)
			}
			switch effective {
			case destKeyOptionAbort:
				fmt.Fprintf(cmd.ErrOrStderr(), "aborting: no service key for destination instance %q (%s)\n", inst.Name, inst.GUID)
				return sdDestClient{}, false, errAborted

			case destKeyOptionCreate, destKeyOptionTransient:
				fmt.Fprintf(cmd.ErrOrStderr(), "creating service key %q for destination instance %q...\n", destAutoKeyName, inst.Name)
				bindingGUID, createErr := cfClient.CreateServiceCredentialBinding(ctx, destAutoKeyName, inst.GUID)
				if createErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: creating service key for %q: %v — skipping\n", inst.Name, createErr)
					return sdDestClient{}, false, nil
				}
				key = &cf.ServiceCredentialBinding{GUID: bindingGUID, Name: destAutoKeyName}
				if effective == destKeyOptionTransient {
					createdBindingGUID = bindingGUID
				}

			default: // destKeyOptionSkip
				fmt.Fprintf(cmd.ErrOrStderr(),
					"warning: no service key for destination instance %q (%s) — skipping (pass --no-key-option=c/t to create one automatically)\n",
					inst.Name, inst.GUID)
				return sdDestClient{}, false, nil
			}
		}

		details, detailErr := cfClient.GetServiceCredentialDetails(ctx, key.GUID)
		if detailErr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: fetching key details for %q: %v\n", inst.Name, detailErr)
			return sdDestClient{}, false, nil
		}
		clientID, clientSecret, tokenURL, uri, missing := destCredentialsFromDetails(details)
		if missing != "" {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: incomplete credentials in service key for %q (missing %q)\n", inst.Name, missing)
			return sdDestClient{}, false, nil
		}

		newToken, expiry, tokErr := destination.GetAccessToken(ctx, tokenURL, clientID, clientSecret)
		if tokErr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: token for destination instance %q: %v\n", inst.Name, tokErr)
			return sdDestClient{}, false, nil
		}

		if cached == nil {
			cached = &store.DestInstanceCache{}
			spaceCache[inst.GUID] = cached
		}
		cached.InstanceName = inst.Name
		cached.TokenURL = tokenURL
		cached.URI = uri
		cached.AccessToken = newToken
		cached.TokenExpiry = expiry
		// clientID and clientSecret intentionally NOT stored.
	}

	if cached == nil || cached.URI == "" {
		return sdDestClient{}, false, nil
	}
	client := sdDestClient{
		InstanceGUID: inst.GUID,
		InstanceName: cached.InstanceName,
		URI:          cached.URI,
		Token:        cached.AccessToken,
	}

	if createdBindingGUID != "" {
		instName, bindingGUID, instGUID := inst.Name, createdBindingGUID, inst.GUID
		client.Cleanup = func() {
			fmt.Fprintf(cmd.ErrOrStderr(), "deleting temporary service key %q for destination instance %q...\n", destAutoKeyName, instName)
			if delErr := cfClient.DeleteServiceCredentialBinding(ctx, bindingGUID); delErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: deleting temporary service key for %q: %v\n", instName, delErr)
			}
			delete(spaceCache, instGUID)
		}
	}
	return client, true, nil
}

// cleanupDestClients calls every non-nil Cleanup among clients, once each —
// callers must do this after they're done using whatever clients
// resolveOrgDestClient/resolveSpaceDestClients* returned, so a temporary
// key created under destKeyOptionTransient actually gets deleted again.
func cleanupDestClients(clients ...sdDestClient) {
	for _, c := range clients {
		if c.Cleanup != nil {
			c.Cleanup()
		}
	}
}
