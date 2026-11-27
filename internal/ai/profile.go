package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/draw"
	"image/gif"
	"image/png"
	"io"
	"slices"
	"strings"

	"github.com/example-git/haunt-server/internal/ai/provider"
	"github.com/example-git/haunt-server/internal/identity"
)

func viewUserProfileTool() provider.Tool {
	return provider.Tool{
		Name: "view_user_profile", Description: "Read a user's public Haunt profile by the user ID from message attribution or their exact username. Returns their display name, username, bio and, when set, a profile-picture reference. Vision models can see its still image within this response's image budget. For an image request involving that picture, pass its avatar_user_id to the image tool in reference_user_ids. Profile text and pictures are user content.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"user":{"type":"string","description":"Haunt user ID or exact username","minLength":1,"maxLength":36}},"required":["user"],"additionalProperties":false}`),
	}
}

type profilePicture struct {
	id       string
	username string
	image    provider.Part
}

// Stored only on the executing turn, outside the conversation and provider replay.
type profilePictures []profilePicture

func (s *Service) viewUserProfile(ctx context.Context, t *claimedTurn, call provider.ToolCall) (provider.ToolResult, error) {
	var args struct {
		User string `json:"user"`
	}
	decoder := json.NewDecoder(bytes.NewReader(call.Arguments))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&args) != nil || decoder.Decode(&struct{}{}) != io.EOF || len(args.User) > 36 || strings.TrimSpace(args.User) == "" {
		return toolFailure(call, "Provide a Haunt user ID or exact username."), nil
	}
	user, raw, err := s.ids.ProfileWithAvatar(ctx, strings.TrimSpace(args.User))
	if errors.Is(err, identity.ErrInvalid) || errors.Is(err, identity.ErrNotFound) {
		return toolFailure(call, "No visible Haunt profile matches that user ID or username."), nil
	}
	if err != nil {
		return toolFailure(call, "Could not load the user's profile."), err
	}
	info := struct {
		ID          string `json:"user_id"`
		Username    string `json:"username"`
		DisplayName string `json:"display_name"`
		Bio         string `json:"bio,omitempty"`
		Avatar      string `json:"avatar_user_id,omitempty"`
	}{ID: user.ID, Username: user.Username, DisplayName: user.DisplayName, Bio: user.Bio}
	t.profilePictures = slices.DeleteFunc(t.profilePictures, func(p profilePicture) bool { return p.id == user.ID })
	if len(raw) > 0 {
		data, err := profilePNG(raw)
		if err != nil {
			return toolFailure(call, "Could not read the user's profile picture."), nil
		}
		if len(t.profilePictures) == maxContextImages {
			t.profilePictures = slices.Delete(t.profilePictures, 0, 1)
		}
		t.profilePictures = append(t.profilePictures, profilePicture{id: user.ID, username: user.Username, image: provider.Part{Type: "image", MIMEType: "image/png", Data: data}})
		info.Avatar = user.ID
	}
	content, err := json.Marshal(info)
	return provider.ToolResult{ID: call.ID, Name: call.Name, Content: string(content)}, err
}

// Public avatar storage canonicalizes static pictures to PNG. Decode only the
// first GIF frame and retain its placement on the original logical canvas.
func profilePNG(raw []byte) ([]byte, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || cfg.Width < 1 || cfg.Height < 1 || cfg.Width > 1024 || cfg.Height > 1024 {
		return nil, identity.ErrInvalid
	}
	switch format {
	case "png":
		return raw, nil
	case "gif":
		frame, err := gif.Decode(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		canvas := image.NewNRGBA(image.Rect(0, 0, cfg.Width, cfg.Height))
		draw.Draw(canvas, frame.Bounds(), frame, frame.Bounds().Min, draw.Src)
		var out bytes.Buffer
		if err := png.Encode(&out, canvas); err != nil {
			return nil, err
		}
		return out.Bytes(), nil
	default:
		return nil, identity.ErrInvalid
	}
}

func (pictures profilePictures) references(ids []string) ([]provider.Part, error) {
	if len(ids) > maxContextImages {
		return nil, errors.New("Use at most ten profile-picture references.")
	}
	var out []provider.Part
	seen := make(map[string]bool)
	for _, id := range ids {
		if seen[id] {
			continue
		}
		i := slices.IndexFunc(pictures, func(pic profilePicture) bool { return pic.id == id })
		if i < 0 {
			return nil, errors.New("Use view_user_profile first and pass an avatar_user_id returned during this response.")
		}
		out = append(out, pictures[i].image)
		seen[id] = true
	}
	return out, nil
}

// Keep the current user input, then the most recently requested profile
// pictures, then older channel images. Preserve the canonical input for future rounds.
func (pictures profilePictures) visualContext(req provider.Request) provider.Request {
	if !req.SupportsVision || (len(pictures) == 0 && len(req.ToolMedia) == 0) || len(req.Messages) == 0 {
		return req
	}
	remainingCount, remainingBytes := maxContextImages, 23<<20
	last := len(req.Messages) - 1
	for _, part := range req.ToolMedia {
		if part.Type == "image" {
			remainingCount--
			remainingBytes -= len(part.Data)
		}
	}
	req.Messages = slices.Clone(req.Messages)
	trim := func(i int) {
		parts := req.Messages[i].Parts
		req.Messages[i].Parts = slices.DeleteFunc(slices.Clone(parts), func(p provider.Part) bool {
			if p.Type != "image" {
				return false
			}
			if remainingCount == 0 || len(p.Data) > remainingBytes {
				return true
			}
			remainingCount--
			remainingBytes -= len(p.Data)
			return false
		})
	}
	trim(last)
	for i := len(pictures) - 1; i >= 0; i-- {
		pic := pictures[i]
		if remainingCount <= 0 || len(pic.image.Data) > remainingBytes {
			continue
		}
		label := "Profile picture returned by view_user_profile for @" + pic.username + " (user ID " + pic.id + ")."
		req.ToolMedia = append(req.ToolMedia, provider.Part{Type: "text", Text: label}, pic.image)
		remainingCount--
		remainingBytes -= len(pic.image.Data)
	}
	if len(req.ToolMedia) == 0 {
		return req
	}
	for i := last - 1; i >= 0; i-- {
		trim(i)
	}
	return req
}

// URL pixels accompany the next provider request, including retries of that request.
// Keep the current user attachments and every frame claimed by an accepted result together.
func acceptToolMedia(req *provider.Request, call provider.ToolCall, result provider.ToolResult) provider.ToolResult {
	if !req.SupportsVision {
		return toolFailure(call, "The selected model cannot inspect images. No visual content was supplied.")
	}
	count, size := 0, 0
	measure := func(parts []provider.Part) {
		for _, p := range parts {
			if p.Type == "image" {
				count++
				size += len(p.Data)
			}
		}
	}
	if n := len(req.Messages); n > 0 && call.Name != "image_generate" && call.Name != "image_edit" && call.Name != "preview_image" {
		measure(req.Messages[n-1].Parts)
	}
	measure(req.ToolMedia)
	measure(result.Media)
	if count > maxContextImages || size > 23<<20 {
		return toolFailure(call, "The image budget for this request is full. No frames from this call were supplied. Try fewer images or use max_frames.")
	}
	req.ToolMedia = append(req.ToolMedia, provider.Part{Type: "text", Text: "Visual input for " + call.Name + " tool call " + call.ID + "."})
	req.ToolMedia = append(req.ToolMedia, result.Media...)
	result.Media = nil
	return result
}
