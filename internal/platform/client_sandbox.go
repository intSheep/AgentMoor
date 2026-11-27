package platform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
)

func validSandboxRoute(path string) bool {
	u, err := url.Parse(path)
	return err == nil && strings.HasPrefix(path, "/v1/tasks/") && strings.HasSuffix(path, "/sandbox") &&
		u.Scheme == "" && u.Host == "" && u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && u.String() == path
}

// SandboxTask requests contain only the private claim. The platform derives
// scope, task, parent, pins, image and native configuration from trusted state.
func (c *Client) SandboxTask(ctx context.Context, path, operation, attempt string) (SandboxTaskStatus, error) {
	if !validSandboxRoute(path) || !sandboxUUID(attempt) || (operation != "" && operation != "/status" && operation != "/cancel") {
		return SandboxTaskStatus{}, errBadRequest
	}
	raw, _ := json.Marshal(struct {
		Attempt string `json:"attempt"`
	}{attempt})
	response, err := c.request(ctx, "POST", path+operation, bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return SandboxTaskStatus{}, err
	}
	defer response.Body.Close()
	var status SandboxTaskStatus
	raw, err = io.ReadAll(io.LimitReader(response.Body, 2<<20+1))
	if err != nil {
		return status, fmt.Errorf("sandbox status was interrupted: %w", err)
	}
	if len(raw) > 2<<20 {
		return status, fmt.Errorf("sandbox status exceeds its bound")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	if err = d.Decode(&status); err != nil {
		return status, err
	}
	if err = d.Decode(new(any)); err != io.EOF {
		return status, fmt.Errorf("invalid sandbox status response")
	}
	return status, nil
}

// SandboxOutput verifies the outer envelope against independently received
// sealed metadata. Callers must read to EOF and check errors before execution
// or publication; hashes contained inside an untrusted archive are insufficient.
func (c *Client) SandboxOutput(ctx context.Context, path, attempt string, expected SandboxBlob) (io.ReadCloser, error) {
	if !validSandboxRoute(path) || !sandboxUUID(attempt) || !expected.valid() || !expected.Sealed || expected.Size < 1 {
		return nil, errBadRequest
	}
	raw, _ := json.Marshal(struct {
		Attempt string `json:"attempt"`
	}{attempt})
	response, err := c.request(ctx, "POST", path+"/output", bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, err
	}
	if response.Header.Get("X-Agentmoor-SHA256") != expected.SHA256 || response.Header.Get("X-Agentmoor-Size") != strconv.FormatInt(expected.Size, 10) ||
		(response.ContentLength >= 0 && response.ContentLength != expected.Size) {
		response.Body.Close()
		return nil, ErrConflict
	}
	return &sandboxVerifiedReader{ctx: ctx, stream: response.Body, reader: io.LimitReader(response.Body, expected.Size+1), expected: expected, hash: sha256.New()}, nil
}
