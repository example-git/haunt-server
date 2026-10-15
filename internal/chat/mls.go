package chat

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/example-git/haunt-server/internal/identity"
	"github.com/jackc/pgx/v5"
	"strings"
)

const MLSPrefix = "!haunt-mls-v1!"

var ErrEncryption = errors.New("dm_encryption_required")
var ErrEpoch = errors.New("dm_epoch_changed")

type DMDevice struct {
	ID         string `json:"id"`
	UserID     string `json:"user_id"`
	PublicKey  []byte `json:"public_key"`
	Credential string `json:"credential"`
	Revoked    bool   `json:"revoked"`
}
type RegisterDMDevice struct {
	ID        string `json:"id"`
	PublicKey []byte `json:"public_key"`
	Signature []byte `json:"signature"`
}
type DMKeyPackage struct {
	ID       string `json:"id"`
	DeviceID string `json:"device_id"`
	Data     []byte `json:"data"`
}
type DMKeyPackages struct {
	Packages []DMKeyPackage `json:"packages"`
}
type DMCryptoBundle struct {
	Revision int64          `json:"revision"`
	Members  []string       `json:"members"`
	Devices  []DMDevice     `json:"devices"`
	Packages []DMKeyPackage `json:"packages"`
}
type DMCryptoEvent struct {
	Revision  int64    `json:"revision"`
	RequestID string   `json:"request_id"`
	Actor     string   `json:"actor"`
	Commit    []byte   `json:"commit"`
	Welcome   []byte   `json:"welcome,omitempty"`
	Devices   []string `json:"devices"`
}
type DMCryptoEvents struct {
	Events []DMCryptoEvent `json:"events"`
}
type DMCryptoCommit struct {
	ExpectedRevision int64    `json:"expected_revision"`
	RequestID        string   `json:"request_id"`
	Actor            string   `json:"actor"`
	Commit           []byte   `json:"commit"`
	Welcome          []byte   `json:"welcome,omitempty"`
	Devices          []string `json:"devices"`
	Packages         []string `json:"packages"`
}
type MLSMessage struct {
	Device   string `json:"device"`
	Revision int64  `json:"revision"`
	Data     string `json:"data"`
}

func (s *Store) RegisterDMDevice(ctx context.Context, p identity.Principal, v RegisterDMDevice) (DMDevice, error) {
	d := DMDevice{ID: v.ID, UserID: p.User.ID, PublicKey: v.PublicKey}
	if !identity.ValidID(v.ID) || len(v.PublicKey) != 32 {
		return d, identity.ErrInvalid
	}
	raw, _ := json.Marshal([]string{s.InstanceID, p.User.ID, v.ID})
	d.Credential = string(raw)
	signed := "haunt-dm-device-v1\n" + s.InstanceID + "\n" + p.User.ID + "\n" + v.ID
	if !ed25519.Verify(v.PublicKey, []byte(signed), v.Signature) {
		return d, identity.ErrInvalid
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return d, err
	}
	defer tx.Rollback(ctx)
	// Account-local enrollment lock; devices and sessions for other users proceed.
	if _, err = tx.Exec(ctx, `SELECT id FROM chatxo.users WHERE id=$1 FOR UPDATE`, p.User.ID); err != nil {
		return d, err
	}
	var count int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM chatxo.dm_devices WHERE user_id=$1 AND id<>$2`, p.User.ID, d.ID).Scan(&count); err != nil {
		return d, err
	}
	if count >= 64 {
		return d, identity.ErrBusy
	}
	result, err := tx.Exec(ctx, `INSERT INTO chatxo.dm_devices(id,user_id,public_key,credential) VALUES($1,$2,$3,$4) ON CONFLICT(id) DO UPDATE SET credential=EXCLUDED.credential WHERE chatxo.dm_devices.user_id=EXCLUDED.user_id AND chatxo.dm_devices.public_key=EXCLUDED.public_key AND NOT chatxo.dm_devices.revoked`, d.ID, d.UserID, d.PublicKey, d.Credential)
	if err != nil {
		return d, err
	}
	if result.RowsAffected() != 1 {
		return d, identity.ErrConflict
	}
	return d, tx.Commit(ctx)
}
func (s *Store) AddDMPackages(ctx context.Context, p identity.Principal, device string, v DMKeyPackages) error {
	if !identity.ValidID(device) || len(v.Packages) > 32 {
		return identity.ErrInvalid
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var owner string
	if err = tx.QueryRow(ctx, `SELECT user_id FROM chatxo.dm_devices WHERE id=$1 AND NOT revoked FOR UPDATE`, device).Scan(&owner); err != nil {
		return identity.ErrNotFound
	}
	if owner != p.User.ID {
		return identity.ErrForbidden
	}
	if _, err = tx.Exec(ctx, `DELETE FROM chatxo.dm_key_packages WHERE device_id=$1 AND used_by IS NULL AND created_at<now()-interval '28 days'`, device); err != nil {
		return err
	}
	var count int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM chatxo.dm_key_packages WHERE device_id=$1 AND used_by IS NULL`, device).Scan(&count); err != nil {
		return err
	}

	for _, kp := range v.Packages {
		if !identity.ValidID(kp.ID) || len(kp.Data) < 100 || len(kp.Data) > 8192 || kp.DeviceID != device {
			return identity.ErrInvalid
		}
		result, e := tx.Exec(ctx, `INSERT INTO chatxo.dm_key_packages(id,device_id,data) VALUES($1,$2,$3) ON CONFLICT(id) DO UPDATE SET id=EXCLUDED.id WHERE chatxo.dm_key_packages.device_id=EXCLUDED.device_id AND chatxo.dm_key_packages.data=EXCLUDED.data`, kp.ID, device, kp.Data)
		if e != nil {
			return e
		}
		if result.RowsAffected() != 1 {
			return identity.ErrConflict
		}
	}
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM chatxo.dm_key_packages WHERE device_id=$1 AND used_by IS NULL`, device).Scan(&count); err != nil {
		return err
	}
	if count > 64 {
		return identity.ErrBusy
	}
	return tx.Commit(ctx)
}
func (s *Store) dmAccess(ctx context.Context, p identity.Principal, conv string) error {
	if err := s.Access(ctx, p, conv, false); err != nil {
		return err
	}
	var dm bool
	if err := s.Pool.QueryRow(ctx, `SELECT kind='dm' FROM chatxo.conversations WHERE id=$1`, conv).Scan(&dm); err != nil {
		return err
	}
	if !dm {
		return identity.ErrInvalid
	}
	return nil
}
func (s *Store) DMBundle(ctx context.Context, p identity.Principal, conv string) (DMCryptoBundle, error) {
	b := DMCryptoBundle{Members: []string{}, Devices: []DMDevice{}, Packages: []DMKeyPackage{}}
	if err := s.dmAccess(ctx, p, conv); err != nil {
		return b, err
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return b, err
	}
	defer tx.Rollback(ctx)
	err = tx.QueryRow(ctx, `SELECT revision,devices FROM chatxo.dm_crypto_groups WHERE conversation_id=$1`, conv).Scan(&b.Revision, &b.Members)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return b, err
	}
	rows, err := tx.Query(ctx, `SELECT d.id,d.user_id,d.public_key,d.credential,(d.revoked OR u.deleted_at IS NOT NULL OR u.disabled OR NOT EXISTS(SELECT 1 FROM chatxo.conversation_members m WHERE m.conversation_id=$1 AND m.user_id=d.user_id)) FROM chatxo.dm_devices d JOIN chatxo.users u ON u.id=d.user_id WHERE EXISTS(SELECT 1 FROM chatxo.conversation_members m WHERE m.conversation_id=$1 AND m.user_id=d.user_id) OR EXISTS(SELECT 1 FROM chatxo.dm_crypto_events e WHERE e.conversation_id=$1 AND (e.actor=d.id OR d.id=ANY(e.devices))) ORDER BY d.id`, conv)
	if err != nil {
		return b, err
	}
	for rows.Next() {
		var d DMDevice
		if err = rows.Scan(&d.ID, &d.UserID, &d.PublicKey, &d.Credential, &d.Revoked); err != nil {
			rows.Close()
			return b, err
		}
		b.Devices = append(b.Devices, d)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return b, err
	}
	rows, err = tx.Query(ctx, `SELECT DISTINCT ON(k.device_id) k.id,k.device_id,k.data FROM chatxo.dm_key_packages k JOIN chatxo.dm_devices d ON d.id=k.device_id JOIN chatxo.conversation_members m ON m.user_id=d.user_id WHERE m.conversation_id=$1 AND NOT d.revoked AND k.used_by IS NULL AND k.created_at>now()-interval '28 days' ORDER BY k.device_id,k.created_at`, conv)
	if err != nil {
		return b, err
	}
	for rows.Next() {
		var kp DMKeyPackage
		if err = rows.Scan(&kp.ID, &kp.DeviceID, &kp.Data); err != nil {
			rows.Close()
			return b, err
		}
		b.Packages = append(b.Packages, kp)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return b, err
	}
	return b, tx.Commit(ctx)
}
func (s *Store) DMEvents(ctx context.Context, p identity.Principal, conv string, after int64) (DMCryptoEvents, error) {
	out := DMCryptoEvents{Events: []DMCryptoEvent{}}
	if after < 0 {
		return out, identity.ErrInvalid
	}
	if err := s.dmAccess(ctx, p, conv); err != nil {
		return out, err
	}
	rows, err := s.Pool.Query(ctx, `SELECT revision,request_id,actor,commit_data,welcome,devices FROM chatxo.dm_crypto_events WHERE conversation_id=$1 AND revision>$2 ORDER BY revision LIMIT 100`, conv, after)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	totalBytes := 0
	for rows.Next() {
		var e DMCryptoEvent
		if err = rows.Scan(&e.Revision, &e.RequestID, &e.Actor, &e.Commit, &e.Welcome, &e.Devices); err != nil {
			return out, err
		}
		n := len(e.Commit) + len(e.Welcome) + len(e.Devices)*40 + 512
		if len(out.Events) > 0 && totalBytes+n > 1<<20 {
			break
		}
		totalBytes += n
		out.Events = append(out.Events, e)
	}
	return out, rows.Err()
}
func (s *Store) CommitDM(ctx context.Context, p identity.Principal, conv string, v DMCryptoCommit) (DMCryptoEvent, error) {
	out := DMCryptoEvent{RequestID: v.RequestID, Actor: v.Actor, Commit: v.Commit, Welcome: v.Welcome, Devices: v.Devices, Revision: v.ExpectedRevision + 1}
	if !identity.ValidID(v.RequestID) || !identity.ValidID(v.Actor) || v.ExpectedRevision < 0 || len(v.Commit) < 32 || len(v.Commit) > 262144 || len(v.Welcome) > 524288 || len(v.Devices) < 2 || len(v.Devices) > 256 || len(v.Packages) > 255 {
		return out, identity.ErrInvalid
	}
	seen := map[string]bool{}
	for _, id := range v.Devices {
		if !identity.ValidID(id) || seen[id] {
			return out, identity.ErrInvalid
		}
		seen[id] = true
	}
	if !seen[v.Actor] {
		return out, identity.ErrInvalid
	}
	for _, id := range v.Packages {
		if !identity.ValidID(id) {
			return out, identity.ErrInvalid
		}
	}
	if err := s.dmAccess(ctx, p, conv); err != nil {
		return out, err
	}
	raw, _ := json.Marshal(v)
	digest := sha256.Sum256(raw)
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	if err = s.LockStorage(ctx, tx); err != nil {
		return out, err
	}
	var previousHash []byte
	var previousConv string
	err = tx.QueryRow(ctx, `SELECT request_hash,conversation_id FROM chatxo.dm_crypto_events WHERE request_id=$1`, v.RequestID).Scan(&previousHash, &previousConv)
	if err == nil {
		if previousConv != conv || string(previousHash) != string(digest[:]) {
			return out, identity.ErrConflict
		}
		return out, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO chatxo.dm_crypto_groups(conversation_id) VALUES($1) ON CONFLICT DO NOTHING`, conv); err != nil {
		return out, err
	}
	var revision int64
	var old []string
	if err = tx.QueryRow(ctx, `SELECT revision,devices FROM chatxo.dm_crypto_groups WHERE conversation_id=$1 FOR UPDATE`, conv).Scan(&revision, &old); err != nil {
		return out, err
	}
	if revision != v.ExpectedRevision {
		return out, ErrEpoch
	}
	if revision >= 4096 {
		return out, identity.ErrBusy
	}
	var actorOK bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM chatxo.dm_devices WHERE id=$1 AND user_id=$2 AND NOT revoked)`, v.Actor, p.User.ID).Scan(&actorOK); err != nil {
		return out, err
	}
	if !actorOK {
		return out, identity.ErrForbidden
	}
	if revision > 0 {
		found := false
		for _, id := range old {
			if id == v.Actor {
				found = true
			}
		}
		if !found {
			return out, identity.ErrForbidden
		}
	}
	var authorized int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM chatxo.dm_devices d JOIN chatxo.conversation_members m ON m.user_id=d.user_id JOIN chatxo.users u ON u.id=d.user_id WHERE m.conversation_id=$1 AND d.id=ANY($2::uuid[]) AND NOT d.revoked AND u.deleted_at IS NULL AND NOT u.disabled`, conv, v.Devices).Scan(&authorized); err != nil {
		return out, err
	}
	if authorized != len(v.Devices) {
		return out, identity.ErrForbidden
	}
	// Every participant must have a device in the encrypted group.
	var missing bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM chatxo.conversation_members m JOIN chatxo.users u ON u.id=m.user_id WHERE m.conversation_id=$1 AND u.deleted_at IS NULL AND NOT u.disabled AND NOT EXISTS(SELECT 1 FROM chatxo.dm_devices d WHERE d.user_id=m.user_id AND d.id=ANY($2::uuid[])))`, conv, v.Devices).Scan(&missing); err != nil {
		return out, err
	}
	if missing {
		return out, identity.ErrInvalid
	}
	oldSet := map[string]bool{}
	for _, id := range old {
		oldSet[id] = true
		if !seen[id] {
			var revoked bool
			if err = tx.QueryRow(ctx, `SELECT d.revoked OR u.disabled OR u.deleted_at IS NOT NULL FROM chatxo.dm_devices d JOIN chatxo.users u ON u.id=d.user_id WHERE d.id=$1`, id).Scan(&revoked); err != nil {
				return out, err
			}
			if !revoked {
				return out, identity.ErrForbidden
			}
		}
	}
	added := map[string]bool{}
	for _, id := range v.Devices {
		if !oldSet[id] && (revision > 0 || id != v.Actor) {
			added[id] = true
		}
	}
	if len(added) != len(v.Packages) || len(added) > 0 && len(v.Welcome) < 32 {
		return out, identity.ErrInvalid
	}
	for _, id := range v.Packages {
		var device string
		err = tx.QueryRow(ctx, `UPDATE chatxo.dm_key_packages SET used_by=$2 WHERE id=$1 AND used_by IS NULL AND created_at>now()-interval '28 days' RETURNING device_id`, id, conv).Scan(&device)
		if errors.Is(err, pgx.ErrNoRows) {
			return out, ErrEpoch
		}
		if err != nil {
			return out, err
		}
		if !added[device] {
			return out, identity.ErrInvalid
		}
		delete(added, device)
	}
	policy, err := s.PolicyForTransaction(ctx, tx)
	if err != nil {
		return out, err
	}
	size := int64(len(raw) + 128)
	var used int64
	if err = tx.QueryRow(ctx, `SELECT dm_crypto_bytes+dm_text_bytes FROM chatxo.storage WHERE singleton`).Scan(&used); err != nil {
		return out, err
	}
	if size > policy.DM.TextMaxBytes-used {
		return out, ErrDMTextFull
	}
	if _, err = tx.Exec(ctx, `INSERT INTO chatxo.dm_crypto_events(conversation_id,revision,request_id,request_hash,actor,commit_data,welcome,devices) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, conv, out.Revision, v.RequestID, digest[:], v.Actor, v.Commit, v.Welcome, v.Devices); err != nil {
		return out, err
	}
	if _, err = tx.Exec(ctx, `UPDATE chatxo.dm_crypto_groups SET revision=$2,devices=$3 WHERE conversation_id=$1`, conv, out.Revision, v.Devices); err != nil {
		return out, err
	}
	if _, err = tx.Exec(ctx, `UPDATE chatxo.storage SET dm_crypto_bytes=dm_crypto_bytes+$1 WHERE singleton`, size); err != nil {
		return out, err
	}
	if err = s.emit(ctx, tx, conv, nil, "dm.keys_changed", map[string]any{"revision": out.Revision}); err != nil {
		return out, err
	}
	if err = s.trimEvents(ctx, tx, policy); err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}
func (s *Store) validateMLS(ctx context.Context, tx pgx.Tx, p identity.Principal, conv, body string) error {
	var revision int64
	var devices []string
	err := tx.QueryRow(ctx, `SELECT revision,devices FROM chatxo.dm_crypto_groups WHERE conversation_id=$1`, conv).Scan(&revision, &devices)
	if errors.Is(err, pgx.ErrNoRows) {
		if strings.HasPrefix(body, MLSPrefix) {
			return ErrEncryption
		}
		return nil
	}
	if err != nil {
		return err
	}
	if !strings.HasPrefix(body, MLSPrefix) {
		return ErrEncryption
	}
	var envelope MLSMessage
	if json.Unmarshal([]byte(strings.TrimPrefix(body, MLSPrefix)), &envelope) != nil || !identity.ValidID(envelope.Device) {
		return identity.ErrInvalid
	}
	if envelope.Revision != revision {
		return ErrEpoch
	}
	data, err := base64.StdEncoding.DecodeString(envelope.Data)
	if err != nil || len(data) < 32 || len(data) > 49152 {
		return identity.ErrInvalid
	}
	found := false
	for _, d := range devices {
		if d == envelope.Device {
			found = true
		}
	}
	if !found {
		return identity.ErrForbidden
	}
	var valid bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM chatxo.dm_devices WHERE id=$1 AND user_id=$2 AND NOT revoked)`, envelope.Device, p.User.ID).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return identity.ErrForbidden
	}
	return nil
}

func (s *Store) OwnDMPackages(ctx context.Context, p identity.Principal, device string) (DMKeyPackages, error) {
	out := DMKeyPackages{Packages: []DMKeyPackage{}}
	if !identity.ValidID(device) {
		return out, identity.ErrInvalid
	}
	rows, err := s.Pool.Query(ctx, `SELECT k.id,k.device_id,k.data FROM chatxo.dm_key_packages k JOIN chatxo.dm_devices d ON d.id=k.device_id WHERE d.id=$1 AND d.user_id=$2 AND NOT d.revoked AND k.used_by IS NULL AND k.created_at>now()-interval '28 days' ORDER BY k.created_at`, device, p.User.ID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var kp DMKeyPackage
		if err = rows.Scan(&kp.ID, &kp.DeviceID, &kp.Data); err != nil {
			return out, err
		}
		out.Packages = append(out.Packages, kp)
	}
	return out, rows.Err()
}

// Notify existing members when a newly enrolled device publishes its packages.
// Controls have no plaintext and do not increment unread message counts.
func (s *Store) AnnounceDMDevice(ctx context.Context, p identity.Principal) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = s.LockStorage(ctx, tx); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT conversation_id FROM chatxo.conversation_members WHERE user_id=$1 ORDER BY conversation_id LIMIT 256`, p.User.ID)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err = s.emit(ctx, tx, id, nil, "dm.devices_changed", map[string]string{"user": p.User.ID}); err != nil {
			return err
		}
	}
	policy, err := s.PolicyForTransaction(ctx, tx)
	if err != nil {
		return err
	}
	if err = s.trimEvents(ctx, tx, policy); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
