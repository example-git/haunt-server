package blobstore

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"

	"github.com/example-git/haunt-server/internal/testutil"
)

func TestS3MultipartRangedReadAndDelete(t *testing.T) {
	server := testutil.NewObjectStore(t)
	ctx := context.Background()
	c := Configuration{Kind: "s3", Endpoint: server.Server.URL, Region: "us-east-1", Bucket: "test", PathStyle: true, Prefix: "private"}
	b := makeBackend(c, Credentials{AccessKey: "test-access", SecretKey: "test-secret"}, "instance", "provider")
	f, err := os.CreateTemp(t.TempDir(), "multipart")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data := bytes.Repeat([]byte("multimedia-0123456789"), 500_000)
	if _, err = f.Write(data); err != nil {
		t.Fatal(err)
	}
	_, _ = f.Seek(0, 0)
	if err = b.put(ctx, "object", f); err != nil {
		t.Fatal(err)
	}
	if server.Multipart.Load() != 1 {
		t.Fatal("large object was not multipart")
	}
	reader, err := b.open(ctx, "object", int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if n, err := reader.Seek(-100, io.SeekEnd); err != nil || n != int64(len(data)-100) {
		t.Fatalf("seek: %d %v", n, err)
	}
	tail, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(tail, data[len(data)-100:]) {
		t.Fatal("range did not return correct tail")
	}
	_, _ = reader.Seek(7, io.SeekStart)
	chunk := make([]byte, 111)
	if _, err = io.ReadFull(reader, chunk); err != nil || !bytes.Equal(chunk, data[7:118]) {
		t.Fatal("second range mismatch")
	}
	reader.Close()
	if _, err = reader.Read(chunk); err == nil {
		t.Fatal("read after close accepted")
	}
	if err = b.remove(ctx, "object"); err != nil || server.Count() != 0 {
		t.Fatal("delete failed")
	}
}
func TestProviderValidationAndRootEscape(t *testing.T) {
	keys := Credentials{AccessKey: "a", SecretKey: "s"}
	for _, endpoint := range []string{"file:///tmp", "https://user:pass@host", "https://host/path", "https://host?query=yes", "http://"} {
		if (Configuration{Kind: "s3", Region: "us-east-1", Bucket: "test", Endpoint: endpoint}).validate(keys) == nil {
			t.Fatalf("accepted %s", endpoint)
		}
	}
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(outside+"/secret", []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside+"/secret", root+"/object"); err != nil {
		t.Fatal(err)
	}
	b := &backend{local: root}
	if f, err := b.open(context.Background(), "object", 6); err == nil {
		f.Close()
		t.Fatal("local read escaped storage root")
	}
}
