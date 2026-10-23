package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	ErrPasswordChange = errors.New("password_change_required")
	ErrSamePassword   = errors.New("password_unchanged")
	ErrKicked         = errors.New("kicked")
	ErrInvalid        = errors.New("invalid_request")
	ErrUnauthorized   = errors.New("unauthorized")
	ErrForbidden      = errors.New("forbidden")
	ErrNotFound       = errors.New("not_found")
	ErrConflict       = errors.New("conflict")
	ErrBusy           = errors.New("authentication_busy")
	ErrInvite         = errors.New("invalid_invitation")
	ErrSessionLimit   = errors.New("session_limit_reached")
)

const (
	ViewChannel int64 = 1 << iota
	SendMessages
	ConnectVoice
	CreateInvites
	ManageChannels
	ManageServer
	ManageRoles
	KickMembers
	BanMembers
	DeleteMembers
	ViewAuditLog
	ViewMemberIPs
	ManageMessages
	ResetPasswords
	ManageConfiguration
	Administrator
	MuteMembers
	DeafenMembers
	AllPermissions     int64 = (1 << 18) - 1
	ChannelPermissions       = ViewChannel | SendMessages | ConnectVoice
)

type User struct {
	ID            string       `json:"id"`
	Username      string       `json:"username"`
	Owner         bool         `json:"is_owner"`
	CreatedAt     time.Time    `json:"created_at"`
	DisplayName   string       `json:"display_name,omitempty"`
	Bio           string       `json:"bio,omitempty"`
	AccentColor   string       `json:"accent_color,omitempty"`
	AvatarVersion string       `json:"avatar_version,omitempty"`
	DisplayRole   *DisplayRole `json:"display_role,omitempty" description:"Highest-ranked role enabled for member-list sections; display metadata only. Absent when no role is enabled."`
}
type DisplayRole struct {
	ID       string `json:"id" required:"true"`
	Name     string `json:"name" required:"true"`
	Position int    `json:"position" required:"true"`
}
type Principal struct {
	User        User   `json:"user"`
	SessionID   string `json:"session_id"`
	Permissions int64  `json:"permissions"`
	Position    int    `json:"role_position"`
}

func (p Principal) Can(bits int64) bool {
	return p.User.Owner || p.Permissions&Administrator != 0 || p.Permissions&bits == bits
}

type Session struct {
	ID         string     `json:"id"`
	DeviceName string     `json:"device_name"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at" description:"Null for a remembered device; otherwise the session's expiry in UTC. All sessions can be revoked."`
}
type Login struct {
	PasswordChangeRequired bool    `json:"password_change_required,omitempty" description:"Temporary password session: may only change the password or log out; expires after 15 minutes even when remember is true."`
	InstanceID             string  `json:"instance_id"`
	User                   User    `json:"user"`
	Session                Session `json:"session"`
	Token                  string  `json:"token"`
	TokenType              string  `json:"token_type"`
}
type Invite struct {
	ID            string    `json:"id"`
	CreatedBy     string    `json:"created_by"`
	RemainingUses int       `json:"remaining_uses"`
	CreatedAt     time.Time `json:"created_at"`
	ExpiresAt     time.Time `json:"expires_at"`
	Token         string    `json:"token,omitempty"`
}
type Role struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Permissions       int64  `json:"permissions"`
	Default           bool   `json:"is_default"`
	Position          int    `json:"position"`
	DisplaySeparately bool   `json:"display_separately" description:"Show online members in a separate member-list section. Disabled by default."`
}
type Category struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
type Channel struct {
	ID          string  `json:"id"`
	CategoryID  *string `json:"category_id"`
	Name        string  `json:"name"`
	Kind        string  `json:"kind"`
	Topic       string  `json:"topic,omitempty"`
	Icon        string  `json:"icon,omitempty"`
	Permissions int64   `json:"permissions,omitempty"`
}
type Override struct {
	RoleID string `json:"role_id"`
	Allow  int64  `json:"allow"`
	Deny   int64  `json:"deny"`
}

var usernamePattern = regexp.MustCompile(`^[a-z0-9_]{3,32}$`)
var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func ValidID(id string) bool { return uuidPattern.MatchString(id) }
func normalizeUsername(raw string) (string, error) {
	name := strings.ToLower(raw)
	if !usernamePattern.MatchString(name) {
		return "", ErrInvalid
	}
	return name, nil
}
func validPassword(password string) bool {
	return utf8.ValidString(password) && utf8.RuneCountInString(password) >= 8 && len(password) <= 1024
}
func validName(name string, max int) bool {
	return utf8.ValidString(name) && strings.TrimSpace(name) != "" && utf8.RuneCountInString(name) <= max && !strings.ContainsAny(name, "\x00\r\n")
}
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func NewID() string { return newID() }
func newToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}
func tokenHash(token string) []byte { hash := sha256.Sum256([]byte(token)); return hash[:] }
func validToken(token string) bool {
	b, err := base64.RawURLEncoding.DecodeString(token)
	return err == nil && len(b) == 32 && base64.RawURLEncoding.EncodeToString(b) == token
}
