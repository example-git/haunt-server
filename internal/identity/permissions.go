package identity

import "slices"

// PermissionDefinition is the role editor and channel authorization catalog.
// Kinds lists the channel types where this permission controls an operation.
type PermissionDefinition struct {
	Key     string   `json:"key"`
	Bit     int64    `json:"bit"`
	Label   string   `json:"label"`
	Kinds   []string `json:"kinds,omitempty"`
	Default bool     `json:"default,omitempty"`
}

var allChannelKinds = []string{"text", "voice", "ai"}
var voiceKind = []string{"voice"}
var aiKind = []string{"ai"}

var RolePermissions = []PermissionDefinition{
	{"Administrator", Administrator, "Administrator", nil, false},
	{"ViewChannel", ViewChannel, "View channels", allChannelKinds, true},
	{"SendMessages", SendMessages, "Send messages", allChannelKinds, true},
	{"ConnectVoice", ConnectVoice, "Join voice channels", voiceKind, true},
	{"UseAI", UseAI, "Request AI responses", aiKind, true},
	{"SwitchAIPreset", SwitchAIPreset, "Switch AI presets", aiKind, true},
	{"CreateInvites", CreateInvites, "Create invitations", nil, false},
	{"ManageChannels", ManageChannels, "Manage channels", allChannelKinds, false},
	{"ManageServer", ManageServer, "Manage server appearance & emojis", nil, false},
	{"ManageRoles", ManageRoles, "Manage lower-ranked roles", nil, false},
	{"KickMembers", KickMembers, "Kick members", nil, false},
	{"BanMembers", BanMembers, "Ban members & review appeals", nil, false},
	{"MuteMembers", MuteMembers, "Mute members", voiceKind, false},
	{"DeafenMembers", DeafenMembers, "Deafen members", voiceKind, false},
	{"DeleteMembers", DeleteMembers, "Delete accounts", nil, false},
	{"ViewAuditLog", ViewAuditLog, "View audit log", nil, false},
	{"ViewMemberIPs", ViewMemberIPs, "View member IPs & manage IP bans", nil, false},
	{"ManageMessages", ManageMessages, "Delete channel messages", allChannelKinds, false},
	{"ResetPasswords", ResetPasswords, "Reset member passwords", nil, false},
	{"ManageConfiguration", ManageConfiguration, "Manage configuration, storage & extensions", nil, false},
}

var ChannelPermissions, DefaultPermissions = permissionMasks()

func permissionMasks() (channel, defaults int64) {
	for _, p := range RolePermissions {
		if len(p.Kinds) != 0 {
			channel |= p.Bit
		}
		if p.Default {
			defaults |= p.Bit
		}
	}
	return
}

// LegacyOverrideMask is the frozen pre-moderation override protocol.
const LegacyOverrideMask = ViewChannel | SendMessages | ConnectVoice | UseAI | SwitchAIPreset

func channelPermissionMask(kind string) (bits int64) {
	for _, p := range RolePermissions {
		if slices.Contains(p.Kinds, kind) {
			bits |= p.Bit
		}
	}
	return bits
}
