package httpapi

import (
	"github.com/example-git/haunt-server/internal/attachments"
	"github.com/example-git/haunt-server/internal/audioplugin"
	"github.com/example-git/haunt-server/internal/blobstore"
	"github.com/example-git/haunt-server/internal/chat"
	"github.com/example-git/haunt-server/internal/config"
	"github.com/example-git/haunt-server/internal/identity"
	"github.com/example-git/haunt-server/internal/voice"
	"time"
)

type usersResponse struct {
	Users []identity.User `json:"users"`
}
type sessionsResponse struct {
	Sessions []identity.Session `json:"sessions"`
}
type invitesResponse struct {
	Invites []identity.Invite `json:"invites"`
}
type rolesResponse struct {
	Roles []identity.Role `json:"roles"`
}
type membersResponse struct {
	Members []identity.Member `json:"members"`
}
type categoriesResponse struct {
	Categories []identity.Category `json:"categories"`
}
type channelsResponse struct {
	Channels []identity.Channel `json:"channels"`
}
type overridesResponse struct {
	Overrides []identity.Override `json:"overrides"`
}
type MarkReadRequest struct {
	Seq int64 `json:"seq"`
}

type conversationsResponse struct {
	Conversations []chat.Conversation `json:"conversations"`
}
type participantsResponse struct {
	Participants []voice.Snapshot `json:"participants"`
}
type iceResponse struct {
	ICE     []config.ICEServer `json:"ice_servers"`
	Expires *time.Time         `json:"expires_at"`
	Policy  string             `json:"ice_transport_policy"`
}
type errorResponse struct {
	Error        string `json:"error"`
	BanType      string `json:"ban_type,omitempty"`
	AppealTicket string `json:"appeal_ticket,omitempty"`
	RequestID    string `json:"request_id,omitempty"`
}
type statusResponse struct {
	Status string `json:"status"`
	Check  string `json:"check,omitempty"`
}
type unavailableResponse struct {
	Status     string `json:"status"`
	Dependency string `json:"dependency"`
}
type statsResponse struct {
	Storage        map[string]any `json:"storage"`
	Policy         config.Policy  `json:"policy"`
	Pool           map[string]any `json:"pool"`
	StorageLock    map[string]any `json:"storage_lock"`
	Events         map[string]any `json:"events"`
	Media          map[string]any `json:"media"`
	Relay          map[string]any `json:"relay"`
	CloudflareTURN map[string]any `json:"cloudflare_turn"`
}
type authenticationInfo struct {
	Scheme       string `json:"scheme"`
	Registration string `json:"registration"`
	SessionDays  int64  `json:"session_days"`
}
type instanceResponse struct {
	ClientUpdates  *ClientUpdateSource `json:"client_updates,omitempty"`
	InstanceID     string              `json:"instance_id"`
	Name           string              `json:"name"`
	Description    string              `json:"description,omitempty"`
	AccentColor    string              `json:"accent_color,omitempty"`
	IconVersion    string              `json:"icon_version,omitempty"`
	BannerVersion  string              `json:"banner_version,omitempty"`
	PublicURL      string              `json:"public_url"`
	APIVersion     string              `json:"api_version"`
	Capabilities   []string            `json:"capabilities"`
	MediaTransport string              `json:"media_transport"`
	Authentication authenticationInfo  `json:"authentication"`
	Limits         config.Policy       `json:"limits"`
}
type afterQuery struct {
	After string `query:"after" description:"Exclusive UUID cursor; up to 100 results."`
}
type historyQuery struct {
	After int64 `query:"after" minimum:"0" default:"0" description:"Exclusive conversation sequence; zero starts at the oldest retained message. Pages are ascending by sequence."`
	Limit int   `query:"limit" minimum:"1" maximum:"100" default:"100"`
}
type uploadQuery struct {
	Encrypted string `query:"encrypted" description:"Set to 1 for an encrypted DM image; required for DM uploads."`
	Size      int64  `query:"size" required:"true" minimum:"1" description:"Exact byte count; enforce the installation's advertised limits."`
	Filename  string `query:"filename" required:"true"`
}
type rangeHeaders struct {
	Range string `header:"Range" description:"Optional HTTP byte range, for example bytes=0-1023."`
}

type endpointContract struct {
	id, summary               string
	input, output             any
	status                    int
	public                    bool
	query                     any
	description               string
	binaryInput, binaryOutput bool
	websocket                 bool
}

func endpointContracts() map[string]endpointContract {
	c := map[string]endpointContract{}
	add := func(pattern, id, summary string, in, out any, status int) {
		c[pattern] = endpointContract{id: id, summary: summary, input: in, output: out, status: status}
	}
	add("GET /healthz", "health", "Check process liveness", nil, statusResponse{}, 200)
	add("GET /readyz", "readiness", "Check database and identity schema readiness", nil, statusResponse{}, 200)
	add("GET /api/v1/instance", "instance", "Discover installation capabilities and limits", nil, instanceResponse{}, 200)
	add("GET /api/v1/client-updates/installers", "clientInstallers", "List locally available desktop installers when enabled", nil, ClientInstallers{}, 200)
	add("GET /api/v1/client-updates/haunt-updates.json", "clientUpdateManifest", "Get the signed desktop update manifest (local distribution enabled)", nil, ClientUpdateEnvelope{}, 200)
	add("GET /api/v1/client-updates/{version}/{name}", "clientUpdateFile", "Download an authenticated desktop release artifact", nil, nil, 200)
	add("POST /api/v1/auth/register", "register", "Register using an invitation", CredentialsRequest{}, identity.Login{}, 201)
	add("POST /api/v1/auth/login", "login", "Create a device session", CredentialsRequest{}, identity.Login{}, 200)
	add("POST /api/v1/auth/logout", "logout", "Revoke the current device session", nil, nil, 204)
	add("GET /api/v1/me/dm-storage", "dmStorage", "Get your DM storage and daily transfer usage", nil, attachments.DMUsage{}, 200)
	add("POST /api/v1/me/dm-storage/purge", "purgeDMUploads", "Preview or purge your older DM uploads; confirmed requests remove up to 128 files", DMPurgeRequest{}, attachments.DMPurge{}, 200)
	add("POST /api/v1/me/dm-devices", "registerDMDevice", "Register an immutable MLS device credential", chat.RegisterDMDevice{}, chat.DMDevice{}, 200)
	add("GET /api/v1/me/dm-devices/{device}/packages", "ownDMKeyPackages", "List your device’s unused public MLS key packages", nil, chat.DMKeyPackages{}, 200)
	add("PUT /api/v1/me/dm-devices/{device}/packages", "publishDMKeyPackages", "Publish single-use MLS key packages", chat.DMKeyPackages{}, nil, 204)
	add("GET /api/v1/conversations/{id}/encryption", "dmEncryptionBundle", "Get participant device credentials and unused key packages", nil, chat.DMCryptoBundle{}, 200)
	add("GET /api/v1/conversations/{id}/encryption/events", "dmEncryptionEvents", "Read ordered MLS group commits and welcome messages", nil, chat.DMCryptoEvents{}, 200)
	add("POST /api/v1/conversations/{id}/encryption/events", "commitDMEncryption", "Compare-and-swap an MLS group revision and consume single-use key packages", chat.DMCryptoCommit{}, chat.DMCryptoEvent{}, 200)
	add("GET /api/v1/me", "me", "Get the current principal", nil, identity.Principal{}, 200)
	add("POST /api/v1/sessions/restore", "restoreSession", "Create an independent device session from a valid backup session without revoking it or extending its expiry", RestoreSessionRequest{}, identity.Login{}, 201)
	add("PUT /api/v1/me/session", "rememberSession", "Remember or stop remembering the current device session", RememberSessionRequest{}, identity.Session{}, 200)
	add("PUT /api/v1/me/password", "changePassword", "Change password and revoke existing sessions", ChangePasswordRequest{}, nil, 204)
	add("PUT /api/v1/me/profile", "updateProfile", "Update your display name, bio and avatar color", identity.ProfileSettingsRequest{}, identity.User{}, 200)
	add("GET /api/v1/audio-plugins", "listAudioPlugins", "List local audio plugins and integrity hashes", nil, AudioPluginsResponse{}, 200)
	add("GET /api/v1/audio-plugins/{digest}/files/{name}", "downloadAudioPlugin", "Download an authenticated Wasm asset", nil, nil, 200)
	add("POST /api/v1/admin/audio-plugins", "installAudioPlugin", "Install a ZIP of Wasm modules and a declarative manifest, at most 50 MiB packed and unpacked (ManageConfiguration)", nil, audioplugin.Plugin{}, 201)
	add("DELETE /api/v1/admin/audio-plugins/{id}", "removeAudioPlugin", "Remove an audio plugin (ManageConfiguration)", nil, nil, 204)
	add("GET /api/v1/menus/server", "getServerMenu", "Permission-filtered declarative server settings and context menu", nil, MenuManifest{}, 200)
	add("GET /api/v1/channels/{id}/menu", "getChannelMenu", "Permission-filtered declarative channel settings and context menu", nil, MenuManifest{}, 200)
	add("GET /api/v1/instance/configuration", "getConfiguration", "Read live server configuration (ManageConfiguration)", nil, ConfigurationResponse{}, 200)
	add("PUT /api/v1/instance/configuration", "saveConfiguration", "Persist and apply live server policies; revision prevents lost edits (ManageConfiguration)", SaveConfigurationRequest{}, identity.RuntimeConfiguration{}, 200)
	add("GET /api/v1/admin/storage", "listStorage", "List attachment storage providers; credentials are never returned (ManageConfiguration)", nil, StorageProvidersResponse{}, 200)
	add("POST /api/v1/admin/storage", "registerStorage", "Verify write/read/delete access and register private attachment storage (ManageConfiguration)", blobstore.RegisterRequest{}, blobstore.Provider{}, 201)
	add("PUT /api/v1/admin/storage/active", "activateStorage", "Select the provider for new uploads; existing files retain their provider (ManageConfiguration)", ActivateStorageRequest{}, nil, 204)
	add("POST /api/v1/admin/storage/{id}/check", "checkStorage", "Verify provider write/read/delete access (ManageConfiguration)", nil, nil, 204)
	add("PUT /api/v1/admin/storage/{id}/credentials", "rotateStorageCredentials", "Verify and replace S3 credentials (ManageConfiguration)", blobstore.Credentials{}, nil, 204)
	add("DELETE /api/v1/admin/storage/{id}", "removeStorage", "Remove an inactive provider with no retained files (ManageConfiguration)", nil, nil, 204)
	add("PUT /api/v1/instance/settings", "updateInstance", "Update server name, description and accent color (ManageServer)", SaveCommunityRequest{}, identity.CommunitySettings{}, 200)
	for _, art := range []struct{ kind, path, get string }{{"avatar", "/api/v1/me/avatar", "/api/v1/users/{id}/avatar"}, {"icon", "/api/v1/instance/icon", "/api/v1/instance/icon"}, {"banner", "/api/v1/instance/banner", "/api/v1/instance/banner"}} {
		add("PUT "+art.path, "upload_"+art.kind, "Upload PNG/JPEG/GIF "+art.kind+"; icons/banners require ManageServer", nil, artResponse{}, 200)
		v := c["PUT "+art.path]
		v.binaryInput = true
		v.description = "Avatars/icons: at most 2 MiB and 1024×1024. Banners: at most 4 MiB and 2048×1024. PNG/JPEG content is decoded and saved as PNG. GIFs preserve animation and may be up to 20 MiB, with the same dimensions, at most 1000 frames and 256 million decoded pixels. Art persists until replaced or removed."
		c["PUT "+art.path] = v
		add("DELETE "+art.path, "delete_"+art.kind, "Remove "+art.kind+"; icons/banners require ManageServer", nil, nil, 204)
		add("GET "+art.get, "get_"+art.kind, "Download "+art.kind+" PNG or GIF", nil, nil, 200)
		v = c["GET "+art.get]
		v.binaryOutput = true
		v.public = art.kind != "avatar"
		c["GET "+art.get] = v
	}
	add("GET /api/v1/emojis", "listEmojis", "List this server's custom emoji catalog", nil, emojisResponse{}, 200)
	add("PUT /api/v1/emojis/{name}", "saveEmoji", "Create or replace a custom emoji (ManageServer)", nil, identity.Emoji{}, 200)
	emojiUpload := c["PUT /api/v1/emojis/{name}"]
	emojiUpload.binaryInput = true
	emojiUpload.description = "Raw PNG/JPEG/GIF, at most 1 MiB and 256×256. Names use 2–32 lowercase letters, digits or underscores. Maximum 100 emojis per server. Replacing a name preserves its ID; metadata changes arrive through directory.changed. Use :name: in channel or DM message bodies; unknown/deleted names remain literal text."
	c["PUT /api/v1/emojis/{name}"] = emojiUpload
	add("DELETE /api/v1/emojis/{id}", "deleteEmoji", "Delete a custom emoji (ManageServer)", nil, nil, 204)
	add("GET /api/v1/emojis/{id}/image", "getEmojiImage", "Download an authenticated emoji PNG or GIF", nil, nil, 200)
	emojiImage := c["GET /api/v1/emojis/{id}/image"]
	emojiImage.binaryOutput = true
	c["GET /api/v1/emojis/{id}/image"] = emojiImage
	add("GET /api/v1/users", "listUsers", "List public user profiles", nil, usersResponse{}, 200)
	add("GET /api/v1/users/{id}", "getUser", "Get one public user profile", nil, identity.User{}, 200)
	add("GET /api/v1/sessions", "listSessions", "List this account's device sessions", nil, sessionsResponse{}, 200)
	add("DELETE /api/v1/sessions/{id}", "revokeSession", "Revoke this account's device session", nil, nil, 204)
	add("POST /api/v1/invites", "createInvite", "Create an invitation", CreateInviteRequest{}, identity.Invite{}, 201)
	add("GET /api/v1/invites", "listInvites", "List visible invitations", nil, invitesResponse{}, 200)
	add("DELETE /api/v1/invites/{id}", "revokeInvite", "Revoke an invitation", nil, nil, 204)
	add("GET /api/v1/roles", "listRoles", "List community roles", nil, rolesResponse{}, 200)
	add("POST /api/v1/roles", "createRole", "Create a role (ManageRoles; lower rank only)", SaveRoleRequest{}, identity.Role{}, 201)
	add("PUT /api/v1/roles/{id}", "updateRole", "Update a role (ManageRoles; lower rank only)", SaveRoleRequest{}, identity.Role{}, 200)
	add("DELETE /api/v1/roles/{id}", "deleteRole", "Delete a nondefault role (ManageRoles; lower rank only)", nil, nil, 204)
	add("GET /api/v1/members", "listMembers", "List members and roles (any member-management permission)", nil, membersResponse{}, 200)
	add("PUT /api/v1/members/{id}/roles", "assignRoles", "Replace a member's roles (ManageRoles; lower rank only)", AssignRolesRequest{}, nil, 204)
	add("PUT /api/v1/admin/members/{id}", "disableMember", "Disable or enable a member (owner)", DisableMemberRequest{}, nil, 204)
	add("POST /api/v1/admin/members/{id}/password-reset", "issueTemporaryPassword", "Issue a temporary password and revoke sessions (Reset passwords permission; lower-ranked members only)", TemporaryPasswordRequest{}, identity.TemporaryPassword{}, 200)
	add("PUT /api/v1/admin/members/{id}/password", "resetMemberPassword", "Set a temporary member password, require replacement, and revoke sessions", ResetPasswordRequest{}, nil, 204)
	add("POST /api/v1/admin/members/{id}/moderate", "moderateMember", "Kick, ban, restore or delete a lower-ranked member", ModerateMemberRequest{}, nil, 204)
	add("GET /api/v1/admin/members/{id}/ips", "memberIPs", "Read private member IP history (ViewMemberIPs)", nil, memberIPsResponse{}, 200)
	add("GET /api/v1/admin/bans", "listBans", "List active bans (BanMembers)", nil, bansResponse{}, 200)
	add("POST /api/v1/admin/bans", "banNetwork", "Ban an IP address or CIDR range (BanMembers and ViewMemberIPs)", BanNetworkRequest{}, identity.Ban{}, 201)
	add("POST /api/v1/admin/bans/{id}/revoke", "revokeBan", "Lift a ban with an audit reason", ModerationReasonRequest{}, nil, 204)
	add("GET /api/v1/admin/appeals", "listAppeals", "List ban appeals (BanMembers)", nil, appealsResponse{}, 200)
	add("POST /api/v1/admin/appeals/{id}/review", "reviewAppeal", "Mark an appeal reviewed without lifting its ban", nil, nil, 204)
	add("GET /api/v1/admin/audit", "auditLog", "List recent moderation and sampled access events (ViewAuditLog)", nil, auditResponse{}, 200)
	auditContract := c["GET /api/v1/admin/audit"]
	auditContract.query = auditQuery{}
	c["GET /api/v1/admin/audit"] = auditContract
	for _, path := range []string{"/api/v1/admin/bans", "/api/v1/admin/appeals"} {
		v := c["GET "+path]
		v.query = afterQuery{}
		c["GET "+path] = v
	}
	add("POST /api/v1/auth/ban/status", "banStatus", "Read a ban using its opaque appeal ticket", BanTicketRequest{}, identity.BanStatus{}, 200)
	add("POST /api/v1/auth/ban/appeal", "appealBan", "Submit one plain-text appeal per ban per rolling 30 days", BanAppealRequest{}, nil, 204)
	for _, path := range []string{"status", "appeal"} {
		key := "POST /api/v1/auth/ban/" + path
		v := c[key]
		v.public = true
		c[key] = v
	}

	add("GET /api/v1/categories", "listCategories", "List visible categories", nil, categoriesResponse{}, 200)
	add("POST /api/v1/categories", "createCategory", "Create a category", SaveCategoryRequest{}, identity.Category{}, 201)
	add("PUT /api/v1/categories/{id}", "updateCategory", "Update a category", SaveCategoryRequest{}, identity.Category{}, 200)
	add("GET /api/v1/channels", "listChannels", "List visible channels", nil, channelsResponse{}, 200)
	add("GET /api/v1/channels/{id}", "getChannel", "Get a channel and effective permissions", nil, identity.Channel{}, 200)
	add("POST /api/v1/channels", "createChannel", "Create a text or voice channel", SaveChannelRequest{}, identity.Channel{}, 201)
	add("PUT /api/v1/channels/{id}", "updateChannel", "Update a channel", SaveChannelRequest{}, identity.Channel{}, 200)
	for _, resource := range []struct{ path, name string }{{"categories", "Category"}, {"channels", "Channel"}} {
		base := "/api/v1/" + resource.path + "/{id}"
		add("DELETE "+base, "delete"+resource.name, "Delete a "+resource.name, nil, nil, 204)
		add("GET "+base+"/permissions", "get"+resource.name+"Permissions", "Get permission overrides", nil, overridesResponse{}, 200)
		add("PUT "+base+"/permissions/{role}", "set"+resource.name+"Permissions", "Set a role's permission override", SetPermissionsRequest{}, nil, 204)
	}
	add("GET /api/v1/read-state", "listReadState", "List persistent unread and mention counts for accessible conversations", nil, chat.ReadPage{}, 200)
	add("PUT /api/v1/conversations/{id}/read", "markConversationRead", "Advance this account's read position monotonically to the highest displayed message sequence", MarkReadRequest{}, chat.ReadState{}, 200)
	add("GET /api/v1/conversations", "listConversations", "List accessible text/voice channel chats and DMs", nil, conversationsResponse{}, 200)
	add("POST /api/v1/dms", "createDM", "Get or create a DM for a participant set", CreateDMRequest{}, chat.Conversation{}, 201)
	add("GET /api/v1/conversations/{id}/messages", "getHistory", "Read message history and recovery cursors", nil, chat.History{}, 200)
	add("POST /api/v1/conversations/{id}/messages", "createMessage", "Commit a message with retry deduplication", CreateMessageRequest{}, chat.Message{}, 201)
	add("PUT /api/v1/conversations/{id}/messages/{message}", "editMessage", "Edit an authored message", EditMessageRequest{}, chat.Message{}, 200)
	add("DELETE /api/v1/conversations/{id}/messages/{message}", "deleteMessage", "Tombstone an authored message", nil, chat.Message{}, 200)
	add("GET /api/v1/conversations/{id}/messages/{message}/embeds", "getMessageEmbeds", "Fetch up to three cached public link previews for a visible retained message", nil, embedsResponse{}, 200)
	add("GET /api/v1/conversations/{id}/messages/{message}/embeds/image", "getEmbedImage", "Download a bounded preview image after rechecking message access", nil, nil, 200)
	add("GET /api/v1/attachments/{id}/metadata", "getAttachmentMetadata", "Get filename, size and detected content type for an authorized attachment", nil, attachments.Attachment{}, 200)
	add("POST /api/v1/conversations/{id}/attachments", "uploadAttachment", "Upload raw image or video bytes", nil, attachments.Attachment{}, 201)
	add("GET /api/v1/attachments/{id}", "downloadAttachment", "Download an authorized attachment; supports Range and HEAD", nil, nil, 200)
	add("DELETE /api/v1/attachments/{id}", "deleteAttachment", "Delete an owned attachment", nil, nil, 204)
	add("POST /api/v1/voice/{id}/admissions", "admitVoice", "Get a single-use room admission and automatic transport", nil, voice.Admission{}, 201)
	add("GET /api/v1/voice/{id}", "getVoiceRoom", "List current room participants and tracks", nil, participantsResponse{}, 200)
	add("GET /api/v1/voice/{id}/ice", "getICE", "Refresh authorized ICE credentials", nil, iceResponse{}, 200)
	add("GET /api/v1/events", "events", "Upgrade to authenticated events and signaling WebSocket", nil, nil, 101)
	add("GET /api/v1/media", "media", "Upgrade to encoded media WebSocket when configured", nil, nil, 101)
	add("GET /api/v1/admin/stats", "getStats", "Inspect storage, pool, queue and media metrics (requires the corresponding server permission)", nil, statsResponse{}, 200)
	add("POST /api/v1/admin/maintenance", "maintenance", "Run bounded maintenance (requires the corresponding server permission)", nil, nil, 204)
	for _, pattern := range []string{"GET /healthz", "GET /readyz", "GET /api/v1/instance", "POST /api/v1/auth/register", "POST /api/v1/auth/login"} {
		v := c[pattern]
		v.public = true
		c[pattern] = v
	}
	for _, pattern := range []string{"GET /api/v1/users", "GET /api/v1/members", "GET /api/v1/conversations", "GET /api/v1/read-state"} {
		v := c[pattern]
		v.query = afterQuery{}
		c[pattern] = v
	}
	updateManifest := c["GET /api/v1/client-updates/haunt-updates.json"]
	updateManifest.query = clientUpdateQuery{}
	c["GET /api/v1/client-updates/haunt-updates.json"] = updateManifest
	updateFile := c["GET /api/v1/client-updates/{version}/{name}"]
	updateFile.binaryOutput = true
	c["GET /api/v1/client-updates/{version}/{name}"] = updateFile
	pluginUpload := c["POST /api/v1/admin/audio-plugins"]
	pluginUpload.binaryInput = true
	c["POST /api/v1/admin/audio-plugins"] = pluginUpload
	pluginDownload := c["GET /api/v1/audio-plugins/{digest}/files/{name}"]
	pluginDownload.binaryOutput = true
	c["GET /api/v1/audio-plugins/{digest}/files/{name}"] = pluginDownload
	embedImage := c["GET /api/v1/conversations/{id}/messages/{message}/embeds/image"]
	embedImage.query = embedImageQuery{}
	embedImage.binaryOutput = true
	c["GET /api/v1/conversations/{id}/messages/{message}/embeds/image"] = embedImage
	v := c["GET /api/v1/conversations/{id}/messages"]
	v.query = historyQuery{}
	c["GET /api/v1/conversations/{id}/messages"] = v
	v = c["POST /api/v1/conversations/{id}/attachments"]
	v.query = uploadQuery{}
	v.binaryInput = true
	c["POST /api/v1/conversations/{id}/attachments"] = v
	v = c["GET /api/v1/attachments/{id}"]
	v.query = rangeHeaders{}
	v.binaryOutput = true
	c["GET /api/v1/attachments/{id}"] = v
	for _, pattern := range []string{"GET /api/v1/events", "GET /api/v1/media"} {
		v := c[pattern]
		v.public = true
		v.websocket = true
		v.description = "HTTP upgrade only. Authenticate in the first WebSocket text frame within five seconds; never put the token in the URL. Message schemas and binary framing are described in /websocket.json and docs/media-transports.md. Swagger Try it out does not open a WebSocket."
		c[pattern] = v
	}
	value := c["GET /api/v1/conversations/{id}/encryption/events"]
	value.query = DMCryptoAfter{}
	c["GET /api/v1/conversations/{id}/encryption/events"] = value

	return c
}
