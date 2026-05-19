package cf

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	maxRetries     = 5
	backoffBase    = 2 * time.Second
	backoffMaxWait = 60 * time.Second
)

// TokenRefresher is called by the client when a 401 is received. It should
// attempt to obtain a fresh access token (via refresh token or re-auth prompt)
// and return it. Returning an error causes the 401 to be propagated to the caller.
type TokenRefresher func(ctx context.Context) (string, error)

type Client struct {
	apiBaseURL     string
	accessToken    string
	http           *http.Client
	tokenRefresher TokenRefresher
	tokenRefreshed bool // prevents infinite refresh loops
}

func NewClient(apiBaseURL, accessToken string) *Client {
	return &Client{
		apiBaseURL:  strings.TrimRight(apiBaseURL, "/"),
		accessToken: accessToken,
		http:        &http.Client{Timeout: 60 * time.Second, Transport: newTransport()},
	}
}

// SetTokenRefresher attaches a callback that is invoked once when the server
// returns HTTP 401. The callback should return a fresh access token.
func (c *Client) SetTokenRefresher(fn TokenRefresher) {
	c.tokenRefresher = fn
}

func (c *Client) BaseURL() string {
	return c.apiBaseURL
}

// doWithRetry executes makeReq and retries on HTTP 429 (Too Many Requests).
// If the response includes a Retry-After header the delay is taken from it;
// otherwise randomised exponential backoff is used. The context is respected
// during waits so Ctrl-C cancels promptly.
func (c *Client) doWithRetry(ctx context.Context, makeReq func() (*http.Request, error)) (*http.Response, []byte, error) {
	for attempt := 0; ; attempt++ {
		req, err := makeReq()
		if err != nil {
			return nil, nil, err
		}

		resp, err := c.http.Do(req)
		if err != nil {
			return nil, nil, err
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, nil, err
		}

		// Handle 401: attempt token refresh exactly once per client lifetime.
		if resp.StatusCode == http.StatusUnauthorized && c.tokenRefresher != nil && !c.tokenRefreshed {
			c.tokenRefreshed = true
			newToken, err := c.tokenRefresher(ctx)
			if err != nil {
				return resp, body, fmt.Errorf("re-authentication failed: %w", err)
			}
			c.accessToken = newToken
			attempt-- // don't count this as a 429 backoff attempt
			continue
		}

		if resp.StatusCode != http.StatusTooManyRequests || attempt >= maxRetries {
			return resp, body, nil
		}

		wait := retryAfterDelay(resp.Header.Get("Retry-After"), attempt)
		slog.Warn("CF API rate limit hit; retrying", "attempt", attempt+1, "wait", wait.Round(time.Millisecond))

		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// retryAfterDelay returns how long to wait before the next attempt.
// If the Retry-After header is present it is respected (seconds or HTTP-date);
// otherwise randomised exponential backoff is applied.
func retryAfterDelay(header string, attempt int) time.Duration {
	if header != "" {
		// Seconds form: "Retry-After: 30"
		if secs, err := strconv.Atoi(strings.TrimSpace(header)); err == nil && secs > 0 {
			return time.Duration(secs) * time.Second
		}
		// HTTP-date form: "Retry-After: Wed, 21 Oct 2025 07:28:00 GMT"
		if t, err := http.ParseTime(header); err == nil {
			if d := time.Until(t); d > 0 {
				return d
			}
		}
	}
	// Exponential backoff: base * 2^attempt, capped, plus random jitter in [0, base).
	exp := backoffBase * (1 << attempt)
	if exp > backoffMaxWait {
		exp = backoffMaxWait
	}
	jitter := time.Duration(rand.Int63n(int64(backoffBase)))
	return exp + jitter
}

func (c *Client) get(ctx context.Context, fullURL string, out interface{}) error {
	makeReq := func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, "GET", fullURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.accessToken)
		req.Header.Set("Accept", "application/json")
		return req, nil
	}

	resp, body, err := c.doWithRetry(ctx, makeReq)
	if err != nil {
		return fmt.Errorf("GET %s: %w", fullURL, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s returned HTTP %d: %s", fullURL, resp.StatusCode, body)
	}
	return json.Unmarshal(body, out)
}

// APIError carries the HTTP status code and raw response body from a failed CF
// API call so callers can inspect the code and decide how to handle it.
type APIError struct {
	StatusCode int
	Body       []byte
}

func (e *APIError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Body)
}

// post marshals body as JSON, POSTs to fullURL, and — if out is non-nil —
// unmarshals a successful response into out. A non-2xx status is returned as
// *APIError so callers can switch on the status code.
func (c *Client) post(ctx context.Context, fullURL string, body, out interface{}) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}

	makeReq := func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, "POST", fullURL, bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.accessToken)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		return req, nil
	}

	resp, respBody, err := c.doWithRetry(ctx, makeReq)
	if err != nil {
		return fmt.Errorf("POST %s: %w", fullURL, err)
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return &APIError{StatusCode: resp.StatusCode, Body: respBody}
	}
	if out != nil {
		return json.Unmarshal(respBody, out)
	}
	return nil
}

// deleteRequest sends DELETE to fullURL and returns *APIError for non-2xx responses.
func (c *Client) deleteRequest(ctx context.Context, fullURL string) error {
	makeReq := func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, "DELETE", fullURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.accessToken)
		return req, nil
	}

	resp, body, err := c.doWithRetry(ctx, makeReq)
	if err != nil {
		return fmt.Errorf("DELETE %s: %w", fullURL, err)
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent, http.StatusAccepted:
		return nil
	default:
		return &APIError{StatusCode: resp.StatusCode, Body: body}
	}
}
