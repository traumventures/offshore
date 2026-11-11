package awsec2

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"

	"github.com/traumventures/offshore/internal/provider"
)

// ec2API is the part of the EC2 client the volume operations use (fakes
// in tests).
type ec2API interface {
	ec2.DescribeInstancesAPIClient
	ec2.DescribeVolumesAPIClient
	DescribeSubnets(ctx context.Context, in *ec2.DescribeSubnetsInput, opts ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error)
	CreateVolume(ctx context.Context, in *ec2.CreateVolumeInput, opts ...func(*ec2.Options)) (*ec2.CreateVolumeOutput, error)
	AttachVolume(ctx context.Context, in *ec2.AttachVolumeInput, opts ...func(*ec2.Options)) (*ec2.AttachVolumeOutput, error)
	DetachVolume(ctx context.Context, in *ec2.DetachVolumeInput, opts ...func(*ec2.Options)) (*ec2.DetachVolumeOutput, error)
	ModifyVolume(ctx context.Context, in *ec2.ModifyVolumeInput, opts ...func(*ec2.Options)) (*ec2.ModifyVolumeOutput, error)
	DeleteVolume(ctx context.Context, in *ec2.DeleteVolumeInput, opts ...func(*ec2.Options)) (*ec2.DeleteVolumeOutput, error)
	CreateTags(ctx context.Context, in *ec2.CreateTagsInput, opts ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error)
	DeleteTags(ctx context.Context, in *ec2.DeleteTagsInput, opts ...func(*ec2.Options)) (*ec2.DeleteTagsOutput, error)
}

var _ provider.Volumes = (*EC2)(nil)

// Cache volumes attach as /dev/sdf; on Nitro instances the device is an
// NVMe namespace whose serial is the volume id without the dash, found
// by the supervisor under /dev/disk/by-id.
const volumeDevice = "/dev/sdf"

// VolumeDevice is the stable device path of an attached EBS volume on a
// Nitro instance.
func VolumeDevice(volumeID string) string {
	return "/dev/disk/by-id/nvme-Amazon_Elastic_Block_Store_" + strings.ReplaceAll(volumeID, "-", "")
}

// volumeWait bounds the waits for volume state changes.
var volumeWait = 5 * time.Minute

// VolumeCaps implements provider.Volumes: gp3 volumes of 1 GiB..16 TiB,
// one modification per six hours, attachable to running or stopped
// instances in the same availability zone.
func (e *EC2) VolumeCaps() provider.VolumeCaps {
	return provider.VolumeCaps{MinBytes: 1 << 30, MaxBytes: 16 << 40, ResizeCooldown: 6 * time.Hour,
		HotAttach: true, Zonal: true, Bounded: true}
}

func tagList(l map[string]string) []ec2types.Tag {
	tags := []ec2types.Tag{{Key: aws.String(tagProject), Value: aws.String(project)}}
	for _, k := range provider.SortedKeys(l) {
		tags = append(tags, ec2types.Tag{Key: aws.String(k), Value: aws.String(l[k])})
	}
	return tags
}

// defaultZone is the zone of the configured subnet, else the first zone
// with a default subnet in the default VPC.
func (e *EC2) defaultZone(ctx context.Context) (string, error) {
	in := &ec2.DescribeSubnetsInput{}
	if e.subnet != "" {
		in.SubnetIds = []string{e.subnet}
	} else {
		in.Filters = []ec2types.Filter{{Name: aws.String("default-for-az"), Values: []string{"true"}}}
	}
	out, err := e.api.DescribeSubnets(ctx, in)
	if err != nil {
		return "", fmt.Errorf("aws-ec2: describe subnets: %w", err)
	}
	var zones []string
	for _, s := range out.Subnets {
		zones = append(zones, aws.ToString(s.AvailabilityZone))
	}
	if len(zones) == 0 {
		return "", errors.New("aws-ec2: no subnet to place volumes in; set the subnet option")
	}
	sort.Strings(zones)
	return zones[0], nil
}

// placement returns the subnet of a new machine (empty: the default
// VPC's default subnet chosen by EC2). With a cache volume the machine
// goes to the volume's zone, else to spec.Zone: the configured subnet if
// it is in that zone, otherwise the zone's default subnet.
func (e *EC2) placement(ctx context.Context, spec provider.MachineSpec) (string, error) {
	zone := spec.Zone
	if spec.Volume != "" {
		v, err := e.volume(ctx, spec.Volume)
		if err != nil {
			return "", err
		}
		if v.State != provider.VolumeAvailable {
			return "", fmt.Errorf("aws-ec2: volume %s: %w (%s)", spec.Volume, provider.ErrVolumeInUse, v.State)
		}
		if zone != "" && zone != v.Zone {
			return "", fmt.Errorf("aws-ec2: machine in %s, volume %s in %s: %w", zone, spec.Volume, v.Zone, provider.ErrVolumeZone)
		}
		zone = v.Zone
	}
	if zone == "" {
		return e.subnet, nil
	}
	in := &ec2.DescribeSubnetsInput{}
	if e.subnet != "" {
		in.SubnetIds = []string{e.subnet}
	} else {
		in.Filters = []ec2types.Filter{
			{Name: aws.String("default-for-az"), Values: []string{"true"}},
			{Name: aws.String("availability-zone"), Values: []string{zone}},
		}
	}
	out, err := e.api.DescribeSubnets(ctx, in)
	if err != nil {
		return "", fmt.Errorf("aws-ec2: describe subnets: %w", err)
	}
	for _, s := range out.Subnets {
		if aws.ToString(s.AvailabilityZone) == zone {
			return aws.ToString(s.SubnetId), nil
		}
	}
	if e.subnet != "" {
		return "", fmt.Errorf("aws-ec2: subnet %s is not in zone %s: %w", e.subnet, zone, provider.ErrVolumeZone)
	}
	return "", fmt.Errorf("aws-ec2: no default subnet in zone %s; set the subnet option", zone)
}

// CreateVolume implements provider.Volumes: an encrypted EBS volume
// (gp3 unless spec.Type says otherwise), tagged with its labels.
func (e *EC2) CreateVolume(ctx context.Context, spec provider.VolumeSpec) (provider.Volume, error) {
	zone := spec.Zone
	if zone == "" {
		z, err := e.defaultZone(ctx)
		if err != nil {
			return provider.Volume{}, err
		}
		zone = z
	}
	gb := provider.GiB(spec.Bytes)
	if gb < 1 {
		gb = 1
	}
	vt := ec2types.VolumeTypeGp3
	if spec.Type != "" {
		vt = ec2types.VolumeType(spec.Type)
	}
	l := provider.VolumeLabels(spec, time.Now())
	l["Name"] = "offshore-" + spec.Name
	in := &ec2.CreateVolumeInput{
		AvailabilityZone: aws.String(zone), Size: aws.Int32(int32(gb)), VolumeType: vt, Encrypted: aws.Bool(true),
		TagSpecifications: []ec2types.TagSpecification{{ResourceType: ec2types.ResourceTypeVolume, Tags: tagList(l)}},
	}
	if spec.IOPS > 0 {
		in.Iops = aws.Int32(int32(spec.IOPS))
	}
	if spec.Throughput > 0 {
		in.Throughput = aws.Int32(int32(spec.Throughput))
	}
	out, err := e.api.CreateVolume(ctx, in)
	if err != nil {
		return provider.Volume{}, fmt.Errorf("aws-ec2: create volume %s: %w", spec.Name, err)
	}
	id := aws.ToString(out.VolumeId)
	if err := ec2.NewVolumeAvailableWaiter(e.api).Wait(ctx, &ec2.DescribeVolumesInput{VolumeIds: []string{id}}, volumeWait); err != nil {
		return provider.Volume{}, fmt.Errorf("aws-ec2: volume %s not available: %w", id, err)
	}
	return e.volume(ctx, id)
}

// AttachVolume implements provider.Volumes.
func (e *EC2) AttachVolume(ctx context.Context, id, machineID string) (provider.Volume, error) {
	v, err := e.volume(ctx, id)
	if err != nil {
		return provider.Volume{}, err
	}
	is, err := e.instances(ctx, "", machineID)
	if err != nil {
		return provider.Volume{}, err
	}
	if len(is) == 0 {
		return provider.Volume{}, fmt.Errorf("aws-ec2: machine %s not found", machineID)
	}
	inst := is[0]
	if az := aws.ToString(inst.Placement.AvailabilityZone); az != v.Zone {
		return provider.Volume{}, fmt.Errorf("aws-ec2: attach %s (%s) to %s (%s): %w", id, v.Zone, machineID, az, provider.ErrVolumeZone)
	}
	if v.State == provider.VolumeAttached && v.Machine == machineID {
		return v, nil
	}
	if v.State != provider.VolumeAvailable {
		return provider.Volume{}, fmt.Errorf("aws-ec2: attach %s: %w (%s)", id, provider.ErrVolumeInUse, v.State)
	}
	if _, err := e.api.AttachVolume(ctx, &ec2.AttachVolumeInput{VolumeId: aws.String(id), InstanceId: inst.InstanceId, Device: aws.String(volumeDevice)}); err != nil {
		return provider.Volume{}, fmt.Errorf("aws-ec2: attach %s to %s: %w", id, machineID, err)
	}
	if err := e.setTags(ctx, id, map[string]string{provider.LabelMachine: machineID}, provider.LabelExpiresAt); err != nil {
		return provider.Volume{}, err
	}
	if err := ec2.NewVolumeInUseWaiter(e.api).Wait(ctx, &ec2.DescribeVolumesInput{VolumeIds: []string{id}}, volumeWait); err != nil {
		return provider.Volume{}, fmt.Errorf("aws-ec2: volume %s not attached: %w", id, err)
	}
	return e.volume(ctx, id)
}

// DetachVolume implements provider.Volumes.
func (e *EC2) DetachVolume(ctx context.Context, id string, expiresAt time.Time) error {
	v, err := e.volume(ctx, id)
	if err != nil {
		return err
	}
	if v.State != provider.VolumeAvailable {
		if _, err := e.api.DetachVolume(ctx, &ec2.DetachVolumeInput{VolumeId: aws.String(id)}); err != nil && !apiCode(err, "IncorrectState") {
			return fmt.Errorf("aws-ec2: detach %s: %w", id, err)
		}
		if err := ec2.NewVolumeAvailableWaiter(e.api).Wait(ctx, &ec2.DescribeVolumesInput{VolumeIds: []string{id}}, volumeWait); err != nil {
			return fmt.Errorf("aws-ec2: volume %s not detached: %w", id, err)
		}
	}
	set := map[string]string{}
	del := []string{provider.LabelMachine}
	if expiresAt.IsZero() {
		del = append(del, provider.LabelExpiresAt)
	} else {
		set[provider.LabelExpiresAt] = provider.UnixLabel(expiresAt)
	}
	return e.setTags(ctx, id, set, del...)
}

// ResizeVolume implements provider.Volumes (ModifyVolume, at most once
// per six hours per volume).
func (e *EC2) ResizeVolume(ctx context.Context, id string, bytes int64) error {
	v, err := e.volume(ctx, id)
	if err != nil {
		return err
	}
	gb := provider.GiB(bytes)
	if err := provider.CheckResize(e.VolumeCaps(), v, gb<<30, time.Now()); err != nil {
		return fmt.Errorf("aws-ec2: resize %s to %d GiB: %w", id, gb, err)
	}
	if _, err := e.api.ModifyVolume(ctx, &ec2.ModifyVolumeInput{VolumeId: aws.String(id), Size: aws.Int32(int32(gb))}); err != nil {
		if apiCode(err, "VolumeModificationRateExceeded") {
			err = fmt.Errorf("%w: %v", provider.ErrResizeCooldown, err)
		}
		return fmt.Errorf("aws-ec2: resize %s to %d GiB: %w", id, gb, err)
	}
	return e.setTags(ctx, id, map[string]string{provider.LabelResizedAt: provider.UnixLabel(time.Now())})
}

// DeleteVolume implements provider.Volumes.
func (e *EC2) DeleteVolume(ctx context.Context, id string) error {
	_, err := e.api.DeleteVolume(ctx, &ec2.DeleteVolumeInput{VolumeId: aws.String(id)})
	switch {
	case err == nil, apiCode(err, "InvalidVolume.NotFound"):
		return nil
	case apiCode(err, "VolumeInUse"):
		return fmt.Errorf("aws-ec2: delete %s: %w", id, provider.ErrVolumeInUse)
	}
	return fmt.Errorf("aws-ec2: delete %s: %w", id, err)
}

// ListVolumes implements provider.Volumes: offshore volumes of owner
// (root disks of instances are not volumes).
func (e *EC2) ListVolumes(ctx context.Context, owner string) ([]provider.Volume, error) {
	filters := []ec2types.Filter{{Name: aws.String("tag-key"), Values: []string{provider.LabelVolumeName}}}
	if owner != "" {
		filters = append(filters, ec2types.Filter{Name: aws.String("tag:" + provider.LabelPool), Values: []string{owner}})
	}
	return e.describeVolumes(ctx, &ec2.DescribeVolumesInput{Filters: filters})
}

func (e *EC2) volume(ctx context.Context, id string) (provider.Volume, error) {
	vs, err := e.describeVolumes(ctx, &ec2.DescribeVolumesInput{VolumeIds: []string{id}})
	if err != nil {
		if apiCode(err, "InvalidVolume.NotFound") {
			return provider.Volume{}, fmt.Errorf("aws-ec2: %s: %w", id, provider.ErrVolumeNotFound)
		}
		return provider.Volume{}, err
	}
	if len(vs) == 0 {
		return provider.Volume{}, fmt.Errorf("aws-ec2: %s: %w", id, provider.ErrVolumeNotFound)
	}
	return vs[0], nil
}

func (e *EC2) describeVolumes(ctx context.Context, in *ec2.DescribeVolumesInput) ([]provider.Volume, error) {
	var out []provider.Volume
	p := ec2.NewDescribeVolumesPaginator(e.api, in)
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("aws-ec2: describe volumes: %w", err)
		}
		for _, v := range page.Volumes {
			out = append(out, toVolume(v))
		}
	}
	return out, nil
}

func toVolume(v ec2types.Volume) provider.Volume {
	l := map[string]string{}
	for _, t := range v.Tags {
		l[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	pv := provider.Volume{ID: aws.ToString(v.VolumeId), Zone: aws.ToString(v.AvailabilityZone),
		Bytes: int64(aws.ToInt32(v.Size)) << 30, Labels: l, CreatedAt: aws.ToTime(v.CreateTime)}
	provider.VolumeFromLabels(&pv)
	switch v.State {
	case ec2types.VolumeStateCreating:
		pv.State = provider.VolumeCreating
	case ec2types.VolumeStateAvailable:
		pv.State = provider.VolumeAvailable
	case ec2types.VolumeStateInUse:
		pv.State = provider.VolumeBusy
		for _, a := range v.Attachments {
			if a.State == ec2types.VolumeAttachmentStateAttached {
				pv.State = provider.VolumeAttached
				pv.Machine = l[provider.LabelMachine]
				pv.Device = VolumeDevice(pv.ID)
			}
		}
	case ec2types.VolumeStateError:
		pv.State = provider.VolumeFailed
	default:
		pv.State = provider.VolumeBusy
	}
	return pv
}

// setTags sets and deletes tags of a resource.
func (e *EC2) setTags(ctx context.Context, id string, set map[string]string, del ...string) error {
	if len(set) > 0 {
		var tags []ec2types.Tag
		for _, k := range provider.SortedKeys(set) {
			tags = append(tags, ec2types.Tag{Key: aws.String(k), Value: aws.String(set[k])})
		}
		if _, err := e.api.CreateTags(ctx, &ec2.CreateTagsInput{Resources: []string{id}, Tags: tags}); err != nil {
			return fmt.Errorf("aws-ec2: tag %s: %w", id, err)
		}
	}
	if len(del) > 0 {
		var tags []ec2types.Tag
		for _, k := range del {
			tags = append(tags, ec2types.Tag{Key: aws.String(k)})
		}
		if _, err := e.api.DeleteTags(ctx, &ec2.DeleteTagsInput{Resources: []string{id}, Tags: tags}); err != nil {
			return fmt.Errorf("aws-ec2: untag %s: %w", id, err)
		}
	}
	return nil
}

func apiCode(err error, code string) bool {
	var ae smithy.APIError
	return errors.As(err, &ae) && ae.ErrorCode() == code
}
