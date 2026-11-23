package platform

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// PrepareUploads runs under the catalog's exclusive service lease. A stopped
// process can leave a partial spool file, but only a published object is live.
func PrepareUploads(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasPrefix(entry.Name(), ".upload-") {
			if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// Object keys are generated from authenticated scopes and verified digests.
type Objects interface {
	Put(context.Context, string, string, int64) error
	Exists(context.Context, string) (bool, error)
	Get(context.Context, string) (io.ReadCloser, error)
	Remove(context.Context, string) error
}

type DiskObjects struct{ Dir string }

func (d DiskObjects) Put(ctx context.Context, key, src string, size int64) error {
	dst := filepath.Join(d.Dir, key)
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(d.Dir, ".upload-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	in, err := os.Open(src)
	if err != nil {
		f.Close()
		return err
	}
	defer in.Close()
	n, err := io.Copy(f, in)
	if err == nil && n != size {
		err = io.ErrUnexpectedEOF
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = os.Link(f.Name(), dst); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	dir, err := os.Open(filepath.Dir(dst))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (d DiskObjects) Exists(ctx context.Context, key string) (bool, error) {
	_, err := os.Stat(filepath.Join(d.Dir, key))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (d DiskObjects) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return os.Open(filepath.Join(d.Dir, key))
}

func (d DiskObjects) Remove(ctx context.Context, key string) error {
	err := os.Remove(filepath.Join(d.Dir, key))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

type S3 struct {
	client *minio.Client
	bucket string
}

func NewS3(endpoint, bucket, access, secret, session string, tls bool) (*S3, error) {
	if bucket == "" || access == "" || secret == "" {
		return nil, fmt.Errorf("missing S3 bucket or credentials")
	}
	c, err := minio.New(endpoint, &minio.Options{Creds: credentials.NewStaticV4(access, secret, session), Secure: tls})
	if err != nil {
		return nil, err
	}
	return &S3{c, bucket}, nil
}

func (s *S3) Put(ctx context.Context, key, src string, size int64) error {
	opts := minio.PutObjectOptions{ContentType: "application/octet-stream"}
	opts.SetMatchETagExcept("*")
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = s.client.PutObject(ctx, s.bucket, key, f, size, opts)
	if minio.ToErrorResponse(err).Code == "PreconditionFailed" {
		return nil
	}
	return err
}

func (s *S3) Exists(ctx context.Context, key string) (bool, error) {
	_, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	code := minio.ToErrorResponse(err).Code
	if code == "NoSuchKey" || code == "NotFound" {
		return false, nil
	}
	return err == nil, err
}

func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
}

func (s *S3) Remove(ctx context.Context, key string) error {
	return s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
}
