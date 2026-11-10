// Package oci runs machines as Oracle Cloud Infrastructure compute
// instances, the counterpart of aws-ec2: each instance runs Docker and the
// supervisor ("cloudsourced supervisor"), which applies the machine
// container layout of the local-docker reference backend (machine.Layout).
// User data carries only the public machine spec; the secret server
// environment travels through a private Object Storage object that the
// supervisor reads once with its instance principal and deletes.
//
// Shared infrastructure (compartment, VCN, bucket, container repository,
// dynamic group and policy) is created on first use, see infra.go.
// Environment images are pushed to the OCI container registry (OCIR) by
// the image pipeline before an instance is launched.
//
// Differences to aws-ec2, by OCI's design:
//   - spot sizes launch preemptible instances. OCI terminates them on
//     preemption (there is no stop behaviour), so they cannot idle-stop
//     either: stopping one terminates it. There is no advance notice the
//     supervisor could drain on.
//   - an OS shutdown only stops an OCI instance: at expiry the supervisor
//     terminates its instance through the API (supervisor.SelfTerminator).
//   - there is no reaper function: expired machines terminate themselves,
//     and the orchestrator destroys what it finds expired.
package oci

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/oracle/oci-go-sdk/v65/artifacts"
	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/core"
	"github.com/oracle/oci-go-sdk/v65/identity"
	"github.com/oracle/oci-go-sdk/v65/objectstorage"

	"github.com/traumventures/cloudsourced/internal/image"
	"github.com/traumventures/cloudsourced/internal/provider"
)

const (
	serverPort = 7443
	tagProject = "project"
	project    = "cloudsourced"
)

func init() {
	provider.Register("oci", open)
}

// OCI is the oci provider.
type OCI struct {
	region      string
	profile     string
	configFile  string
	compartment string // name below the tenancy root, or an OCID
	ad          string // availability domain (name or suffix; default: the first)
	subnet      string // optional existing subnet
	allow       []string
	images      map[string]string // optional fixed image OCID per architecture
	diskGB      int64
	docker      []string
	binaries    map[string]string // server binary per architecture
	bucket      string
	repository  string
	registry    string // OCIR host
	iam         bool   // create the dynamic group and policy
	pollEvery   time.Duration
	log         *slog.Logger

	initMu    sync.Mutex
	initErr   error
	ready     bool
	cfg       common.ConfigurationProvider
	compute   computeAPI
	network   networkAPI
	objects   objectAPI
	identity  identityAPI
	artifacts artifactsAPI

	infraMu sync.Mutex
	infra   *infra

	binMu sync.Mutex
	bins  map[string]binaryURL // architecture -> download URL of the uploaded binary

	imgMu    sync.Mutex
	imageIDs map[string]string // shape -> platform image OCID

	builderOnce sync.Once
	builder     *image.DockerBuilder
}

type binaryURL struct {
	url     string
	expires time.Time
}

var (
	_ provider.ImageHost = (*OCI)(nil)
	_ provider.Provider  = (*OCI)(nil)
)

func open(opts map[string]any) (provider.Provider, error) {
	o := &OCI{
		compartment: project, bucket: project, repository: project + "-env",
		diskGB: 100, docker: []string{"docker"}, iam: true, pollEvery: 5 * time.Second,
		images: map[string]string{}, bins: map[string]binaryURL{}, imageIDs: map[string]string{},
		log: slog.Default(),
	}
	o.region = provider.String(opts, "region", "")
	o.profile = provider.String(opts, "profile", "DEFAULT")
	o.configFile = provider.String(opts, "config_file", "")
	if v := provider.String(opts, "compartment", ""); v != "" {
		o.compartment = v
	}
	o.ad = provider.String(opts, "availability_domain", "")
	o.subnet = provider.String(opts, "subnet", "")
	o.allow = provider.StringList(opts["allow_cidrs"])
	o.images[archAMD64] = provider.String(opts, "image", "")
	o.images[archARM64] = provider.String(opts, "image_arm64", "")
	if v := provider.String(opts, "bucket", ""); v != "" {
		o.bucket = v
	}
	if v := provider.String(opts, "repository", ""); v != "" {
		o.repository = v
	}
	o.registry = provider.String(opts, "registry", "")
	if v, ok := opts["iam"].(bool); ok {
		o.iam = v
	}
	if v := provider.StringList(opts["docker_command"]); len(v) > 0 {
		o.docker = v
	}
	switch v := opts["disk_gb"].(type) {
	case int64:
		o.diskGB = v
	case float64:
		o.diskGB = int64(v)
	}
	if o.diskGB < 50 {
		return nil, fmt.Errorf("oci: disk_gb must be at least 50 (the minimum boot volume)")
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	o.binaries = serverBinaries(opts, self, runtime.GOOS, runtime.GOARCH)
	return o, nil
}

// init creates the SDK clients on first use, so that a configured but
// unused provider needs no credentials.
func (o *OCI) init() error {
	o.initMu.Lock()
	defer o.initMu.Unlock()
	if o.ready || o.initErr != nil {
		return o.initErr
	}
	o.initErr = o.connect()
	o.ready = o.initErr == nil
	return o.initErr
}

func (o *OCI) connect() error {
	cfg := common.CustomProfileConfigProvider(o.configFile, o.profile)
	if o.region == "" {
		r, err := cfg.Region()
		if err != nil || r == "" {
			return fmt.Errorf("oci: no region (set the region option or region in the OCI config profile %s): %v", o.profile, err)
		}
		o.region = r
	}
	cc, err := core.NewComputeClientWithConfigurationProvider(cfg)
	if err != nil {
		return fmt.Errorf("oci: %w", err)
	}
	nc, err := core.NewVirtualNetworkClientWithConfigurationProvider(cfg)
	if err != nil {
		return fmt.Errorf("oci: %w", err)
	}
	oc, err := objectstorage.NewObjectStorageClientWithConfigurationProvider(cfg)
	if err != nil {
		return fmt.Errorf("oci: %w", err)
	}
	ic, err := identity.NewIdentityClientWithConfigurationProvider(cfg)
	if err != nil {
		return fmt.Errorf("oci: %w", err)
	}
	ac, err := artifacts.NewArtifactsClientWithConfigurationProvider(cfg)
	if err != nil {
		return fmt.Errorf("oci: %w", err)
	}
	cc.SetRegion(o.region)
	nc.SetRegion(o.region)
	oc.SetRegion(o.region)
	ic.SetRegion(o.region)
	ac.SetRegion(o.region)
	o.cfg, o.compute, o.network, o.objects, o.identity, o.artifacts = cfg, cc, nc, oc, ic, ac
	if o.registry == "" {
		o.registry = registryHost(o.region)
	}
	return nil
}

// registryHost is the OCIR host of a region.
func registryHost(region string) string {
	return "ocir." + region + ".oci." + common.StringToRegion(region).SecondLevelDomain()
}

func (o *OCI) Name() string { return "oci" }

func (o *OCI) Caps() provider.Caps {
	// Instances boot in about a minute: poll readiness slowly, backing off.
	return provider.Caps{FUSE: true, StopKeepsDisk: true, PollInterval: 2 * time.Second, PollMax: 10 * time.Second}
}

func publicIP(ctx context.Context) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://checkip.amazonaws.com", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// --- images ---

// ImagePlatform: the pool knows the architecture of its sizes and uses
// that; unknown sizes are amd64.
func (o *OCI) ImagePlatform(string) image.Platform { return image.LinuxAMD64 }

// ImageRegistry is the provider's OCIR repository.
func (o *OCI) ImageRegistry() image.Registry {
	return &image.OCIR{
		CLI: o.docker,
		Repository: func(ctx context.Context) (string, error) {
			if err := o.init(); err != nil {
				return "", err
			}
			in, err := o.ensureInfra(ctx)
			if err != nil {
				return "", err
			}
			return in.repo, nil
		},
		Exists: o.imageExists,
		Token:  o.registryToken,
	}
}

// ImageBuilder builds with the configured Docker CLI (docker_command).
func (o *OCI) ImageBuilder() image.Builder {
	o.builderOnce.Do(func() { o.builder = &image.DockerBuilder{CLI: o.docker} })
	return o.builder
}

// imageExists reports whether the repository holds a version (tag).
func (o *OCI) imageExists(ctx context.Context, repo, tag string) (bool, error) {
	in, err := o.ensureInfra(ctx)
	if err != nil {
		return false, err
	}
	resp, err := o.artifacts.ListContainerImages(ctx, artifacts.ListContainerImagesRequest{
		CompartmentId: common.String(in.compartment), RepositoryName: common.String(repo), Version: common.String(tag)})
	if err != nil {
		return false, fmt.Errorf("oci: list container images: %w", err)
	}
	return len(resp.Items) > 0, nil
}

// registryToken returns an OCIR bearer token for the API key of the
// configuration (docker login --username BEARER_TOKEN).
func (o *OCI) registryToken(ctx context.Context, registry string) (string, error) {
	if err := o.init(); err != nil {
		return "", err
	}
	return ocirToken(ctx, o.cfg, registry)
}

// ocirToken asks the registry for a docker token with a request signed by
// cfg (an API key or an instance principal).
func ocirToken(ctx context.Context, cfg common.ConfigurationProvider, registry string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+registry+"/20180419/docker/token", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Date", time.Now().UTC().Format(http.TimeFormat))
	if err := common.DefaultRequestSigner(cfg).Sign(req); err != nil {
		return "", fmt.Errorf("oci: sign registry token request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("oci: registry token: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("oci: registry token: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var t struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(b, &t); err != nil || t.Token == "" {
		return "", fmt.Errorf("oci: registry token: unexpected response")
	}
	return t.Token, nil
}

// machineImage returns the OCIR reference of spec's image: the pipeline
// passes it resolved; a bare environment tag is mapped to the repository.
func machineImage(repo string, spec provider.MachineSpec) string {
	if strings.HasPrefix(spec.Image, repo+":") {
		return spec.Image
	}
	return image.RemoteTag(repo, spec.Image, image.Platform("linux/"+archOf(spec.Arch)))
}

// --- binaries ---

// binaryKey is the content addressed object name of a server binary.
func binaryKey(arch string, data []byte) string {
	sum := sha256.Sum256(data)
	return "bin/cloudsourced-linux-" + arch + "-" + hex.EncodeToString(sum[:])[:16]
}

// ensureBinary uploads the server binary of arch (content addressed) once
// and returns a read-only pre-authenticated URL of it, which first boot
// downloads with curl (an instance has no OCI CLI). The URL is renewed a
// day before it expires.
func (o *OCI) ensureBinary(ctx context.Context, in *infra, arch string, now time.Time) (string, error) {
	o.binMu.Lock()
	defer o.binMu.Unlock()
	if b, ok := o.bins[arch]; ok && b.expires.After(now.Add(24*time.Hour)) {
		return b.url, nil
	}
	path := o.binaries[arch]
	if err := checkBinary(path, arch); err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	key := binaryKey(arch, data)
	_, err = o.objects.HeadObject(ctx, objectstorage.HeadObjectRequest{NamespaceName: common.String(in.namespace),
		BucketName: common.String(in.bucket), ObjectName: common.String(key)})
	if notFound(err) {
		_, err = o.objects.PutObject(ctx, objectstorage.PutObjectRequest{NamespaceName: common.String(in.namespace),
			BucketName: common.String(in.bucket), ObjectName: common.String(key),
			ContentLength: common.Int64(int64(len(data))), PutObjectBody: io.NopCloser(bytes.NewReader(data))})
	}
	if err != nil {
		return "", fmt.Errorf("oci: upload server binary: %w", err)
	}
	expires := now.Add(7 * 24 * time.Hour)
	pr, err := o.objects.CreatePreauthenticatedRequest(ctx, objectstorage.CreatePreauthenticatedRequestRequest{
		NamespaceName: common.String(in.namespace), BucketName: common.String(in.bucket),
		CreatePreauthenticatedRequestDetails: objectstorage.CreatePreauthenticatedRequestDetails{
			Name: common.String(strings.TrimPrefix(key, "bin/")), ObjectName: common.String(key),
			AccessType:  objectstorage.CreatePreauthenticatedRequestDetailsAccessTypeObjectread,
			TimeExpires: &common.SDKTime{Time: expires},
		}})
	if err != nil {
		return "", fmt.Errorf("oci: binary download URL: %w", err)
	}
	url := parURL(o.region, pr.PreauthenticatedRequest)
	o.bins[arch] = binaryURL{url: url, expires: expires}
	return url, nil
}

// parURL is the full URL of a pre-authenticated request.
func parURL(region string, p objectstorage.PreauthenticatedRequest) string {
	if p.FullPath != nil && *p.FullPath != "" {
		return *p.FullPath
	}
	return "https://objectstorage." + region + "." + common.StringToRegion(region).SecondLevelDomain() + *p.AccessUri
}

// platformImage returns the boot image for a shape: the image option of
// the architecture, else the newest Oracle Linux 9 platform image
// compatible with the shape.
func (o *OCI) platformImage(ctx context.Context, in *infra, arch, shape string) (string, error) {
	if id := o.images[arch]; id != "" {
		return id, nil
	}
	o.imgMu.Lock()
	defer o.imgMu.Unlock()
	if id := o.imageIDs[shape]; id != "" {
		return id, nil
	}
	resp, err := o.compute.ListImages(ctx, core.ListImagesRequest{
		CompartmentId: common.String(in.tenancy), OperatingSystem: common.String("Oracle Linux"),
		OperatingSystemVersion: common.String("9"), Shape: common.String(shape),
		LifecycleState: core.ImageLifecycleStateAvailable,
		SortBy:         core.ListImagesSortByTimecreated, SortOrder: core.ListImagesSortOrderDesc,
	})
	if err != nil {
		return "", fmt.Errorf("oci: Oracle Linux 9 images for %s: %w", shape, err)
	}
	for _, im := range resp.Items {
		if n := strings.ToLower(deref(im.DisplayName)); strings.Contains(n, "gpu") || strings.Contains(n, "minimal") {
			continue
		}
		o.imageIDs[shape] = *im.Id
		return *im.Id, nil
	}
	return "", fmt.Errorf("oci: no Oracle Linux 9 image for shape %s (set the image option)", shape)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// --- machines ---

// tagKey maps a label to a freeform tag key ("cloudsourced.pool" becomes
// "cloudsourced_pool"; OCI tag keys avoid dots), labelKey back.
func tagKey(label string) string {
	if rest, ok := strings.CutPrefix(label, project+"."); ok {
		return project + "_" + rest
	}
	return label
}

func labelKey(tag string) string {
	if rest, ok := strings.CutPrefix(tag, project+"_"); ok {
		return project + "." + rest
	}
	return tag
}

func unix(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }

// machineTags are the freeform tags of a machine's instance.
func machineTags(spec provider.MachineSpec, arch string, now time.Time) map[string]string {
	l := map[string]string{
		tagProject:              project,
		provider.LabelPool:      spec.Owner,
		provider.LabelMachine:   spec.ID,
		provider.LabelKind:      "machine",
		provider.LabelEnvHash:   spec.EnvHash,
		provider.LabelCreatedAt: unix(now),
		provider.LabelExpiresAt: unix(spec.ExpiresAt),
		provider.LabelCPU:       strconv.FormatFloat(spec.CPU, 'f', -1, 64),
		provider.LabelMemory:    strconv.FormatInt(spec.Memory, 10),
		provider.LabelFinger:    spec.Finger,
		provider.LabelSize:      spec.Size,
		provider.LabelArch:      arch,
	}
	t := make(map[string]string, len(l))
	for k, v := range l {
		t[tagKey(k)] = v
	}
	return t
}

// secretObject is the object holding a machine's secret environment until
// its supervisor has read it.
func secretObject(machineID string) string { return "secrets/" + pathSegment(machineID) + ".json" }

// Create launches an instance whose user data installs Docker and starts
// the supervisor.
func (o *OCI) Create(ctx context.Context, spec provider.MachineSpec) (provider.Machine, error) {
	if err := o.init(); err != nil {
		return provider.Machine{}, err
	}
	in, err := o.ensureInfra(ctx)
	if err != nil {
		return provider.Machine{}, err
	}
	arch := archOf(spec.Arch)
	binURL, err := o.ensureBinary(ctx, in, arch, time.Now())
	if err != nil {
		return provider.Machine{}, err
	}
	img, err := o.platformImage(ctx, in, arch, shapes(spec)[0])
	if err != nil {
		return provider.Machine{}, err
	}
	ud, err := userData(spec, o.region, in, binURL, machineImage(in.repo, spec))
	if err != nil {
		return provider.Machine{}, err
	}
	if len(ud) > 16000 {
		return provider.Machine{}, fmt.Errorf("oci: user data too large (%d bytes)", len(ud))
	}
	base := core.LaunchInstanceDetails{
		AvailabilityDomain: common.String(in.ad),
		CompartmentId:      common.String(in.compartment),
		DisplayName:        common.String("cloudsourced-" + spec.ID),
		CreateVnicDetails: &core.CreateVnicDetails{SubnetId: common.String(in.subnet), AssignPublicIp: common.Bool(true),
			FreeformTags: freeform()},
		SourceDetails: core.InstanceSourceViaImageDetails{ImageId: common.String(img), BootVolumeSizeInGBs: common.Int64(o.diskGB)},
		Metadata:      map[string]string{"user_data": base64.StdEncoding.EncodeToString([]byte(ud))},
		// Metadata service v2 only (requests need the Authorization
		// header; containers are cut off from it, see userData).
		InstanceOptions: &core.InstanceOptions{AreLegacyImdsEndpointsDisabled: common.Bool(true)},
		FreeformTags:    machineTags(spec, arch, time.Now()),
	}
	// The secret environment goes through Object Storage: the supervisor
	// reads it with the instance principal and deletes it.
	if err := o.putSecrets(ctx, in, spec); err != nil {
		return provider.Machine{}, err
	}
	inst, how, failed, err := launch(ctx, o.compute.LaunchInstance, base, spec)
	if err != nil {
		o.deleteSecrets(context.Background(), in, spec.ID)
		return provider.Machine{}, err
	}
	if len(failed) > 0 && how.spot {
		o.log.Info("oci: launched on a later preemptible shape", "machine", spec.ID, "size", spec.Size, "shape", how.shape, "failed", strings.Join(failed, "; "))
	}
	if spec.Spot && !how.spot {
		o.log.Warn("oci: no preemptible capacity, launched on regular capacity", "machine", spec.ID, "size", spec.Size, "shape", how.shape, "spot_attempts", strings.Join(failed, "; "))
	}
	if err := o.waitState(ctx, *inst.Id, 10*time.Minute, core.InstanceLifecycleStateRunning); err != nil {
		o.terminate(context.Background(), *inst.Id)
		o.deleteSecrets(context.Background(), in, spec.ID)
		return provider.Machine{}, err
	}
	return o.byMachine(ctx, spec.ID)
}

func (o *OCI) putSecrets(ctx context.Context, in *infra, spec provider.MachineSpec) error {
	b, err := json.Marshal(spec.SecretEnv)
	if err != nil {
		return err
	}
	_, err = o.objects.PutObject(ctx, objectstorage.PutObjectRequest{NamespaceName: common.String(in.namespace),
		BucketName: common.String(in.bucket), ObjectName: common.String(secretObject(spec.ID)),
		ContentLength: common.Int64(int64(len(b))), PutObjectBody: io.NopCloser(bytes.NewReader(b)),
		ContentType: common.String("application/json")})
	if err != nil {
		return fmt.Errorf("oci: store machine secrets: %w", err)
	}
	return nil
}

func (o *OCI) deleteSecrets(ctx context.Context, in *infra, id string) error {
	_, err := o.objects.DeleteObject(ctx, objectstorage.DeleteObjectRequest{NamespaceName: common.String(in.namespace),
		BucketName: common.String(in.bucket), ObjectName: common.String(secretObject(id))})
	if err != nil && !notFound(err) {
		return fmt.Errorf("oci: delete machine secrets: %w", err)
	}
	return nil
}

// waitState waits until an instance is in one of states.
func (o *OCI) waitState(ctx context.Context, id string, timeout time.Duration, states ...core.InstanceLifecycleStateEnum) error {
	var last core.InstanceLifecycleStateEnum
	err := o.poll(ctx, timeout, func() (bool, error) {
		r, err := o.compute.GetInstance(ctx, core.GetInstanceRequest{InstanceId: common.String(id)})
		if err != nil {
			return false, err
		}
		last = r.LifecycleState
		for _, s := range states {
			if last == s {
				return true, nil
			}
		}
		if last == core.InstanceLifecycleStateTerminated || last == core.InstanceLifecycleStateTerminating {
			return false, fmt.Errorf("instance %s is %s", id, last)
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("oci: waiting for instance %s (%s): %w", id, last, err)
	}
	return nil
}

// instances returns the live instances of owner (optionally one machine).
func (o *OCI) instances(ctx context.Context, owner, machine string) ([]core.Instance, error) {
	in, err := o.ensureInfra(ctx)
	if err != nil {
		return nil, err
	}
	var out []core.Instance
	req := core.ListInstancesRequest{CompartmentId: common.String(in.compartment)}
	for {
		resp, err := o.compute.ListInstances(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("oci: list instances: %w", err)
		}
		for _, i := range resp.Items {
			if selected(i, owner, machine) {
				out = append(out, i)
			}
		}
		if resp.OpcNextPage == nil {
			return out, nil
		}
		req.Page = resp.OpcNextPage
	}
}

func selected(i core.Instance, owner, machine string) bool {
	t := i.FreeformTags
	if i.LifecycleState == core.InstanceLifecycleStateTerminated || t[tagKey(provider.LabelKind)] != "machine" {
		return false
	}
	return (owner == "" || t[tagKey(provider.LabelPool)] == owner) && (machine == "" || t[tagKey(provider.LabelMachine)] == machine)
}

// toMachine maps an instance (and its public address, "" if unknown).
func toMachine(i core.Instance, publicIP string) provider.Machine {
	l := map[string]string{}
	for k, v := range i.FreeformTags {
		l[labelKey(k)] = v
	}
	m := provider.Machine{ID: l[provider.LabelMachine], EnvHash: l[provider.LabelEnvHash], Labels: l, Fingerprint: l[provider.LabelFinger]}
	l["instance-id"] = deref(i.Id)
	l["shape"] = deref(i.Shape)
	if i.PreemptibleInstanceConfig != nil {
		l[provider.LabelMarket], l[provider.LabelSpot] = provider.MarketSpot, "true"
	}
	m.CPU, _ = strconv.ParseFloat(l[provider.LabelCPU], 64)
	m.Memory, _ = strconv.ParseInt(l[provider.LabelMemory], 10, 64)
	if n, err := strconv.ParseInt(l[provider.LabelCreatedAt], 10, 64); err == nil {
		m.CreatedAt = time.Unix(n, 0)
	}
	if n, err := strconv.ParseInt(l[provider.LabelExpiresAt], 10, 64); err == nil {
		m.ExpiresAt = time.Unix(n, 0)
	}
	switch i.LifecycleState {
	case core.InstanceLifecycleStateRunning:
		m.State = provider.Running
	case core.InstanceLifecycleStateProvisioning, core.InstanceLifecycleStateStarting, core.InstanceLifecycleStateMoving:
		m.State = provider.Starting
	case core.InstanceLifecycleStateStopping, core.InstanceLifecycleStateStopped:
		m.State = provider.Stopped
	default:
		m.State = provider.Failed
		if i.PreemptibleInstanceConfig != nil && i.LifecycleState == core.InstanceLifecycleStateTerminating {
			l["interrupted"] = "spot"
		}
	}
	if m.State == provider.Running && publicIP != "" {
		m.Addr = publicIP + ":" + strconv.Itoa(serverPort)
	}
	return m
}

// publicAddr returns the public IP of an instance's primary VNIC.
func (o *OCI) publicAddr(ctx context.Context, i core.Instance) (string, error) {
	ar, err := o.compute.ListVnicAttachments(ctx, core.ListVnicAttachmentsRequest{CompartmentId: i.CompartmentId, InstanceId: i.Id})
	if err != nil {
		return "", fmt.Errorf("oci: vnic attachments: %w", err)
	}
	for _, a := range ar.Items {
		if a.VnicId == nil || a.LifecycleState != core.VnicAttachmentLifecycleStateAttached {
			continue
		}
		v, err := o.network.GetVnic(ctx, core.GetVnicRequest{VnicId: a.VnicId})
		if err != nil {
			return "", fmt.Errorf("oci: vnic: %w", err)
		}
		if v.IsPrimary != nil && *v.IsPrimary {
			return deref(v.PublicIp), nil
		}
	}
	return "", nil
}

func (o *OCI) machine(ctx context.Context, i core.Instance) (provider.Machine, error) {
	ip := ""
	if i.LifecycleState == core.InstanceLifecycleStateRunning {
		var err error
		if ip, err = o.publicAddr(ctx, i); err != nil {
			return provider.Machine{}, err
		}
	}
	return toMachine(i, ip), nil
}

func (o *OCI) byInstance(ctx context.Context, id string) (core.Instance, error) {
	is, err := o.instances(ctx, "", id)
	if err != nil {
		return core.Instance{}, err
	}
	if len(is) == 0 {
		return core.Instance{}, fmt.Errorf("oci: machine %s not found", id)
	}
	return is[0], nil
}

func (o *OCI) byMachine(ctx context.Context, id string) (provider.Machine, error) {
	i, err := o.byInstance(ctx, id)
	if err != nil {
		return provider.Machine{}, err
	}
	return o.machine(ctx, i)
}

// Start starts a stopped instance; the supervisor unit restarts the
// containers at boot.
func (o *OCI) Start(ctx context.Context, id string) (provider.Machine, error) {
	if err := o.init(); err != nil {
		return provider.Machine{}, err
	}
	i, err := o.byInstance(ctx, id)
	if err != nil {
		return provider.Machine{}, err
	}
	// A machine that is still stopping cannot be started yet.
	if err := o.waitState(ctx, *i.Id, 5*time.Minute, core.InstanceLifecycleStateStopped,
		core.InstanceLifecycleStateRunning, core.InstanceLifecycleStateStarting); err != nil {
		return provider.Machine{}, err
	}
	if _, err := o.compute.InstanceAction(ctx, core.InstanceActionRequest{InstanceId: i.Id, Action: core.InstanceActionActionStart}); err != nil {
		return provider.Machine{}, fmt.Errorf("oci: start %s: %w", id, err)
	}
	if err := o.waitState(ctx, *i.Id, 10*time.Minute, core.InstanceLifecycleStateRunning); err != nil {
		return provider.Machine{}, err
	}
	return o.byMachine(ctx, id)
}

// Stop stops the instance (ACPI shutdown, then power off); its boot
// volume, and with it the caches, is kept. Preemptible instances cannot
// be stopped on OCI: stopping one terminates it.
func (o *OCI) Stop(ctx context.Context, id string) error {
	if err := o.init(); err != nil {
		return err
	}
	i, err := o.byInstance(ctx, id)
	if err != nil {
		return err
	}
	if i.PreemptibleInstanceConfig != nil {
		o.log.Info("oci: preemptible machines cannot stop, terminating it", "machine", id)
		return o.Destroy(ctx, id)
	}
	_, err = o.compute.InstanceAction(ctx, core.InstanceActionRequest{InstanceId: i.Id, Action: core.InstanceActionActionSoftstop})
	if err != nil {
		return fmt.Errorf("oci: stop %s: %w", id, err)
	}
	return nil
}

// Destroy terminates the instance; its boot volume is deleted with it.
func (o *OCI) Destroy(ctx context.Context, id string) error {
	if err := o.init(); err != nil {
		return err
	}
	in, err := o.ensureInfra(ctx)
	if err != nil {
		return err
	}
	is, err := o.instances(ctx, "", id)
	if err != nil {
		return err
	}
	for _, i := range is {
		if err := o.terminate(ctx, *i.Id); err != nil {
			return err
		}
	}
	// Normally the supervisor has deleted it already.
	return o.deleteSecrets(ctx, in, id)
}

func (o *OCI) terminate(ctx context.Context, instanceID string) error {
	_, err := o.compute.TerminateInstance(ctx, core.TerminateInstanceRequest{InstanceId: common.String(instanceID), PreserveBootVolume: common.Bool(false)})
	if err != nil && !notFound(err) {
		return fmt.Errorf("oci: terminate %s: %w", instanceID, err)
	}
	return nil
}

// List returns the machines of owner.
func (o *OCI) List(ctx context.Context, owner string) ([]provider.Machine, error) {
	if err := o.init(); err != nil {
		return nil, err
	}
	is, err := o.instances(ctx, owner, "")
	if err != nil {
		return nil, err
	}
	var ms []provider.Machine
	for _, i := range is {
		m, err := o.machine(ctx, i)
		if err != nil {
			return nil, err
		}
		ms = append(ms, m)
	}
	return ms, nil
}

// Volumes: caches live on the boot volume and die with the instance.
func (o *OCI) Volumes(context.Context, string) ([]provider.Volume, error) { return nil, nil }

func (o *OCI) DeleteVolume(context.Context, string) error { return nil }
