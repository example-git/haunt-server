package identity

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

type ipContextKey struct{}

func WithRemoteIP(ctx context.Context, raw string) context.Context {
	ip, err := netip.ParseAddr(raw)
	if err != nil || ip.Zone() != "" {
		return ctx
	}
	return context.WithValue(ctx, ipContextKey{}, ip.Unmap().String())
}
func remoteIP(ctx context.Context) string { v, _ := ctx.Value(ipContextKey{}).(string); return v }
func nullableIP(ctx context.Context) any {
	if ip := remoteIP(ctx); ip != "" {
		return ip
	}
	return nil
}

type Ban struct {
	ID        string     `json:"id"`
	UserID    *string    `json:"user_id"`
	Network   *string    `json:"network"`
	Reason    string     `json:"reason"`
	CreatedBy string     `json:"created_by"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at"`
}
type AccessBan struct {
	Type   string `json:"ban_type"`
	Ticket string `json:"appeal_ticket"`
}

func (b *AccessBan) Error() string { return "banned" }

type BanStatus struct {
	Type         string     `json:"ban_type"`
	Reason       string     `json:"reason"`
	Active       bool       `json:"active"`
	NextAppealAt *time.Time `json:"next_appeal_at"`
}
type Appeal struct {
	ID         string     `json:"id"`
	BanID      string     `json:"ban_id"`
	Body       string     `json:"body"`
	CreatedAt  time.Time  `json:"created_at"`
	ReviewedAt *time.Time `json:"reviewed_at"`
}
type MemberIP struct {
	IP        string    `json:"ip"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}
type AuditEntry struct {
	ID        int64     `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	ActorID   *string   `json:"actor_id"`
	TargetID  string    `json:"target_id"`
	Action    string    `json:"action"`
	Reason    string    `json:"reason"`
	IP        *string   `json:"ip"`
}

func moderationText(value string, min, max int) bool {
	if !utf8.ValidString(value) || len(value) > max*4 || utf8.RuneCountInString(strings.TrimSpace(value)) < min || utf8.RuneCountInString(value) > max {
		return false
	}
	for _, r := range value {
		if (unicode.IsControl(r) && r != '\n') || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

// Moderation reads and writes share a serializable transaction. PostgreSQL
// resolves concurrent role changes; unrelated users/rooms are never serialized
// through an application-wide lock. Callers may retry a busy response.
func (s *Service) moderationTx(ctx context.Context, p Principal, permission int64) (pgx.Tx, Principal, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return nil, p, err
	}
	current, err := s.principal(ctx, tx, p.SessionID)
	if err == nil && !current.Can(permission) {
		err = ErrForbidden
	}
	if err != nil {
		tx.Rollback(ctx)
		return nil, p, err
	}
	return tx, current, nil
}

type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func (s *Service) principal(ctx context.Context, q rowQuerier, session string) (Principal, error) {
	return s.principalLookup(ctx, q, session, nil)
}
func (s *Service) principalLookup(ctx context.Context, q rowQuerier, session string, hash []byte, allowPasswordChange ...bool) (Principal, error) {
	p := Principal{}
	var changeRequired bool
	err := q.QueryRow(ctx, `SELECT u.id,u.username,u.is_owner,u.created_at,se.id,u.password_change_required,
 COALESCE((SELECT bit_or(r.permissions) FROM chatxo.roles r WHERE r.instance_id=u.instance_id AND (r.is_default OR EXISTS(SELECT 1 FROM chatxo.user_roles ur WHERE ur.user_id=u.id AND ur.role_id=r.id))),0),
 COALESCE((SELECT max(r.position) FROM chatxo.roles r JOIN chatxo.user_roles ur ON ur.role_id=r.id WHERE ur.user_id=u.id),0)
 FROM chatxo.sessions se JOIN chatxo.users u ON u.id=se.user_id WHERE se.instance_id=$1 AND (se.id=NULLIF($2,'')::uuid OR se.token_hash=$4) AND (se.expires_at IS NULL OR se.expires_at>now()) AND NOT u.disabled AND NOT u.kicked AND u.deleted_at IS NULL
 AND NOT EXISTS(SELECT 1 FROM chatxo.bans b WHERE b.instance_id=$1 AND b.revoked_at IS NULL AND (b.user_id=u.id OR COALESCE($3::inet,se.remote_ip) <<= b.network))`, s.instanceID, session, nullableIP(ctx), hash).Scan(&p.User.ID, &p.User.Username, &p.User.Owner, &p.User.CreatedAt, &p.SessionID, &changeRequired, &p.Permissions, &p.Position)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ErrUnauthorized
	}
	if err == nil && changeRequired && (len(allowPasswordChange) == 0 || !allowPasswordChange[0]) {
		return Principal{}, ErrPasswordChange
	}
	if p.Can(Administrator) {
		p.Permissions = AllPermissions
	}
	if p.User.Owner {
		p.Position = 1001
	}
	return p, err
}
func (s *Service) moderateTarget(ctx context.Context, tx pgx.Tx, p Principal, id string) error {
	if !ValidID(id) {
		return ErrInvalid
	}
	var owner bool
	var position int
	var deleted *time.Time
	err := tx.QueryRow(ctx, `SELECT is_owner,deleted_at,COALESCE((SELECT max(r.position) FROM chatxo.roles r JOIN chatxo.user_roles ur ON ur.role_id=r.id WHERE ur.user_id=u.id),0) FROM chatxo.users u WHERE instance_id=$1 AND id=$2 FOR UPDATE`, s.instanceID, id).Scan(&owner, &deleted, &position)
	if err != nil {
		return translate(err)
	}
	if owner || id == p.User.ID || (!p.User.Owner && position >= p.Position) {
		return ErrForbidden
	}
	if deleted != nil {
		return ErrNotFound
	}
	return nil
}
func (s *Service) audit(ctx context.Context, tx pgx.Tx, actor, target, action, reason string) error {
	var a any
	if actor != "" {
		a = actor
	}
	_, err := tx.Exec(ctx, `INSERT INTO chatxo.audit_log(instance_id,actor_id,target_id,action,reason,ip) VALUES($1,$2,$3,$4,$5,$6)`, s.instanceID, a, target, action, reason, nullableIP(ctx))
	return err
}

// RecordAudit is used for sampled access failures and successful legacy settings
// actions. New moderation operations write their audit record in the same tx.
func (s *Service) RecordAudit(ctx context.Context, actor, target, action, reason string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = s.audit(ctx, tx, actor, target, action, reason); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Service) ModerateMember(ctx context.Context, p Principal, id, action, reason string) error {
	permission := int64(0)
	switch action {
	case "kick":
		permission = KickMembers
	case "ban", "restore":
		permission = BanMembers
	case "delete":
		permission = DeleteMembers
	default:
		return ErrInvalid
	}
	if !moderationText(reason, 1, 512) {
		return ErrInvalid
	}
	tx, p, err := s.moderationTx(ctx, p, permission)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = s.moderateTarget(ctx, tx, p, id); err != nil {
		return err
	}
	switch action {
	case "kick":
		_, err = tx.Exec(ctx, `UPDATE chatxo.users SET kicked=true WHERE id=$1`, id)
	case "ban":
		_, err = tx.Exec(ctx, `INSERT INTO chatxo.bans(id,instance_id,user_id,reason,created_by,appeal_token) VALUES($1,$2,$3,$4,$5,$6)`, newID(), s.instanceID, id, reason, p.User.ID, newToken())
	case "restore":
		_, err = tx.Exec(ctx, `UPDATE chatxo.users SET kicked=false,disabled=false WHERE id=$1`, id)
		// Restore does not implicitly lift a ban; bans have their own audited action.
	case "delete":
		_, err = tx.Exec(ctx, `UPDATE chatxo.users SET disabled=true,kicked=false,deleted_at=now(),username=$2,password_hash='',display_name='Deleted user',bio='',avatar=NULL,avatar_version='',accent_color='#77d9b0' WHERE id=$1`, id, "deleted_"+strings.ReplaceAll(id, "-", "")[:24])
		if err == nil {
			_, err = tx.Exec(ctx, `DELETE FROM chatxo.member_ips WHERE user_id=$1`, id)
		}
		if err == nil {
			_, err = tx.Exec(ctx, `DELETE FROM chatxo.read_positions WHERE user_id=$1`, id)
		}
		if err == nil {
			_, err = tx.Exec(ctx, `DELETE FROM chatxo.conversation_members WHERE user_id=$1`, id)
		}
	}
	if err != nil {
		return translate(err)
	}
	if action != "restore" {
		if _, err = tx.Exec(ctx, `DELETE FROM chatxo.sessions WHERE user_id=$1`, id); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `DELETE FROM chatxo.invites WHERE created_by=$1`, id); err != nil {
			return err
		}
	}
	if action == "kick" || action == "delete" {
		if _, err = tx.Exec(ctx, `DELETE FROM chatxo.user_roles WHERE user_id=$1`, id); err != nil {
			return err
		}
	}
	if err = s.audit(ctx, tx, p.User.ID, id, "member."+action, reason); err != nil {
		return err
	}
	return translate(tx.Commit(ctx))
}
func canonicalNetwork(raw string) (netip.Prefix, error) {
	if addr, err := netip.ParseAddr(raw); err == nil && addr.Zone() == "" {
		addr = addr.Unmap()
		return netip.PrefixFrom(addr, addr.BitLen()), nil
	}
	prefix, err := netip.ParsePrefix(raw)
	if err != nil {
		return netip.Prefix{}, ErrInvalid
	}
	if prefix.Addr().Is4In6() {
		if prefix.Bits() < 96 {
			return netip.Prefix{}, ErrInvalid
		}
		prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
	}
	return prefix.Masked(), nil
}
func (s *Service) BanNetwork(ctx context.Context, p Principal, raw, reason string) (Ban, error) {
	prefix, err := canonicalNetwork(raw)
	if err != nil || !moderationText(reason, 1, 512) {
		return Ban{}, ErrInvalid
	}
	tx, p, err := s.moderationTx(ctx, p, BanMembers|ViewMemberIPs)
	if err != nil {
		return Ban{}, err
	}
	defer tx.Rollback(ctx)
	// Prevent a delegated moderator from using a range to bypass account hierarchy.
	var protected bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM chatxo.member_ips i JOIN chatxo.users u ON u.id=i.user_id WHERE u.instance_id=$1 AND i.ip <<= $2::cidr AND (u.is_owner OR u.id=$3 OR (NOT $4 AND COALESCE((SELECT max(r.position) FROM chatxo.user_roles ur JOIN chatxo.roles r ON r.id=ur.role_id WHERE ur.user_id=u.id),0)>=$5))) OR COALESCE($6::inet <<= $2::cidr,false)`, s.instanceID, prefix.String(), p.User.ID, p.User.Owner, p.Position, nullableIP(ctx)).Scan(&protected)
	if err != nil {
		return Ban{}, err
	}
	if protected {
		return Ban{}, ErrForbidden
	}
	var count int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM chatxo.bans WHERE instance_id=$1 AND revoked_at IS NULL AND network IS NOT NULL`, s.instanceID).Scan(&count); err != nil {
		return Ban{}, err
	}
	if count >= 1024 {
		return Ban{}, ErrConflict
	}
	b := Ban{ID: newID(), Reason: reason, CreatedBy: p.User.ID}
	network := prefix.String()
	b.Network = &network
	err = tx.QueryRow(ctx, `INSERT INTO chatxo.bans(id,instance_id,network,reason,created_by,appeal_token) VALUES($1,$2,$3,$4,$5,$6) RETURNING created_at`, b.ID, s.instanceID, network, reason, p.User.ID, newToken()).Scan(&b.CreatedAt)
	if err != nil {
		return b, translate(err)
	}
	if _, err = tx.Exec(ctx, `DELETE FROM chatxo.sessions WHERE instance_id=$1 AND remote_ip <<= $2::cidr`, s.instanceID, network); err != nil {
		return b, err
	}
	if err = s.audit(ctx, tx, p.User.ID, b.ID, "network.ban", reason); err != nil {
		return b, err
	}
	return b, translate(tx.Commit(ctx))
}
func (s *Service) RevokeBan(ctx context.Context, p Principal, id, reason string) error {
	if !ValidID(id) || !moderationText(reason, 1, 512) {
		return ErrInvalid
	}
	tx, p, err := s.moderationTx(ctx, p, BanMembers)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var user *string
	var network *string
	if err = tx.QueryRow(ctx, `SELECT user_id,network::text FROM chatxo.bans WHERE instance_id=$1 AND id=$2 AND revoked_at IS NULL FOR UPDATE`, s.instanceID, id).Scan(&user, &network); err != nil {
		return translate(err)
	}
	if user != nil {
		if err = s.moderateTarget(ctx, tx, p, *user); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	if network != nil && !p.Can(ViewMemberIPs) {
		return ErrForbidden
	}
	if _, err = tx.Exec(ctx, `UPDATE chatxo.bans SET revoked_at=now() WHERE id=$1`, id); err != nil {
		return err
	}
	if err = s.audit(ctx, tx, p.User.ID, id, "ban.revoke", reason); err != nil {
		return err
	}
	return translate(tx.Commit(ctx))
}
func (s *Service) CheckBan(ctx context.Context, userID string) error {
	var user any
	if userID != "" {
		user = userID
	}
	b := &AccessBan{}
	err := s.pool.QueryRow(ctx, `SELECT CASE WHEN user_id IS NULL THEN 'ip' ELSE 'user' END,appeal_token FROM chatxo.bans WHERE instance_id=$1 AND revoked_at IS NULL AND (user_id=$2 OR $3::inet <<= network) ORDER BY created_at,id LIMIT 1`, s.instanceID, user, nullableIP(ctx)).Scan(&b.Type, &b.Ticket)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return b
}
func (s *Service) BanStatus(ctx context.Context, ticket string) (BanStatus, error) {
	if !validToken(ticket) {
		return BanStatus{}, ErrNotFound
	}
	v := BanStatus{}
	err := s.pool.QueryRow(ctx, `SELECT CASE WHEN user_id IS NULL THEN 'ip' ELSE 'user' END,reason,revoked_at IS NULL,last_appeal_at+interval '30 days' FROM chatxo.bans WHERE instance_id=$1 AND appeal_token=$2`, s.instanceID, ticket).Scan(&v.Type, &v.Reason, &v.Active, &v.NextAppealAt)
	return v, translate(err)
}
func (s *Service) AppealBan(ctx context.Context, ticket, body string) error {
	if !validToken(ticket) || !moderationText(body, 10, 1000) || (strings.Contains(strings.ToLower(body), "://") || strings.Contains(strings.ToLower(body), "www.") || strings.ContainsAny(body, "<>")) {
		return ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var id string
	// A single conditional UPDATE locks this ban and makes concurrent monthly
	// submissions mutually exclusive, including submissions from different IPs.
	err = tx.QueryRow(ctx, `UPDATE chatxo.bans SET last_appeal_at=now() WHERE instance_id=$1 AND appeal_token=$2 AND revoked_at IS NULL AND (last_appeal_at IS NULL OR last_appeal_at<=now()-interval '30 days') RETURNING id`, s.instanceID, ticket).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO chatxo.ban_appeals(id,ban_id,body) VALUES($1,$2,$3)`, newID(), id, strings.TrimSpace(body)); err != nil {
		return err
	}
	if err = s.audit(ctx, tx, "", id, "ban.appeal", ""); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Service) Bans(ctx context.Context, p Principal, after string) ([]Ban, error) {
	if !p.Can(BanMembers) {
		return nil, ErrForbidden
	}
	if after != "" && !ValidID(after) {
		return nil, ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT id,user_id,CASE WHEN $3 THEN network::text ELSE NULL END,reason,created_by,created_at,revoked_at FROM chatxo.bans WHERE instance_id=$1 AND ($2='' OR id>NULLIF($2,'')::uuid) AND revoked_at IS NULL ORDER BY id LIMIT 100`, s.instanceID, after, p.Can(ViewMemberIPs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Ban{}
	for rows.Next() {
		var b Ban
		if err = rows.Scan(&b.ID, &b.UserID, &b.Network, &b.Reason, &b.CreatedBy, &b.CreatedAt, &b.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
func (s *Service) Appeals(ctx context.Context, p Principal, after string) ([]Appeal, error) {
	if !p.Can(BanMembers) {
		return nil, ErrForbidden
	}
	if after != "" && !ValidID(after) {
		return nil, ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT a.id,a.ban_id,a.body,a.created_at,a.reviewed_at FROM chatxo.ban_appeals a JOIN chatxo.bans b ON b.id=a.ban_id WHERE b.instance_id=$1 AND ($2='' OR a.id>NULLIF($2,'')::uuid) ORDER BY a.id LIMIT 100`, s.instanceID, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Appeal{}
	for rows.Next() {
		var a Appeal
		if err = rows.Scan(&a.ID, &a.BanID, &a.Body, &a.CreatedAt, &a.ReviewedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
func (s *Service) ReviewAppeal(ctx context.Context, p Principal, id string) error {
	if !ValidID(id) {
		return ErrInvalid
	}
	tx, p, err := s.moderationTx(ctx, p, BanMembers)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `UPDATE chatxo.ban_appeals a SET reviewed_at=now(),reviewed_by=$3 FROM chatxo.bans b WHERE a.id=$2 AND a.ban_id=b.id AND b.instance_id=$1 AND a.reviewed_at IS NULL`, s.instanceID, id, p.User.ID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	if err = s.audit(ctx, tx, p.User.ID, id, "appeal.review", ""); err != nil {
		return err
	}
	return translate(tx.Commit(ctx))
}
func (s *Service) MemberIPs(ctx context.Context, p Principal, id string) ([]MemberIP, error) {
	if !p.Can(ViewMemberIPs) {
		return nil, ErrForbidden
	}
	if !ValidID(id) {
		return nil, ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT host(i.ip),i.first_seen,i.last_seen FROM chatxo.member_ips i JOIN chatxo.users u ON u.id=i.user_id WHERE u.instance_id=$1 AND u.id=$2 ORDER BY i.last_seen DESC LIMIT 32`, s.instanceID, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MemberIP{}
	for rows.Next() {
		var i MemberIP
		if err = rows.Scan(&i.IP, &i.FirstSeen, &i.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}
func (s *Service) AuditLog(ctx context.Context, p Principal, before int64) ([]AuditEntry, error) {
	if !p.Can(ViewAuditLog) {
		return nil, ErrForbidden
	}
	if before < 0 {
		return nil, ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT id,created_at,actor_id,target_id,action,reason,CASE WHEN $3 THEN host(ip) ELSE NULL END FROM chatxo.audit_log WHERE instance_id=$1 AND ($2=0 OR id<$2) ORDER BY id DESC LIMIT 100`, s.instanceID, before, p.Can(ViewMemberIPs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var a AuditEntry
		if err = rows.Scan(&a.ID, &a.CreatedAt, &a.ActorID, &a.TargetID, &a.Action, &a.Reason, &a.IP); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
func (s *Service) observeIP(ctx context.Context, tx pgx.Tx, user string) error {
	if remoteIP(ctx) == "" {
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO chatxo.member_ips(user_id,ip) VALUES($1,$2) ON CONFLICT(user_id,ip) DO UPDATE SET last_seen=now()`, user, nullableIP(ctx))
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `DELETE FROM chatxo.member_ips WHERE user_id=$1 AND ip NOT IN (SELECT ip FROM chatxo.member_ips WHERE user_id=$1 ORDER BY last_seen DESC,ip LIMIT 32)`, user)
	return err
}

// Observation is sampled by the HTTP boundary, not written per gateway tick.
func (s *Service) ObserveSessionIP(ctx context.Context, p Principal) error {
	if remoteIP(ctx) == "" {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Account deletion and observation lock the same user to avoid recreating IP
	// history after an account was scrubbed.
	var id string
	if err = tx.QueryRow(ctx, `SELECT id FROM chatxo.users WHERE id=$1 AND NOT disabled AND NOT kicked AND deleted_at IS NULL FOR UPDATE`, p.User.ID).Scan(&id); err != nil {
		return ErrUnauthorized
	}
	if err = s.observeIP(ctx, tx, p.User.ID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE chatxo.sessions SET remote_ip=$2 WHERE id=$1 AND remote_ip IS DISTINCT FROM $2::inet`, p.SessionID, nullableIP(ctx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// AuditTransaction lets channel-message deletion commit with its audit entry.
func (s *Service) AuditTransaction(ctx context.Context, tx pgx.Tx, p Principal, target, action string) error {
	return s.audit(ctx, tx, p.User.ID, target, action, "")
}
