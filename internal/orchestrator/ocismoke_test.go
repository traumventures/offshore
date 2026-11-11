//go:build ocismoke

// Live smoke test of the oci provider against a real tenancy (~/.oci/config
// profile DEFAULT); never part of the regular test run:
//
//	make build-linux   # or: CGO_ENABLED=0 GOOS=linux go build -o /tmp/cs-oci-smoke/cloudsourced-linux-amd64 ./cmd/cloudsourced
//	go test -tags ocismoke -run 'TestOCILimits|TestOCISmoke' -v ./internal/orchestrator
//	go test -tags ocismoke -run TestOCICleanup -v ./internal/orchestrator
//
// TestOCISmoke pushes a Debian image to OCIR, creates one machine through
// the orchestrator (waiting until its worker reports ready), probes it and
// destroys it. TestOCICleanup lists what is left and deletes the bucket's
// objects, pre-authenticated requests and the repository's images (the
// compartment, network, bucket and repository stay).

package orchestrator

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/oracle/oci-go-sdk/v65/artifacts"
	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/core"
	"github.com/oracle/oci-go-sdk/v65/identity"
	"github.com/oracle/oci-go-sdk/v65/limits"
	"github.com/oracle/oci-go-sdk/v65/objectstorage"

	"github.com/traumventures/cloudsourced/internal/image"
	"github.com/traumventures/cloudsourced/internal/provider"
	_ "github.com/traumventures/cloudsourced/internal/provider/oci"
	"github.com/traumventures/cloudsourced/internal/scheduler"
)

func TestOCILimits(t *testing.T) {
	cfg := common.DefaultConfigProvider()
	tenancy, err := cfg.TenancyOCID()
	if err != nil {
		t.Fatal(err)
	}
	lc, err := limits.NewLimitsClientWithConfigurationProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	req := limits.ListLimitValuesRequest{CompartmentId: &tenancy, ServiceName: common.String("compute")}
	for {
		r, err := lc.ListLimitValues(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range r.Items {
			n := *v.Name
			if !(strings.Contains(n, "standard-e5") || strings.Contains(n, "standard-e4") || strings.Contains(n, "standard-a1") ||
				strings.Contains(n, "standard-e3") || strings.Contains(n, "vm-standard2")) {
				continue
			}
			ad := ""
			if v.AvailabilityDomain != nil {
				ad = *v.AvailabilityDomain
			}
			used := "?"
			ar, err := lc.GetResourceAvailability(ctx, limits.GetResourceAvailabilityRequest{ServiceName: common.String("compute"),
				LimitName: v.Name, CompartmentId: &tenancy, AvailabilityDomain: v.AvailabilityDomain})
			if err == nil && ar.Used != nil {
				used = strconv.FormatInt(*ar.Used, 10)
			}
			t.Logf("limit %-40s %-28s value %d used %s", n, ad, *v.Value, used)
		}
		if r.OpcNextPage == nil {
			break
		}
		req.Page = r.OpcNextPage
	}
}

func TestOCISmoke(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)
	shape := os.Getenv("SMOKE_SHAPE")
	if shape == "" {
		shape = "VM.Standard.E5.Flex"
	}
	arch := "amd64"
	if strings.Contains(shape, ".A1.") {
		arch = "arm64"
	}
	p, err := provider.Open("oci", map[string]any{
		"binary_amd64":   "/tmp/cs-oci-smoke/cloudsourced-linux-amd64",
		"binary_arm64":   "/tmp/cs-oci-smoke/cloudsourced-linux-arm64",
		"docker_command": []any{"sudo", "-n", "docker"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ih := p.(provider.ImageHost)
	plat := image.Platform("linux/" + arch)
	src := "docker.io/library/debian:bookworm-slim"
	cli := image.CLI{"sudo", "-n", "docker"}
	if err := cli.Run(ctx, nil, io.Discard, "pull", "--platform", string(plat), src); err != nil {
		t.Fatal(err)
	}
	if err := cli.Run(ctx, nil, io.Discard, "tag", src, "cloudsourced-env:smoke"); err != nil {
		t.Fatal(err)
	}
	reg := ih.ImageRegistry()
	ref, err := reg.Ref(ctx, "cloudsourced-env:smoke", plat)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := reg.Has(ctx, ref); err != nil {
		t.Fatal(err)
	} else if !ok {
		if ref, err = reg.Push(ctx, "cloudsourced-env:smoke", plat, os.Stderr); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("image %s", ref)
	if ok, err := reg.Has(ctx, ref); err != nil || !ok {
		t.Logf("registry Has after push: %v %v (OCIR may index with a delay)", ok, err)
	}

	cfg := Config{Owner: "smoke", MaxLifetime: 2 * time.Hour,
		Sizes: []scheduler.Size{{Provider: "oci", Name: "smoke", Arch: arch, Capacity: scheduler.Resources{CPU: 2, Memory: 8 << 30},
			InstanceTypes: []string{shape}}}}
	o, err := New(cfg, map[string]provider.Provider{"oci": p}, t.TempDir(), log)
	if err != nil {
		t.Fatal(err)
	}
	id := "smoke-" + time.Now().UTC().Format("0102150405")
	defer func() {
		dctx, dc := context.WithTimeout(context.Background(), 5*time.Minute)
		defer dc()
		if err := p.Destroy(dctx, id); err != nil {
			t.Errorf("destroy: %v", err)
		}
		ms, err := p.List(dctx, "smoke")
		t.Logf("after destroy: %d machines listed (%v)", len(ms), err)
		for _, m := range ms {
			t.Logf("  %s %s", m.ID, m.State)
		}
	}()
	start := time.Now()
	if err := o.CreateMachine(ctx, id, "smoke0000000000000000", ref, cfg.Sizes[0]); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Logf("machine ready after %s", time.Since(start).Round(time.Second))
	o.mu.Lock()
	m := o.machines[id].Machine
	o.mu.Unlock()
	st, err := Probe(ctx, m.Addr, o.fingerprint(m), o.probeToken(id))
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	t.Logf("status probe of %s at %s: ready=%v labels shape=%s", id, m.Addr, st.Ready, m.Labels["shape"])
}

func TestOCICleanup(t *testing.T) {
	ctx := context.Background()
	cfg := common.DefaultConfigProvider()
	tenancy, _ := cfg.TenancyOCID()
	ic, _ := identity.NewIdentityClientWithConfigurationProvider(cfg)
	cr, err := ic.ListCompartments(ctx, identity.ListCompartmentsRequest{CompartmentId: &tenancy, Name: common.String("cloudsourced")})
	if err != nil || len(cr.Items) == 0 {
		t.Fatal(err)
	}
	comp := cr.Items[0].Id
	cc, _ := core.NewComputeClientWithConfigurationProvider(cfg)
	ir, err := cc.ListInstances(ctx, core.ListInstancesRequest{CompartmentId: comp})
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range ir.Items {
		t.Logf("instance %s %s", *i.DisplayName, i.LifecycleState)
	}
	bc, _ := core.NewBlockstorageClientWithConfigurationProvider(cfg)
	ads, _ := ic.ListAvailabilityDomains(ctx, identity.ListAvailabilityDomainsRequest{CompartmentId: comp})
	for _, ad := range ads.Items {
		br, err := bc.ListBootVolumes(ctx, core.ListBootVolumesRequest{CompartmentId: comp, AvailabilityDomain: ad.Name})
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range br.Items {
			t.Logf("boot volume %s %s", *b.DisplayName, b.LifecycleState)
		}
	}
	oc, _ := objectstorage.NewObjectStorageClientWithConfigurationProvider(cfg)
	ns, _ := oc.GetNamespace(ctx, objectstorage.GetNamespaceRequest{})
	lr, err := oc.ListObjects(ctx, objectstorage.ListObjectsRequest{NamespaceName: ns.Value, BucketName: common.String("cloudsourced")})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range lr.Objects {
		t.Logf("object %s (deleting)", *o.Name)
		if _, err := oc.DeleteObject(ctx, objectstorage.DeleteObjectRequest{NamespaceName: ns.Value, BucketName: common.String("cloudsourced"), ObjectName: o.Name}); err != nil {
			t.Error(err)
		}
	}
	pr, err := oc.ListPreauthenticatedRequests(ctx, objectstorage.ListPreauthenticatedRequestsRequest{NamespaceName: ns.Value, BucketName: common.String("cloudsourced")})
	if err == nil {
		for _, p := range pr.Items {
			t.Logf("PAR %s (deleting)", *p.Name)
			oc.DeletePreauthenticatedRequest(ctx, objectstorage.DeletePreauthenticatedRequestRequest{NamespaceName: ns.Value, BucketName: common.String("cloudsourced"), ParId: p.Id})
		}
	}
	lp, err := oc.GetObjectLifecyclePolicy(ctx, objectstorage.GetObjectLifecyclePolicyRequest{NamespaceName: ns.Value, BucketName: common.String("cloudsourced")})
	t.Logf("lifecycle rules: %d (%v)", len(lp.Items), err)
	ac, _ := artifacts.NewArtifactsClientWithConfigurationProvider(cfg)
	im, err := ac.ListContainerImages(ctx, artifacts.ListContainerImagesRequest{CompartmentId: comp})
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range im.Items {
		t.Logf("image %s (deleting)", *i.DisplayName)
		if _, err := ac.DeleteContainerImage(ctx, artifacts.DeleteContainerImageRequest{ImageId: i.Id}); err != nil {
			t.Error(err)
		}
	}
}
