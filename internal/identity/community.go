package identity

import (
	"context"
	"errors"
	"fmt"
	"github.com/example-git/haunt-server/internal/platform/postgres"
)

func (s *Service) Roles(ctx context.Context) ([]Role, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,name,permissions,is_default,position,display_separately FROM chatxo.roles WHERE instance_id=$1 ORDER BY is_default DESC,position DESC,name,id`, s.instanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Role{}
	for rows.Next() {
		var v Role
		if err := rows.Scan(&v.ID, &v.Name, &v.Permissions, &v.Default, &v.Position, &v.DisplaySeparately); err != nil {
			return nil, err
		}
		items = append(items, v)
	}
	return items, rows.Err()
}

func (s *Service) SaveRole(ctx context.Context, p Principal, id, name string, bits int64, requestedPosition *int, displaySeparately *bool) (Role, error) {
	position := 1
	if requestedPosition != nil {
		position = *requestedPosition
	}
	if !validName(name, 64) || bits < 0 || bits & ^AllPermissions != 0 || (id != "" && !ValidID(id)) || position < 0 || position > 1000 {
		return Role{}, ErrInvalid
	}
	tx, p, err := s.moderationTx(ctx, p, ManageRoles)
	if err != nil {
		return Role{}, err
	}
	defer tx.Rollback(ctx)
	v := Role{ID: id, Name: name, Permissions: bits, Position: position}
	if !p.User.Owner && (bits & ^p.Permissions != 0 || position >= p.Position) {
		return v, ErrForbidden
	}
	if id != "" {
		var oldPosition int
		var oldBits int64
		if err = tx.QueryRow(ctx, `SELECT position,permissions,is_default,display_separately FROM chatxo.roles WHERE instance_id=$1 AND id=$2 FOR UPDATE`, s.instanceID, id).Scan(&oldPosition, &oldBits, &v.Default, &v.DisplaySeparately); err != nil {
			return v, translate(err)
		}
		if !p.User.Owner && (oldPosition >= p.Position || oldBits & ^p.Permissions != 0 || (v.Default && !p.Can(Administrator))) {
			return v, ErrForbidden
		}
		if requestedPosition == nil {
			v.Position = oldPosition
		}
		if v.Default {
			v.Position = 0

		}
		if displaySeparately != nil {
			v.DisplaySeparately = *displaySeparately
		}
		_, err = tx.Exec(ctx, `UPDATE chatxo.roles SET name=$3,permissions=$4,position=$5,display_separately=$6 WHERE instance_id=$1 AND id=$2`, s.instanceID, id, name, bits, v.Position, v.DisplaySeparately)
	} else {
		v.ID = newID()
		if displaySeparately != nil {
			v.DisplaySeparately = *displaySeparately
		}
		_, err = tx.Exec(ctx, `INSERT INTO chatxo.roles(id,instance_id,name,permissions,position,display_separately) VALUES($1,$2,$3,$4,$5,$6)`, v.ID, s.instanceID, name, bits, position, v.DisplaySeparately)
	}
	if err != nil {
		return v, translate(err)
	}
	if err = s.audit(ctx, tx, p.User.ID, v.ID, "role.save", fmt.Sprintf("%s; permissions=%d; rank=%d; display_separately=%t", name, bits, v.Position, v.DisplaySeparately)); err != nil {
		return v, err
	}
	return v, translate(tx.Commit(ctx))
}
func (s *Service) DeleteRole(ctx context.Context, p Principal, id string) error {
	if !ValidID(id) {
		return ErrInvalid
	}
	tx, p, err := s.moderationTx(ctx, p, ManageRoles)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var position int
	var bits int64
	var isDefault bool
	if err = tx.QueryRow(ctx, `SELECT position,permissions,is_default FROM chatxo.roles WHERE instance_id=$1 AND id=$2 FOR UPDATE`, s.instanceID, id).Scan(&position, &bits, &isDefault); err != nil {
		return translate(err)
	}
	if isDefault {
		return ErrNotFound
	}
	if !p.User.Owner && (position >= p.Position || bits & ^p.Permissions != 0) {
		return ErrForbidden
	}
	if _, err = tx.Exec(ctx, `DELETE FROM chatxo.roles WHERE id=$1`, id); err != nil {
		return translate(err)
	}
	if err = s.audit(ctx, tx, p.User.ID, id, "role.delete", ""); err != nil {
		return err
	}
	return translate(tx.Commit(ctx))
}
func (s *Service) AssignRoles(ctx context.Context, p Principal, userID string, roles []string) error {
	if !ValidID(userID) || len(roles) > 32 {
		return ErrInvalid
	}
	seen := map[string]bool{}
	for _, id := range roles {
		if !ValidID(id) || seen[id] {
			return ErrInvalid
		}
		seen[id] = true
	}
	tx, p, err := s.moderationTx(ctx, p, ManageRoles)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = s.moderateTarget(ctx, tx, p, userID); err != nil {
		return err
	}
	for _, id := range roles {
		var position int
		var bits int64
		if err = tx.QueryRow(ctx, `SELECT position,permissions FROM chatxo.roles WHERE instance_id=$1 AND id=$2 AND NOT is_default`, s.instanceID, id).Scan(&position, &bits); err != nil {
			return translate(err)
		}
		if !p.User.Owner && (position >= p.Position || bits & ^p.Permissions != 0) {
			return ErrForbidden
		}
	}
	if _, err = tx.Exec(ctx, `DELETE FROM chatxo.user_roles WHERE user_id=$1`, userID); err != nil {
		return err
	}
	for _, id := range roles {
		if _, err = tx.Exec(ctx, `INSERT INTO chatxo.user_roles(user_id,role_id) VALUES($1,$2)`, userID, id); err != nil {
			return translate(err)
		}
	}
	if err = s.audit(ctx, tx, p.User.ID, userID, "member.roles", ""); err != nil {
		return err
	}
	return translate(tx.Commit(ctx))
}

type Member struct {
	User
	RoleIDs  []string `json:"role_ids"`
	Disabled bool     `json:"disabled"`
	Kicked   bool     `json:"kicked"`
	Deleted  bool     `json:"deleted"`
	Banned   bool     `json:"banned"`
	Position int      `json:"role_position"`
}

func (s *Service) Members(ctx context.Context, p Principal, after string) ([]Member, error) {
	if !p.Can(Administrator) && p.Permissions&(ManageRoles|KickMembers|BanMembers|DeleteMembers|ViewMemberIPs|ResetPasswords) == 0 {
		return nil, ErrForbidden
	}
	if after != "" && !ValidID(after) {
		return nil, ErrInvalid
	}
	var cursor any
	if after != "" {
		cursor = after
	}
	rows, err := s.pool.Query(ctx, `SELECT u.id,u.username,u.is_owner,u.created_at,COALESCE(array_agg(ur.role_id::text) FILTER (WHERE ur.role_id IS NOT NULL),'{}'),u.disabled,u.kicked,u.deleted_at IS NOT NULL,EXISTS(SELECT 1 FROM chatxo.bans b WHERE b.user_id=u.id AND b.revoked_at IS NULL),COALESCE((SELECT max(r.position) FROM chatxo.roles r JOIN chatxo.user_roles ur2 ON ur2.role_id=r.id WHERE ur2.user_id=u.id),0)
        FROM chatxo.users u LEFT JOIN chatxo.user_roles ur ON ur.user_id=u.id
        WHERE u.instance_id=$1 AND u.deleted_at IS NULL AND ($2::uuid IS NULL OR u.id>$2::uuid)
        GROUP BY u.id ORDER BY u.id LIMIT 100`, s.instanceID, cursor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Member{}
	for rows.Next() {
		var v Member
		if err := rows.Scan(&v.ID, &v.Username, &v.Owner, &v.CreatedAt, &v.RoleIDs, &v.Disabled, &v.Kicked, &v.Deleted, &v.Banned, &v.Position); err != nil {
			return nil, err
		}
		items = append(items, v)
	}
	return items, rows.Err()
}

func (s *Service) Categories(ctx context.Context, p Principal) ([]Category, error) {
	// Ordinary users see only categories containing an accessible channel.
	visible := map[string]bool{}
	if !p.Can(ManageChannels) {
		channels, err := s.Channels(ctx, p, "")
		if err != nil {
			return nil, err
		}
		for _, ch := range channels {
			if ch.CategoryID != nil {
				visible[*ch.CategoryID] = true
			}
		}
	}
	rows, err := s.pool.Query(ctx, `SELECT id,name FROM chatxo.categories WHERE instance_id=$1 ORDER BY name,id`, s.instanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Category{}
	for rows.Next() {
		var v Category
		if err := rows.Scan(&v.ID, &v.Name); err != nil {
			return nil, err
		}
		if p.Can(ManageChannels) || visible[v.ID] {
			items = append(items, v)
		}
	}
	return items, rows.Err()
}

func (s *Service) SaveCategory(ctx context.Context, p Principal, id, name string) (Category, error) {
	if !p.Can(ManageChannels) {
		return Category{}, ErrForbidden
	}
	if !validName(name, 64) || (id != "" && !ValidID(id)) {
		return Category{}, ErrInvalid
	}
	v := Category{Name: name}
	var err error
	if id == "" {
		v.ID = newID()
		_, err = s.pool.Exec(ctx, `INSERT INTO chatxo.categories(id,instance_id,name) VALUES ($1,$2,$3)`, v.ID, s.instanceID, name)
	} else {
		v.ID = id
		err = s.pool.QueryRow(ctx, `UPDATE chatxo.categories SET name=$3 WHERE instance_id=$1 AND id=$2 RETURNING id`, s.instanceID, id, name).Scan(&v.ID)
	}
	return v, translate(err)
}

// Apply category overrides, then channel overrides, to the authenticated
// request's base roles. At each scope deny wins across roles.
// Only read/send/connect bits can be overridden; management is instance-wide.
func (s *Service) Channels(ctx context.Context, p Principal, id string) ([]Channel, error) {
	if id != "" && !ValidID(id) {
		return nil, ErrInvalid
	}
	var selected any
	if id != "" {
		selected = id
	}
	rows, err := s.pool.Query(ctx, `SELECT c.id,c.category_id,c.name,c.kind,c.topic,c.icon,
        CASE WHEN $5 THEN $6::bigint ELSE (((($3::bigint | cat.allow_bits) & ~cat.deny_bits) | ch.allow_bits) & ~ch.deny_bits) END
        FROM chatxo.channels c
        LEFT JOIN LATERAL (
            SELECT COALESCE(bit_or(o.allow_bits),0) allow_bits,COALESCE(bit_or(o.deny_bits),0) deny_bits
            FROM chatxo.category_permissions o JOIN chatxo.roles r ON r.id=o.role_id
            WHERE o.category_id=c.category_id AND (r.is_default OR EXISTS(SELECT 1 FROM chatxo.user_roles ur WHERE ur.user_id=$2 AND ur.role_id=r.id))
        ) cat ON true
        LEFT JOIN LATERAL (
            SELECT COALESCE(bit_or(o.allow_bits),0) allow_bits,COALESCE(bit_or(o.deny_bits),0) deny_bits
            FROM chatxo.channel_permissions o JOIN chatxo.roles r ON r.id=o.role_id
            WHERE o.channel_id=c.id AND (r.is_default OR EXISTS(SELECT 1 FROM chatxo.user_roles ur WHERE ur.user_id=$2 AND ur.role_id=r.id))
        ) ch ON true
        WHERE c.instance_id=$1 AND ($4::uuid IS NULL OR c.id=$4::uuid) ORDER BY c.name,c.id`, s.instanceID, p.User.ID, p.Permissions, selected, p.Can(Administrator), AllPermissions)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Channel{}
	for rows.Next() {
		var v Channel
		if err := rows.Scan(&v.ID, &v.CategoryID, &v.Name, &v.Kind, &v.Topic, &v.Icon, &v.Permissions); err != nil {
			return nil, err
		}
		if v.Permissions&ViewChannel != 0 {
			items = append(items, v)
		}
	}
	return items, rows.Err()
}

func (s *Service) Channel(ctx context.Context, p Principal, id string) (Channel, error) {
	items, err := s.Channels(ctx, p, id)
	if err != nil {
		return Channel{}, err
	}
	if len(items) == 0 {
		return Channel{}, ErrNotFound
	}
	return items[0], nil
}

// ChannelDetails allows older clients to omit optional metadata without erasing it.
type ChannelDetails struct {
	Topic *string
	Icon  *string
}

func validChannelIcon(icon string) bool {
	switch icon {
	case "", "hash", "headphones", "message-circle", "megaphone", "volume-2", "mic", "music", "radio", "gamepad-2", "monitor", "video", "camera", "image", "film", "code", "terminal", "book-open", "graduation-cap", "coffee", "users", "calendar", "star", "heart", "shield", "wrench":
		return true
	}
	return false
}

func (s *Service) SaveChannel(ctx context.Context, p Principal, id, name, kind string, category *string, details ...ChannelDetails) (Channel, error) {
	if !p.Can(ManageChannels) {
		return Channel{}, ErrForbidden
	}
	if !validName(name, 64) || (kind != "text" && kind != "voice") || (id != "" && !ValidID(id)) || (category != nil && !ValidID(*category)) {
		return Channel{}, ErrInvalid
	}
	var topic, icon *string
	if len(details) > 0 {
		topic, icon = details[0].Topic, details[0].Icon
	}
	if icon != nil && !validChannelIcon(*icon) {
		return Channel{}, ErrInvalid
	}
	if topic != nil && !validText(*topic, 512) {
		return Channel{}, ErrInvalid
	}
	// Category references must belong to this installation. The schema's
	// singleton identity prevents a second installation sharing this database.
	if category != nil {
		var exists bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM chatxo.categories WHERE instance_id=$1 AND id=$2)`, s.instanceID, *category).Scan(&exists); err != nil {
			return Channel{}, err
		}
		if !exists {
			return Channel{}, ErrNotFound
		}
	}
	v := Channel{ID: id, CategoryID: category, Name: name, Kind: kind}
	var err error
	if id == "" {
		v.ID = newID()
		err = s.pool.QueryRow(ctx, `INSERT INTO chatxo.channels(id,instance_id,category_id,name,kind,topic,icon) VALUES ($1,$2,$3,$4,$5,COALESCE($6,''),COALESCE($7,'')) RETURNING topic,icon`, v.ID, s.instanceID, category, name, kind, topic, icon).Scan(&v.Topic, &v.Icon)
	} else {
		// Channel kinds are immutable so later message/media state cannot be
		// reinterpreted by changing a text channel into a voice channel.
		err = s.pool.QueryRow(ctx, `UPDATE chatxo.channels SET name=$3,category_id=$4,topic=COALESCE($6,topic),icon=COALESCE($7,icon) WHERE instance_id=$1 AND id=$2 AND kind=$5 RETURNING id,topic,icon`, s.instanceID, id, name, category, kind, topic, icon).Scan(&v.ID, &v.Topic, &v.Icon)
	}
	if err != nil {
		return Channel{}, translate(err)
	}
	// Return metadata independent of manager's read permission; managers may
	// manage channels they cannot read.
	return v, nil
}

func (s *Service) DeleteResource(ctx context.Context, p Principal, kind, id string) error {
	if !p.Can(ManageChannels) {
		return ErrForbidden
	}
	if !ValidID(id) {
		return ErrInvalid
	}
	query := `DELETE FROM chatxo.channels WHERE instance_id=$1 AND id=$2`
	if kind == "category" {
		query = `DELETE FROM chatxo.categories WHERE instance_id=$1 AND id=$2`
	}
	tag, err := s.pool.Exec(ctx, query, s.instanceID, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) SetOverride(ctx context.Context, p Principal, kind, id, roleID string, allow, deny int64) error {
	if !p.Can(ManageChannels) {
		return ErrForbidden
	}
	if !ValidID(id) || !ValidID(roleID) || allow < 0 || deny < 0 || (allow|deny) & ^ChannelPermissions != 0 || allow&deny != 0 {
		return ErrInvalid
	}
	query := `INSERT INTO chatxo.channel_permissions(channel_id,role_id,allow_bits,deny_bits)
        SELECT c.id,r.id,$4,$5 FROM chatxo.channels c JOIN chatxo.roles r ON r.instance_id=c.instance_id
        WHERE c.instance_id=$1 AND c.id=$2 AND r.id=$3
        ON CONFLICT (channel_id,role_id) DO UPDATE SET allow_bits=excluded.allow_bits,deny_bits=excluded.deny_bits`
	if kind == "category" {
		query = `INSERT INTO chatxo.category_permissions(category_id,role_id,allow_bits,deny_bits)
        SELECT c.id,r.id,$4,$5 FROM chatxo.categories c JOIN chatxo.roles r ON r.instance_id=c.instance_id
        WHERE c.instance_id=$1 AND c.id=$2 AND r.id=$3
        ON CONFLICT (category_id,role_id) DO UPDATE SET allow_bits=excluded.allow_bits,deny_bits=excluded.deny_bits`
	}
	tag, err := s.pool.Exec(ctx, query, s.instanceID, id, roleID, allow, deny)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) Overrides(ctx context.Context, p Principal, kind, id string) ([]Override, error) {
	if !p.Can(ManageChannels) {
		return nil, ErrForbidden
	}
	if !ValidID(id) {
		return nil, ErrInvalid
	}
	existsQuery := `SELECT id FROM chatxo.channels WHERE instance_id=$1 AND id=$2`
	query := `SELECT role_id,allow_bits,deny_bits FROM chatxo.channel_permissions WHERE channel_id=$1 ORDER BY role_id`
	if kind == "category" {
		existsQuery = `SELECT id FROM chatxo.categories WHERE instance_id=$1 AND id=$2`
		query = `SELECT role_id,allow_bits,deny_bits FROM chatxo.category_permissions WHERE category_id=$1 ORDER BY role_id`
	}
	var found string
	if err := s.pool.QueryRow(ctx, existsQuery, s.instanceID, id).Scan(&found); err != nil {
		return nil, translate(err)
	}
	rows, err := s.pool.Query(ctx, query, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Override{}
	for rows.Next() {
		var v Override
		if err := rows.Scan(&v.RoleID, &v.Allow, &v.Deny); err != nil {
			return nil, err
		}
		items = append(items, v)
	}
	return items, rows.Err()
}

// Ready verifies the migrated schema and persisted installation identity.
func (s *Service) Ready(ctx context.Context) error {
	var id string
	err := s.pool.QueryRow(ctx, `SELECT i.id FROM chatxo.instances i WHERE i.singleton AND (SELECT max(version) FROM chatxo.schema_migrations)=$1 AND EXISTS(SELECT 1 FROM chatxo.users WHERE instance_id=i.id AND is_owner)`, postgres.SchemaVersion).Scan(&id)
	if err != nil {
		return err
	}
	if id != s.instanceID {
		return errors.New("installation identity mismatch")
	}
	return nil
}

// Directory exposes only the local profiles needed to address DMs and messages.
// Administrative role assignments, disabled state, and credentials stay private.
func (s *Service) Directory(ctx context.Context, after, id string) ([]User, error) {
	if (after != "" && !ValidID(after)) || (id != "" && !ValidID(id)) {
		return nil, ErrInvalid
	}
	var cursor, selected any
	if after != "" {
		cursor = after
	}
	if id != "" {
		selected = id
	}
	rows, err := s.pool.Query(ctx, `SELECT u.id,u.username,u.is_owner,u.created_at,u.display_name,u.bio,u.accent_color,u.avatar_version,
        (SELECT jsonb_build_object('id',r.id,'name',r.name,'position',r.position) FROM chatxo.roles r
         WHERE r.instance_id=u.instance_id AND r.display_separately
         AND (r.is_default OR EXISTS(SELECT 1 FROM chatxo.user_roles ur WHERE ur.user_id=u.id AND ur.role_id=r.id))
         ORDER BY r.position DESC,r.id LIMIT 1)
        FROM chatxo.users u WHERE u.instance_id=$1 AND u.deleted_at IS NULL AND ($2::uuid IS NULL OR u.id>$2) AND ($3::uuid IS NULL OR u.id=$3) ORDER BY u.id LIMIT 100`, s.instanceID, cursor, selected)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []User{}
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.Owner, &u.CreatedAt, &u.DisplayName, &u.Bio, &u.AccentColor, &u.AvatarVersion, &u.DisplayRole); err != nil {
			return nil, err
		}
		items = append(items, u)
	}
	return items, rows.Err()
}
