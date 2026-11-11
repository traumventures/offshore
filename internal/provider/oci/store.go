package oci

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/objectstorage"

	"github.com/traumventures/offshore/internal/blobcache"
	"github.com/traumventures/offshore/internal/provider"
	"github.com/traumventures/offshore/internal/store"
)

var _ provider.StoreGranter = (*OCI)(nil)

// NativeGranter makes the provider's bucket the pool's store: objects
// below cache/<pool>/ (deleted after a day by the bucket's lifecycle rule).
// Workers get pre-authenticated request URLs, one per granted scope
// (prefix PARs with AnyObjectReadWrite, listing denied), instead of
// credentials: OCI's S3 compatible API would need a user's customer secret
// keys, which cannot be scoped.
func (o *OCI) NativeGranter(ctx context.Context, name, pool string) (store.Granter, bool, error) {
	in, err := o.ensureInfra(ctx)
	if err != nil {
		return nil, false, err
	}
	return newPARGranter(o.objects, name, o.region, in.namespace, in.bucket, path.Join("cache", pool)), true, nil
}

// parAPI is the part of the Object Storage client the granter uses.
type parAPI interface {
	CreatePreauthenticatedRequest(ctx context.Context, req objectstorage.CreatePreauthenticatedRequestRequest) (objectstorage.CreatePreauthenticatedRequestResponse, error)
	ListPreauthenticatedRequests(ctx context.Context, req objectstorage.ListPreauthenticatedRequestsRequest) (objectstorage.ListPreauthenticatedRequestsResponse, error)
	DeletePreauthenticatedRequest(ctx context.Context, req objectstorage.DeletePreauthenticatedRequestRequest) (objectstorage.DeletePreauthenticatedRequestResponse, error)
}

// parGranter mints store grants of pre-authenticated request URLs. A PAR
// per scope is reused while it outlives the requested grant TTL (it is
// created for twice the TTL), so a busy pool creates few of them; expired
// ones are deleted at most once an hour.
type parGranter struct {
	api       parAPI
	name      string
	region    string
	namespace string
	bucket    string
	prefix    string // pool prefix, no trailing "/"
	now       func() time.Time

	mu        sync.Mutex
	pars      map[string]parEntry // scope -> PAR
	lastSweep time.Time
}

type parEntry struct {
	url     string // URL of the scope's prefix, ending in "/"
	expires time.Time
}

func newPARGranter(api parAPI, name, region, namespace, bucket, prefix string) *parGranter {
	return &parGranter{api: api, name: name, region: region, namespace: namespace, bucket: bucket,
		prefix: strings.Trim(prefix, "/"), now: time.Now, pars: map[string]parEntry{}}
}

func (g *parGranter) Name() string { return g.name }

func (g *parGranter) Grant(ctx context.Context, scopes []string, ttl time.Duration) (store.Grant, error) {
	ttl = blobcache.ClampTTL(ttl)
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	gr := store.Grant{Store: g.name, Prefix: g.prefix, URLs: map[string]string{}}
	var expires time.Time
	for _, s := range scopes {
		s = strings.Trim(path.Clean("/"+s), "/")
		if s == "" || slices.Contains(gr.Scopes, s) {
			continue
		}
		e, ok := g.pars[s]
		if !ok || e.expires.Before(now.Add(ttl)) {
			var err error
			if e, err = g.mint(ctx, s, now.Add(2*ttl)); err != nil {
				return store.Grant{}, err
			}
			g.pars[s] = e
		}
		gr.Scopes = append(gr.Scopes, s)
		gr.URLs[s] = e.url
		if expires.IsZero() || e.expires.Before(expires) {
			expires = e.expires
		}
	}
	if !expires.IsZero() {
		gr.Expires = expires.Unix()
	}
	if now.Sub(g.lastSweep) > time.Hour {
		g.lastSweep = now
		g.sweep(ctx, now)
	}
	return gr, nil
}

// mint creates a prefix PAR for the objects of scope.
func (g *parGranter) mint(ctx context.Context, scope string, expires time.Time) (parEntry, error) {
	object := g.prefix + "/" + scope + "/"
	pr, err := g.api.CreatePreauthenticatedRequest(ctx, objectstorage.CreatePreauthenticatedRequestRequest{
		NamespaceName: common.String(g.namespace), BucketName: common.String(g.bucket),
		CreatePreauthenticatedRequestDetails: objectstorage.CreatePreauthenticatedRequestDetails{
			Name:                common.String("offshore-store " + scope),
			ObjectName:          common.String(object),
			AccessType:          objectstorage.CreatePreauthenticatedRequestDetailsAccessTypeAnyobjectreadwrite,
			BucketListingAction: objectstorage.PreauthenticatedRequestBucketListingActionDeny,
			TimeExpires:         &common.SDKTime{Time: expires},
		}})
	if err != nil {
		return parEntry{}, fmt.Errorf("oci: store grant for %s: %w", scope, err)
	}
	base := parURL(g.region, pr.PreauthenticatedRequest)
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	// A bucket-level PAR URL ends in /o/; objects keep their full names.
	return parEntry{url: base + object, expires: expires}, nil
}

// sweep deletes expired store PARs of the pool (best effort).
func (g *parGranter) sweep(ctx context.Context, now time.Time) {
	var page *string
	for {
		out, err := g.api.ListPreauthenticatedRequests(ctx, objectstorage.ListPreauthenticatedRequestsRequest{
			NamespaceName: common.String(g.namespace), BucketName: common.String(g.bucket),
			ObjectNamePrefix: common.String(g.prefix + "/"), Page: page,
		})
		if err != nil {
			return
		}
		for _, p := range out.Items {
			if p.TimeExpires != nil && p.TimeExpires.Time.Before(now) && p.Id != nil {
				g.api.DeletePreauthenticatedRequest(ctx, objectstorage.DeletePreauthenticatedRequestRequest{
					NamespaceName: common.String(g.namespace), BucketName: common.String(g.bucket), ParId: p.Id})
			}
		}
		if out.OpcNextPage == nil {
			return
		}
		page = out.OpcNextPage
	}
}
