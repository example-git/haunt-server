package identity

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

type VoiceRestriction struct {
	UserID   string `json:"user_id"`
	Muted    bool   `json:"muted"`
	Deafened bool   `json:"deafened"`
}

type VoiceRestrictionMember struct {
	VoiceRestriction
	User User `json:"user"`
}

func (s *Service) VoiceRestriction(ctx context.Context, channel, user string) (VoiceRestriction, error) {
	v := VoiceRestriction{UserID: user}
	err := s.pool.QueryRow(ctx, `SELECT muted,deafened FROM chatxo.voice_restrictions WHERE instance_id=$1 AND channel_id=$2 AND user_id=$3`, s.instanceID, channel, user).Scan(&v.Muted, &v.Deafened)
	if errors.Is(err, pgx.ErrNoRows) {
		err = nil
	}
	return v, err
}

func (s *Service) voiceModerationChannel(ctx context.Context, tx pgx.Tx, p Principal, channel string) error {
	if !ValidID(channel) {
		return ErrInvalid
	}
	var kind string
	var categoryAllow, categoryDeny, channelAllow, channelDeny int64
	err := tx.QueryRow(ctx, `SELECT c.kind,cat.allow_bits,cat.deny_bits,ch.allow_bits,ch.deny_bits
 FROM chatxo.channels c
 LEFT JOIN LATERAL (
 SELECT COALESCE(bit_or(o.allow_bits),0) allow_bits,COALESCE(bit_or(o.deny_bits),0) deny_bits
 FROM chatxo.category_permissions o JOIN chatxo.roles r ON r.id=o.role_id
 WHERE o.category_id=c.category_id AND (r.is_default OR EXISTS(SELECT 1 FROM chatxo.user_roles ur WHERE ur.role_id=r.id AND ur.user_id=$2))
 ) cat ON true
 LEFT JOIN LATERAL (
 SELECT COALESCE(bit_or(o.allow_bits),0) allow_bits,COALESCE(bit_or(o.deny_bits),0) deny_bits
 FROM chatxo.channel_permissions o JOIN chatxo.roles r ON r.id=o.role_id
 WHERE o.channel_id=c.id AND (r.is_default OR EXISTS(SELECT 1 FROM chatxo.user_roles ur WHERE ur.role_id=r.id AND ur.user_id=$2))
 ) ch ON true
 WHERE c.instance_id=$1 AND c.id=$3`, s.instanceID, p.User.ID, channel).Scan(&kind, &categoryAllow, &categoryDeny, &channelAllow, &channelDeny)
	if err != nil {
		return translate(err)
	}
	if kind != "voice" {
		return ErrInvalid
	}
	bits := (((p.Permissions | categoryAllow) & ^categoryDeny) | channelAllow) & ^channelDeny
	if !p.Can(Administrator) && bits&ViewChannel == 0 {
		return ErrForbidden
	}
	return nil
}

func (s *Service) VoiceRestrictions(ctx context.Context, p Principal, channel, after string) ([]VoiceRestrictionMember, error) {
	if after != "" && !ValidID(after) {
		return nil, ErrInvalid
	}
	tx, p, err := s.moderationTx(ctx, p, 0)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if !p.Can(MuteMembers) && !p.Can(DeafenMembers) {
		return nil, ErrForbidden
	}
	if err = s.voiceModerationChannel(ctx, tx, p, channel); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT v.user_id,v.muted,v.deafened,u.id,u.username,u.is_owner,u.created_at,u.display_name,u.accent_color
 FROM chatxo.voice_restrictions v JOIN chatxo.users u ON u.id=v.user_id AND u.instance_id=v.instance_id
 WHERE v.instance_id=$1 AND v.channel_id=$2 AND (v.muted OR v.deafened) AND ($3='' OR v.user_id>NULLIF($3,'')::uuid) ORDER BY v.user_id LIMIT 100`, s.instanceID, channel, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []VoiceRestrictionMember{}
	for rows.Next() {
		var v VoiceRestrictionMember
		if err = rows.Scan(&v.UserID, &v.Muted, &v.Deafened, &v.User.ID, &v.User.Username, &v.User.Owner, &v.User.CreatedAt, &v.User.DisplayName, &v.User.AccentColor); err != nil {
			return nil, err
		}
		items = append(items, v)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return items, translate(tx.Commit(ctx))
}

func (s *Service) SetVoiceRestriction(ctx context.Context, p Principal, channel, user string, muted, deafened *bool) (VoiceRestriction, error) {
	v := VoiceRestriction{UserID: user}
	if muted == nil && deafened == nil {
		return v, ErrInvalid
	}
	permission := int64(0)
	if muted != nil {
		permission |= MuteMembers
	}
	if deafened != nil {
		permission |= DeafenMembers
	}
	tx, p, err := s.moderationTx(ctx, p, permission)
	if err != nil {
		return v, err
	}
	defer tx.Rollback(ctx)
	if err = s.voiceModerationChannel(ctx, tx, p, channel); err != nil {
		return v, err
	}
	if err = s.moderateTarget(ctx, tx, p, user); err != nil {
		if !errors.Is(err, ErrNotFound) || (muted != nil && *muted) || (deafened != nil && *deafened) {
			return v, err
		}
		err = tx.QueryRow(ctx, `UPDATE chatxo.voice_restrictions SET muted=COALESCE($4,muted),deafened=COALESCE($5,deafened) WHERE instance_id=$1 AND channel_id=$2 AND user_id=$3 RETURNING muted,deafened`, s.instanceID, channel, user, muted, deafened).Scan(&v.Muted, &v.Deafened)
	} else {
		err = tx.QueryRow(ctx, `INSERT INTO chatxo.voice_restrictions(instance_id,channel_id,user_id,muted,deafened) VALUES($1,$2,$3,COALESCE($4,false),COALESCE($5,false))
 ON CONFLICT(instance_id,channel_id,user_id) DO UPDATE SET muted=COALESCE($4,chatxo.voice_restrictions.muted),deafened=COALESCE($5,chatxo.voice_restrictions.deafened) RETURNING muted,deafened`, s.instanceID, channel, user, muted, deafened).Scan(&v.Muted, &v.Deafened)
	}
	if err != nil {
		return v, translate(err)
	}
	for _, change := range []struct {
		name  string
		value *bool
	}{{"mute", muted}, {"deafen", deafened}} {
		if change.value == nil {
			continue
		}
		action := "voice." + change.name
		if !*change.value {
			action = "voice.un" + change.name
		}
		if err = s.audit(ctx, tx, p.User.ID, user, action, channel); err != nil {
			return v, err
		}
	}
	return v, translate(tx.Commit(ctx))
}
