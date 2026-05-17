package cf

import (
	"crypto/tls"
	"net/http"
	"os"
)

// newTransport returns an HTTP transport that:
//   - honours HTTPS_PROXY / HTTP_PROXY / NO_PROXY environment variables
//     (Go's http.DefaultTransport already does this; we clone it to avoid
//     mutating the shared default)
//   - skips TLS certificate verification when HTTPS_PROXY_INSECURE=true,
//     which is required when intercepting traffic with mitmproxy
func newTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	if os.Getenv("HTTPS_PROXY_INSECURE") == "true" {
		t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}
	return t
}
