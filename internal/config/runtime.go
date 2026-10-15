package config

import "errors"

// RuntimeSettings contains policies that can change without rebuilding listeners,
// reopening the database, or interrupting active media sessions.
type RuntimeSettings struct {
	Limits              Policy `json:"limits" required:"true"`
	MediaMaxPeers       int    `json:"media_max_peers" required:"true" minimum:"2" maximum:"4096"`
	MediaRoomLimit      int    `json:"media_room_limit" required:"true" minimum:"2" maximum:"64"`
	SessionDays         int64  `json:"session_days" required:"true" minimum:"1" maximum:"365"`
	RegistrationEnabled bool   `json:"registration_enabled" required:"true"`
}

func (c Config) RuntimeDefaults() RuntimeSettings {
	r := RuntimeSettings{Limits: c.Policy.WithDefaults(), MediaMaxPeers: c.MediaMaxPeers, MediaRoomLimit: c.MediaRoomLimit, SessionDays: c.SessionDays, RegistrationEnabled: true}
	if r.MediaMaxPeers == 0 {
		r.MediaMaxPeers = 512
	}
	if r.MediaRoomLimit == 0 {
		r.MediaRoomLimit = 16
	}
	if r.SessionDays == 0 {
		r.SessionDays = 30
	}
	return r
}
func (r RuntimeSettings) Validate() error {
	if !r.Limits.WithDefaults().DM.Valid() {
		return errors.New("invalid_configuration")
	}
	p := r.Limits
	if p.TextMaxBytes < 4096 || p.TextMaxBytes > 1<<50 || p.ImageMaxBytes < 1 || p.VideoMaxBytes < 1 || p.AttachmentMaxBytes > 1<<50 || p.ImageMaxBytes > p.AttachmentMaxBytes || p.VideoMaxBytes > p.AttachmentMaxBytes || p.TextRetentionDays < 1 || p.TextRetentionDays > 36500 || p.AttachmentRetentionDays < 1 || p.AttachmentRetentionDays > 36500 || r.SessionDays < 1 || r.SessionDays > 365 || r.MediaRoomLimit < 2 || r.MediaRoomLimit > 64 || r.MediaMaxPeers < 2 || r.MediaMaxPeers > 4096 {
		return errors.New("invalid_configuration")
	}
	return nil
}
