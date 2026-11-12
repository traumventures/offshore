package awsec2

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"

	"github.com/traumventures/offshore/internal/provider"
	"github.com/traumventures/offshore/internal/supervisor"
)

// Baked machine images. A machine boots from the stock Amazon Linux AMI
// in about a minute, half of it installing Docker and a sixth pulling
// the environment image. A baked image is the stock AMI with Docker
// installed and the environment image pulled: machines of that image
// boot from it and skip both. The first create of an image (per
// architecture and base AMI) bakes it in the background on a builder
// instance that holds no machine state (no spec, no secrets, no registry
// credentials); later creates use the result. While a newer base AMI is
// baked, machines use the image baked from the previous one.
//
// Baking needs ec2:CreateImage, ec2:DeregisterImage, ec2:DeleteSnapshot
// and RunInstances from the account's own images (deploy/aws/iam.yaml);
// without them the provider logs once and keeps booting the stock AMI.

const (
	// tagBake is the bake key of a baked image and of its builder
	// (bakeKey); tagBakeImage identifies the environment image.
	tagBake      = "offshore.bake"
	tagBakeImage = "offshore.bake-image"
	// tagBaked on a machine names the baked image it booted.
	tagBaked = "offshore.baked"
	// bakeVersion changes the bake key when bakeScript changes.
	bakeVersion = "1"
	// bakeKind is the offshore.kind of builders and baked images (never
	// "machine": machine listings and the reaper ignore them).
	bakeKind = "bake"
	// bakeDiskGB is the builder's root volume, the size of the image's
	// snapshot (machines boot it with their own disk size).
	bakeDiskGB = 30
	// bakeBuildTimeout bounds the builder's run (a failed bake script
	// leaves it running until then); bakeImageTimeout the snapshot.
	bakeBuildTimeout = 20 * time.Minute
	bakeImageTimeout = 40 * time.Minute
	// bakeKeep is how many baked images per environment image and
	// architecture are kept (the newest).
	bakeKeep = 2
	// bakeCacheTTL is how long a lookup of baked images is reused.
	bakeCacheTTL = time.Minute
	// bakedMarker on a baked disk tells the boot script it runs on one.
	bakedMarker = supervisor.StateDir + "/baked"
)

// bakeTypes are the builder instance types per architecture, in order.
var bakeTypes = map[string][]string{
	archAMD64: {"c7a.xlarge", "m7a.xlarge", "c6a.xlarge", "m6a.xlarge"},
	archARM64: {"c7g.xlarge", "m7g.xlarge", "c6g.xlarge"},
}

// bakeAPI is the EC2 surface of baking (*ec2.Client; fakes in tests).
type bakeAPI interface {
	ec2.DescribeInstancesAPIClient
	ec2.DescribeImagesAPIClient
	RunInstances(ctx context.Context, in *ec2.RunInstancesInput, opts ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error)
	TerminateInstances(ctx context.Context, in *ec2.TerminateInstancesInput, opts ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
	CreateImage(ctx context.Context, in *ec2.CreateImageInput, opts ...func(*ec2.Options)) (*ec2.CreateImageOutput, error)
	DeregisterImage(ctx context.Context, in *ec2.DeregisterImageInput, opts ...func(*ec2.Options)) (*ec2.DeregisterImageOutput, error)
	DeleteSnapshot(ctx context.Context, in *ec2.DeleteSnapshotInput, opts ...func(*ec2.Options)) (*ec2.DeleteSnapshotOutput, error)
}

// baker bakes and finds machine images.
type baker struct {
	api    bakeAPI
	off    bool // option bake = false
	mu     sync.Mutex
	denied bool                 // the credentials may not bake (logged once)
	active map[string]bool      // bake keys being baked
	cache  map[string]bakeCache // image tag -> lookup
	// waits bound the builder and image waits (tests shorten them).
	buildTimeout, imageTimeout time.Duration
	delay                      time.Duration // waiter poll delay (0: SDK default)
}

type bakeCache struct {
	at     time.Time
	images []ec2types.Image
}

func newBaker(api bakeAPI, off bool) *baker {
	return &baker{api: api, off: off, active: map[string]bool{}, cache: map[string]bakeCache{},
		buildTimeout: bakeBuildTimeout, imageTimeout: bakeImageTimeout}
}

// bakeKey identifies a baked image: base AMI, environment image and bake
// script. The server binary is not part of it (machines fetch it at boot).
func bakeKey(baseAMI, image string) string {
	sum := sha256.Sum256([]byte(bakeVersion + "\x00" + baseAMI + "\x00" + image))
	return hex.EncodeToString(sum[:8])
}

// imageTag identifies an environment image and architecture in tags.
func imageTag(image, arch string) string {
	sum := sha256.Sum256([]byte(arch + "\x00" + image))
	return hex.EncodeToString(sum[:8])
}

func tagOf(tags []ec2types.Tag, k string) string {
	for _, t := range tags {
		if aws.ToString(t.Key) == k {
			return aws.ToString(t.Value)
		}
	}
	return ""
}

// chooseBaked picks the AMI for key among the baked images of its
// environment image: the exact one (exact), else the newest (an older
// base AMI) while the exact one is baked.
func chooseBaked(images []ec2types.Image, key string) (ami string, exact bool) {
	newest := ""
	for _, im := range images {
		if im.State != ec2types.ImageStateAvailable {
			continue
		}
		if tagOf(im.Tags, tagBake) == key {
			return aws.ToString(im.ImageId), true
		}
		if c := aws.ToString(im.CreationDate); c > newest {
			newest, ami = c, aws.ToString(im.ImageId)
		}
	}
	return ami, false
}

// images lists the baked images of an environment image (cached briefly).
func (b *baker) images(ctx context.Context, tag string) ([]ec2types.Image, error) {
	b.mu.Lock()
	c, ok := b.cache[tag]
	b.mu.Unlock()
	if ok && time.Since(c.at) < bakeCacheTTL {
		return c.images, nil
	}
	out, err := b.api.DescribeImages(ctx, &ec2.DescribeImagesInput{Owners: []string{"self"}, Filters: []ec2types.Filter{
		{Name: aws.String("tag:" + tagBakeImage), Values: []string{tag}},
		{Name: aws.String("tag:" + tagProject), Values: []string{project}},
	}})
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.cache[tag] = bakeCache{at: time.Now(), images: out.Images}
	b.mu.Unlock()
	return out.Images, nil
}

// bakeJob is everything a bake needs.
type bakeJob struct {
	base, image, arch, region, registry, profile, sg, subnet, env string
}

func (j bakeJob) key() string { return bakeKey(j.base, j.image) }

// machineAMI returns the AMI a machine of job's image boots: a baked one
// when there is one (baked true), else the base AMI. Without an exact
// baked image it starts baking one in the background.
func (b *baker) machineAMI(ctx context.Context, j bakeJob) (ami string, baked bool) {
	if b == nil || b.off {
		return j.base, false
	}
	b.mu.Lock()
	denied := b.denied
	b.mu.Unlock()
	if denied {
		return j.base, false
	}
	images, err := b.images(ctx, imageTag(j.image, j.arch))
	if err != nil {
		slog.Warn("aws-ec2: looking up baked machine images, booting the stock AMI", "err", err)
		return j.base, false
	}
	ami, exact := chooseBaked(images, j.key())
	if !exact {
		b.start(j)
	}
	if ami == "" {
		return j.base, false
	}
	return ami, true
}

// start bakes j in the background unless it is being baked.
func (b *baker) start(j bakeJob) {
	b.mu.Lock()
	if b.active[j.key()] || b.denied {
		b.mu.Unlock()
		return
	}
	b.active[j.key()] = true
	b.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), b.buildTimeout+b.imageTimeout+5*time.Minute)
		defer cancel()
		t0 := time.Now()
		ami, err := b.bake(ctx, j)
		b.mu.Lock()
		delete(b.active, j.key())
		delete(b.cache, imageTag(j.image, j.arch))
		b.mu.Unlock()
		if err != nil && denied(err) {
			b.deny(err)
		}
		switch {
		case err == nil:
			slog.Info("aws-ec2: baked machine image", "ami", ami, "image", j.image, "base", j.base, "duration", time.Since(t0).Round(time.Second))
		case !denied(err):
			slog.Error("aws-ec2: baking machine image", "image", j.image, "err", err)
		}
	}()
}

// deny turns baking off for the process: the credentials may not bake
// or boot baked images.
func (b *baker) deny(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.denied {
		b.denied = true
		slog.Warn("aws-ec2: the credentials may not bake or boot machine images (ec2:CreateImage, RunInstances from own images; see deploy/aws/iam.yaml); machines boot the stock AMI", "err", err)
	}
}

// untagBaked drops the baked image tag from a launch's tags.
func untagBaked(specs []ec2types.TagSpecification) []ec2types.TagSpecification {
	out := make([]ec2types.TagSpecification, len(specs))
	for i, s := range specs {
		out[i] = s
		out[i].Tags = nil
		for _, t := range s.Tags {
			if aws.ToString(t.Key) != tagBaked {
				out[i].Tags = append(out[i].Tags, t)
			}
		}
	}
	return out
}

// denied reports an authorization error.
func denied(err error) bool {
	var ae smithy.APIError
	if !errors.As(err, &ae) {
		return false
	}
	switch ae.ErrorCode() {
	case "UnauthorizedOperation", "AccessDenied", "AccessDeniedException", "AuthFailure":
		return true
	}
	return false
}

// bakeScript renders the builder's user data: Docker as on machines, the
// environment image pulled, no registry credentials, machine identity
// and cloud-init state reset, then power off (the builder stops). A
// failing step leaves the builder running; the provider terminates it.
func bakeScript(region, registry, image string) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	w("#!/bin/bash")
	w("set -euo pipefail")
	w("exec >>/var/log/offshore-bake.log 2>&1")
	w("dnf install -y -q docker")
	w(`mkdir -p /etc/docker && echo %s > /etc/docker/daemon.json`, shq(dockerDaemonJSON))
	w("systemctl enable docker && systemctl start docker")
	w("aws ecr get-login-password --region %s | docker login --username AWS --password-stdin %s", shq(region), shq(registry))
	w("docker pull --quiet %s", shq(image))
	w("docker logout %s; rm -f /root/.docker/config.json", shq(registry))
	w("mkdir -p %s && date -u +%%FT%%TZ > %s", supervisor.StateDir, bakedMarker)
	w("cloud-init clean --logs --seed || true")
	w("truncate -s 0 /etc/machine-id; rm -f /var/lib/systemd/random-seed /etc/ssh/ssh_host_*")
	w("sync && poweroff")
	return b.String()
}

// bake builds and registers the image of j and prunes older ones.
func (b *baker) bake(ctx context.Context, j bakeJob) (string, error) {
	key := j.key()
	b.cleanBuilders(ctx)
	now := time.Now()
	tags := []ec2types.Tag{
		{Key: aws.String("Name"), Value: aws.String("offshore-bake-" + key)},
		{Key: aws.String(tagProject), Value: aws.String(project)},
		{Key: aws.String(provider.LabelKind), Value: aws.String(bakeKind)},
		{Key: aws.String(tagBake), Value: aws.String(key)},
		{Key: aws.String(tagBakeImage), Value: aws.String(imageTag(j.image, j.arch))},
		{Key: aws.String(provider.LabelEnvHash), Value: aws.String(j.env)},
		{Key: aws.String(provider.LabelArch), Value: aws.String(j.arch)},
		{Key: aws.String(provider.LabelCreatedAt), Value: aws.String(unix(now))},
		{Key: aws.String(provider.LabelExpiresAt), Value: aws.String(unix(now.Add(b.buildTimeout)))},
	}
	in := &ec2.RunInstancesInput{
		ImageId:  aws.String(j.base),
		MinCount: aws.Int32(1), MaxCount: aws.Int32(1),
		UserData:                          aws.String(base64.StdEncoding.EncodeToString([]byte(bakeScript(j.region, j.registry, j.image)))),
		IamInstanceProfile:                &ec2types.IamInstanceProfileSpecification{Arn: aws.String(j.profile)},
		InstanceInitiatedShutdownBehavior: ec2types.ShutdownBehaviorStop,
		MetadataOptions: &ec2types.InstanceMetadataOptionsRequest{
			HttpTokens: ec2types.HttpTokensStateRequired, HttpPutResponseHopLimit: aws.Int32(1),
			HttpEndpoint: ec2types.InstanceMetadataEndpointStateEnabled,
		},
		BlockDeviceMappings: []ec2types.BlockDeviceMapping{{
			DeviceName: aws.String("/dev/xvda"),
			Ebs: &ec2types.EbsBlockDevice{VolumeSize: aws.Int32(bakeDiskGB), VolumeType: ec2types.VolumeTypeGp3,
				Iops: aws.Int32(6000), Throughput: aws.Int32(500), DeleteOnTermination: aws.Bool(true), Encrypted: aws.Bool(true)},
		}},
		NetworkInterfaces: []ec2types.InstanceNetworkInterfaceSpecification{{
			DeviceIndex: aws.Int32(0), AssociatePublicIpAddress: aws.Bool(true),
			Groups: []string{j.sg}, DeleteOnTermination: aws.Bool(true),
		}},
		TagSpecifications: []ec2types.TagSpecification{
			{ResourceType: ec2types.ResourceTypeInstance, Tags: tags},
			{ResourceType: ec2types.ResourceTypeVolume, Tags: tags},
			{ResourceType: ec2types.ResourceTypeNetworkInterface, Tags: tags},
		},
	}
	if j.subnet != "" {
		in.NetworkInterfaces[0].SubnetId = aws.String(j.subnet)
	}
	out, _, _, err := launch(ctx, b.api.RunInstances, in, provider.MachineSpec{Size: "bake", InstanceTypes: bakeTypes[j.arch]})
	if err != nil {
		return "", err
	}
	iid := aws.ToString(out.Instances[0].InstanceId)
	defer b.api.TerminateInstances(context.WithoutCancel(ctx), &ec2.TerminateInstancesInput{InstanceIds: []string{iid}})
	stopped := ec2.NewInstanceStoppedWaiter(b.api, func(o *ec2.InstanceStoppedWaiterOptions) {
		o.MinDelay, o.MaxDelay = b.waitDelay(5*time.Second), b.waitDelay(15*time.Second)
	})
	if err := stopped.Wait(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{iid}}, b.buildTimeout); err != nil {
		return "", fmt.Errorf("aws-ec2: bake builder %s did not finish: %w", iid, err)
	}
	itags := tags[:7] // without the builder's times
	img, err := b.api.CreateImage(ctx, &ec2.CreateImageInput{
		InstanceId: aws.String(iid), Name: aws.String("offshore-bake-" + key + "-" + unix(now)),
		Description: aws.String("offshore machine image: Docker and " + j.image),
		NoReboot:    aws.Bool(true), // stopped already
		TagSpecifications: []ec2types.TagSpecification{
			{ResourceType: ec2types.ResourceTypeImage, Tags: itags},
			{ResourceType: ec2types.ResourceTypeSnapshot, Tags: itags},
		},
	})
	if err != nil {
		return "", fmt.Errorf("aws-ec2: create image of %s: %w", iid, err)
	}
	ami := aws.ToString(img.ImageId)
	avail := ec2.NewImageAvailableWaiter(b.api, func(o *ec2.ImageAvailableWaiterOptions) {
		o.MinDelay, o.MaxDelay = b.waitDelay(10*time.Second), b.waitDelay(30*time.Second)
	})
	if err := avail.Wait(ctx, &ec2.DescribeImagesInput{ImageIds: []string{ami}}, b.imageTimeout); err != nil {
		return ami, fmt.Errorf("aws-ec2: baked image %s not available: %w", ami, err)
	}
	b.prune(ctx, imageTag(j.image, j.arch))
	return ami, nil
}

func (b *baker) waitDelay(d time.Duration) time.Duration {
	if b.delay > 0 {
		return b.delay
	}
	return d
}

// prune deregisters all but the newest bakeKeep images of an environment
// image and deletes their snapshots.
func (b *baker) prune(ctx context.Context, tag string) {
	out, err := b.api.DescribeImages(ctx, &ec2.DescribeImagesInput{Owners: []string{"self"}, Filters: []ec2types.Filter{
		{Name: aws.String("tag:" + tagBakeImage), Values: []string{tag}},
		{Name: aws.String("tag:" + tagProject), Values: []string{project}},
	}})
	if err != nil {
		slog.Warn("aws-ec2: listing baked images to prune", "err", err)
		return
	}
	ims := out.Images
	sort.Slice(ims, func(i, k int) bool { return aws.ToString(ims[i].CreationDate) > aws.ToString(ims[k].CreationDate) })
	for _, im := range ims[min(len(ims), bakeKeep):] {
		if _, err := b.api.DeregisterImage(ctx, &ec2.DeregisterImageInput{ImageId: im.ImageId}); err != nil {
			slog.Warn("aws-ec2: deregister baked image", "ami", aws.ToString(im.ImageId), "err", err)
			continue
		}
		for _, m := range im.BlockDeviceMappings {
			if m.Ebs != nil && m.Ebs.SnapshotId != nil {
				if _, err := b.api.DeleteSnapshot(ctx, &ec2.DeleteSnapshotInput{SnapshotId: m.Ebs.SnapshotId}); err != nil {
					slog.Warn("aws-ec2: delete snapshot of baked image", "snapshot", aws.ToString(m.Ebs.SnapshotId), "err", err)
				}
			}
		}
	}
}

// cleanBuilders terminates builders left by an interrupted bake.
func (b *baker) cleanBuilders(ctx context.Context) {
	out, err := b.api.DescribeInstances(ctx, &ec2.DescribeInstancesInput{Filters: []ec2types.Filter{
		{Name: aws.String("tag:" + provider.LabelKind), Values: []string{bakeKind}},
		{Name: aws.String("tag:" + tagProject), Values: []string{project}},
		{Name: aws.String("instance-state-name"), Values: []string{"pending", "running", "stopping", "stopped"}},
	}})
	if err != nil {
		return
	}
	b.mu.Lock()
	active := map[string]bool{}
	for k := range b.active {
		active[k] = true
	}
	b.mu.Unlock()
	var stale []string
	now := time.Now().Unix()
	for _, r := range out.Reservations {
		for _, i := range r.Instances {
			var exp int64
			fmt.Sscan(tagOf(i.Tags, provider.LabelExpiresAt), &exp)
			if exp > 0 && now > exp && !(active[tagOf(i.Tags, tagBake)] && now < exp+int64(b.imageTimeout.Seconds())) {
				stale = append(stale, aws.ToString(i.InstanceId))
			}
		}
	}
	if len(stale) > 0 {
		slog.Info("aws-ec2: terminating stale bake builders", "instances", stale)
		b.api.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: stale})
	}
}
