// Shared wire types are used by handlers and automatic documentation.
package httpapi

import "github.com/example-git/haunt-server/internal/voice"

type ChangePasswordRequest struct {
	Current  string `json:"current_password" required:"true" format:"password"`
	Password string `json:"new_password" required:"true" minLength:"8" description:"At most 1024 UTF-8 bytes." format:"password"`
}

type DisableMemberRequest struct {
	Disabled *bool `json:"disabled" required:"true" nullable:"false"`
}

type ResetPasswordRequest struct {
	Password string `json:"new_password" required:"true" minLength:"8" description:"At most 1024 UTF-8 bytes." format:"password"`
}

type CreateInviteRequest struct {
	Uses  int `json:"uses" required:"true" minimum:"1" maximum:"100"`
	Hours int `json:"expires_in_hours" required:"true" minimum:"1" maximum:"720"`
}

type SaveRoleRequest struct {
	DisplaySeparately *bool  `json:"display_separately,omitempty" description:"Show online members separately in the member list. Defaults to false for new roles; omission preserves existing settings."`
	Position          *int   `json:"position,omitempty" minimum:"0" maximum:"1000" description:"Role rank; default role is zero. Delegates can only manage lower-ranked roles and members."`
	Name              string `json:"name" required:"true" minLength:"1" maxLength:"64"`
	Permissions       *int64 `json:"permissions" required:"true" nullable:"false" minimum:"0" maximum:"32767"`
}

type AssignRolesRequest struct {
	Roles *[]string `json:"role_ids" required:"true" nullable:"false" description:"Complete replacement; an empty array removes assigned nondefault roles."`
}

type SaveCategoryRequest struct {
	Name string `json:"name" required:"true" minLength:"1" maxLength:"64"`
}

type SaveChannelRequest struct {
	Name     string  `json:"name" required:"true" minLength:"1" maxLength:"64"`
	Kind     string  `json:"kind" required:"true" enum:"text,voice"`
	Category *string `json:"category_id"`
	Topic    *string `json:"topic,omitempty" maxLength:"512"`
	Icon     *string `json:"icon,omitempty" maxLength:"32" description:"Built-in SVG icon key; empty restores the default for the channel kind. Omission preserves the current icon." enum:",hash,headphones,message-circle,megaphone,volume-2,mic,music,radio,gamepad-2,monitor,video,camera,image,film,code,terminal,book-open,graduation-cap,coffee,users,calendar,star,heart,shield,wrench"`
}

type SetPermissionsRequest struct {
	Allow *int64 `json:"allow" required:"true" nullable:"false" minimum:"0" maximum:"7"`
	Deny  *int64 `json:"deny" required:"true" nullable:"false" minimum:"0" maximum:"7"`
}

type CredentialsRequest struct {
	Username   string `json:"username" required:"true" pattern:"^[a-zA-Z0-9_]{3,32}$"`
	Password   string `json:"password" required:"true" format:"password" description:"Registration requires at least 8 characters; at most 1024 UTF-8 bytes."`
	Device     string `json:"device_name" required:"true" minLength:"1" maxLength:"128"`
	Invitation string `json:"invitation,omitempty" description:"Required for registration; unused on login."`
	Remember   bool   `json:"remember,omitempty" description:"Keep this device signed in until revocation, with no calendar expiry. Defaults to false."`
}

type RestoreSessionRequest struct {
	Device string `json:"device_name" required:"true" minLength:"1" maxLength:"128"`
}

type RememberSessionRequest struct {
	Remember *bool `json:"remember" required:"true" nullable:"false"`
}

type CreateDMRequest struct {
	Members []string `json:"members" required:"true" minItems:"1" maxItems:"15" description:"Other participant UUIDs; the current user is added automatically."`
}

type CreateMessageRequest struct {
	Body        string   `json:"body" description:"Nonblank body or at least one attachment. UTF-8 bytes are bounded by installation policy (at most 64 KiB)."`
	Request     string   `json:"request_id" required:"true" format:"uuid"`
	Reply       string   `json:"reply_to_id,omitempty" format:"uuid" description:"Message in the same conversation to reference as a reply."`
	Attachments []string `json:"attachment_ids" maxItems:"8"`
}

type EditMessageRequest struct {
	Body string `json:"body"`
}

type MediaAuthenticateCommand struct {
	Type        string `json:"type" required:"true" enum:"authenticate"`
	Token       string `json:"token" required:"true"`
	Participant string `json:"participant_id" required:"true" format:"uuid"`
}

type MediaCommand struct {
	Type      string               `json:"type" required:"true" enum:"configure,keyframe"`
	Tracks    []voice.EncodedTrack `json:"tracks,omitempty"`
	Publisher string               `json:"publisher_id,omitempty"`
	Purpose   string               `json:"purpose,omitempty"`
}

type TemporaryPasswordRequest struct {
	Reason string `json:"reason" required:"true" minLength:"1" maxLength:"512"`
}
