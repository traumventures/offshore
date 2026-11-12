package awsec2

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
)

// fakeBake is an EC2 account for baking.
type fakeBake struct {
	mu         sync.Mutex
	images     []ec2types.Image
	run        []*ec2.RunInstancesInput
	terminated []string
	deleted    []string // deregistered images and deleted snapshots
	createErr  error
	describes  int
	states     []ec2types.InstanceStateName // builder states in turn, then stopped
}

func (f *fakeBake) DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	if len(in.InstanceIds) == 0 { // stale builder lookup
		return &ec2.DescribeInstancesOutput{}, nil
	}
	f.mu.Lock()
	state := ec2types.InstanceStateNameStopped
	if len(f.states) > 0 {
		state, f.states = f.states[0], f.states[1:]
	}
	f.mu.Unlock()
	return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: []ec2types.Instance{{
		InstanceId: aws.String(in.InstanceIds[0]), State: &ec2types.InstanceState{Name: state}}}}}}, nil
}

func (f *fakeBake) DescribeImages(ctx context.Context, in *ec2.DescribeImagesInput, _ ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.describes++
	var out []ec2types.Image
	for _, im := range f.images {
		if len(in.ImageIds) > 0 && aws.ToString(im.ImageId) != in.ImageIds[0] {
			continue
		}
		out = append(out, im)
	}
	return &ec2.DescribeImagesOutput{Images: out}, nil
}

func (f *fakeBake) RunInstances(ctx context.Context, in *ec2.RunInstancesInput, _ ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.run = append(f.run, in)
	return &ec2.RunInstancesOutput{Instances: []ec2types.Instance{{InstanceId: aws.String("i-builder")}}}, nil
}

func (f *fakeBake) TerminateInstances(ctx context.Context, in *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminated = append(f.terminated, in.InstanceIds...)
	return &ec2.TerminateInstancesOutput{}, nil
}

func (f *fakeBake) CreateImage(ctx context.Context, in *ec2.CreateImageInput, _ ...func(*ec2.Options)) (*ec2.CreateImageOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.images = append(f.images, ec2types.Image{ImageId: aws.String("ami-new"), State: ec2types.ImageStateAvailable,
		CreationDate: aws.String("2026-11-12T00:00:00Z"), Tags: in.TagSpecifications[0].Tags})
	return &ec2.CreateImageOutput{ImageId: aws.String("ami-new")}, nil
}

func (f *fakeBake) DeregisterImage(ctx context.Context, in *ec2.DeregisterImageInput, _ ...func(*ec2.Options)) (*ec2.DeregisterImageOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, aws.ToString(in.ImageId))
	return &ec2.DeregisterImageOutput{}, nil
}

func (f *fakeBake) DeleteSnapshot(ctx context.Context, in *ec2.DeleteSnapshotInput, _ ...func(*ec2.Options)) (*ec2.DeleteSnapshotOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, aws.ToString(in.SnapshotId))
	return &ec2.DeleteSnapshotOutput{}, nil
}

func bakedImage(id, created, key, tag string) ec2types.Image {
	return ec2types.Image{ImageId: aws.String(id), State: ec2types.ImageStateAvailable, CreationDate: aws.String(created),
		Tags:                []ec2types.Tag{{Key: aws.String(tagBake), Value: aws.String(key)}, {Key: aws.String(tagBakeImage), Value: aws.String(tag)}},
		BlockDeviceMappings: []ec2types.BlockDeviceMapping{{Ebs: &ec2types.EbsBlockDevice{SnapshotId: aws.String("snap-" + id)}}}}
}

func waitIdle(t *testing.T, b *baker) {
	t.Helper()
	for i := 0; i < 500; i++ {
		b.mu.Lock()
		n := len(b.active)
		b.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("bake did not finish")
}

func TestBakeKey(t *testing.T) {
	if bakeKey("ami-1", "repo:a") != bakeKey("ami-1", "repo:a") || bakeKey("ami-1", "repo:a") == bakeKey("ami-2", "repo:a") ||
		bakeKey("ami-1", "repo:a") == bakeKey("ami-1", "repo:b") || imageTag("repo:a", "amd64") == imageTag("repo:a", "arm64") {
		t.Fatal("bake keys do not separate base AMI, image and architecture")
	}
}

func TestChooseBaked(t *testing.T) {
	ims := []ec2types.Image{
		bakedImage("ami-old", "2026-11-01", "k-old", "t"),
		bakedImage("ami-mid", "2026-11-05", "k-mid", "t"),
		{ImageId: aws.String("ami-pending"), State: ec2types.ImageStatePending, CreationDate: aws.String("2026-11-09"),
			Tags: []ec2types.Tag{{Key: aws.String(tagBake), Value: aws.String("k-new")}}},
	}
	if ami, exact := chooseBaked(ims, "k-old"); ami != "ami-old" || !exact {
		t.Fatalf("exact: %s %v", ami, exact)
	}
	// The exact image is still pending: the newest available meanwhile.
	if ami, exact := chooseBaked(ims, "k-new"); ami != "ami-mid" || exact {
		t.Fatalf("fallback: %s %v", ami, exact)
	}
	if ami, exact := chooseBaked(nil, "k"); ami != "" || exact {
		t.Fatalf("none: %s %v", ami, exact)
	}
}

func TestBakeScript(t *testing.T) {
	s := bakeScript("eu-central-1", "123.dkr.ecr.eu-central-1.amazonaws.com", "123.dkr.ecr.eu-central-1.amazonaws.com/repo:env-amd64")
	for _, want := range []string{"dnf install -y -q docker", "docker pull --quiet '123.dkr.ecr.eu-central-1.amazonaws.com/repo:env-amd64'",
		"docker logout", "rm -f /root/.docker/config.json", bakedMarker, "cloud-init clean", "truncate -s 0 /etc/machine-id", "poweroff"} {
		if !strings.Contains(s, want) {
			t.Errorf("bake script lacks %q:\n%s", want, s)
		}
	}
	// No machine state: spec, secrets, supervisor.
	for _, bad := range []string{"machine.json", "offshore-supervisor", "ssm"} {
		if strings.Contains(s, bad) {
			t.Errorf("bake script contains %q", bad)
		}
	}
	if strings.Index(s, "docker logout") < strings.Index(s, "docker pull") || strings.Index(s, "poweroff") < strings.Index(s, "docker logout") {
		t.Error("registry credentials must be removed after the pull and before the power off")
	}
}

func testJob() bakeJob {
	return bakeJob{base: "ami-base", image: "repo:env-amd64", arch: archAMD64, region: "eu-central-1", registry: "123.dkr.ecr",
		profile: "arn:profile", sg: "sg-1", env: "env"}
}

func TestBakeAndUse(t *testing.T) {
	j := testJob()
	tag := imageTag(j.image, j.arch)
	f := &fakeBake{images: []ec2types.Image{
		bakedImage("ami-a", "2026-11-01", "k-a", tag),
		bakedImage("ami-b", "2026-11-05", "k-b", tag),
	},
		// A launched builder is pending, then running, before it stops.
		states: []ec2types.InstanceStateName{ec2types.InstanceStateNamePending, ec2types.InstanceStateNameRunning, ec2types.InstanceStateNameStopping}}
	b := newBaker(f, false)
	b.delay = time.Millisecond
	ctx := context.Background()
	// No image for this base AMI yet: the newest baked one boots while
	// the exact one bakes.
	if ami, baked := b.machineAMI(ctx, j); ami != "ami-b" || !baked {
		t.Fatalf("while baking: %s %v", ami, baked)
	}
	b.machineAMI(ctx, j) // no second bake
	waitIdle(t, b)
	f.mu.Lock()
	if len(f.run) != 1 {
		t.Fatalf("%d builders launched", len(f.run))
	}
	in := f.run[0]
	if aws.ToString(in.ImageId) != "ami-base" || in.InstanceInitiatedShutdownBehavior != ec2types.ShutdownBehaviorStop ||
		aws.ToInt32(in.BlockDeviceMappings[0].Ebs.VolumeSize) != bakeDiskGB || !strings.HasPrefix(string(in.InstanceType), "c7a") {
		t.Fatalf("builder %+v", in)
	}
	kind := ""
	for _, tg := range in.TagSpecifications[0].Tags {
		if aws.ToString(tg.Key) == "offshore.kind" {
			kind = aws.ToString(tg.Value)
		}
	}
	if kind != bakeKind {
		t.Fatalf("builder kind %q (machine listings must ignore it)", kind)
	}
	if len(f.terminated) != 1 || f.terminated[0] != "i-builder" {
		t.Fatalf("builder not terminated: %v", f.terminated)
	}
	// Three images, two kept: the oldest goes with its snapshot.
	if strings.Join(f.deleted, ",") != "ami-a,snap-ami-a" {
		t.Fatalf("pruned %v", f.deleted)
	}
	f.mu.Unlock()
	if ami, baked := b.machineAMI(ctx, j); ami != "ami-new" || !baked {
		t.Fatalf("after baking: %s %v", ami, baked)
	}
	f.mu.Lock()
	runs := len(f.run)
	f.mu.Unlock()
	if runs != 1 {
		t.Fatalf("baked again: %d builders", runs)
	}
}

func TestBakeDenied(t *testing.T) {
	f := &fakeBake{createErr: &smithy.GenericAPIError{Code: "UnauthorizedOperation", Message: "no"}}
	b := newBaker(f, false)
	b.delay = time.Millisecond
	ctx := context.Background()
	if ami, baked := b.machineAMI(ctx, testJob()); ami != "ami-base" || baked {
		t.Fatalf("%s %v", ami, baked)
	}
	waitIdle(t, b)
	f.mu.Lock()
	n := f.describes
	f.mu.Unlock()
	// Denied once: the stock AMI without lookups or bakes from now on.
	if ami, baked := b.machineAMI(ctx, testJob()); ami != "ami-base" || baked {
		t.Fatalf("%s %v", ami, baked)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.describes != n || len(f.run) != 1 || len(f.terminated) != 1 {
		t.Fatalf("after denial: %d lookups (was %d), %d builders, %v terminated", f.describes, n, len(f.run), f.terminated)
	}
}

func TestBakeOff(t *testing.T) {
	f := &fakeBake{}
	if ami, baked := newBaker(f, true).machineAMI(context.Background(), testJob()); ami != "ami-base" || baked || f.describes != 0 {
		t.Fatalf("%s %v %d", ami, baked, f.describes)
	}
}

func TestBakeFailureBacksOff(t *testing.T) {
	f := &fakeBake{states: []ec2types.InstanceStateName{ec2types.InstanceStateNamePending, ec2types.InstanceStateNameTerminated}}
	b := newBaker(f, false)
	b.delay = time.Millisecond
	ctx := context.Background()
	b.machineAMI(ctx, testJob())
	waitIdle(t, b)
	b.machineAMI(ctx, testJob())
	waitIdle(t, b)
	f.mu.Lock()
	if len(f.run) != 1 || len(f.images) != 0 {
		f.mu.Unlock()
		t.Fatalf("failed bake retried at once: %d builders, %d images", len(f.run), len(f.images))
	}
	f.mu.Unlock()
	// After the backoff the next create bakes again; a second failure doubles it.
	k := testJob().key()
	b.mu.Lock()
	if d := time.Until(b.failed[k].until); d < bakeBackoff-time.Minute || d > bakeBackoff {
		b.mu.Unlock()
		t.Fatalf("backoff %s", d)
	}
	b.failed[k] = bakeFail{n: 1}
	b.mu.Unlock()
	f.mu.Lock()
	f.states = []ec2types.InstanceStateName{ec2types.InstanceStateNameShuttingDown}
	f.mu.Unlock()
	b.machineAMI(ctx, testJob())
	waitIdle(t, b)
	b.mu.Lock()
	d := time.Until(b.failed[k].until)
	b.mu.Unlock()
	if d < 2*bakeBackoff-time.Minute || len(f.run) != 2 {
		t.Fatalf("second failure: backoff %s, %d builders", d, len(f.run))
	}
}
