// Package rest is a small JSON over HTTP client. The Forgejo, Harbor and
// Kubernetes clients use it.
package rest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client sends JSON requests to one service.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	// Auth adds credentials to a request.
	Auth func(r *http.Request) error
}

// StatusError is a response with a status code that is not 2xx.
type StatusError struct {
	Method string
	URL    string
	Code   int
	Body   string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s %s: status %d: %s", e.Method, e.URL, e.Code, e.Body)
}

// HasStatus tells if err is a StatusError with the code.
func HasStatus(err error, code int) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == code
}

// NewHTTPClient returns an HTTP client with a timeout.
func NewHTTPClient(transport http.RoundTripper) *http.Client {
	return &http.Client{Timeout: 30 * time.Second, Transport: transport}
}

// Do sends a request. If in is not nil, Do sends it as JSON. If out is not
// nil, Do decodes the response body into it.
func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	return c.DoContent(ctx, method, path, "application/json", in, out)
}

// DoContent is Do with a different request content type.
func (c *Client) DoContent(ctx context.Context, method, path, contentType string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(b)
	}
	url := strings.TrimRight(c.BaseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "squared")
	if in != nil {
		req.Header.Set("Content-Type", contentType)
	}
	if c.Auth != nil {
		if err := c.Auth(req); err != nil {
			return err
		}
	}
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = NewHTTPClient(nil)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Limit the size: a bad server must not make the process use too much
	// memory.
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return fmt.Errorf("%s %s: read response: %w", method, url, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := strings.TrimSpace(string(data))
		if len(msg) > 500 {
			msg = msg[:500]
		}
		return &StatusError{Method: method, URL: url, Code: resp.StatusCode, Body: msg}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("%s %s: decode response: %w", method, url, err)
		}
	}
	return nil
}
