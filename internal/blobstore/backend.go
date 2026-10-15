// Package blobstore stores private attachment bytes outside the metadata database.
package blobstore

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

var ErrUnavailable = errors.New("storage_provider_unavailable")
var ErrInUse = errors.New("storage_provider_in_use")
var ErrInvalid = errors.New("invalid_storage_provider")

type Configuration struct {
	Kind      string `json:"kind" enum:"local,s3"`
	Path      string `json:"path,omitempty" maxLength:"1024"`
	Endpoint  string `json:"endpoint,omitempty" maxLength:"1024" description:"Empty uses AWS S3. Custom endpoints may use HTTP for trusted LAN services."`
	Region    string `json:"region,omitempty" maxLength:"80"`
	Bucket    string `json:"bucket,omitempty" maxLength:"255"`
	Prefix    string `json:"prefix,omitempty" maxLength:"512"`
	PathStyle bool   `json:"path_style"`
}
type Credentials struct {
	AccessKey    string `json:"access_key" maxLength:"256"`
	SecretKey    string `json:"secret_key" maxLength:"4096" format:"password"`
	SessionToken string `json:"session_token,omitempty" maxLength:"8192" format:"password"`
}

func (c Configuration) validate(keys Credentials) error {
	for _, v := range []string{c.Path, c.Endpoint, c.Region, c.Bucket, c.Prefix, keys.AccessKey, keys.SecretKey, keys.SessionToken} {
		if strings.ContainsAny(v, "\x00\r\n") {
			return ErrInvalid
		}
	}
	if c.Kind == "local" {
		if !filepath.IsAbs(c.Path) || len(c.Path) > 1024 || c.Endpoint != "" || c.Bucket != "" || keys.AccessKey != "" || keys.SecretKey != "" || keys.SessionToken != "" {
			return ErrInvalid
		}
		return nil
	}
	if c.Kind != "s3" || c.Path != "" || len(c.Endpoint) > 1024 || c.Region == "" || len(c.Region) > 80 || c.Bucket == "" || len(c.Bucket) > 255 || strings.ContainsAny(c.Bucket, "/\\:@ ") || len(c.Prefix) > 512 || strings.Contains(c.Prefix, "..") || strings.Contains(c.Prefix, "\\") || len(keys.AccessKey) < 1 || len(keys.AccessKey) > 256 || len(keys.SecretKey) < 1 || len(keys.SecretKey) > 4096 || len(keys.SessionToken) > 8192 {
		return ErrInvalid
	}
	if c.Endpoint != "" {
		u, err := url.Parse(c.Endpoint)
		if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return ErrInvalid
		}
	}
	return nil
}

type ReadSeekCloser interface {
	io.Reader
	io.Seeker
	io.Closer
}

var storageTransport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = 10 * time.Second
	t.MaxIdleConns = 32
	t.MaxIdleConnsPerHost = 8
	return t
}()

type backend struct {
	local          string
	client         *s3.Client
	bucket, prefix string
}

func namespace(instance, id string) string {
	h := sha256.Sum256([]byte(instance))
	return fmt.Sprintf("haunt/%x/%s", h[:12], id)
}
func makeBackend(c Configuration, k Credentials, instance, id string) *backend {
	ns := namespace(instance, id)
	if c.Kind == "local" {
		return &backend{local: filepath.Join(c.Path, filepath.FromSlash(ns))}
	}
	client := s3.New(s3.Options{Region: c.Region, Credentials: credentials.NewStaticCredentialsProvider(k.AccessKey, k.SecretKey, k.SessionToken), UsePathStyle: c.PathStyle, RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired, ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired, HTTPClient: &http.Client{Transport: storageTransport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, RetryMaxAttempts: 2}, func(o *s3.Options) {
		if c.Endpoint != "" {
			o.BaseEndpoint = aws.String(strings.TrimRight(c.Endpoint, "/"))
		}
	})
	return &backend{client: client, bucket: c.Bucket, prefix: path.Join(c.Prefix, ns)}
}
func (b *backend) put(ctx context.Context, id string, reader io.Reader) error {
	if b.local != "" {
		root, err := os.OpenRoot(b.local)
		if err != nil {
			return err
		}
		defer root.Close()
		f, err := root.OpenFile(id+".part", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer root.Remove(id + ".part")
		_, copyErr := io.Copy(f, reader)
		syncErr := f.Sync()
		closeErr := f.Close()
		if err = errors.Join(copyErr, syncErr, closeErr, ctx.Err()); err != nil {
			return err
		}
		if err = root.Rename(id+".part", id); err != nil {
			return err
		}
		d, err := root.Open(".")
		if err != nil {
			return err
		}
		defer d.Close()
		return d.Sync()
	}
	uploader := transfermanager.New(b.client, func(o *transfermanager.Options) {
		o.Concurrency = 1
		o.PartSizeBytes = 8 << 20
		o.MultipartUploadThreshold = 8 << 20
		o.FailTimeout = 10 * time.Second
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
	})
	_, err := uploader.UploadObject(ctx, &transfermanager.UploadObjectInput{Bucket: aws.String(b.bucket), Key: aws.String(path.Join(b.prefix, id)), Body: reader, ContentType: aws.String("application/octet-stream")})
	return err
}
func (b *backend) open(ctx context.Context, id string, size int64) (ReadSeekCloser, error) {
	if b.local != "" {
		root, err := os.OpenRoot(b.local)
		if err != nil {
			return nil, err
		}
		defer root.Close()
		f, err := root.Open(id)
		if err != nil {
			return nil, err
		}
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() != size {
			f.Close()
			return nil, ErrUnavailable
		}
		return f, nil
	}
	key := path.Join(b.prefix, id)
	headCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	head, err := b.client.HeadObject(headCtx, &s3.HeadObjectInput{Bucket: aws.String(b.bucket), Key: aws.String(key)})
	if err != nil {
		return nil, err
	}
	if aws.ToInt64(head.ContentLength) != size {
		return nil, ErrUnavailable
	}
	return &remoteReader{ctx: ctx, client: b.client, bucket: b.bucket, key: key, size: size}, nil
}
func (b *backend) remove(ctx context.Context, id string) error {
	if b.local != "" {
		root, err := os.OpenRoot(b.local)
		if err != nil {
			return err
		}
		defer root.Close()
		for _, suffix := range []string{"", ".part"} {
			err = root.Remove(id + suffix)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		return nil
	}
	_, err := b.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(b.bucket), Key: aws.String(path.Join(b.prefix, id))})
	return err
}

// A seek starts a ranged request rather than downloading the full object. The
// authenticated HTTP handler can serve videos and multi-range image requests.
type remoteReader struct {
	ctx          context.Context
	client       *s3.Client
	bucket, key  string
	size, offset int64
	body         io.ReadCloser
	closed       bool
}

func (r *remoteReader) Read(p []byte) (int, error) {
	if r.closed {
		return 0, os.ErrClosed
	}
	if len(p) == 0 {
		return 0, nil
	}
	if r.offset >= r.size {
		return 0, io.EOF
	}
	if r.body == nil {
		v, err := r.client.GetObject(r.ctx, &s3.GetObjectInput{Bucket: aws.String(r.bucket), Key: aws.String(r.key), Range: aws.String(fmt.Sprintf("bytes=%d-", r.offset))})
		if err != nil {
			return 0, ErrUnavailable
		}
		if aws.ToInt64(v.ContentLength) != r.size-r.offset {
			v.Body.Close()
			return 0, ErrUnavailable
		}
		r.body = v.Body
	}
	n, err := r.body.Read(p)
	r.offset += int64(n)
	return n, err
}
func (r *remoteReader) Seek(offset int64, whence int) (int64, error) {
	if r.closed {
		return 0, os.ErrClosed
	}
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += r.offset
	case io.SeekEnd:
		offset += r.size
	default:
		return 0, ErrInvalid
	}
	if offset < 0 {
		return 0, ErrInvalid
	}
	if offset != r.offset && r.body != nil {
		r.body.Close()
		r.body = nil
	}
	r.offset = offset
	return offset, nil
}
func (r *remoteReader) Close() error {
	r.closed = true
	if r.body != nil {
		return r.body.Close()
	}
	return nil
}
