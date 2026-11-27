package ai

import (
	"errors"
	"github.com/example-git/haunt-server/internal/chat"
	"github.com/example-git/haunt-server/internal/config"
	"time"
)

var ErrStorage = errors.New("ai_storage_full")
var ErrConfiguration = errors.New("ai_configuration_required")
var ErrHistoryChanged = errors.New("ai_history_changed")

const DefaultChannelMaxBytes int64 = 64 << 20
const DefaultGlobalMaxBytes int64 = 128 << 20

type Options struct {
	PublicURL       string
	URLReader       config.URLReaderLimits
	DiffbotKey      string `json:"-"`
	InstanceID      string
	DataDir         string
	ChannelMaxBytes int64
	GlobalMaxBytes  int64
}
type SettingsInput struct {
	Timezone           *string `json:"timezone,omitempty"`
	AllowConsultations *bool   `json:"allow_consultations,omitempty"`
	MaxConsultations   *int    `json:"max_consultations,omitempty"`
	AllowAgentSwitch   bool    `json:"allow_agent_switch"`
	SwitchDuringReply  *bool   `json:"switch_during_reply,omitempty"`
	AccountID          string  `json:"account_id"`
	Provider           string  `json:"provider"`
	Model              string  `json:"model"`
	Instructions       string  `json:"instructions"`
	Name               string  `json:"name"`
	AvatarURL          string  `json:"avatar_url,omitempty"`
	MaxBytes           int64   `json:"max_bytes"`
}
type Settings struct {
	SettingsInput
	ProviderName     string `json:"provider_name,omitempty"`
	ModelName        string `json:"model_name,omitempty"`
	ChannelID        string `json:"channel_id"`
	ActivePresetID   string `json:"active_preset_id,omitempty"`
	Revision         int64  `json:"revision"`
	ConnectionStatus string `json:"connection_status"`
}
type PresetInput struct {
	Description  string `json:"description"`
	AvatarData   []byte `json:"avatar_data,omitempty"`
	RequestID    string `json:"request_id,omitempty"`
	Name         string `json:"name"`
	AvatarURL    string `json:"avatar_url,omitempty"`
	Instructions string `json:"instructions"`
}
type Preset struct {
	PresetInput
	ID        string `json:"id"`
	ChannelID string `json:"channel_id"`
	Revision  int64  `json:"revision"`
}
type Account struct {
	ID        string     `json:"id"`
	Provider  string     `json:"provider"`
	Status    string     `json:"status"`
	Label     string     `json:"label"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}
type OAuthPrepareInput struct {
	Provider    string `json:"provider"`
	RedirectURI string `json:"redirect_uri"`
	AccountID   string `json:"account_id,omitempty"`
}
type OAuthSession struct {
	ID               string    `json:"id"`
	Provider         string    `json:"provider"`
	AuthorizationURL string    `json:"authorization_url"`
	RedirectURI      string    `json:"redirect_uri"`
	State            string    `json:"state"`
	ExpiresAt        time.Time `json:"expires_at"`
}
type OAuthCompleteInput struct {
	Code  string `json:"code"`
	State string `json:"state"`
}
type Turn struct {
	Action           string     `json:"action,omitempty"`
	Detail           string     `json:"detail,omitempty"`
	LastProgressAt   *time.Time `json:"last_progress_at,omitempty"`
	ID               string     `json:"id"`
	ChannelID        string     `json:"channel_id"`
	TriggerMessageID string     `json:"trigger_message_id"`
	ActorID          string     `json:"actor_id"`
	ParticipantIDs   []string   `json:"participant_ids,omitempty"`
	Status           string     `json:"status"`
	Revision         int64      `json:"revision"`
	PartialText      string     `json:"partial_text,omitempty"`
	ErrorCode        string     `json:"error_code,omitempty"`
	RetryAt          *time.Time `json:"retry_at,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}
type Usage struct {
	PrunedThroughSeq int64 `json:"pruned_through_seq"`
	HistoryEpoch     int64 `json:"history_epoch"`
	ChannelBytes     int64 `json:"channel_bytes"`
	ChannelMaxBytes  int64 `json:"channel_max_bytes"`
	GlobalBytes      int64 `json:"global_bytes"`
	GlobalMaxBytes   int64 `json:"global_max_bytes"`
	PrunedThrough    int64 `json:"pruned_through"`
}
type snapshot struct {
	Inputs             []turnInput   `json:"inputs,omitempty"`
	InputBytes         int           `json:"input_bytes,omitempty"`
	SelectionSeq       int64         `json:"selection_seq"`
	PresetInstructions string        `json:"preset_instructions"`
	Provider           string        `json:"provider"`
	AccountID          string        `json:"account_id"`
	Model              string        `json:"model"`
	Instructions       string        `json:"instructions"`
	Author             chat.AIAuthor `json:"author"`
	Revision           int64         `json:"revision"`
	SessionID          string        `json:"session_id"`
}

// Inputs are stored only for a combined turn. Single-message snapshots from
// older installations continue to use the turn's original trigger and actor.
type turnInput struct {
	MessageID string `json:"message_id"`
	ActorID   string `json:"actor_id"`
	SessionID string `json:"session_id"`
	Seq       int64  `json:"seq"`
}
