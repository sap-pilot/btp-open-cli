package cf

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	apiBaseURL  string
	accessToken string
	http        *http.Client
}

func NewClient(apiBaseURL, accessToken string) *Client {
	return &Client{
		apiBaseURL:  strings.TrimRight(apiBaseURL, "/"),
		accessToken: accessToken,
		http:        &http.Client{Timeout: 60 * time.Second},
	}
}

func (c *Client) BaseURL() string {
	return c.apiBaseURL
}

func (c *Client) get(ctx context.Context, fullURL string, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, "GET", fullURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.accessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", fullURL, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
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
	req, err := http.NewRequestWithContext(ctx, "POST", fullURL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", fullURL, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return &APIError{StatusCode: resp.StatusCode, Body: respBody}
	}
	if out != nil {
		return json.Unmarshal(respBody, out)
	}
	return nil
}
