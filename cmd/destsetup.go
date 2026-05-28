package cmd

// destCredentialsFromDetails extracts the four credential fields needed to
// authenticate against a destination service instance from the raw map
// returned by GetServiceCredentialDetails (a CF service key's credentials JSON).
//
// Field mapping:
//
//	clientid       → OAuth 2.0 client ID
//	clientsecret   → OAuth 2.0 client secret
//	url            → UAA token endpoint base URL
//	uri            → Destination service base URI
//
// Some destination service key versions omit the top-level "uri" field and
// instead place the value under endpoints["destination"]. The function falls
// back to that nested path automatically.
//
// Returns the four credential strings and, if any are still empty, the name of
// the first missing field (for use in a diagnostic warning message).
func destCredentialsFromDetails(details map[string]interface{}) (clientID, clientSecret, tokenURL, uri, missing string) {
	clientID, _ = details["clientid"].(string)
	clientSecret, _ = details["clientsecret"].(string)
	tokenURL, _ = details["url"].(string)
	uri, _ = details["uri"].(string)

	// Fallback: some destination service key versions nest the base URI under
	// endpoints.destination rather than exposing it as a top-level "uri" field.
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
		missing = "url"
	case uri == "":
		missing = "uri (and endpoints.destination)"
	}
	return
}
