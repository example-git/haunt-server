package ai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"github.com/example-git/haunt-server/internal/ai/provider"
	"github.com/example-git/haunt-server/internal/identity"
	"github.com/jackc/pgx/v5"
	"strings"
	"unicode/utf8"
)

func validName(v string) bool {
	return utf8.ValidString(v) && strings.TrimSpace(v) != "" && utf8.RuneCountInString(v) <= 80 && !strings.ContainsAny(v, "\x00\r\n")
}
func validInstructions(v string) bool {
	return utf8.ValidString(v) && len(v) <= 32768 && !strings.ContainsRune(v, 0)
}
func (s *Service) ensureSettings(ctx context.Context, tx pgx.Tx, id string) error {
	policy, err := s.chat.PolicyForTransaction(ctx, tx)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO chatxo.conversations(id,instance_id,kind,channel_id) VALUES($1,$2,'channel',$1) ON CONFLICT DO NOTHING`, id, s.options.InstanceID); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO chatxo.ai_channel_settings(channel_id,max_bytes) VALUES($1,$2) ON CONFLICT DO NOTHING`, id, policy.AIChannelMaxBytes)
	return err
}
func (s *Service) settingsChanged(ctx context.Context, tx pgx.Tx, id string) error {
	var revision int64
	if err := tx.QueryRow(ctx, `UPDATE chatxo.ai_channel_settings SET revision=revision+1 WHERE channel_id=$1 RETURNING revision`, id).Scan(&revision); err != nil {
		return err
	}
	if err := s.cleanupAvatars(ctx, tx, id, 0); err != nil {
		return err
	}
	if err := s.chat.Emit(ctx, tx, id, nil, "ai.settings.updated", map[string]any{"revision": revision}); err != nil {
		return err
	}
	return s.enforce(ctx, tx, id)
}
func (s *Service) Settings(ctx context.Context, p identity.Principal, id string) (Settings, error) {
	if _, err := s.channel(ctx, p, id, false); err != nil {
		return Settings{}, err
	}
	var out Settings
	err := s.transaction(ctx, func(tx pgx.Tx) error {
		if err := s.ensureSettings(ctx, tx, id); err != nil {
			return err
		}
		if err := s.enforce(ctx, tx, id); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT s.channel_id,COALESCE(s.account_id::text,''),s.provider,s.model,s.instructions,s.name,s.avatar_url,s.max_bytes,COALESCE(s.active_preset_id::text,''),s.revision,COALESCE(a.status,'disconnected') FROM chatxo.ai_channel_settings s LEFT JOIN chatxo.ai_accounts a ON a.id=s.account_id WHERE s.channel_id=$1`, id).Scan(&out.ChannelID, &out.AccountID, &out.Provider, &out.Model, &out.Instructions, &out.Name, &out.AvatarURL, &out.MaxBytes, &out.ActivePresetID, &out.Revision, &out.ConnectionStatus)
	})
	return out, err
}
func (s *Service) SaveSettings(ctx context.Context, p identity.Principal, id string, v SettingsInput) (Settings, error) {
	if _, err := s.channel(ctx, p, id, true); err != nil {
		return Settings{}, err
	}
	if !validName(v.Name) || !validInstructions(v.Instructions) || v.MaxBytes < 4096 || (v.AccountID != "" && !identity.ValidID(v.AccountID)) {
		return Settings{}, identity.ErrInvalid
	}
	a := s.adapters[provider.ID(v.Provider)]
	if a == nil {
		return Settings{}, identity.ErrInvalid
	}
	if v.Model != "" {
		models, err := a.Models(ctx, provider.Credentials{})
		if err != nil {
			return Settings{}, err
		}
		found := false
		for _, m := range models {
			found = found || m.ID == v.Model
		}
		if !found {
			return Settings{}, identity.ErrInvalid
		}
	}
	err := s.transaction(ctx, func(tx pgx.Tx) error {
		if err := s.authorizeChannelWrite(ctx, tx, p, id); err != nil {
			return err
		}
		policy, err := s.chat.PolicyForTransaction(ctx, tx)
		if err != nil {
			return err
		}
		if v.MaxBytes > policy.AIChannelMaxBytes {
			return identity.ErrInvalid
		}
		if v.AccountID != "" {
			var kind string
			if err := tx.QueryRow(ctx, `SELECT provider FROM chatxo.ai_accounts WHERE id=$1 AND instance_id=$2`, v.AccountID, s.options.InstanceID).Scan(&kind); err != nil {
				return translate(err)
			}
			if kind != v.Provider {
				return identity.ErrInvalid
			}
		}
		if err := s.ensureSettings(ctx, tx, id); err != nil {
			return err
		}
		var avatar string
		if err = tx.QueryRow(ctx, `SELECT avatar_url FROM chatxo.ai_channel_settings WHERE channel_id=$1`, id).Scan(&avatar); err != nil {
			return err
		}
		if v.AvatarURL != "" && v.AvatarURL != avatar {
			return identity.ErrInvalid
		}
		_, err = tx.Exec(ctx, `UPDATE chatxo.ai_channel_settings SET account_id=$2,provider=$3,model=$4,instructions=$5,name=$6,max_bytes=$7,revision=revision+1 WHERE channel_id=$1`, id, nullable(v.AccountID), v.Provider, v.Model, v.Instructions, v.Name, v.MaxBytes)
		if err != nil {
			return err
		}
		return s.settingsChanged(ctx, tx, id)
	})
	if err != nil {
		return Settings{}, err
	}
	return s.Settings(ctx, p, id)
}
func (s *Service) Presets(ctx context.Context, p identity.Principal, id string) ([]Preset, error) {
	if _, err := s.channel(ctx, p, id, false); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT id,channel_id,name,avatar_url,instructions,revision FROM chatxo.ai_presets WHERE channel_id=$1 ORDER BY name,id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Preset{}
	for rows.Next() {
		var v Preset
		if err = rows.Scan(&v.ID, &v.ChannelID, &v.Name, &v.AvatarURL, &v.Instructions, &v.Revision); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Service) SavePreset(ctx context.Context, p identity.Principal, id, preset string, v PresetInput) (Preset, error) {
	if _, err := s.channel(ctx, p, id, true); err != nil {
		return Preset{}, err
	}
	if !validName(v.Name) || !validInstructions(v.Instructions) || (preset != "" && !identity.ValidID(preset)) {
		return Preset{}, identity.ErrInvalid
	}
	create := preset == ""
	if create {
		preset = identity.NewID()
	}
	out := Preset{ID: preset, ChannelID: id}
	err := s.transaction(ctx, func(tx pgx.Tx) error {
		if err := s.authorizeChannelWrite(ctx, tx, p, id); err != nil {
			return err
		}
		if err := s.ensureSettings(ctx, tx, id); err != nil {
			return err
		}
		if v.AvatarURL != "" {
			var avatar string
			if create {
				return identity.ErrInvalid
			}
			if err := tx.QueryRow(ctx, `SELECT avatar_url FROM chatxo.ai_presets WHERE id=$1 AND channel_id=$2`, preset, id).Scan(&avatar); err != nil {
				return translate(err)
			}
			if v.AvatarURL != avatar {
				return identity.ErrInvalid
			}
		}
		if create {
			var count int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM chatxo.ai_presets WHERE channel_id=$1`, id).Scan(&count); err != nil {
				return err
			}
			if count >= 32 {
				return identity.ErrInvalid
			}
			_, err := tx.Exec(ctx, `INSERT INTO chatxo.ai_presets(id,channel_id,name,instructions) VALUES($1,$2,$3,$4)`, preset, id, v.Name, v.Instructions)
			if err != nil {
				return err
			}
		} else {
			r, err := tx.Exec(ctx, `UPDATE chatxo.ai_presets SET name=$3,instructions=$4,revision=revision+1 WHERE id=$1 AND channel_id=$2`, preset, id, v.Name, v.Instructions)
			if err != nil {
				return err
			}
			if r.RowsAffected() == 0 {
				return identity.ErrNotFound
			}
		}
		if err := tx.QueryRow(ctx, `SELECT name,avatar_url,instructions,revision FROM chatxo.ai_presets WHERE id=$1`, preset).Scan(&out.Name, &out.AvatarURL, &out.Instructions, &out.Revision); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE chatxo.ai_channel_settings SET revision=revision+1 WHERE channel_id=$1`, id)
		if err != nil {
			return err
		}
		return s.settingsChanged(ctx, tx, id)
	})
	return out, err
}
func (s *Service) DeletePreset(ctx context.Context, p identity.Principal, id, preset string) error {
	if _, err := s.channel(ctx, p, id, true); err != nil {
		return err
	}
	if !identity.ValidID(preset) {
		return identity.ErrInvalid
	}
	return s.transaction(ctx, func(tx pgx.Tx) error {
		if err := s.authorizeChannelWrite(ctx, tx, p, id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE chatxo.ai_channel_settings SET active_preset_id=NULL,revision=revision+1 WHERE channel_id=$1 AND active_preset_id=$2`, id, preset)
		if err != nil {
			return err
		}
		r, err := tx.Exec(ctx, `DELETE FROM chatxo.ai_presets WHERE id=$1 AND channel_id=$2`, preset, id)
		if err != nil {
			return err
		}
		if r.RowsAffected() == 0 {
			return identity.ErrNotFound
		}
		return s.settingsChanged(ctx, tx, id)
	})
}
func (s *Service) SaveAvatar(ctx context.Context, p identity.Principal, id, preset string, raw []byte) (string, error) {
	if _, err := s.channel(ctx, p, id, true); err != nil {
		return "", err
	}
	if preset != "" && !identity.ValidID(preset) {
		return "", identity.ErrInvalid
	}
	data, err := s.ids.NormalizeAvatar(raw)
	if err != nil {
		return "", err
	}
	url := "/api/v1/channels/" + id + "/ai"
	if preset != "" {
		url += "/presets/" + preset
	}
	url += "/avatar?v=" + avatarVersion(data)
	err = s.transaction(ctx, func(tx pgx.Tx) error {
		if err := s.authorizeChannelWrite(ctx, tx, p, id); err != nil {
			return err
		}
		if err := s.ensureSettings(ctx, tx, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO chatxo.ai_avatars(channel_id,version,data) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, id, avatarVersion(data), data); err != nil {
			return err
		}
		var r interface{ RowsAffected() int64 }
		var e error
		if preset == "" {
			r, e = tx.Exec(ctx, `UPDATE chatxo.ai_channel_settings SET avatar_url=$2,revision=revision+1 WHERE channel_id=$1`, id, url)
		} else {
			r, e = tx.Exec(ctx, `UPDATE chatxo.ai_presets SET avatar_url=$2,revision=revision+1 WHERE id=$1 AND channel_id=$3`, preset, url, id)
		}
		if e != nil {
			return e
		}
		if r.RowsAffected() == 0 {
			return identity.ErrNotFound
		}
		return s.settingsChanged(ctx, tx, id)
	})
	return url, err
}
func (s *Service) Avatar(ctx context.Context, p identity.Principal, id, preset string, versions ...string) ([]byte, error) {
	if _, err := s.channel(ctx, p, id, false); err != nil {
		return nil, err
	}
	if preset != "" && !identity.ValidID(preset) {
		return nil, identity.ErrInvalid
	}
	version := ""
	if len(versions) > 0 {
		version = versions[0]
	}
	if version == "" {
		var path string
		var err error
		if preset == "" {
			err = s.pool.QueryRow(ctx, `SELECT avatar_url FROM chatxo.ai_channel_settings WHERE channel_id=$1`, id).Scan(&path)
		} else {
			err = s.pool.QueryRow(ctx, `SELECT avatar_url FROM chatxo.ai_presets WHERE id=$1 AND channel_id=$2`, preset, id).Scan(&path)
		}
		if err != nil {
			return nil, translate(err)
		}
		_, version, _ = strings.Cut(path, "?v=")
	}
	if len(version) != 32 {
		return nil, identity.ErrNotFound
	}
	if _, err := hex.DecodeString(version); err != nil {
		return nil, identity.ErrInvalid
	}
	var data []byte
	err := s.pool.QueryRow(ctx, `SELECT data FROM chatxo.ai_avatars WHERE channel_id=$1 AND version=$2`, id, version).Scan(&data)
	return data, translate(err)
}
func (s *Service) DeleteAvatar(ctx context.Context, p identity.Principal, id, preset string) error {
	if _, err := s.channel(ctx, p, id, true); err != nil {
		return err
	}
	if preset != "" && !identity.ValidID(preset) {
		return identity.ErrInvalid
	}
	return s.transaction(ctx, func(tx pgx.Tx) error {
		if err := s.authorizeChannelWrite(ctx, tx, p, id); err != nil {
			return err
		}
		var err error
		if preset == "" {
			_, err = tx.Exec(ctx, `UPDATE chatxo.ai_channel_settings SET avatar_url='',revision=revision+1 WHERE channel_id=$1`, id)
		} else {
			_, err = tx.Exec(ctx, `UPDATE chatxo.ai_presets SET avatar_url='',revision=revision+1 WHERE id=$1 AND channel_id=$2`, preset, id)
		}
		if err != nil {
			return err
		}
		return s.settingsChanged(ctx, tx, id)
	})
}

func avatarVersion(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16])
}
func (s *Service) authorizeChannelWrite(ctx context.Context, tx pgx.Tx, p identity.Principal, id string) error {
	ch, err := s.ids.ChannelForTransaction(ctx, tx, p, id)
	if err != nil {
		return err
	}
	if ch.Kind != "ai" {
		return identity.ErrInvalid
	}
	if ch.Permissions&identity.ManageChannels == 0 {
		return identity.ErrForbidden
	}
	return nil
}

func (s *Service) cleanupAvatars(ctx context.Context, tx pgx.Tx, id string, through int64) error {
	_, err := tx.Exec(ctx, `DELETE FROM chatxo.ai_avatars a WHERE ($1='' OR a.channel_id=NULLIF($1,'')::uuid) AND NOT EXISTS(SELECT 1 FROM chatxo.ai_channel_settings s WHERE s.channel_id=a.channel_id AND split_part(s.avatar_url,'?v=',2)=a.version) AND NOT EXISTS(SELECT 1 FROM chatxo.ai_presets p WHERE p.channel_id=a.channel_id AND split_part(p.avatar_url,'?v=',2)=a.version) AND NOT EXISTS(SELECT 1 FROM chatxo.messages m WHERE m.conversation_id=a.channel_id AND NOT m.deleted AND m.seq>$2 AND split_part(m.ai_author->>'avatar_url','?v=',2)=a.version) AND NOT EXISTS(SELECT 1 FROM chatxo.ai_turns t WHERE t.channel_id=a.channel_id AND split_part(t.snapshot->'author'->>'avatar_url','?v=',2)=a.version)`, id, through)
	return err
}
