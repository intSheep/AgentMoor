package platform

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"agentmoor/internal/asset"
)

// Bound waiting for headers while allowing large objects to stream.
var workerTransport = func() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 2 * time.Minute
	return transport
}()

type Client struct {
	endpoint, token string
	tokenFile       string
	http            *http.Client
}

// HTTPError exposes the status without requiring callers to parse error text.
// Error and Unwrap preserve the existing messages and missing/conflict identity.
type HTTPError struct {
	StatusCode int
	Message    string
}

func (e *HTTPError) Error() string {
	switch e.StatusCode {
	case http.StatusNotFound:
		return ErrMissing.Error()
	case http.StatusConflict:
		return fmt.Sprintf("%s: %s", ErrConflict, e.Message)
	default:
		return fmt.Sprintf("platform HTTP %d: %s", e.StatusCode, e.Message)
	}
}

func (e *HTTPError) Unwrap() error {
	switch e.StatusCode {
	case http.StatusNotFound:
		return ErrMissing
	case http.StatusConflict:
		return ErrConflict
	default:
		return nil
	}
}

func NewClient(endpoint, token string) (*Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || token == "" {
		return nil, fmt.Errorf("invalid platform endpoint or missing AGENTMOOR_TOKEN")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (net.ParseIP(u.Hostname()).IsLoopback() || u.Hostname() == "localhost")) {
		return nil, fmt.Errorf("remote platform endpoints require HTTPS")
	}
	return &Client{endpoint: strings.TrimSuffix(endpoint, "/"), token: token, http: &http.Client{Transport: workerTransport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// NewFileClient picks up atomic credential rotations without restarting watch.
func NewFileClient(endpoint, file string) (*Client, error) {
	c, err := NewClient(endpoint, "file")
	if err != nil {
		return nil, err
	}
	c.tokenFile = file
	return c, nil
}

func (c *Client) request(ctx context.Context, method, path string, body io.Reader, length int64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, body)
	if err != nil {
		return nil, err
	}
	token := c.token
	if c.tokenFile != "" {
		b, err := os.ReadFile(c.tokenFile)
		if err != nil {
			return nil, err
		}
		token = strings.TrimSpace(string(b))
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if length >= 0 {
		req.ContentLength = length
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, &HTTPError{StatusCode: resp.StatusCode, Message: strings.TrimSpace(string(msg))}
	}
	return resp, nil
}

func (c *Client) JSON(ctx context.Context, method, path string, input, output any) error {
	var body io.Reader
	if input != nil {
		b, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	resp, err := c.request(ctx, method, path, body, -1)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(output)
}

func (c *Client) Context(ctx context.Context) (Grant, error) {
	var g Grant
	err := c.JSON(ctx, "GET", "/v1/context", nil, &g)
	return g, err
}

func (c *Client) Publish(ctx context.Context, ch asset.Change, file string) (int64, error) {
	var t asset.Ticket
	if err := c.JSON(ctx, "POST", "/v1/changes", ch, &t); err != nil {
		return 0, err
	}
	if t.Version > 0 {
		return t.Version, nil
	}
	path := "/v1/changes/" + t.ID
	if t.Upload {
		f, err := os.Open(file)
		if err != nil {
			return 0, err
		}
		defer f.Close()
		resp, err := c.request(ctx, "PUT", path+"/content", f, ch.Size)
		if err != nil {
			return 0, err
		}
		resp.Body.Close()
	}
	err := c.JSON(ctx, "POST", path+"/commit", nil, &t)
	return t.Version, err
}

func (c *Client) Content(ctx context.Context, k asset.Key, version int64) (asset.Revision, io.ReadCloser, error) {
	var rev asset.Revision
	resp, err := c.request(ctx, "GET", k.ContentPath(version), nil, -1)
	if err != nil {
		return rev, nil, err
	}
	data, err := base64.RawURLEncoding.DecodeString(resp.Header.Get("X-Agentmoor-Revision"))
	if err != nil || json.Unmarshal(data, &rev) != nil || rev.Key != k || rev.Version <= 0 || (version != 0 && version != rev.Version) || (asset.Change{Metadata: rev.Metadata}).Validate() != nil {
		resp.Body.Close()
		return rev, nil, fmt.Errorf("invalid platform revision")
	}
	return rev, resp.Body, nil
}
