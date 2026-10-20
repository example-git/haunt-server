package httpapi

import (
	"github.com/example-git/haunt-server/internal/identity"
	"net/http"
)

// MenuManifest is deliberately declarative. Version 1 never carries executable
// code, markup, credentials, or URLs outside this installation's API.
type MenuManifest struct {
	Version int        `json:"version" required:"true"`
	Pages   []MenuPage `json:"pages" required:"true"`
}
type MenuPage struct {
	ID           string       `json:"id" required:"true"`
	Label        string       `json:"label" required:"true"`
	ContextLabel string       `json:"context_label,omitempty"`
	Builtin      string       `json:"builtin,omitempty"`
	Form         *MenuForm    `json:"form,omitempty"`
	Action       *MenuCommand `json:"action,omitempty"`
}
type MenuForm struct {
	LoadPath       string      `json:"load_path" required:"true"`
	SavePath       string      `json:"save_path" required:"true"`
	Method         string      `json:"method" required:"true" enum:"PUT,POST"`
	Description    string      `json:"description,omitempty"`
	SubmitLabel    string      `json:"submit_label" required:"true"`
	SuccessMessage string      `json:"success_message,omitempty"`
	CarryFields    []string    `json:"carry_fields,omitempty"`
	Fields         []MenuField `json:"fields" required:"true"`
}
type MenuField struct {
	Path      string       `json:"path" required:"true"`
	Label     string       `json:"label" required:"true"`
	Type      string       `json:"type" required:"true" enum:"text,textarea,password,number,toggle,select"`
	Help      string       `json:"help,omitempty"`
	Required  bool         `json:"required,omitempty"`
	Min       *float64     `json:"min,omitempty"`
	Max       *float64     `json:"max,omitempty"`
	Step      *float64     `json:"step,omitempty"`
	Scale     float64      `json:"scale,omitempty"`
	MaxLength int          `json:"max_length,omitempty"`
	Options   []MenuOption `json:"options,omitempty"`
}
type MenuOption struct {
	Value string `json:"value" required:"true"`
	Label string `json:"label" required:"true"`
}
type MenuCommand struct {
	SubmitLabel       string `json:"submit_label,omitempty"`
	ConfirmationLabel string `json:"confirmation_label,omitempty"`
	Path              string `json:"path" required:"true"`
	Method            string `json:"method" required:"true" enum:"POST,PUT,DELETE"`
	Confirmation      string `json:"confirmation" required:"true"`
	ConfirmationValue string `json:"confirmation_value,omitempty"`
	SuccessMessage    string `json:"success_message,omitempty"`
	Dangerous         bool   `json:"dangerous,omitempty"`
}

func menuNumber(path, label string, min, max, step, scale float64) MenuField {
	return MenuField{Path: path, Label: label, Type: "number", Required: true, Min: &min, Max: &max, Step: &step, Scale: scale}
}
func runtimeMenu() MenuPage {
	const mib = 1048576.0
	fields := []MenuField{
		menuNumber("settings.limits.image_max_bytes", "Maximum image upload (MiB)", 1/mib, 1<<30, 1/mib, mib),
		menuNumber("settings.limits.video_max_bytes", "Maximum video upload (MiB)", 1/mib, 1<<30, 1/mib, mib),
		menuNumber("settings.limits.attachment_max_bytes", "Total attachment storage budget (MiB)", 1/mib, 1<<30, 1/mib, mib),
		menuNumber("settings.limits.attachment_retention_days", "Keep new attachments (days)", 1, 36500, 1, 1),
		menuNumber("settings.limits.text_max_bytes", "Message and replay storage budget (MiB)", 4096/mib, 1<<30, 1/mib, mib),
		menuNumber("settings.limits.text_retention_days", "Keep messages (days)", 1, 36500, 1, 1),
		menuNumber("settings.media_room_limit", "Connections per voice channel", 2, 64, 1, 1),
		menuNumber("settings.media_max_peers", "Connections across all voice channels", 2, 4096, 1, 1),
		{Path: "settings.registration_enabled", Label: "Allow registration with invitations", Type: "toggle"},
		menuNumber("settings.session_days", "Non-remembered session duration (days)", 1, 365, 1, 1),
	}
	fields = append(fields,
		MenuField{Path: "settings.limits.dm.images_enabled", Label: "Allow encrypted DM images", Type: "toggle"},
		menuNumber("settings.limits.dm.image_max_bytes", "Maximum encrypted DM image (MiB)", 1024/mib, 20, 1/mib, mib),
		menuNumber("settings.limits.dm.user_storage_bytes", "DM uploads per account (MiB)", 1024/mib, 1<<30, 1/mib, mib),
		menuNumber("settings.limits.dm.storage_bytes", "Total DM upload storage (MiB)", 1024/mib, 1<<30, 1/mib, mib),
		menuNumber("settings.limits.dm.text_max_bytes", "DM message storage (MiB)", 1, 1<<30, 1/mib, mib),
		menuNumber("settings.limits.dm.text_retention_days", "Keep DM messages (days; 0 means indefinitely)", 0, 36500, 1, 1),
		menuNumber("settings.limits.dm.image_retention_days", "Keep encrypted DM images (days; 0 means indefinitely)", 0, 36500, 1, 1),
		menuNumber("settings.limits.dm.upload_bytes_per_day", "DM image upload allowance per account per UTC day (MiB)", 1024/mib, 1<<30, 1/mib, mib),
		menuNumber("settings.limits.dm.download_bytes_per_day", "DM image download allowance per account per UTC day (MiB)", 1024/mib, 1<<30, 1/mib, mib),
	)
	fields[2].Help = "Cannot be smaller than the space already reserved by files and uploads."
	fields[3].Help = "New uploads use the retention and size limits in effect when they start. Existing files keep their expiry dates."
	fields[5].Help = "Reducing message retention or the message budget can permanently remove older messages and replay history."
	fields[6].Help = "Existing calls remain connected when capacity is reduced. New connections use the new limits."
	fields[9].Help = "Applies to newly created sessions. Remembered sessions stay signed in until revoked."
	return MenuPage{ID: "configuration", Label: "Limits & access", ContextLabel: "Limits & access", Form: &MenuForm{
		LoadPath: "/api/v1/instance/configuration", SavePath: "/api/v1/instance/configuration", Method: "PUT", CarryFields: []string{"revision", "settings.limits.dm.version"}, Fields: fields,
		Description: "Saved on this server and applied without a restart.", SubmitLabel: "Save configuration", SuccessMessage: "Server configuration saved and applied.",
	}}
}
func serverMenu(p identity.Principal) MenuManifest {
	v := MenuManifest{Version: 1, Pages: []MenuPage{}}
	if p.Can(identity.ManageConfiguration) {
		v.Pages = append(v.Pages, MenuPage{ID: "server-extensions", Label: "Server extensions", ContextLabel: "Server extensions", Builtin: "server-extensions"})
	}
	add := func(permission int64, id, label string) {
		if p.Can(permission) {
			v.Pages = append(v.Pages, MenuPage{ID: id, Label: label, ContextLabel: label, Builtin: id})
		}
	}
	if p.Can(identity.ManageConfiguration) {
		v.Pages = append(v.Pages, runtimeMenu())
		add(identity.ManageConfiguration, "storage", "Storage providers")
		add(identity.ManageConfiguration, "audio-plugins", "Audio plugins")
	}
	add(identity.ManageServer, "overview", "Overview & appearance")
	add(identity.ManageServer, "emojis", "Emojis")
	add(identity.ManageChannels, "channels", "Channels & categories")
	add(identity.ManageRoles, "roles", "Roles & permissions")
	if p.Can(identity.ManageRoles) || p.Can(identity.KickMembers) || p.Can(identity.BanMembers) || p.Can(identity.DeleteMembers) || p.Can(identity.ViewMemberIPs) || p.Can(identity.ResetPasswords) {
		v.Pages = append(v.Pages, MenuPage{ID: "members", Label: "Members", ContextLabel: "Members", Builtin: "members"})
	}
	add(identity.BanMembers, "bans", "Bans & appeals")
	add(identity.ViewAuditLog, "audit", "Audit log")
	return v
}
func (f *Features) menuRoutes(a *api, mux routeRegistrar) {
	mux.HandleFunc("GET /api/v1/menus/server", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) { writeJSON(w, 200, serverMenu(p)) }))
	mux.HandleFunc("GET /api/v1/channels/{id}/menu", a.authenticated(func(w http.ResponseWriter, r *http.Request, p identity.Principal) {
		channel, err := a.identity.Channel(r.Context(), p, r.PathValue("id"))
		if err != nil {
			a.fail(w, r, err)
			return
		}
		v := MenuManifest{Version: 1, Pages: []MenuPage{}}
		if p.Can(identity.ManageChannels) {
			v.Pages = []MenuPage{
				{ID: "overview", Label: "Overview", ContextLabel: "Edit channel", Builtin: "overview"},
				{ID: "permissions", Label: "Permissions", ContextLabel: "Channel permissions", Builtin: "permissions"},
				{ID: "delete", Label: "Delete channel", ContextLabel: "Delete channel…", Action: &MenuCommand{Path: "/api/v1/channels/" + channel.ID, Method: "DELETE", Confirmation: "Delete this channel and its server history? This cannot be undone. Type the channel name to confirm.", ConfirmationValue: channel.Name, SubmitLabel: "Permanently delete channel", ConfirmationLabel: "Confirm channel name", Dangerous: true}},
			}
		}
		writeJSON(w, 200, v)
	}))
}
