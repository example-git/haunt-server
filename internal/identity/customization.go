package identity

import (
	"bytes"
	"context"
	"image"
	_ "image/jpeg"
	"image/png"
	"regexp"
	"strings"
	"unicode/utf8"
)

type ProfileSettingsRequest struct {
	DisplayName string `json:"display_name" maxLength:"64"`
	Bio         string `json:"bio" maxLength:"500"`
	AccentColor string `json:"accent_color" required:"true" pattern:"^#[0-9a-fA-F]{6}$"`
}
type CommunitySettings struct {
	Name          string `json:"name" minLength:"1" maxLength:"100"`
	Description   string `json:"description" maxLength:"1000"`
	AccentColor   string `json:"accent_color" pattern:"^#[0-9a-fA-F]{6}$"`
	IconVersion   string `json:"icon_version,omitempty"`
	BannerVersion string `json:"banner_version,omitempty"`
}

var colorPattern = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func validText(value string, max int) bool {
	return utf8.ValidString(value) && utf8.RuneCountInString(value) <= max && !strings.ContainsRune(value, '\x00')
}
func (s *Service) Profile(ctx context.Context, id string) (User, error) {
	users, err := s.Directory(ctx, "", id)
	if err != nil {
		return User{}, err
	}
	if len(users) != 1 {
		return User{}, ErrNotFound
	}
	return users[0], nil
}
func (s *Service) SaveProfile(ctx context.Context, p Principal, v ProfileSettingsRequest) (User, error) {
	v.DisplayName = strings.TrimSpace(v.DisplayName)
	if !validText(v.DisplayName, 64) || strings.ContainsAny(v.DisplayName, "\r\n") || !validText(v.Bio, 500) || !colorPattern.MatchString(v.AccentColor) {
		return User{}, ErrInvalid
	}
	_, err := s.pool.Exec(ctx, `UPDATE chatxo.users SET display_name=$3,bio=$4,accent_color=$5 WHERE instance_id=$1 AND id=$2 AND NOT disabled AND NOT kicked AND deleted_at IS NULL`, s.instanceID, p.User.ID, v.DisplayName, v.Bio, strings.ToLower(v.AccentColor))
	if err != nil {
		return User{}, translate(err)
	}
	return s.Profile(ctx, p.User.ID)
}
func (s *Service) Community(ctx context.Context, defaultName string) (CommunitySettings, error) {
	var v CommunitySettings
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(NULLIF(name,''),$2),description,accent_color,icon_version,banner_version FROM chatxo.instances WHERE id=$1`, s.instanceID, defaultName).Scan(&v.Name, &v.Description, &v.AccentColor, &v.IconVersion, &v.BannerVersion)
	return v, translate(err)
}
func (s *Service) SaveCommunity(ctx context.Context, p Principal, v CommunitySettings) (CommunitySettings, error) {
	if !p.Can(ManageServer) {
		return CommunitySettings{}, ErrForbidden
	}
	v.Name = strings.TrimSpace(v.Name)
	if !validName(v.Name, 100) || !validText(v.Description, 1000) || !colorPattern.MatchString(v.AccentColor) {
		return CommunitySettings{}, ErrInvalid
	}
	_, err := s.pool.Exec(ctx, `UPDATE chatxo.instances SET name=$2,description=$3,accent_color=$4 WHERE id=$1`, s.instanceID, v.Name, v.Description, strings.ToLower(v.AccentColor))
	if err != nil {
		return CommunitySettings{}, translate(err)
	}
	return s.Community(ctx, v.Name)
}

// Static art is canonicalized as PNG; validated GIF animation is preserved. Art does not
// expire with message attachments. Metadata queries never load image bytes.
func normalizeArt(raw []byte, banner bool) ([]byte, error) {
	maxBytes, maxWidth, maxHeight := 2<<20, 1024, 1024
	if banner {
		maxBytes, maxWidth, maxHeight = 4<<20, 2048, 1024
	}
	if len(raw) >= 6 && (string(raw[:6]) == "GIF87a" || string(raw[:6]) == "GIF89a") {
		if len(raw) > MaxArtBytes {
			return nil, ErrInvalid
		}
		if err := validateGIF(raw, maxWidth, maxHeight); err != nil {
			return nil, err
		}
		return raw, nil
	}
	if len(raw) == 0 || len(raw) > maxBytes {
		return nil, ErrInvalid
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || (format != "png" && format != "jpeg") || cfg.Width < 1 || cfg.Height < 1 || cfg.Width > maxWidth || cfg.Height > maxHeight {
		return nil, ErrInvalid
	}
	decoded, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, ErrInvalid
	}
	var out bytes.Buffer
	if err = png.Encode(&out, decoded); err != nil {
		return nil, ErrInvalid
	}
	if out.Len() > maxBytes {
		return nil, ErrInvalid
	}
	return out.Bytes(), nil
}
func (s *Service) SaveArt(ctx context.Context, p Principal, kind string, raw []byte) (string, error) {
	if kind != "avatar" && kind != "icon" && kind != "banner" {
		return "", ErrInvalid
	}
	if kind != "avatar" && !p.Can(ManageServer) {
		return "", ErrForbidden
	}
	version := ""
	var data []byte
	if raw != nil {
		// Bound simultaneous image decoding independently of database traffic.
		select {
		case s.artSlots <- struct{}{}:
			defer func() { <-s.artSlots }()
		default:
			return "", ErrBusy
		}
		var err error
		data, err = normalizeArt(raw, kind == "banner")
		if err != nil {
			return "", err
		}
		version = newID()
	}
	// SQL identifiers come only from the fixed kinds checked above.
	var err error
	if kind == "avatar" {
		_, err = s.pool.Exec(ctx, `UPDATE chatxo.users SET avatar=$3,avatar_version=$4 WHERE instance_id=$1 AND id=$2 AND NOT disabled AND NOT kicked AND deleted_at IS NULL`, s.instanceID, p.User.ID, data, version)
	} else {
		_, err = s.pool.Exec(ctx, `UPDATE chatxo.instances SET `+kind+`=$2,`+kind+`_version=$3 WHERE id=$1`, s.instanceID, data, version)
	}
	return version, translate(err)
}
func (s *Service) Art(ctx context.Context, kind, id string) ([]byte, error) {
	var data []byte
	var err error
	switch kind {
	case "avatar":
		if !ValidID(id) {
			return nil, ErrInvalid
		}
		err = s.pool.QueryRow(ctx, `SELECT avatar FROM chatxo.users WHERE instance_id=$1 AND id=$2`, s.instanceID, id).Scan(&data)
	case "icon", "banner":
		err = s.pool.QueryRow(ctx, `SELECT `+kind+` FROM chatxo.instances WHERE id=$1`, s.instanceID).Scan(&data)
	default:
		return nil, ErrInvalid
	}
	if err != nil {
		return nil, translate(err)
	}
	if len(data) == 0 {
		return nil, ErrNotFound
	}
	return data, nil
}
