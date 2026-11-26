package platform

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"

	"agentmoor/internal/asset"
	"agentmoor/internal/snapshot"
)

// PublishTarParts transfers missing file bodies from an already frozen tar.
// The logical revision, operation identity and public content remain unchanged.
func (c *Client) PublishTarParts(ctx context.Context, ch asset.Change, file string) (int64, error) {
	if err := ch.Validate(); err != nil {
		return 0, err
	}
	if ch.Kind != "workspace" || ch.Format != "workspace-tar.v1" || ch.MediaType != "application/x-tar" {
		return 0, fmt.Errorf("tar parts require a workspace tar")
	}
	var ticket asset.Ticket
	if err := c.JSON(ctx, "POST", "/v1/changes", ch, &ticket); err != nil {
		return 0, err
	}
	if ticket.ID != asset.Fingerprint(ch) || ticket.Version < 0 {
		return 0, fmt.Errorf("invalid publication ticket")
	}
	if ticket.Version > 0 {
		return ticket.Version, nil
	}
	path := "/v1/changes/" + ticket.ID
	if ticket.Upload {
		parts, ranges, err := snapshot.PartitionTar(ctx, snapshot.Blob{Path: file, SHA256: ch.SHA256, Size: ch.Size})
		if err != nil {
			return 0, err
		}
		var response struct {
			Missing []asset.TarPart `json:"missing"`
		}
		if err := c.prepareTarParts(ctx, path, parts, &response); err != nil {
			return 0, err
		}
		seen := make(map[string]bool, len(response.Missing))
		for _, part := range response.Missing {
			section, ok := ranges[part.SHA256]
			if !ok || part.Size != section.Size || len(part.Inline) != 0 || seen[part.SHA256] {
				return 0, fmt.Errorf("invalid missing tar part")
			}
			seen[part.SHA256] = true
		}
		if err := c.uploadTarParts(ctx, path, file, response.Missing, ranges); err != nil {
			return 0, err
		}
	}
	if err := c.JSON(ctx, "POST", path+"/commit", nil, &ticket); err != nil {
		return 0, err
	}
	if ticket.ID != asset.Fingerprint(ch) || ticket.Version <= 0 {
		return 0, fmt.Errorf("invalid committed publication ticket")
	}
	return ticket.Version, nil
}

func (c *Client) prepareTarParts(ctx context.Context, path string, parts asset.TarParts, output any) error {
	raw, err := json.Marshal(parts)
	if err != nil {
		return err
	}
	if len(raw) > asset.MaxTarPartsManifestBytes {
		return fmt.Errorf("tar parts manifest exceeds size limit")
	}
	var encoded bytes.Buffer
	zw := gzip.NewWriter(&encoded)
	if _, err := zw.Write(raw); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	response, err := c.requestEncoded(ctx, "POST", path+"/parts", bytes.NewReader(encoded.Bytes()), int64(encoded.Len()), "gzip")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(response.Body, asset.MaxTarPartsManifestBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("invalid tar parts response")
	}
	return nil
}

func (c *Client) uploadTarParts(ctx context.Context, path, file string, missing []asset.TarPart, ranges map[string]snapshot.PartRange) error {
	if len(missing) == 0 {
		return nil
	}
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	jobs := make(chan asset.TarPart)
	var workers sync.WaitGroup
	var first sync.Once
	var failure error
	for i := 0; i < min(8, len(missing)); i++ {
		workers.Go(func() {
			for {
				select {
				case <-ctx.Done():
					return
				case part, ok := <-jobs:
					if !ok {
						return
					}
					section := ranges[part.SHA256]
					body := io.NewSectionReader(f, section.Offset, section.Size)
					response, err := c.request(ctx, "PUT", path+"/parts/"+part.SHA256, body, section.Size)
					if err == nil {
						err = response.Body.Close()
					}
					if err != nil {
						first.Do(func() { failure = err; stop() })
						return
					}
				}
			}
		})
	}
send:
	for _, part := range missing {
		select {
		case jobs <- part:
		case <-ctx.Done():
			break send
		}
	}
	close(jobs)
	workers.Wait()
	if failure != nil {
		return failure
	}
	return ctx.Err()
}
