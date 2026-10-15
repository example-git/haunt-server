package identity

import (
	"bytes"
	"context"
	"image"
	"regexp"
)

const MaxEmojiBytes = 1 << 20
const MaxEmojis = 100

var emojiNamePattern = regexp.MustCompile(`^[a-z0-9_]{2,32}$`)

type Emoji struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Version  string `json:"version"`
	Animated bool   `json:"animated"`
}

func (s *Service) Emojis(ctx context.Context) ([]Emoji, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,name,version,animated FROM chatxo.emojis WHERE instance_id=$1 ORDER BY name`, s.instanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Emoji, 0)
	for rows.Next() {
		var v Emoji
		if err = rows.Scan(&v.ID, &v.Name, &v.Version, &v.Animated); err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, rows.Err()
}
func (s *Service) SaveEmoji(ctx context.Context, p Principal, name string, raw []byte) (Emoji, error) {
	if !p.Can(ManageServer) {
		return Emoji{}, ErrForbidden
	}
	if !emojiNamePattern.MatchString(name) || len(raw) == 0 || len(raw) > MaxEmojiBytes {
		return Emoji{}, ErrInvalid
	}
	select {
	case s.artSlots <- struct{}{}:
		defer func() { <-s.artSlots }()
	default:
		return Emoji{}, ErrBusy
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || cfg.Width > 256 || cfg.Height > 256 {
		return Emoji{}, ErrInvalid
	}
	data, err := normalizeArt(raw, false)
	if err != nil || len(data) > MaxEmojiBytes {
		return Emoji{}, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Emoji{}, err
	}
	defer tx.Rollback(ctx)
	// Only emoji mutations in this instance contend. Decoding happens before the
	// transaction; no unrelated database operations are serialized.
	if _, err = tx.Exec(ctx, `SELECT id FROM chatxo.instances WHERE id=$1 FOR UPDATE`, s.instanceID); err != nil {
		return Emoji{}, err
	}
	var count int
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT count(*),COALESCE(bool_or(name=$2),false) FROM chatxo.emojis WHERE instance_id=$1`, s.instanceID, name).Scan(&count, &exists); err != nil {
		return Emoji{}, err
	}
	if !exists && count >= MaxEmojis {
		return Emoji{}, ErrConflict
	}
	v := Emoji{ID: newID(), Name: name, Version: newID(), Animated: format == "gif"}
	err = tx.QueryRow(ctx, `INSERT INTO chatxo.emojis(id,instance_id,name,version,animated,image) VALUES($1,$2,$3,$4,$5,$6)
 ON CONFLICT(instance_id,name) DO UPDATE SET version=EXCLUDED.version,animated=EXCLUDED.animated,image=EXCLUDED.image RETURNING id`, v.ID, s.instanceID, v.Name, v.Version, v.Animated, data).Scan(&v.ID)
	if err != nil {
		return Emoji{}, translate(err)
	}
	return v, tx.Commit(ctx)
}
func (s *Service) DeleteEmoji(ctx context.Context, p Principal, id string) error {
	if !p.Can(ManageServer) {
		return ErrForbidden
	}
	if !ValidID(id) {
		return ErrInvalid
	}
	result, err := s.pool.Exec(ctx, `DELETE FROM chatxo.emojis WHERE instance_id=$1 AND id=$2`, s.instanceID, id)
	if err != nil {
		return translate(err)
	}
	if result.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
func (s *Service) EmojiImage(ctx context.Context, id string) ([]byte, error) {
	if !ValidID(id) {
		return nil, ErrInvalid
	}
	var data []byte
	err := s.pool.QueryRow(ctx, `SELECT image FROM chatxo.emojis WHERE instance_id=$1 AND id=$2`, s.instanceID, id).Scan(&data)
	return data, translate(err)
}
