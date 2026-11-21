package codex

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/example-git/haunt-server/internal/ai/provider"
	"github.com/example-git/haunt-server/internal/ai/providers/wire"
)

const imageBaseURL = "https://chatgpt.com/backend-api/codex/images/"

func (c *Client) Image(ctx context.Context, cred provider.Credentials, prompt string, sources []provider.Part, maxBytes int64) ([]byte, error) {
	if cred.AccessToken == "" || cred.AccountID == "" {
		return nil, &provider.Error{Kind: provider.RequiresAuth, Message: "Reconnect the selected Codex account to use image tools."}
	}
	id, err := c.Identity.Snapshot(provider.Codex)
	if err != nil {
		return nil, err
	}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+cred.AccessToken)
	header.Set("ChatGPT-Account-ID", cred.AccountID)
	header.Set("User-Agent", id.UserAgent)
	header.Set("originator", id.Originator)
	header.Set("version", id.Version)
	body := map[string]any{"model": "gpt-image-2", "prompt": prompt, "quality": "auto", "size": "auto", "background": "auto"}
	route := "generations"
	if len(sources) > 0 {
		route = "edits"
		images := make([]map[string]string, 0, len(sources))
		for _, source := range sources {
			images = append(images, map[string]string{"image_url": "data:" + source.MIMEType + ";base64," + base64.StdEncoding.EncodeToString(source.Data)})
		}
		body["images"] = images
	}
	resp, err := wire.StreamRequest(ctx, c.HTTP, imageBaseURL+route, body, header, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	maxBytes = min(maxBytes, 100<<20)
	if maxBytes < 1 {
		return nil, errors.New("image exceeds channel attachment limit")
	}
	limit := int64(base64.StdEncoding.EncodedLen(int(maxBytes))) + 1<<16
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("Codex image exceeds channel attachment limit")
	}
	var result struct {
		Data []struct {
			Image []byte `json:"b64_json"`
		} `json:"data"`
	}
	if err = json.NewDecoder(bytes.NewReader(b)).Decode(&result); err != nil || len(result.Data) != 1 || len(result.Data[0].Image) == 0 {
		return nil, errors.New("Codex returned no usable image")
	}
	if int64(len(result.Data[0].Image)) > maxBytes {
		return nil, errors.New("Codex image exceeds channel attachment limit")
	}
	return result.Data[0].Image, nil
}
