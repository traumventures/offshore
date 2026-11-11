package oci

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/core"
	"github.com/oracle/oci-go-sdk/v65/objectstorage"

	"github.com/traumventures/cloudsourced/internal/blobcache"
	"github.com/traumventures/cloudsourced/internal/provider"
	"github.com/traumventures/cloudsourced/internal/supervisor"
)

type svcErr struct {
	status    int
	code, msg string
}

func (e svcErr) Error() string           { return e.code + ": " + e.msg }
func (e svcErr) GetHTTPStatusCode() int  { return e.status }
func (e svcErr) GetMessage() string      { return e.msg }
func (e svcErr) GetCode() string         { return e.code }
func (e svcErr) GetOpcRequestID() string { return "req" }

func shapedSpec() provider.MachineSpec {
	return provider.MachineSpec{ID: "m1", Size: "x4", CPU: 4, Memory: 16 << 30,
		InstanceTypes: []string{"VM.Standard.E5.Flex", "VM.Standard.E4.Flex"}}
}

func plan(s provider.MachineSpec) string { return strings.Join(launchPlan(s), ", ") }

func TestLaunchPlan(t *testing.T) {
	if g, w := plan(shapedSpec()), "VM.Standard.E5.Flex, VM.Standard.E4.Flex"; g != w {
		t.Errorf("got %s\nwant %s", g, w)
	}
	if g := plan(provider.MachineSpec{Arch: "arm64"}); g != "VM.Standard.A1.Flex" {
		t.Errorf("arm64 default: %s", g)
	}
	if g := plan(provider.MachineSpec{}); g != "VM.Standard.E5.Flex" {
		t.Errorf("amd64 default: %s", g)
	}
}

func TestShapeConfig(t *testing.T) {
	c := shapeConfig("VM.Standard.E5.Flex", 4, 16<<30)
	if *c.Ocpus != 2 || *c.MemoryInGBs != 16 {
		t.Errorf("E5: %v ocpus %v GB", *c.Ocpus, *c.MemoryInGBs)
	}
	c = shapeConfig("VM.Standard.A1.Flex", 3, 5<<30+1)
	if *c.Ocpus != 3 || *c.MemoryInGBs != 6 {
		t.Errorf("A1: %v ocpus %v GB", *c.Ocpus, *c.MemoryInGBs)
	}
	if c := shapeConfig("VM.Standard.E5.Flex", 1, 0); *c.Ocpus != 1 || c.MemoryInGBs != nil {
		t.Errorf("small: %+v", c)
	}
	if shapeConfig("VM.Standard2.1", 2, 1<<30) != nil {
		t.Error("fixed shapes take no shape config")
	}
}

func TestLaunchFallsBackOnCapacity(t *testing.T) {
	var tried []string
	run := func(_ context.Context, req core.LaunchInstanceRequest) (core.LaunchInstanceResponse, error) {
		d := req.LaunchInstanceDetails
		tried = append(tried, *d.Shape)
		if d.PreemptibleInstanceConfig != nil {
			t.Error("no preemptible instances")
		}
		if *d.Shape == "VM.Standard.E5.Flex" {
			return core.LaunchInstanceResponse{}, svcErr{500, "InternalError", "Out of host capacity."}
		}
		return core.LaunchInstanceResponse{Instance: core.Instance{Id: common.String("i1")}}, nil
	}
	base := core.LaunchInstanceDetails{FreeformTags: map[string]string{"project": "cloudsourced"}}
	inst, shape, failed, err := launch(context.Background(), run, base, shapedSpec())
	if err != nil || *inst.Id != "i1" || shape != "VM.Standard.E4.Flex" || len(failed) != 1 {
		t.Fatalf("inst %v shape %v failed %v err %v", inst.Id, shape, failed, err)
	}
	if strings.Join(tried, ",") != "VM.Standard.E5.Flex,VM.Standard.E4.Flex" {
		t.Errorf("tried %v", tried)
	}
	if len(base.FreeformTags) != 1 {
		t.Error("the base tags must not be modified")
	}

	// Other errors end the launch.
	bad := func(context.Context, core.LaunchInstanceRequest) (core.LaunchInstanceResponse, error) {
		return core.LaunchInstanceResponse{}, svcErr{400, "InvalidParameter", "image not found"}
	}
	if _, _, _, err := launch(context.Background(), bad, base, shapedSpec()); err == nil || strings.Contains(err.Error(), "no capacity") {
		t.Errorf("want the configuration error, got %v", err)
	}
}

func TestCapacityError(t *testing.T) {
	for err, want := range map[error]bool{
		svcErr{500, "InternalError", "Out of host capacity."}:                     true,
		svcErr{400, "LimitExceeded", "standard-e5-core-count"}:                    true,
		svcErr{400, "InvalidParameter", "Shape VM.X is not supported in this AD"}: true,
		svcErr{400, "InvalidParameter", "Invalid image id"}:                       false,
		errors.New("network down"):                                                false,
	} {
		if capacityError(err) != want {
			t.Errorf("%v: want %v", err, want)
		}
	}
}

func TestToMachine(t *testing.T) {
	spec := provider.MachineSpec{ID: "m1", Owner: "pool", EnvHash: "eh", CPU: 4, Memory: 8 << 30, Finger: "fp", Size: "x4",
		ExpiresAt: time.Unix(2_000_000_000, 0)}
	tags, err := machineTags(spec, "amd64", time.Unix(1_900_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	var d core.LaunchInstanceDetails
	d.FreeformTags = tags
	applyShape(&d, "VM.Standard.E5.Flex", spec)
	tags = d.FreeformTags
	if len(tags) > 10 {
		t.Errorf("%d tags, OCI allows 10", len(tags))
	}
	for k, v := range tags {
		if strings.Contains(k, ".") || len(v) > maxTagSize {
			t.Errorf("tag %q=%q", k, v)
		}
	}
	i := core.Instance{Id: common.String("ocid1.instance"), Shape: common.String("VM.Standard.E5.Flex"),
		LifecycleState: core.InstanceLifecycleStateRunning, FreeformTags: tags}
	if !selected(i, "pool", "m1") || selected(i, "other", "") {
		t.Error("selection by pool and machine")
	}
	m := toMachine(i, "203.0.113.7")
	if m.ID != "m1" || m.State != provider.Running || m.Addr != "203.0.113.7:7443" || m.EnvHash != "eh" || m.CPU != 4 ||
		m.Memory != 8<<30 || !m.ExpiresAt.Equal(spec.ExpiresAt) || m.Labels[provider.LabelPool] != "pool" || m.Labels["instance-id"] != "ocid1.instance" ||
		m.Labels[provider.LabelSize] != "x4" {
		t.Errorf("machine %+v", m)
	}
	i.LifecycleState = core.InstanceLifecycleStateStopped
	if m := toMachine(i, ""); m.State != provider.Stopped || m.Addr != "" {
		t.Errorf("stopped: %+v", m)
	}
	i.LifecycleState = core.InstanceLifecycleStateTerminating
	if m := toMachine(i, ""); m.State != provider.Failed {
		t.Errorf("terminating: %+v", m)
	}
	i.LifecycleState = core.InstanceLifecycleStateTerminated
	if selected(i, "", "") {
		t.Error("terminated instances are gone")
	}
}

func TestSecurityRules(t *testing.T) {
	ssh := core.IngressSecurityRule{Protocol: common.String("6"), Source: common.String(allCIDR)}
	sl := core.SecurityList{IngressSecurityRules: []core.IngressSecurityRule{ssh}}
	in, out, changed := securityRules(sl, []string{"198.51.100.1/32"}, true)
	if !changed || len(in) != 2 || *in[0].Protocol != "1" || !isQUIC(in[1], "198.51.100.1/32") || len(out) != 1 {
		t.Fatalf("fresh: %+v %+v", in, out)
	}
	sl = core.SecurityList{IngressSecurityRules: in, EgressSecurityRules: out}
	if _, _, changed := securityRules(sl, []string{"198.51.100.1/32"}, false); changed {
		t.Error("rules already present: no update")
	}
	in, _, changed = securityRules(sl, []string{"198.51.100.1/32", "192.0.2.0/24"}, false)
	if !changed || len(in) != 3 {
		t.Errorf("added cidr: %+v", in)
	}
	// An existing (user's) security list keeps its other rules.
	in, _, _ = securityRules(core.SecurityList{IngressSecurityRules: []core.IngressSecurityRule{ssh}}, []string{"192.0.2.0/24"}, false)
	if len(in) != 2 || *in[0].Protocol != "6" {
		t.Errorf("kept rules: %+v", in)
	}
}

func TestPickAD(t *testing.T) {
	names := []string{"Uocm:EU-FRANKFURT-1-AD-1", "Uocm:EU-FRANKFURT-1-AD-2"}
	if ad, _ := pickAD(names, ""); ad != names[0] {
		t.Error(ad)
	}
	if ad, _ := pickAD(names, "AD-2"); ad != names[1] {
		t.Error(ad)
	}
	if _, err := pickAD(names, "AD-9"); err == nil {
		t.Error("want an error")
	}
}

func testInfra() *infra {
	return &infra{tenancy: "t", compartment: "c", ad: "AD-1", subnet: "s", namespace: "ns", bucket: "cloudsourced",
		repo: "ocir.eu-frankfurt-1.oci.oraclecloud.com/ns/cloudsourced-env"}
}

func TestUserData(t *testing.T) {
	spec := provider.MachineSpec{ID: "m1", Owner: "my pool", ExpiresAt: time.Unix(2_000_000_000, 0), Swap: true,
		Env: map[string]string{"CLOUDSOURCED_A": "1"}, SecretEnv: map[string]string{"CLOUDSOURCED_TOKEN": "s3cret"}}
	ud, err := userData(spec, "eu-frankfurt-1", testInfra(), "https://objectstorage.example/p/x/n/ns/b/cloudsourced/o/bin", "img:tag")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ud, "s3cret") {
		t.Fatal("user data must not contain secrets")
	}
	for _, want := range []string{"docker-ce", "curl -fsS", "/swapfile", "DOCKER-USER -d 169.254.0.0/16", "cloudsourced-supervisor.service", "oci-growfs"} {
		if !strings.Contains(ud, want) {
			t.Errorf("user data lacks %q", want)
		}
	}
	line := ud[strings.Index(ud, "echo ")+5:]
	line = line[:strings.Index(line, " ")]
	raw, err := base64.StdEncoding.DecodeString(line)
	if err != nil {
		t.Fatal(err)
	}
	var hs supervisor.Spec
	if err := json.Unmarshal(raw, &hs); err != nil {
		t.Fatal(err)
	}
	if hs.Provider != "oci" || hs.Params[paramSecret] != "secrets/m1.json" || hs.Params[paramRegistry] != "ocir.eu-frankfurt-1.oci.oraclecloud.com" ||
		hs.Params[paramCache] != "cache/my-pool/sccache" || hs.Layout.SecretKeys[0] != "CLOUDSOURCED_TOKEN" {
		t.Errorf("host spec %+v", hs)
	}
}

func TestTagKeys(t *testing.T) {
	for _, l := range []string{provider.LabelEnvHash, provider.LabelPool, "project"} {
		if labelKey(tagKey(l)) != l {
			t.Errorf("round trip %s", l)
		}
	}
}

func TestRegistryHost(t *testing.T) {
	if h := registryHost("eu-frankfurt-1"); h != "ocir.eu-frankfurt-1.oci.oraclecloud.com" {
		t.Error(h)
	}
	if m := machineImage("r/ns/env", provider.MachineSpec{Image: "cloudsourced-env:abc", Arch: "arm64"}); m != "r/ns/env:abc-arm64" {
		t.Error(m)
	}
}

// fakeObjects is an in-memory bucket.
type fakeObjects struct {
	objects map[string][]byte
	mod     time.Time
}

func (f *fakeObjects) HeadObject(_ context.Context, r objectstorage.HeadObjectRequest) (objectstorage.HeadObjectResponse, error) {
	b, ok := f.objects[*r.ObjectName]
	if !ok {
		return objectstorage.HeadObjectResponse{}, svcErr{404, "ObjectNotFound", "no"}
	}
	return objectstorage.HeadObjectResponse{ContentLength: common.Int64(int64(len(b))), LastModified: &common.SDKTime{Time: f.mod}}, nil
}

func (f *fakeObjects) PutObject(_ context.Context, r objectstorage.PutObjectRequest) (objectstorage.PutObjectResponse, error) {
	b, _ := io.ReadAll(r.PutObjectBody)
	if int64(len(b)) != *r.ContentLength {
		return objectstorage.PutObjectResponse{}, errors.New("length mismatch")
	}
	f.objects[*r.ObjectName] = b
	return objectstorage.PutObjectResponse{}, nil
}

func (f *fakeObjects) GetObject(_ context.Context, r objectstorage.GetObjectRequest) (objectstorage.GetObjectResponse, error) {
	b, ok := f.objects[*r.ObjectName]
	if !ok {
		return objectstorage.GetObjectResponse{}, svcErr{404, "ObjectNotFound", "no"}
	}
	return objectstorage.GetObjectResponse{Content: io.NopCloser(bytes.NewReader(b)), ContentLength: common.Int64(int64(len(b))),
		LastModified: &common.SDKTime{Time: f.mod}}, nil
}

func (f *fakeObjects) DeleteObject(_ context.Context, r objectstorage.DeleteObjectRequest) (objectstorage.DeleteObjectResponse, error) {
	if _, ok := f.objects[*r.ObjectName]; !ok {
		return objectstorage.DeleteObjectResponse{}, svcErr{404, "ObjectNotFound", "no"}
	}
	delete(f.objects, *r.ObjectName)
	return objectstorage.DeleteObjectResponse{}, nil
}

func TestObjectStore(t *testing.T) {
	now := time.Now()
	f := &fakeObjects{objects: map[string][]byte{}, mod: now}
	s := newObjectStore(f, "ns", "b", "cache/p/sccache", time.Hour)
	ctx := context.Background()
	if err := s.Put(ctx, "a/b", strings.NewReader("hello"), 5); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.objects["cache/p/sccache/a/b"]; !ok {
		t.Fatalf("objects %v", f.objects)
	}
	if err := s.Put(ctx, "big", strings.NewReader("unknown size"), -1); err != nil {
		t.Fatal(err)
	}
	r, info, err := s.Get(ctx, "a/b")
	if err != nil || info.Size != 5 {
		t.Fatal(info, err)
	}
	b, _ := io.ReadAll(r)
	r.Close()
	if string(b) != "hello" {
		t.Error(string(b))
	}
	s.now = func() time.Time { return now.Add(2 * time.Hour) }
	if _, err := s.Stat(ctx, "a/b"); !errors.Is(err, blobcache.ErrNotFound) {
		t.Errorf("expired: %v", err)
	}
	if err := s.Delete(ctx, "a/b"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "a/b"); err != nil {
		t.Errorf("deleting a missing object: %v", err)
	}
	if _, err := s.Stat(ctx, "../x"); err == nil {
		t.Error("invalid key")
	}
}

type fakeHostCompute struct {
	actions []string
}

func (f *fakeHostCompute) InstanceAction(_ context.Context, r core.InstanceActionRequest) (core.InstanceActionResponse, error) {
	f.actions = append(f.actions, string(r.Action))
	return core.InstanceActionResponse{}, nil
}

func (f *fakeHostCompute) TerminateInstance(_ context.Context, r core.TerminateInstanceRequest) (core.TerminateInstanceResponse, error) {
	if r.PreserveBootVolume == nil || *r.PreserveBootVolume {
		return core.TerminateInstanceResponse{}, errors.New("boot volume must go")
	}
	f.actions = append(f.actions, "TERMINATE")
	return core.TerminateInstanceResponse{}, nil
}

func TestHost(t *testing.T) {
	spec := hostSpec(provider.MachineSpec{ID: "m1", Owner: "p", SecretEnv: map[string]string{"CLOUDSOURCED_TOKEN": "t"}}, "eu-frankfurt-1", testInfra(), "img")
	objs := &fakeObjects{objects: map[string][]byte{"secrets/m1.json": []byte(`{"CLOUDSOURCED_TOKEN":"t"}`)}, mod: time.Now()}
	c := &fakeHostCompute{}
	h := &host{spec: spec, objects: objs, compute: c,
		instanceID: func(context.Context) (string, error) { return "ocid1.instance", nil },
		token:      func(context.Context, string) (string, error) { return "tok", nil }}
	ctx := context.Background()
	m, err := h.FetchSecrets(ctx)
	if err != nil || m["CLOUDSOURCED_TOKEN"] != "t" {
		t.Fatal(m, err)
	}
	if err := h.DeleteSecrets(ctx); err != nil || len(objs.objects) != 0 {
		t.Fatal(err, objs.objects)
	}
	if err := h.DeleteSecrets(ctx); err != nil {
		t.Errorf("second delete: %v", err)
	}
	var login []string
	var stdin string
	docker := func(_ context.Context, _ []string, in []byte, args ...string) ([]byte, error) {
		login, stdin = args, string(in)
		return nil, nil
	}
	if err := h.Prepare(ctx, docker); err != nil || strings.Join(login, " ") != "login --username BEARER_TOKEN --password-stdin ocir.eu-frankfurt-1.oci.oraclecloud.com" || stdin != "tok" {
		t.Errorf("login %v %q %v", login, stdin, err)
	}
	if err := h.StopSelf(ctx); err != nil || strings.Join(c.actions, ",") != "SOFTSTOP" {
		t.Errorf("stop: %v %v", c.actions, err)
	}
	c.actions = nil
	if err := h.TerminateSelf(ctx); err != nil || strings.Join(c.actions, ",") != "TERMINATE" {
		t.Errorf("terminate: %v %v", c.actions, err)
	}
	if st, err := h.CompilerCacheStore(ctx); err != nil || st == nil {
		t.Errorf("cache store %v %v", st, err)
	}
}

func TestServerBinaries(t *testing.T) {
	b := serverBinaries(map[string]any{"binary_arm64": "/x/arm"}, "/opt/bin/cloudsourced", "linux", "amd64")
	if b["amd64"] != "/opt/bin/cloudsourced" || b["arm64"] != "/x/arm" {
		t.Errorf("%v", b)
	}
	b = serverBinaries(nil, "/u/bin/cloudsourced", "darwin", "arm64")
	if b["amd64"] != "/u/bin/cloudsourced-linux-amd64" || b["arm64"] != "/u/bin/cloudsourced-linux-arm64" {
		t.Errorf("%v", b)
	}
	if err := checkBinary("/nonexistent", "amd64"); err == nil || !strings.Contains(err.Error(), "make build") {
		t.Error(err)
	}
}
