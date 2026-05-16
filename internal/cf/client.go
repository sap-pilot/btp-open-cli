package cf

import (
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
