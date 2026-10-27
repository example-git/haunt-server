package attachments

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/example-git/haunt-server/internal/blobstore"
	"github.com/example-git/haunt-server/internal/chat"
	"github.com/example-git/haunt-server/internal/config"
	"github.com/example-git/haunt-server/internal/identity"
	"github.com/example-git/haunt-server/internal/platform/postgres"
	"github.com/jackc/pgx/v5"
)

var ErrFull = errors.New("attachment_storage_full")
var ErrMedia = errors.New("unsupported_or_invalid_media")

type Attachment struct {
	IsDM           bool `json:"is_dm"`
	Encrypted      bool `json:"encrypted"`
	provider       *string
	policy         config.Policy
	ID             string     `json:"id"`
	ConversationID string     `json:"conversation_id"`
	UserID         string     `json:"user_id"`
	Filename       string     `json:"filename"`
	ContentType    string     `json:"content_type"`
	Bytes          int64      `json:"bytes"`
	ExpiresAt      *time.Time `json:"expires_at"`
}
type Store struct {
	Providers *blobstore.Registry
	chat      *chat.Store
	dir       string
	probe     string
	lock      *os.File
	slots     chan struct{}
}

func New(ctx context.Context, c config.Config, ch *chat.Store) (*Store, error) {
	probe, err := exec.LookPath(c.FFprobe)
	if err != nil {
		return nil, fmt.Errorf("ffprobe is required for actual-content attachment validation")
	}
	root, err := filepath.Abs(filepath.Join(c.DataDir, "attachments"))
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(root, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("attachment directory is already owned by a running process")
	}
	s := &Store{chat: ch, dir: root, probe: probe, lock: lock, slots: make(chan struct{}, 2)}
	s.Providers, err = blobstore.New(ctx, ch.Pool, ch.Identity, c.DataDir, c.InstanceID)
	if err != nil {
		s.Close()
		return nil, err
	}
	if err = s.reconcile(ctx); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error          { return s.lock.Close() }
func (s *Store) path(id string) string { return filepath.Join(s.dir, id) }
func (s *Store) reserve(ctx context.Context, p identity.Principal, conv, name string, size int64, encrypted bool) (Attachment, error) {
	if err := s.chat.Access(ctx, p, conv, true); err != nil {
		return Attachment{}, err
	}
	tx, err := s.chat.Pool.Begin(ctx)
	if err != nil {
		return Attachment{}, err
	}
	defer tx.Rollback(ctx)
	var used int64
	if err = s.chat.LockStorage(ctx, tx); err != nil {
		return Attachment{}, err
	}
	policy, err := s.chat.PolicyForTransaction(ctx, tx)
	if err != nil {
		return Attachment{}, err
	}
	var isDM bool
	if err = tx.QueryRow(ctx, `SELECT kind='dm' FROM chatxo.conversations WHERE id=$1`, conv).Scan(&isDM); err != nil {
		return Attachment{}, err
	}
	if isDM && !policy.DM.ImagesEnabled {
		return Attachment{}, ErrDMDisabled
	}
	if isDM != encrypted {
		return Attachment{}, ErrMedia
	}
	max := policy.VideoMaxBytes
	if policy.ImageMaxBytes > max {
		max = policy.ImageMaxBytes
	}
	if isDM {
		max = policy.DM.ImageMaxBytes
		name = "encrypted-image"
	}
	if size < 1 || size > max || len(name) < 1 || len(name) > 128 || strings.ContainsAny(name, "\x00\r\n/\\") {
		return Attachment{}, identity.ErrInvalid
	}
	if err = tx.QueryRow(ctx, `SELECT attachment_bytes-dm_attachment_bytes FROM chatxo.storage WHERE singleton`).Scan(&used); err != nil {
		return Attachment{}, err
	}
	if !isDM && size > policy.AttachmentMaxBytes-used {
		return Attachment{}, ErrFull
	}
	if isDM {
		var own, all int64
		if err = tx.QueryRow(ctx, `SELECT COALESCE((SELECT sum(bytes) FROM chatxo.attachments WHERE is_dm AND user_id=$1),0),dm_attachment_bytes FROM chatxo.storage WHERE singleton`, p.User.ID).Scan(&own, &all); err != nil {
			return Attachment{}, err
		}
		if size > policy.DM.UserStorageBytes-own || size > policy.DM.StorageBytes-all {
			return Attachment{}, ErrDMFull
		}
		if err = chargeDMTransfer(ctx, tx, p.User.ID, size, true, policy.DM); err != nil {
			return Attachment{}, err
		}
	}
	var active int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM chatxo.attachments WHERE user_id=$1 AND status='uploading'`, p.User.ID).Scan(&active); err != nil {
		return Attachment{}, err
	}
	if active >= 4 {
		return Attachment{}, identity.ErrBusy
	}
	a := Attachment{IsDM: isDM, Encrypted: encrypted, ID: identity.NewID(), ConversationID: conv, UserID: p.User.ID, Filename: name, Bytes: size, policy: policy}
	if err = tx.QueryRow(ctx, `SELECT (SELECT id FROM chatxo.storage_providers WHERE active)`).Scan(&a.provider); err != nil {
		return Attachment{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO chatxo.attachments(id,conversation_id,user_id,status,filename,bytes,expires_at,storage_provider_id,is_dm,encrypted) VALUES($1,$2,$3,'uploading',$4,$5,now()+interval '10 minutes',$6,$7,$8)`, a.ID, conv, p.User.ID, name, size, a.provider, isDM, encrypted); err != nil {
		return Attachment{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE chatxo.storage SET attachment_bytes=attachment_bytes+$1 WHERE singleton`, size); err != nil {
		return Attachment{}, err
	}
	return a, tx.Commit(ctx)
}

func (s *Store) Upload(ctx context.Context, p identity.Principal, conv, name string, size int64, reader io.Reader, encrypted ...bool) (Attachment, error) {
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		return Attachment{}, identity.ErrBusy
	}
	a, err := s.reserve(ctx, p, conv, name, size, len(encrypted) > 0 && encrypted[0])
	if err != nil {
		return Attachment{}, err
	}
	complete := false
	defer func() {
		if !complete {
			cleanupCtx, cancel := context.WithTimeout(postgres.WithWorkload(context.Background(), config.Media), 5*time.Second)
			defer cancel()
			_ = s.markDeleting(cleanupCtx, a.ID)
			_ = s.remove(cleanupCtx, a.ID)
		}
	}()
	file, err := os.OpenFile(s.path(a.ID)+".part", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return Attachment{}, err
	}
	received, copyErr := io.Copy(file, io.LimitReader(reader, size+1))
	syncErr := file.Sync()
	closeErr := file.Close()
	if copyErr != nil {
		return Attachment{}, copyErr
	}
	if syncErr != nil {
		return Attachment{}, syncErr
	}
	if closeErr != nil {
		return Attachment{}, closeErr
	}
	if received != size || ctx.Err() != nil {
		return Attachment{}, identity.ErrInvalid
	}
	mediaType := "application/octet-stream"
	if a.Encrypted {
		if size < 29 {
			return Attachment{}, ErrMedia
		}
	} else {
		mediaType, err = s.validate(ctx, s.path(a.ID)+".part", size, a.policy)
	}
	if err != nil {
		return Attachment{}, err
	}
	// Recheck membership after receiving bytes; permissions may have changed.
	fresh, err := s.chat.Identity.Refresh(ctx, p)
	if err != nil {
		return Attachment{}, err
	}
	if err = s.chat.Access(ctx, fresh, conv, true); err != nil {
		return Attachment{}, err
	}
	if a.provider != nil {
		staged, openErr := os.Open(s.path(a.ID) + ".part")
		if openErr != nil {
			return Attachment{}, openErr
		}
		err = s.Providers.Put(ctx, *a.provider, a.ID, staged)
		staged.Close()
		if err != nil {
			return Attachment{}, err
		}
		if err = os.Remove(s.path(a.ID) + ".part"); err != nil {
			return Attachment{}, err
		}
	} else {
		if err = os.Rename(s.path(a.ID)+".part", s.path(a.ID)); err != nil {
			return Attachment{}, err
		}
		dir, err := os.Open(s.dir)
		if err != nil {
			return Attachment{}, err
		}
		err = dir.Sync()
		dir.Close()
		if err != nil {
			return Attachment{}, err
		}

	}
	tx, err := s.chat.Pool.Begin(ctx)
	if err != nil {
		return Attachment{}, err
	}
	defer tx.Rollback(ctx)
	if err = s.chat.LockStorage(ctx, tx); err != nil {
		return Attachment{}, err
	}
	a.ContentType = mediaType
	retention := a.policy.AttachmentRetentionDays
	if a.IsDM {
		retention = 1
	} // unclaimed DM uploads expire; claiming sets DM retention
	err = tx.QueryRow(ctx, `UPDATE chatxo.attachments SET status='ready',content_type=$2,created_at=clock_timestamp(),expires_at=clock_timestamp()+$3::bigint*interval '1 day' WHERE id=$1 AND status='uploading' RETURNING expires_at`, a.ID, mediaType, retention).Scan(&a.ExpiresAt)
	if err != nil {
		return Attachment{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Attachment{}, err
	}
	complete = true
	return a, nil
}

type probeOutput struct {
	Streams []struct {
		Type      string `json:"codec_type"`
		Codec     string `json:"codec_name"`
		Width     int    `json:"width"`
		Height    int    `json:"height"`
		FrameRate string `json:"avg_frame_rate"`
	} `json:"streams"`
	Format struct {
		Name     string `json:"format_name"`
		Duration string `json:"duration"`
	} `json:"format"`
}
type boundedBuffer struct{ bytes.Buffer }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 1<<20 {
		return 0, errors.New("probe output too large")
	}
	return b.Buffer.Write(p)
}
func (s *Store) validate(ctx context.Context, path string, size int64, policy config.Policy) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	header := make([]byte, 512)
	n, _ := file.Read(header)
	file.Close()
	kind := http.DetectContentType(header[:n])
	image := strings.HasPrefix(kind, "image/")
	switch kind {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		if size > policy.ImageMaxBytes {
			return "", ErrMedia
		}
	case "video/mp4", "video/webm", "application/octet-stream":
		if size > policy.VideoMaxBytes {
			return "", ErrMedia
		}
	default:
		return "", ErrMedia
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, s.probe, "-v", "error", "-protocol_whitelist", "file", "-probesize", "10485760", "-analyzeduration", "3000000", "-show_entries", "stream=codec_type,codec_name,width,height,avg_frame_rate:format=format_name,duration", "-of", "json", path)
	var output boundedBuffer
	cmd.Stdout = &output
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return "", ErrMedia
	}
	var metadata probeOutput
	if json.Unmarshal(output.Bytes(), &metadata) != nil || len(metadata.Streams) == 0 || len(metadata.Streams) > 3 {
		return "", ErrMedia
	}
	if kind == "application/octet-stream" {
		if !slices.Contains(strings.Split(metadata.Format.Name, ","), "mp4") {
			return "", ErrMedia
		}
		kind = "video/mp4"
	}
	videos := 0
	for _, stream := range metadata.Streams {
		switch stream.Type {
		case "video":
			videos++
			dimension := 4096
			if image {
				dimension = 8192
			}
			if stream.Width < 1 || stream.Height < 1 || stream.Width > dimension || stream.Height > dimension || int64(stream.Width)*int64(stream.Height) > 32_000_000 {
				return "", ErrMedia
			}
			if !image {
				parts := strings.Split(stream.FrameRate, "/")
				if len(parts) == 2 {
					numerator, _ := strconv.ParseFloat(parts[0], 64)
					denominator, _ := strconv.ParseFloat(parts[1], 64)
					if denominator > 0 && numerator/denominator > 120 {
						return "", ErrMedia
					}
				}
			}
		case "audio":
			if image {
				return "", ErrMedia
			}
		default:
			return "", ErrMedia
		}
	}
	if videos != 1 {
		return "", ErrMedia
	}
	if !image {
		duration, err := strconv.ParseFloat(metadata.Format.Duration, 64)
		if err != nil || duration <= 0 || duration > 300 {
			return "", ErrMedia
		}
	}
	return kind, nil
}

func (s *Store) Metadata(ctx context.Context, p identity.Principal, id string) (Attachment, error) {
	if !identity.ValidID(id) {
		return Attachment{}, identity.ErrInvalid
	}
	var a Attachment
	err := s.chat.Pool.QueryRow(ctx, `SELECT id,conversation_id,user_id,filename,content_type,bytes,expires_at,storage_provider_id,is_dm,encrypted FROM chatxo.attachments WHERE id=$1 AND status='ready' AND CASE WHEN is_dm AND encrypted AND claimed THEN $2::bigint=0 OR created_at>now()-$2::bigint*interval '1 day' ELSE expires_at IS NULL OR expires_at>now() END`, id, s.chat.CurrentPolicy().DM.ImageRetentionDays).Scan(&a.ID, &a.ConversationID, &a.UserID, &a.Filename, &a.ContentType, &a.Bytes, &a.ExpiresAt, &a.provider, &a.IsDM, &a.Encrypted)
	if errors.Is(err, pgx.ErrNoRows) {
		return Attachment{}, identity.ErrNotFound
	}
	if err != nil {
		return Attachment{}, err
	}
	if err = s.chat.Access(ctx, p, a.ConversationID, false); err != nil {
		return Attachment{}, err
	}
	return a, nil
}
func (s *Store) Get(ctx context.Context, p identity.Principal, id string) (Attachment, blobstore.ReadSeekCloser, error) {
	a, err := s.Metadata(ctx, p, id)
	if err != nil {
		return Attachment{}, nil, err
	}
	// Charge before reading local or third-party storage.
	if err = s.ReserveDMDownload(ctx, p, a); err != nil {
		return Attachment{}, nil, err
	}
	if a.provider != nil {
		file, err := s.Providers.Open(ctx, *a.provider, id, a.Bytes)
		return a, file, err
	}
	file, err := os.Open(s.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return Attachment{}, nil, identity.ErrNotFound
	}
	return a, file, err
}
func (s *Store) Delete(ctx context.Context, p identity.Principal, id string) error {
	if !identity.ValidID(id) {
		return identity.ErrInvalid
	}
	var conv, user string
	err := s.chat.Pool.QueryRow(ctx, `SELECT conversation_id,user_id FROM chatxo.attachments WHERE id=$1`, id).Scan(&conv, &user)
	if errors.Is(err, pgx.ErrNoRows) {
		return identity.ErrNotFound
	}
	if err != nil {
		return err
	}
	if user != p.User.ID {
		return identity.ErrForbidden
	}
	if err = s.chat.Access(ctx, p, conv, true); err != nil {
		return err
	}
	if err = s.markDeleting(ctx, id); err != nil {
		return err
	}
	return s.remove(ctx, id)
}
func (s *Store) markDeleting(ctx context.Context, id string) error {
	tx, err := s.chat.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = s.chat.LockStorage(ctx, tx); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE chatxo.attachments SET status='deleting' WHERE id=$1`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) remove(ctx context.Context, id string) error {
	var provider *string
	err := s.chat.Pool.QueryRow(ctx, `SELECT storage_provider_id FROM chatxo.attachments WHERE id=$1 AND status='deleting'`, id).Scan(&provider)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if provider != nil {
		if err = s.Providers.Remove(ctx, *provider, id); err != nil {
			return err
		}
	}

	for _, suffix := range []string{"", ".part"} {
		err := os.Remove(s.path(id) + suffix)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	// Keep accounting until physical deletion succeeds. Concurrent removals
	// are idempotent, and releasing storage is transactional.
	tx, err := s.chat.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = s.chat.LockStorage(ctx, tx); err != nil {
		return err
	}
	var size int64
	err = tx.QueryRow(ctx, `DELETE FROM chatxo.attachments WHERE id=$1 AND status='deleting' RETURNING bytes`, id).Scan(&size)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE chatxo.storage SET attachment_bytes=attachment_bytes-$1 WHERE singleton`, size); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) Maintain(ctx context.Context) error {
	if _, err := s.chat.Pool.Exec(ctx, `DELETE FROM chatxo.dm_transfers WHERE day < (now() AT TIME ZONE 'UTC')::date-2`); err != nil {
		return err
	}
	rows, err := s.chat.Pool.Query(ctx, `SELECT id FROM chatxo.attachments WHERE status='deleting' OR CASE WHEN is_dm AND encrypted AND claimed THEN $1::bigint>0 AND created_at<=now()-$1::bigint*interval '1 day' ELSE expires_at<=now() END ORDER BY expires_at NULLS LAST LIMIT 64`, s.chat.CurrentPolicy().DM.ImageRetentionDays)
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
	var failures []error
	for _, id := range ids {
		if err := s.markDeleting(ctx, id); err != nil {
			failures = append(failures, err)
			continue
		}
		if err := s.remove(ctx, id); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
func (s *Store) reconcile(ctx context.Context) error {
	if _, err := s.chat.Pool.Exec(ctx, `UPDATE chatxo.attachments SET status='deleting' WHERE status='uploading'`); err != nil {
		return err
	}
	directory, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	for {
		entries, err := directory.ReadDir(256)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		for _, entry := range entries {
			if entry.Name() == ".lock" {
				continue
			}
			id := strings.TrimSuffix(entry.Name(), ".part")
			if !identity.ValidID(id) || !entry.Type().IsRegular() {
				return fmt.Errorf("unexpected entry in attachment directory")
			}
			var keep bool
			if err := s.chat.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM chatxo.attachments WHERE id=$1 AND status='ready')`, id).Scan(&keep); err != nil {
				return err
			}
			if !keep || strings.HasSuffix(entry.Name(), ".part") {
				if err := os.Remove(filepath.Join(s.dir, entry.Name())); err != nil {
					return err
				}
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
	}
	// Reconcile in bounded pages, releasing the pooled connection before
	// filesystem access and accounting transactions.
	after := "00000000-0000-0000-0000-000000000000"
	for {
		rows, err := s.chat.Pool.Query(ctx, `SELECT id,bytes FROM chatxo.attachments WHERE status='ready' AND storage_provider_id IS NULL AND id>$1 ORDER BY id LIMIT 256`, after)
		if err != nil {
			return err
		}
		items := []Attachment{}
		for rows.Next() {
			var item Attachment
			if err := rows.Scan(&item.ID, &item.Bytes); err != nil {
				rows.Close()
				return err
			}
			items = append(items, item)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, item := range items {
			info, err := os.Lstat(s.path(item.ID))
			if errors.Is(err, os.ErrNotExist) || (err == nil && (!info.Mode().IsRegular() || info.Size() != item.Bytes)) {
				if err := s.markDeleting(ctx, item.ID); err != nil {
					return err
				}
			} else if err != nil {
				return err
			}
			after = item.ID
		}
		if len(items) < 256 {
			break
		}
	}
	for {
		var remaining int
		if err := s.chat.Pool.QueryRow(ctx, `SELECT count(*) FROM chatxo.attachments WHERE status='deleting' OR CASE WHEN is_dm AND encrypted AND claimed THEN $1::bigint>0 AND created_at<=now()-$1::bigint*interval '1 day' ELSE expires_at<=now() END`, s.chat.CurrentPolicy().DM.ImageRetentionDays).Scan(&remaining); err != nil {
			return err
		}
		if remaining == 0 {
			break
		}
		if err := s.Maintain(ctx); err != nil {
			if errors.Is(err, blobstore.ErrUnavailable) {
				break
			}
			return err
		}
	}
	var accounted int64
	if err := s.chat.Pool.QueryRow(ctx, `SELECT attachment_bytes-(SELECT COALESCE(sum(bytes),0) FROM chatxo.attachments) FROM chatxo.storage WHERE singleton`).Scan(&accounted); err != nil {
		return err
	}
	if accounted != 0 {
		return errors.New("attachment accounting mismatch")
	}
	return nil
}
