package blobcache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// S3API is the subset of the S3 client used by S3Store.
type S3API interface {
	GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	HeadObject(ctx context.Context, in *s3.HeadObjectInput, opts ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	DeleteObject(ctx context.Context, in *s3.DeleteObjectInput, opts ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
}

// S3Store keeps objects in an S3 bucket below a key prefix. Expiry is
// checked against LastModified on every read.
type S3Store struct {
	api    S3API
	bucket string
	prefix string // "" or ending in "/"
	ttl    time.Duration
	now    func() time.Time
	// spool bounds the in-memory buffer for uploads of unknown or large
	// size; larger bodies are spooled to a temporary file so the SDK gets
	// a seekable body.
	spool int64
}

// NewS3Store creates a store. ttl is clamped to MaxTTL.
func NewS3Store(api S3API, bucket, prefix string, ttl time.Duration) *S3Store {
	prefix = strings.Trim(prefix, "/")
	if prefix != "" {
		prefix += "/"
	}
	return &S3Store{api: api, bucket: bucket, prefix: prefix, ttl: ClampTTL(ttl), now: time.Now, spool: 16 << 20}
}

// ParseS3Location splits "bucket/prefix" (prefix optional).
func ParseS3Location(s string) (bucket, prefix string, err error) {
	bucket, prefix, _ = strings.Cut(strings.TrimPrefix(s, "s3://"), "/")
	if bucket == "" {
		return "", "", fmt.Errorf("blobcache: invalid S3 location %q (want bucket/prefix)", s)
	}
	return bucket, strings.Trim(prefix, "/"), nil
}

func (s *S3Store) objectKey(key string) (string, error) {
	if !ValidKey(key) {
		return "", fmt.Errorf("blobcache: invalid key %q", key)
	}
	return s.prefix + key, nil
}

func isNotFound(err error) bool {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return true
		}
	}
	return false
}

func (s *S3Store) info(size *int64, mod *time.Time) (Info, error) {
	if mod == nil || s.now().Sub(*mod) > s.ttl {
		return Info{}, ErrNotFound
	}
	return Info{Size: aws.ToInt64(size), ModTime: *mod}, nil
}

// Get implements Store.
func (s *S3Store) Get(ctx context.Context, key string) (io.ReadCloser, Info, error) {
	k, err := s.objectKey(key)
	if err != nil {
		return nil, Info{}, err
	}
	out, err := s.api.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &k})
	if err != nil {
		if isNotFound(err) {
			return nil, Info{}, ErrNotFound
		}
		return nil, Info{}, err
	}
	info, err := s.info(out.ContentLength, out.LastModified)
	if err != nil {
		out.Body.Close()
		return nil, Info{}, err
	}
	return out.Body, info, nil
}

// Stat implements Store.
func (s *S3Store) Stat(ctx context.Context, key string) (Info, error) {
	k, err := s.objectKey(key)
	if err != nil {
		return Info{}, err
	}
	out, err := s.api.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.bucket, Key: &k})
	if err != nil {
		if isNotFound(err) {
			return Info{}, ErrNotFound
		}
		return Info{}, err
	}
	return s.info(out.ContentLength, out.LastModified)
}

// Put implements Store. The body is buffered (memory or temporary file) so
// that the SDK can sign and retry the upload.
func (s *S3Store) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	k, err := s.objectKey(key)
	if err != nil {
		return err
	}
	var body io.ReadSeeker
	var n int64
	if size >= 0 && size <= s.spool {
		b, err := io.ReadAll(io.LimitReader(r, size+1))
		if err != nil {
			return err
		}
		body, n = bytes.NewReader(b), int64(len(b))
	} else {
		f, err := os.CreateTemp("", "blobcache-s3-")
		if err != nil {
			return err
		}
		defer os.Remove(f.Name())
		defer f.Close()
		if n, err = io.Copy(f, r); err != nil {
			return err
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		body = f
	}
	if size >= 0 && n != size {
		return fmt.Errorf("blobcache: short body (%d of %d bytes)", n, size)
	}
	_, err = s.api.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &s.bucket, Key: &k, Body: body, ContentLength: aws.Int64(n),
		ContentType: aws.String("application/octet-stream"),
	})
	return err
}

// Delete implements Store.
func (s *S3Store) Delete(ctx context.Context, key string) error {
	k, err := s.objectKey(key)
	if err != nil {
		return err
	}
	_, err = s.api.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &s.bucket, Key: &k})
	if err != nil && isNotFound(err) {
		return nil
	}
	return err
}
