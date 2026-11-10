package oci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/common/auth"
	"github.com/oracle/oci-go-sdk/v65/core"
	"github.com/oracle/oci-go-sdk/v65/objectstorage"

	"github.com/traumventures/cloudsourced/internal/blobcache"
	"github.com/traumventures/cloudsourced/internal/supervisor"
)

// Spec parameters of the supervisor on OCI.
const (
	paramRegion    = "region"
	paramNamespace = "namespace"     // Object Storage namespace
	paramBucket    = "bucket"        // bucket of secrets and compiler cache
	paramSecret    = "secret_object" // object with the secret server environment
	paramRegistry  = "registry"      // OCIR host of the environment image
	paramCache     = "cache_prefix"  // object prefix of the compiler cache
	// sccachePort is the host's compiler cache port on the Docker bridge.
	sccachePort = 7480
	// metadataURL is the instance metadata service (v2 only).
	metadataURL = "http://169.254.169.254/opc/v2/"
)

func init() {
	supervisor.RegisterPlatform("oci", openHost)
}

// hostCompute is the part of the compute client the supervisor uses.
type hostCompute interface {
	GetInstance(ctx context.Context, req core.GetInstanceRequest) (core.GetInstanceResponse, error)
	InstanceAction(ctx context.Context, req core.InstanceActionRequest) (core.InstanceActionResponse, error)
	TerminateInstance(ctx context.Context, req core.TerminateInstanceRequest) (core.TerminateInstanceResponse, error)
}

// host is the supervisor side of the provider. It authenticates as the
// instance (instance principal, certificates from the metadata service,
// which the containers cannot reach).
type host struct {
	spec       supervisor.Spec
	cfg        common.ConfigurationProvider
	objects    objectStoreAPI
	compute    hostCompute
	instanceID func(ctx context.Context) (string, error)
	token      func(ctx context.Context, registry string) (string, error)
}

func openHost(ctx context.Context, spec supervisor.Spec) (supervisor.Platform, error) {
	p := spec.Params
	if p[paramRegion] == "" || p[paramNamespace] == "" || p[paramBucket] == "" || p[paramSecret] == "" {
		return nil, errors.New("oci host: spec lacks region, namespace, bucket or secret object")
	}
	cfg, err := auth.InstancePrincipalConfigurationProviderForRegion(common.StringToRegion(p[paramRegion]))
	if err != nil {
		return nil, fmt.Errorf("oci host: instance principal: %w", err)
	}
	oc, err := objectstorage.NewObjectStorageClientWithConfigurationProvider(cfg)
	if err != nil {
		return nil, err
	}
	cc, err := core.NewComputeClientWithConfigurationProvider(cfg)
	if err != nil {
		return nil, err
	}
	oc.SetRegion(p[paramRegion])
	cc.SetRegion(p[paramRegion])
	h := &host{spec: spec, cfg: cfg, objects: oc, compute: cc, instanceID: metadataInstanceID}
	h.token = func(ctx context.Context, registry string) (string, error) { return ocirToken(ctx, cfg, registry) }
	return h, nil
}

func (h *host) object() objectstorage.GetObjectRequest {
	p := h.spec.Params
	return objectstorage.GetObjectRequest{NamespaceName: common.String(p[paramNamespace]),
		BucketName: common.String(p[paramBucket]), ObjectName: common.String(p[paramSecret])}
}

func (h *host) FetchSecrets(ctx context.Context) (map[string]string, error) {
	out, err := h.objects.GetObject(ctx, h.object())
	if err != nil {
		return nil, fmt.Errorf("oci host: read secret object: %w", err)
	}
	defer out.Content.Close()
	b, err := io.ReadAll(io.LimitReader(out.Content, 1<<20))
	if err != nil {
		return nil, err
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("oci host: secret object: %w", err)
	}
	return m, nil
}

func (h *host) DeleteSecrets(ctx context.Context) error {
	o := h.object()
	_, err := h.objects.DeleteObject(ctx, objectstorage.DeleteObjectRequest{NamespaceName: o.NamespaceName, BucketName: o.BucketName, ObjectName: o.ObjectName})
	if err != nil && !notFound(err) {
		return err
	}
	return nil
}

// Prepare logs the Docker CLI in to OCIR with a token of the instance
// principal.
func (h *host) Prepare(ctx context.Context, docker supervisor.Runner) error {
	registry := h.spec.Params[paramRegistry]
	if registry == "" {
		return nil
	}
	tok, err := h.token(ctx, registry)
	if err != nil {
		return err
	}
	_, err = docker(ctx, nil, []byte(tok), "login", "--username", "BEARER_TOKEN", "--password-stdin", registry)
	return err
}

// CompilerCacheStore keeps compiler cache objects in the bucket below the
// pool's cache/ prefix.
func (h *host) CompilerCacheStore(ctx context.Context) (blobcache.Store, error) {
	prefix := h.spec.Params[paramCache]
	if prefix == "" {
		return nil, nil
	}
	return newObjectStore(h.objects, h.spec.Params[paramNamespace], h.spec.Params[paramBucket], prefix, blobcache.MaxTTL), nil
}

// StopSelf stops this instance, keeping its boot volume. A preemptible
// instance cannot be stopped on OCI: it terminates instead.
func (h *host) StopSelf(ctx context.Context) error {
	id, err := h.instanceID(ctx)
	if err != nil {
		return err
	}
	r, err := h.compute.GetInstance(ctx, core.GetInstanceRequest{InstanceId: common.String(id)})
	if err != nil {
		return fmt.Errorf("oci host: instance: %w", err)
	}
	if r.PreemptibleInstanceConfig != nil {
		return h.terminate(ctx, id)
	}
	_, err = h.compute.InstanceAction(ctx, core.InstanceActionRequest{InstanceId: common.String(id), Action: core.InstanceActionActionSoftstop})
	return err
}

// TerminateSelf ends this instance at expiry (an OS shutdown would only
// stop it).
func (h *host) TerminateSelf(ctx context.Context) error {
	id, err := h.instanceID(ctx)
	if err != nil {
		return err
	}
	return h.terminate(ctx, id)
}

func (h *host) terminate(ctx context.Context, id string) error {
	_, err := h.compute.TerminateInstance(ctx, core.TerminateInstanceRequest{InstanceId: common.String(id), PreserveBootVolume: common.Bool(false)})
	return err
}

var _ supervisor.SelfTerminator = (*host)(nil)

// metadataInstanceID reads this instance's OCID from the metadata service.
func metadataInstanceID(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, metadataURL+"instance/id", nil)
	req.Header.Set("Authorization", "Bearer Oracle")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("oci host: instance id: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("oci host: instance id: %s", resp.Status)
	}
	return strings.TrimSpace(string(b)), nil
}
