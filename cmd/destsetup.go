package cmd

// destCredentialsFromDetails extracts the four credential fields needed to
// authenticate against a destination service instance from the raw map
// returned by GetServiceCredentialDetails (a CF service key's credentials JSON).
//
// The destination service broker has produced at least two credential layouts:
//
// Flat (older):
//
//	{ "clientid": "...", "clientsecret": "...", "url": "...", "uri": "..." }
//
// Nested under "uaa" (newer):
//
//	{ "uaa": { "clientid": "...", "clientsecret": "...", "url": "...", "uri": "..." },
//	  "content_endpoint": "..." }
//
// The function tries the "uaa" sub-object first (preferred), then falls back to
// the top-level map for older key formats. For the uri field it additionally
// falls back to endpoints["destination"] if neither primary source has it.
//
// Returns the four credential strings and, if any are still empty, the name of
// the first missing field (for use in a diagnostic warning message).
func destCredentialsFromDetails(details map[string]interface{}) (clientID, clientSecret, tokenURL, uri, missing string) {
	// Prefer the "uaa" sub-object (newer key format); fall back to the root map.
	src := details
	if uaa, ok := details["uaa"].(map[string]interface{}); ok {
		src = uaa
	}

	clientID, _ = src["clientid"].(string)
	clientSecret, _ = src["clientsecret"].(string)
	tokenURL, _ = src["url"].(string)
	uri, _ = src["uri"].(string)

	// Extra uri fallbacks for edge cases:
	// 1. Top-level "uri" when credentials were in the "uaa" sub-object but uri wasn't.
	if uri == "" {
		uri, _ = details["uri"].(string)
	}
	// 2. endpoints["destination"] used by some older service plan variants.
	if uri == "" {
		if eps, ok := details["endpoints"].(map[string]interface{}); ok {
			uri, _ = eps["destination"].(string)
		}
	}

	switch {
	case clientID == "":
		missing = "clientid"
	case clientSecret == "":
		missing = "clientsecret"
	case tokenURL == "":
		missing = "url (uaa token endpoint)"
	case uri == "":
		missing = "uri (destination service endpoint)"
	}
	return
}
