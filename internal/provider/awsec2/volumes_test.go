package awsec2

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"

	"github.com/traumventures/offshore/internal/provider"
)

// fakeEC2 keeps volumes and instances in memory; state changes are
// immediate.
type fakeEC2 struct {
	ec2API
	vols      map[string]*ec2types.Volume
	instances []ec2types.Instance
	n         int
	modified  int
}

func newFake() *fakeEC2 {
	return &fakeEC2{vols: map[string]*ec2types.Volume{}, instances: []ec2types.Instance{
		{InstanceId: aws.String("i-1"), Placement: &ec2types.Placement{AvailabilityZone: aws.String("eu-central-1a")},
			State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
			Tags:  []ec2types.Tag{{Key: aws.String(provider.LabelMachine), Value: aws.String("m1")}}},
		{InstanceId: aws.String("i-2"), Placement: &ec2types.Placement{AvailabilityZone: aws.String("eu-central-1b")},
			State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
			Tags:  []ec2types.Tag{{Key: aws.String(provider.LabelMachine), Value: aws.String("m2")}}},
	}}
}

func volErr(code string) error { return &smithy.GenericAPIError{Code: code, Message: code} }

func tagValue(tags []ec2types.Tag, k string) (string, bool) {
	for _, t := range tags {
		if aws.ToString(t.Key) == k {
			return aws.ToString(t.Value), true
		}
	}
	return "", false
}

func (f *fakeEC2) DescribeInstances(_ context.Context, in *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	var out []ec2types.Instance
	for _, i := range f.instances {
		ok := true
		for _, fl := range in.Filters {
			name := aws.ToString(fl.Name)
			if name == "tag:"+provider.LabelMachine {
				v, _ := tagValue(i.Tags, provider.LabelMachine)
				ok = ok && v == fl.Values[0]
			}
		}
		if ok {
			out = append(out, i)
		}
	}
	return &ec2.DescribeInstancesOutput{Reservations: []ec2types.Reservation{{Instances: out}}}, nil
}

func (f *fakeEC2) DescribeSubnets(_ context.Context, in *ec2.DescribeSubnetsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error) {
	all := []ec2types.Subnet{{SubnetId: aws.String("subnet-b"), AvailabilityZone: aws.String("eu-central-1b")},
		{SubnetId: aws.String("subnet-a"), AvailabilityZone: aws.String("eu-central-1a")}}
	var out []ec2types.Subnet
	for _, s := range all {
		ok := len(in.SubnetIds) == 0 || in.SubnetIds[0] == aws.ToString(s.SubnetId)
		for _, fl := range in.Filters {
			if aws.ToString(fl.Name) == "availability-zone" {
				ok = ok && fl.Values[0] == aws.ToString(s.AvailabilityZone)
			}
		}
		if ok {
			out = append(out, s)
		}
	}
	return &ec2.DescribeSubnetsOutput{Subnets: out}, nil
}

func (f *fakeEC2) DescribeVolumes(_ context.Context, in *ec2.DescribeVolumesInput, _ ...func(*ec2.Options)) (*ec2.DescribeVolumesOutput, error) {
	var out []ec2types.Volume
	if len(in.VolumeIds) > 0 {
		for _, id := range in.VolumeIds {
			v, ok := f.vols[id]
			if !ok {
				return nil, volErr("InvalidVolume.NotFound")
			}
			out = append(out, *v)
		}
		return &ec2.DescribeVolumesOutput{Volumes: out}, nil
	}
	for i := 1; i <= f.n; i++ {
		v, ok := f.vols["vol-"+strconv.Itoa(i)]
		if !ok {
			continue
		}
		match := true
		for _, fl := range in.Filters {
			name := aws.ToString(fl.Name)
			switch {
			case name == "tag-key":
				_, match = tagValue(v.Tags, fl.Values[0])
			case len(name) > 4 && name[:4] == "tag:":
				val, _ := tagValue(v.Tags, name[4:])
				match = match && val == fl.Values[0]
			}
		}
		if match {
			out = append(out, *v)
		}
	}
	return &ec2.DescribeVolumesOutput{Volumes: out}, nil
}

func (f *fakeEC2) CreateVolume(_ context.Context, in *ec2.CreateVolumeInput, _ ...func(*ec2.Options)) (*ec2.CreateVolumeOutput, error) {
	f.n++
	id := "vol-" + strconv.Itoa(f.n)
	f.vols[id] = &ec2types.Volume{VolumeId: aws.String(id), AvailabilityZone: in.AvailabilityZone, Size: in.Size,
		VolumeType: in.VolumeType, Iops: in.Iops, Throughput: in.Throughput, State: ec2types.VolumeStateAvailable,
		Tags: in.TagSpecifications[0].Tags, CreateTime: aws.Time(time.Now())}
	return &ec2.CreateVolumeOutput{VolumeId: aws.String(id)}, nil
}

func (f *fakeEC2) AttachVolume(_ context.Context, in *ec2.AttachVolumeInput, _ ...func(*ec2.Options)) (*ec2.AttachVolumeOutput, error) {
	v := f.vols[aws.ToString(in.VolumeId)]
	if v.State != ec2types.VolumeStateAvailable {
		return nil, volErr("VolumeInUse")
	}
	v.State = ec2types.VolumeStateInUse
	v.Attachments = []ec2types.VolumeAttachment{{InstanceId: in.InstanceId, Device: in.Device, State: ec2types.VolumeAttachmentStateAttached}}
	return &ec2.AttachVolumeOutput{}, nil
}

func (f *fakeEC2) DetachVolume(_ context.Context, in *ec2.DetachVolumeInput, _ ...func(*ec2.Options)) (*ec2.DetachVolumeOutput, error) {
	v := f.vols[aws.ToString(in.VolumeId)]
	v.State, v.Attachments = ec2types.VolumeStateAvailable, nil
	return &ec2.DetachVolumeOutput{}, nil
}

func (f *fakeEC2) ModifyVolume(_ context.Context, in *ec2.ModifyVolumeInput, _ ...func(*ec2.Options)) (*ec2.ModifyVolumeOutput, error) {
	f.modified++
	f.vols[aws.ToString(in.VolumeId)].Size = in.Size
	return &ec2.ModifyVolumeOutput{}, nil
}

func (f *fakeEC2) DeleteVolume(_ context.Context, in *ec2.DeleteVolumeInput, _ ...func(*ec2.Options)) (*ec2.DeleteVolumeOutput, error) {
	v, ok := f.vols[aws.ToString(in.VolumeId)]
	if !ok {
		return nil, volErr("InvalidVolume.NotFound")
	}
	if v.State == ec2types.VolumeStateInUse {
		return nil, volErr("VolumeInUse")
	}
	delete(f.vols, aws.ToString(in.VolumeId))
	return &ec2.DeleteVolumeOutput{}, nil
}

func (f *fakeEC2) CreateTags(_ context.Context, in *ec2.CreateTagsInput, _ ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error) {
	v := f.vols[in.Resources[0]]
	for _, t := range in.Tags {
		f.untag(v, aws.ToString(t.Key))
		v.Tags = append(v.Tags, t)
	}
	return &ec2.CreateTagsOutput{}, nil
}

func (f *fakeEC2) DeleteTags(_ context.Context, in *ec2.DeleteTagsInput, _ ...func(*ec2.Options)) (*ec2.DeleteTagsOutput, error) {
	v := f.vols[in.Resources[0]]
	for _, t := range in.Tags {
		f.untag(v, aws.ToString(t.Key))
	}
	return &ec2.DeleteTagsOutput{}, nil
}

func (f *fakeEC2) untag(v *ec2types.Volume, k string) {
	var keep []ec2types.Tag
	for _, t := range v.Tags {
		if aws.ToString(t.Key) != k {
			keep = append(keep, t)
		}
	}
	v.Tags = keep
}

func TestVolumeLifecycle(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	e := &EC2{api: f}
	v, err := e.CreateVolume(ctx, provider.VolumeSpec{Owner: "pool", Name: "c1", Bytes: 100<<30 + 1, IOPS: 4000, Throughput: 250})
	if err != nil {
		t.Fatal(err)
	}
	if v.Zone != "eu-central-1a" || v.Bytes != 101<<30 || v.State != provider.VolumeAvailable || v.Kind != provider.VolumeCache || v.Name != "c1" {
		t.Fatalf("created %+v", v)
	}
	raw := f.vols[v.ID]
	if raw.VolumeType != ec2types.VolumeTypeGp3 || aws.ToInt32(raw.Iops) != 4000 || aws.ToInt32(raw.Throughput) != 250 {
		t.Fatalf("ebs parameters %+v", raw)
	}
	if _, err := e.AttachVolume(ctx, v.ID, "m2"); !errors.Is(err, provider.ErrVolumeZone) {
		t.Fatalf("cross-zone attach: %v", err)
	}
	v, err = e.AttachVolume(ctx, v.ID, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if v.State != provider.VolumeAttached || v.Machine != "m1" || v.Device != VolumeDevice(v.ID) {
		t.Fatalf("attached %+v", v)
	}
	if err := e.DeleteVolume(ctx, v.ID); !errors.Is(err, provider.ErrVolumeInUse) {
		t.Fatalf("delete attached: %v", err)
	}
	if err := e.ResizeVolume(ctx, v.ID, 50<<30); !errors.Is(err, provider.ErrVolumeShrink) {
		t.Fatalf("shrink: %v", err)
	}
	if err := e.ResizeVolume(ctx, v.ID, 200<<30); err != nil {
		t.Fatal(err)
	}
	if err := e.ResizeVolume(ctx, v.ID, 300<<30); !errors.Is(err, provider.ErrResizeCooldown) || f.modified != 1 {
		t.Fatalf("second resize within 6h: %v (%d modifications)", err, f.modified)
	}
	exp := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	if err := e.DetachVolume(ctx, v.ID, exp); err != nil {
		t.Fatal(err)
	}
	vs, err := e.ListVolumes(ctx, "pool")
	if err != nil || len(vs) != 1 {
		t.Fatalf("list: %v %+v", err, vs)
	}
	if got := vs[0]; got.State != provider.VolumeAvailable || got.Machine != "" || !got.ExpiresAt.Equal(exp) || got.Bytes != 200<<30 || got.ResizedAt.IsZero() {
		t.Fatalf("detached %+v", got)
	}
	if vs, _ := e.ListVolumes(ctx, "other"); len(vs) != 0 {
		t.Fatalf("other pool sees %+v", vs)
	}
	// Re-attaching clears the expiry.
	if v, err = e.AttachVolume(ctx, v.ID, "m1"); err != nil || !v.ExpiresAt.IsZero() {
		t.Fatalf("reattach %v %+v", err, v)
	}
	if err := e.DetachVolume(ctx, v.ID, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := e.DeleteVolume(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.DeleteVolume(ctx, v.ID); err != nil {
		t.Fatalf("deleting a deleted volume: %v", err)
	}
	if _, err := e.AttachVolume(ctx, v.ID, "m1"); !errors.Is(err, provider.ErrVolumeNotFound) {
		t.Fatalf("attach deleted: %v", err)
	}
}

func TestMachineZone(t *testing.T) {
	m := toMachine(newFake().instances[1])
	if m.Zone != "eu-central-1b" || m.ID != "m2" {
		t.Fatalf("%+v", m)
	}
}

func TestPlacementFollowsVolume(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	e := &EC2{api: f}
	v, err := e.CreateVolume(ctx, provider.VolumeSpec{Owner: "p", Name: "c", Zone: "eu-central-1b", Bytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	if s, err := e.placement(ctx, provider.MachineSpec{Volume: v.ID}); err != nil || s != "subnet-b" {
		t.Fatalf("subnet %q %v", s, err)
	}
	if _, err := e.placement(ctx, provider.MachineSpec{Volume: v.ID, Zone: "eu-central-1a"}); !errors.Is(err, provider.ErrVolumeZone) {
		t.Fatalf("conflicting zone: %v", err)
	}
	e.subnet = "subnet-a"
	if _, err := e.placement(ctx, provider.MachineSpec{Volume: v.ID}); !errors.Is(err, provider.ErrVolumeZone) {
		t.Fatalf("configured subnet in another zone: %v", err)
	}
	if s, err := e.placement(ctx, provider.MachineSpec{}); err != nil || s != "subnet-a" {
		t.Fatalf("no volume: %q %v", s, err)
	}
	if _, err := e.AttachVolume(ctx, v.ID, "m2"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.placement(ctx, provider.MachineSpec{Volume: v.ID}); !errors.Is(err, provider.ErrVolumeInUse) {
		t.Fatalf("attached volume: %v", err)
	}
}

func TestCacheSpec(t *testing.T) {
	c := cacheSpec(provider.MachineSpec{Volume: "vol-0ab", CacheTTL: time.Hour})
	if c.Medium != "volume" || c.Device != "/dev/disk/by-id/nvme-Amazon_Elastic_Block_Store_vol0ab" || c.TTLSeconds != 3600 {
		t.Fatalf("%+v", c)
	}
	if c := cacheSpec(provider.MachineSpec{}); c.Medium != "local" || c.LocalDevices != instanceStore {
		t.Fatalf("%+v", c)
	}
}
