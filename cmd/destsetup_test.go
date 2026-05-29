package cmd

import "testing"

// TestDestCredentials_FlatFormat verifies that flat (top-level) credential
// maps are parsed correctly — the older service key format.
func TestDestCredentials_FlatFormat(t *testing.T) {
	details := map[string]interface{}{
		"clientid":     "my-client-id",
		"clientsecret": "my-client-secret",
		"url":          "https://auth.example.com",
		"uri":          "https://dest.example.com",
	}
	clientID, clientSecret, tokenURL, uri, missing := destCredentialsFromDetails(details)
	if clientID != "my-client-id" {
		t.Errorf("clientID: want my-client-id, got %q", clientID)
	}
	if clientSecret != "my-client-secret" {
		t.Errorf("clientSecret: want my-client-secret, got %q", clientSecret)
	}
	if tokenURL != "https://auth.example.com" {
		t.Errorf("tokenURL: want https://auth.example.com, got %q", tokenURL)
	}
	if uri != "https://dest.example.com" {
		t.Errorf("uri: want https://dest.example.com, got %q", uri)
	}
	if missing != "" {
		t.Errorf("missing: want empty, got %q", missing)
	}
}

// TestDestCredentials_UAASub verifies that the newer service key format —
// where credentials are nested under a "uaa" sub-object — is preferred.
func TestDestCredentials_UAASub(t *testing.T) {
	details := map[string]interface{}{
		"uaa": map[string]interface{}{
			"clientid":     "uaa-client-id",
			"clientsecret": "uaa-client-secret",
			"url":          "https://uaa-auth.example.com",
			"uri":          "https://uaa-dest.example.com",
		},
		// Top-level fields should be ignored when "uaa" is present
		"clientid":     "ignored",
		"clientsecret": "ignored",
	}
	clientID, clientSecret, tokenURL, uri, missing := destCredentialsFromDetails(details)
	if clientID != "uaa-client-id" {
		t.Errorf("clientID: want uaa-client-id, got %q", clientID)
	}
	if clientSecret != "uaa-client-secret" {
		t.Errorf("clientSecret: want uaa-client-secret, got %q", clientSecret)
	}
	if tokenURL != "https://uaa-auth.example.com" {
		t.Errorf("tokenURL: want https://uaa-auth.example.com, got %q", tokenURL)
	}
	if uri != "https://uaa-dest.example.com" {
		t.Errorf("uri: want https://uaa-dest.example.com, got %q", uri)
	}
	if missing != "" {
		t.Errorf("missing: want empty, got %q", missing)
	}
}

// TestDestCredentials_URIFallback_TopLevel verifies that when the "uaa" sub-object
// has no "uri" field, the function falls back to the top-level "uri" field.
func TestDestCredentials_URIFallback_TopLevel(t *testing.T) {
	details := map[string]interface{}{
		"uaa": map[string]interface{}{
			"clientid":     "cid",
			"clientsecret": "cs",
			"url":          "https://auth.example.com",
			// no "uri" here
		},
		"uri": "https://top-level-dest.example.com", // fallback
	}
	_, _, _, uri, missing := destCredentialsFromDetails(details)
	if uri != "https://top-level-dest.example.com" {
		t.Errorf("uri: want https://top-level-dest.example.com, got %q", uri)
	}
	if missing != "" {
		t.Errorf("missing: want empty, got %q", missing)
	}
}

// TestDestCredentials_URIFallback_Endpoints verifies that when neither the uaa
// sub-object nor the top-level map has "uri", the function falls back to
// endpoints["destination"].
func TestDestCredentials_URIFallback_Endpoints(t *testing.T) {
	details := map[string]interface{}{
		"uaa": map[string]interface{}{
			"clientid":     "cid",
			"clientsecret": "cs",
			"url":          "https://auth.example.com",
		},
		"endpoints": map[string]interface{}{
			"destination": "https://endpoints-dest.example.com",
		},
	}
	_, _, _, uri, missing := destCredentialsFromDetails(details)
	if uri != "https://endpoints-dest.example.com" {
		t.Errorf("uri: want https://endpoints-dest.example.com, got %q", uri)
	}
	if missing != "" {
		t.Errorf("missing: want empty, got %q", missing)
	}
}

// TestDestCredentials_MissingClientID verifies that an empty details map
// reports "clientid" as the missing field.
func TestDestCredentials_MissingClientID(t *testing.T) {
	_, _, _, _, missing := destCredentialsFromDetails(map[string]interface{}{})
	if missing != "clientid" {
		t.Errorf("missing: want clientid, got %q", missing)
	}
}

// TestDestCredentials_MissingSecret verifies that having only clientid reports
// "clientsecret" as the missing field.
func TestDestCredentials_MissingSecret(t *testing.T) {
	details := map[string]interface{}{"clientid": "cid"}
	_, _, _, _, missing := destCredentialsFromDetails(details)
	if missing != "clientsecret" {
		t.Errorf("missing: want clientsecret, got %q", missing)
	}
}

// TestDestCredentials_MissingTokenURL verifies that having clientid and
// clientsecret but no url reports the token URL as missing.
func TestDestCredentials_MissingTokenURL(t *testing.T) {
	details := map[string]interface{}{
		"clientid":     "cid",
		"clientsecret": "cs",
	}
	_, _, _, _, missing := destCredentialsFromDetails(details)
	if missing != "url (uaa token endpoint)" {
		t.Errorf("missing: want 'url (uaa token endpoint)', got %q", missing)
	}
}

// TestDestCredentials_MissingURI verifies that when clientid, clientsecret, and
// url are all present but uri is absent, the uri field is reported as missing.
func TestDestCredentials_MissingURI(t *testing.T) {
	details := map[string]interface{}{
		"clientid":     "cid",
		"clientsecret": "cs",
		"url":          "https://auth.example.com",
	}
	_, _, _, _, missing := destCredentialsFromDetails(details)
	if missing != "uri (destination service endpoint)" {
		t.Errorf("missing: want 'uri (destination service endpoint)', got %q", missing)
	}
}

// TestDestCredentials_RealSampleKey verifies parsing of the actual service key
// structure used in production (from tmp/destination-sk.json).
func TestDestCredentials_RealSampleKey(t *testing.T) {
	details := map[string]interface{}{
		"token-type": []interface{}{"xsuaa", "ias"},
		"uaa": map[string]interface{}{
			"uaadomain":       "authentication.us10.hana.ondemand.com",
			"tenantmode":      "dedicated",
			"clientid":        "sb-clone!b147007|destination-xsappname!b62",
			"credential-type": "binding-secret",
			"instanceid":      "2e4ccb74-c25a-442e-aa4b-fd7a21b59470",
			"clientsecret":    "d3ef929c-64d2-4058-9be3-44c57756f7dc",
			"tenantid":        "6731c1b4-a33a-44b2-b3e7-e492bcf4e250",
			"uri":             "https://destination-configuration.cfapps.us10.hana.ondemand.com",
			"url":             "https://ncfaws-17.authentication.us10.hana.ondemand.com",
		},
		"content_endpoint": "https://destination-configuration.cfapps.us10.hana.ondemand.com/destination-configuration/gacd",
	}
	clientID, clientSecret, tokenURL, uri, missing := destCredentialsFromDetails(details)
	if clientID == "" {
		t.Error("clientID should not be empty")
	}
	if clientSecret == "" {
		t.Error("clientSecret should not be empty")
	}
	if tokenURL == "" {
		t.Error("tokenURL should not be empty")
	}
	if uri == "" {
		t.Error("uri should not be empty")
	}
	if missing != "" {
		t.Errorf("missing: want empty, got %q", missing)
	}
}
