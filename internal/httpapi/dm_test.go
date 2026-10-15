package httpapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"github.com/example-git/haunt-server/internal/attachments"
	"github.com/example-git/haunt-server/internal/chat"
	"github.com/example-git/haunt-server/internal/identity"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"
)

func dmConversation(t *testing.T, f *fixture, token string, members ...string) chat.Conversation {
	t.Helper()
	var c chat.Conversation
	f.call(t, "POST", "/api/v1/dms", token, map[string]any{"members": members}, 201, &c)
	return c
}
func dmUpload(f *fixture, token, conv string, n int, encrypted bool) (int, attachments.Attachment, error) {
	suffix := ""
	if encrypted {
		suffix = "&encrypted=1"
	}
	req, _ := http.NewRequest("POST", f.server.URL+"/api/v1/conversations/"+conv+"/attachments?filename=image&size="+strconv64(int64(n))+suffix, bytes.NewReader(bytes.Repeat([]byte{42}, n)))
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := f.server.Client().Do(req)
	if err != nil {
		return 0, attachments.Attachment{}, err
	}
	defer res.Body.Close()
	var a attachments.Attachment
	_ = json.NewDecoder(res.Body).Decode(&a)
	return res.StatusCode, a, nil
}
func TestDMQuotaRetentionTransfersAndPurge(t *testing.T) {
	f, features := featureFixture(t, 4096)
	alice := f.register(t, "alice_dm")
	bob := f.register(t, "bob_dm")
	dm := dmConversation(t, f, f.owner.Token, alice.User.ID)
	other := dmConversation(t, f, alice.Token, bob.User.ID)
	cfg := getConfiguration(t, f).RuntimeConfiguration
	cfg.Settings.Limits.DM.UserStorageBytes = 1024
	cfg.Settings.Limits.DM.StorageBytes = 4096
	cfg.Settings.Limits.DM.ImageMaxBytes = 1024
	cfg.Settings.Limits.DM.DownloadBytesPerDay = 1024
	saveConfiguration(t, f, cfg)
	if status, _, e := dmUpload(f, f.owner.Token, dm.ID, 600, false); e != nil || status != 415 {
		t.Fatalf("unencrypted upload %d %v", status, e)
	}
	status, a, e := dmUpload(f, f.owner.Token, dm.ID, 600, true)
	if e != nil || status != 201 || !a.Encrypted {
		t.Fatalf("encrypted upload %d %v", status, e)
	}
	if status, _, e = dmUpload(f, f.owner.Token, dm.ID, 600, true); e != nil || status != 507 {
		t.Fatalf("quota %d %v", status, e)
	}
	var m chat.Message
	f.call(t, "POST", "/api/v1/conversations/"+dm.ID+"/messages", f.owner.Token, map[string]any{"request_id": identity.NewID(), "body": "legacy DM with encrypted upload", "attachment_ids": []string{a.ID}}, 201, &m)
	// DM retention is independent of the tiny channel budget and 30-day retention.
	if _, e = f.pool.Exec(context.Background(), `UPDATE chatxo.messages SET created_at=now()-interval '400 days' WHERE id=$1`, m.ID); e != nil {
		t.Fatal(e)
	}
	if e = features.Chat.Maintain(context.Background()); e != nil {
		t.Fatal(e)
	}
	var history chat.History
	f.call(t, "GET", "/api/v1/conversations/"+dm.ID+"/messages", alice.Token, nil, 200, &history)
	if len(history.Messages) != 1 {
		t.Fatal("indefinite DM text was pruned")
	}
	var expiry *time.Time
	if e = f.pool.QueryRow(context.Background(), `SELECT expires_at FROM chatxo.attachments WHERE id=$1`, a.ID).Scan(&expiry); e != nil || expiry != nil {
		t.Fatalf("indefinite image retention %v %v", expiry, e)
	}
	f.call(t, "GET", "/api/v1/attachments/"+a.ID, bob.Token, nil, 404, nil)
	for i, want := range []int{200, 429} {
		req, _ := http.NewRequest("GET", f.server.URL+"/api/v1/attachments/"+a.ID, nil)
		req.Header.Set("Authorization", "Bearer "+alice.Token)
		res, e := f.server.Client().Do(req)
		if e != nil {
			t.Fatal(e)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode != want {
			t.Fatalf("download %d: %d", i, res.StatusCode)
		}
	}
	var usage attachments.DMUsage
	f.call(t, "GET", "/api/v1/me/dm-storage", f.owner.Token, nil, 200, &usage)
	if usage.UsedBytes != 600 || usage.UploadedToday != 600 {
		t.Fatalf("accounting %+v", usage)
	}
	// Two sessions cannot both pass the same user's remaining storage reservation.
	statuses := make(chan int, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, _, _ := dmUpload(f, alice.Token, other.ID, 600, true)
			statuses <- status
		}()
	}
	wg.Wait()
	close(statuses)
	got := map[int]int{}
	for v := range statuses {
		got[v]++
	}
	if got[201] != 1 || got[507] != 1 {
		t.Fatalf("concurrent quota %#v", got)
	}
	before := time.Now().Add(time.Second)
	f.call(t, "POST", "/api/v1/me/dm-storage/purge", f.owner.Token, DMPurgeRequest{Before: before}, 400, nil)
	before = time.Now()
	var preview attachments.DMPurge
	f.call(t, "POST", "/api/v1/me/dm-storage/purge", f.owner.Token, DMPurgeRequest{Before: before}, 200, &preview)
	if preview.Files != 1 || preview.Bytes != 600 {
		t.Fatalf("purge preview %+v", preview)
	}
	f.call(t, "POST", "/api/v1/me/dm-storage/purge", f.owner.Token, DMPurgeRequest{Before: before, Confirm: true}, 200, &preview)
	f.call(t, "GET", "/api/v1/attachments/"+a.ID, f.owner.Token, nil, 404, nil)
	f.call(t, "GET", "/api/v1/me/dm-storage", f.owner.Token, nil, 200, &usage)
	if usage.UsedBytes != 0 || usage.UploadedToday != 600 {
		t.Fatal("purge refunded traffic or retained bytes")
	}
	f.call(t, "GET", "/api/v1/conversations/"+dm.ID+"/messages", alice.Token, nil, 200, &history)
	if len(history.Messages) != 1 {
		t.Fatal("purge deleted text")
	}
	var measured, actual int64
	if e = f.pool.QueryRow(context.Background(), `SELECT dm_attachment_bytes,(SELECT COALESCE(sum(bytes),0) FROM chatxo.attachments WHERE is_dm) FROM chatxo.storage`).Scan(&measured, &actual); e != nil || measured != actual || actual != 600 {
		t.Fatalf("DM bytes %d/%d %v", measured, actual, e)
	}
	cfg = getConfiguration(t, f).RuntimeConfiguration
	cfg.Settings.Limits.DM.ImagesEnabled = false
	cfg.Settings.Limits.DM.TextRetentionDays = 30
	saveConfiguration(t, f, cfg)
	if status, _, e = dmUpload(f, f.owner.Token, dm.ID, 100, true); e != nil || status != 403 {
		t.Fatalf("disabled %d %v", status, e)
	}
	if e = features.Chat.Maintain(context.Background()); e != nil {
		t.Fatal(e)
	}
	f.call(t, "GET", "/api/v1/conversations/"+dm.ID+"/messages", alice.Token, nil, 200, &history)
	if len(history.Messages) != 0 {
		t.Fatal("DM age retention not applied")
	}
}
func enrollDMDevice(t *testing.T, f *fixture, login identity.Login) (chat.DMDevice, chat.DMKeyPackage) {
	t.Helper()
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	id := identity.NewID()
	input := chat.RegisterDMDevice{ID: id, PublicKey: public, Signature: ed25519.Sign(private, []byte("haunt-dm-device-v1\n"+f.cfg.InstanceID+"\n"+login.User.ID+"\n"+id))}
	var device chat.DMDevice
	f.call(t, "POST", "/api/v1/me/dm-devices", login.Token, input, 200, &device)
	f.call(t, "POST", "/api/v1/me/dm-devices", login.Token, input, 200, nil)
	kp := chat.DMKeyPackage{ID: identity.NewID(), DeviceID: id, Data: bytes.Repeat([]byte{13}, 128)}
	f.call(t, "PUT", "/api/v1/me/dm-devices/"+id+"/packages", login.Token, chat.DMKeyPackages{Packages: []chat.DMKeyPackage{kp}}, 204, nil)
	// A key package retry cannot add entries or replace published bytes.
	f.call(t, "PUT", "/api/v1/me/dm-devices/"+id+"/packages", login.Token, chat.DMKeyPackages{Packages: []chat.DMKeyPackage{kp}}, 204, nil)
	input.Signature[0] ^= 1
	f.call(t, "POST", "/api/v1/me/dm-devices", login.Token, input, 400, nil)
	return device, kp
}
func TestDMEncryptionDeliveryAuthorizationAndCAS(t *testing.T) {
	f, _ := featureFixture(t, 256_000_000)
	alice := f.register(t, "alice_keys")
	bob := f.register(t, "bob_keys")
	dm := dmConversation(t, f, f.owner.Token, alice.User.ID)
	ownerDevice, _ := enrollDMDevice(t, f, f.owner)
	aliceDevice, packageA := enrollDMDevice(t, f, alice)
	path := "/api/v1/conversations/" + dm.ID + "/encryption"
	f.call(t, "GET", path, bob.Token, nil, 404, nil)
	commit := chat.DMCryptoCommit{RequestID: identity.NewID(), Actor: ownerDevice.ID, ExpectedRevision: 0, Commit: bytes.Repeat([]byte{3}, 100), Welcome: bytes.Repeat([]byte{4}, 100), Devices: []string{ownerDevice.ID, aliceDevice.ID}, Packages: []string{packageA.ID}}
	var event chat.DMCryptoEvent
	f.call(t, "POST", path+"/events", f.owner.Token, commit, 200, &event)
	if event.Revision != 1 {
		t.Fatal(event)
	}
	f.call(t, "POST", path+"/events", f.owner.Token, commit, 200, nil)
	conflict := commit
	conflict.RequestID = identity.NewID()
	f.call(t, "POST", path+"/events", f.owner.Token, conflict, 409, nil)
	conflict = commit
	conflict.Commit = bytes.Repeat([]byte{5}, 100)
	f.call(t, "POST", path+"/events", f.owner.Token, conflict, 409, nil)
	f.call(t, "POST", "/api/v1/conversations/"+dm.ID+"/messages", f.owner.Token, map[string]any{"request_id": identity.NewID(), "body": "must not downgrade"}, 409, nil)
	encoded, _ := json.Marshal(chat.MLSMessage{Device: ownerDevice.ID, Revision: 1, Data: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 100))})
	input := map[string]any{"request_id": identity.NewID(), "body": chat.MLSPrefix + string(encoded)}
	f.call(t, "POST", "/api/v1/conversations/"+dm.ID+"/messages", alice.Token, input, 403, nil)
	var m chat.Message
	f.call(t, "POST", "/api/v1/conversations/"+dm.ID+"/messages", f.owner.Token, input, 201, &m)
	f.call(t, "POST", "/api/v1/conversations/"+dm.ID+"/messages", f.owner.Token, input, 201, nil)
	var packages chat.DMKeyPackages
	f.call(t, "GET", "/api/v1/me/dm-devices/"+aliceDevice.ID+"/packages", alice.Token, nil, 200, &packages)
	if len(packages.Packages) != 0 {
		t.Fatal("key package reused")
	}
	var bundle chat.DMCryptoBundle
	f.call(t, "GET", path, alice.Token, nil, 200, &bundle)
	if bundle.Revision != 1 || len(bundle.Members) != 2 {
		t.Fatal(bundle)
	}
}

func TestDMImageRuntimeRetentionAndDeletion(t *testing.T) {
	f, features := featureFixture(t, 4096)
	alice := f.register(t, "alice_image_age")
	dm := dmConversation(t, f, f.owner.Token, alice.User.ID)
	upload := func() (attachments.Attachment, chat.Message) {
		t.Helper()
		status, a, err := dmUpload(f, f.owner.Token, dm.ID, 600, true)
		if err != nil || status != 201 {
			t.Fatalf("upload: %d %v", status, err)
		}
		var m chat.Message
		f.call(t, "POST", "/api/v1/conversations/"+dm.ID+"/messages", f.owner.Token, map[string]any{"request_id": identity.NewID(), "body": "image", "attachment_ids": []string{a.ID}}, 201, &m)
		return a, m
	}
	a, _ := upload()
	if _, err := f.pool.Exec(context.Background(), `UPDATE chatxo.attachments SET created_at=now()-interval '31 days' WHERE id=$1`, a.ID); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/attachments/" + a.ID + "/metadata"
	f.call(t, "GET", path, alice.Token, nil, 200, nil)
	cfg := getConfiguration(t, f).RuntimeConfiguration
	cfg.Settings.Limits.DM.ImageRetentionDays = 30
	saveConfiguration(t, f, cfg)
	f.call(t, "GET", path, alice.Token, nil, 404, nil)
	// Increasing retention before physical cleanup makes retained images readable again.
	cfg = getConfiguration(t, f).RuntimeConfiguration
	cfg.Settings.Limits.DM.ImageRetentionDays = 0
	saveConfiguration(t, f, cfg)
	f.call(t, "GET", path, alice.Token, nil, 200, nil)
	if err := features.Attachments.Maintain(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.call(t, "GET", path, alice.Token, nil, 200, nil)
	var usage attachments.DMUsage
	f.call(t, "GET", "/api/v1/me/dm-storage", alice.Token, nil, 200, &usage)
	if usage.DownloadedToday != 0 {
		t.Fatal("metadata consumed file transfer budget")
	}
	cfg = getConfiguration(t, f).RuntimeConfiguration
	cfg.Settings.Limits.DM.ImageRetentionDays = 30
	saveConfiguration(t, f, cfg)
	if err := features.Attachments.Maintain(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.call(t, "GET", path, alice.Token, nil, 404, nil)
	f.call(t, "GET", "/api/v1/me/dm-storage", f.owner.Token, nil, 200, &usage)
	if usage.UsedBytes != 0 {
		t.Fatal("expired image retained storage")
	}
	cfg = getConfiguration(t, f).RuntimeConfiguration
	cfg.Settings.Limits.DM.ImageRetentionDays = 0
	saveConfiguration(t, f, cfg)
	b, m := upload()
	f.call(t, "DELETE", "/api/v1/conversations/"+dm.ID+"/messages/"+m.ID, f.owner.Token, nil, 200, nil)
	f.call(t, "GET", "/api/v1/attachments/"+b.ID, alice.Token, nil, 404, nil)
	if err := features.Attachments.Maintain(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.call(t, "GET", "/api/v1/me/dm-storage", f.owner.Token, nil, 200, &usage)
	if usage.UsedBytes != 0 {
		t.Fatal("deleted message image retained storage")
	}
}

func TestDMTransferBudgetsArePerUser(t *testing.T) {
	f, _ := featureFixture(t, 4096)
	alice := f.register(t, "alice_traffic")
	dm := dmConversation(t, f, f.owner.Token, alice.User.ID)
	cfg := getConfiguration(t, f).RuntimeConfiguration
	cfg.Settings.Limits.DM.UploadBytesPerDay = 1024
	cfg.Settings.Limits.DM.DownloadBytesPerDay = 1024
	saveConfiguration(t, f, cfg)
	// A pre-upgrade aggregate row must not limit independent users anymore.
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO chatxo.dm_transfers(day,account,uploaded,downloaded) VALUES ((now() AT TIME ZONE 'UTC')::date,'server',1099511627776,1099511627776)`); err != nil {
		t.Fatal(err)
	}
	status, a, err := dmUpload(f, f.owner.Token, dm.ID, 600, true)
	if err != nil || status != 201 {
		t.Fatalf("upload: %d %v", status, err)
	}
	if status, _, err = dmUpload(f, alice.Token, dm.ID, 600, true); err != nil || status != 201 {
		t.Fatalf("independent upload budget: %d %v", status, err)
	}
	if status, _, err = dmUpload(f, alice.Token, dm.ID, 600, true); err != nil || status != 429 {
		t.Fatalf("personal upload budget: %d %v", status, err)
	}
	var usage attachments.DMUsage
	f.call(t, "GET", "/api/v1/me/dm-storage", alice.Token, nil, 200, &usage)
	if usage.UploadedToday != 600 || usage.UsedBytes != 600 {
		t.Fatal("rejected reservation consumed user budget")
	}
	var m chat.Message
	f.call(t, "POST", "/api/v1/conversations/"+dm.ID+"/messages", f.owner.Token, map[string]any{"request_id": identity.NewID(), "body": "traffic", "attachment_ids": []string{a.ID}}, 201, &m)
	req, _ := http.NewRequest("GET", f.server.URL+"/api/v1/attachments/"+a.ID, nil)
	req.Header.Set("Authorization", "Bearer "+alice.Token)
	req.Header.Set("Range", "bytes=0-9")
	res, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != 206 {
		t.Fatalf("range response: %d", res.StatusCode)
	}
	f.call(t, "GET", "/api/v1/me/dm-storage", alice.Token, nil, 200, &usage)
	if usage.DownloadedToday != 600 {
		t.Fatal("range bypassed full reservation")
	}
	f.call(t, "GET", "/api/v1/attachments/"+a.ID, alice.Token, nil, 429, nil)
	req, _ = http.NewRequest("GET", f.server.URL+"/api/v1/attachments/"+a.ID, nil)
	req.Header.Set("Authorization", "Bearer "+f.owner.Token)
	res, err = f.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("independent download budget: %d", res.StatusCode)
	}
	f.call(t, "GET", "/api/v1/attachments/"+a.ID, f.owner.Token, nil, 429, nil)
	f.call(t, "GET", "/api/v1/me/dm-storage", f.owner.Token, nil, 200, &usage)
	if usage.DownloadedToday != 600 {
		t.Fatal("rejected download consumed user budget")
	}
}

func TestDMDeletedGroupMemberRetainsVerificationKeys(t *testing.T) {
	f, _ := featureFixture(t, 4096)
	alice := f.register(t, "alice_old_keys")
	bob := f.register(t, "bob_live_keys")
	dm := dmConversation(t, f, f.owner.Token, alice.User.ID, bob.User.ID)
	od, _ := enrollDMDevice(t, f, f.owner)
	ad, ak := enrollDMDevice(t, f, alice)
	bd, bk := enrollDMDevice(t, f, bob)
	path := "/api/v1/conversations/" + dm.ID + "/encryption"
	commit := chat.DMCryptoCommit{RequestID: identity.NewID(), Actor: od.ID, Commit: bytes.Repeat([]byte{3}, 100), Welcome: bytes.Repeat([]byte{4}, 100), Devices: []string{od.ID, ad.ID, bd.ID}, Packages: []string{ak.ID, bk.ID}}
	f.call(t, "POST", path+"/events", f.owner.Token, commit, 200, nil)
	moderate(t, f, f.owner, alice.User.ID, "delete", 204)
	var b chat.DMCryptoBundle
	f.call(t, "GET", path, bob.Token, nil, 200, &b)
	found := false
	for _, d := range b.Devices {
		if d.ID == ad.ID {
			found = d.Revoked && bytes.Equal(d.PublicKey, ad.PublicKey)
		}
	}
	if !found {
		t.Fatal("deleted participant lost public keys needed to verify old group commits")
	}
	commit.RequestID = identity.NewID()
	commit.ExpectedRevision = 1
	commit.Devices = []string{od.ID, bd.ID}
	commit.Packages = nil
	commit.Welcome = nil
	f.call(t, "POST", path+"/events", f.owner.Token, commit, 200, nil)
	f.call(t, "GET", path, bob.Token, nil, 200, &b)
	if b.Revision != 2 || len(b.Members) != 2 {
		t.Fatal("deleted device remained in group")
	}
	f.call(t, "GET", path, alice.Token, nil, 401, nil)
}
