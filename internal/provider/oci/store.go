package oci

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/objectstorage"

	"github.com/traumventures/offshore/internal/blobcache"
)

// objectStoreAPI is the part of the Object Storage client the cache uses.
type objectStoreAPI interface {
	HeadObject(ctx context.Context, req objectstorage.HeadObjectRequest) (objectstorage.HeadObjectResponse, error)
	PutObject(ctx context.Context, req objectstorage.PutObjectRequest) (objectstorage.PutObjectResponse, error)
	GetObject(ctx context.Context, req objectstorage.GetObjectRequest) (objectstorage.GetObjectResponse, error)
	DeleteObject(ctx context.Context, req objectstorage.DeleteObjectRequest) (objectstorage.DeleteObjectResponse, error)
}

// objectStore is a compiler cache store in an Object Storage bucket below
// a prefix (the counterpart of blobcache.S3Store; OCI's S3 compatible API
// would need customer secret keys, the native API takes the instance
// principal). Expiry is checked against the modification time on every
// read; the bucket's lifecycle rule deletes cache/ objects after a day.
type objectStore struct {
	api       objectStoreAPI
	namespace string
	bucket    string
	prefix    string // "" or ending in "/"
	ttl       time.Duration
	now       func() time.Time
	spool     int64 // larger or unknown-size bodies go through a temporary file
}

var _ blobcache.Store = (*objectStore)(nil)

func newObjectStore(api objectStoreAPI, namespace, bucket, prefix string, ttl time.Duration) *objectStore {
	prefix = strings.Trim(prefix, "/")
	if prefix != "" {
		prefix += "/"
	}
	return &objectStore{api: api, namespace: namespace, bucket: bucket, prefix: prefix, ttl: blobcache.ClampTTL(ttl), now: time.Now, spool: 16 << 20}
}

func (s *objectStore) name(key string) (*string, error) {
	if !blobcache.ValidKey(key) {
		return nil, fmt.Errorf("blobcache: invalid key %q", key)
	}
	return common.String(s.prefix + key), nil
}

func (s *objectStore) info(size *int64, mod *common.SDKTime) (blobcache.Info, error) {
	if mod == nil || s.now().Sub(mod.Time) > s.ttl {
		return blobcache.Info{}, blobcache.ErrNotFound
	}
	var n int64
	if size != nil {
		n = *size
	}
	return blobcache.Info{Size: n, ModTime: mod.Time}, nil
}

func (s *objectStore) Get(ctx context.Context, key string) (io.ReadCloser, blobcache.Info, error) {
	name, err := s.name(key)
	if err != nil {
		return nil, blobcache.Info{}, err
	}
	out, err := s.api.GetObject(ctx, objectstorage.GetObjectRequest{NamespaceName: &s.namespace, BucketName: &s.bucket, ObjectName: name})
	if err != nil {
		if notFound(err) {
			return nil, blobcache.Info{}, blobcache.ErrNotFound
		}
		return nil, blobcache.Info{}, err
	}
	info, err := s.info(out.ContentLength, out.LastModified)
	if err != nil {
		out.Content.Close()
		return nil, blobcache.Info{}, err
	}
	return out.Content, info, nil
}

func (s *objectStore) Stat(ctx context.Context, key string) (blobcache.Info, error) {
	name, err := s.name(key)
	if err != nil {
		return blobcache.Info{}, err
	}
	out, err := s.api.HeadObject(ctx, objectstorage.HeadObjectRequest{NamespaceName: &s.namespace, BucketName: &s.bucket, ObjectName: name})
	if err != nil {
		if notFound(err) {
			return blobcache.Info{}, blobcache.ErrNotFound
		}
		return blobcache.Info{}, err
	}
	return s.info(out.ContentLength, out.LastModified)
}

// Put buffers the body (memory or temporary file): uploads need the length.
func (s *objectStore) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	name, err := s.name(key)
	if err != nil {
		return err
	}
	var body io.Reader
	var n int64
	if size >= 0 && size <= s.spool {
		b, err := io.ReadAll(io.LimitReader(r, size+1))
		if err != nil {
			return err
		}
		body, n = bytes.NewReader(b), int64(len(b))
	} else {
		f, err := os.CreateTemp("", "blobcache-oci-")
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
	_, err = s.api.PutObject(ctx, objectstorage.PutObjectRequest{NamespaceName: &s.namespace, BucketName: &s.bucket, ObjectName: name,
		ContentLength: common.Int64(n), PutObjectBody: io.NopCloser(body), ContentType: common.String("application/octet-stream")})
	return err
}

func (s *objectStore) Delete(ctx context.Context, key string) error {
	name, err := s.name(key)
	if err != nil {
		return err
	}
	_, err = s.api.DeleteObject(ctx, objectstorage.DeleteObjectRequest{NamespaceName: &s.namespace, BucketName: &s.bucket, ObjectName: name})
	if err != nil && notFound(err) {
		return nil
	}
	return err
}
