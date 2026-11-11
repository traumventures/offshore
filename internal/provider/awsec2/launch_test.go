package awsec2

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"

	"github.com/traumventures/offshore/internal/provider"
)

func typedSpec() provider.MachineSpec {
	return provider.MachineSpec{ID: "m1", Size: "x4", InstanceTypes: []string{"c7a.xlarge", "c6a.xlarge", "m7a.xlarge"}}
}

func TestLaunchPlan(t *testing.T) {
	if g := strings.Join(launchPlan(typedSpec()), ", "); g != "c7a.xlarge, c6a.xlarge, m7a.xlarge" {
		t.Errorf("instance types: got %s", g)
	}
	// Plain sizes launch the size name.
	if g := strings.Join(launchPlan(provider.MachineSpec{Size: "c7a.xlarge"}), ", "); g != "c7a.xlarge" {
		t.Errorf("size name: got %s", g)
	}
}

func baseInput() *ec2.RunInstancesInput {
	tags := []ec2types.Tag{{Key: aws.String("project"), Value: aws.String("offshore")}}
	return &ec2.RunInstancesInput{
		InstanceInitiatedShutdownBehavior: ec2types.ShutdownBehaviorTerminate,
		TagSpecifications: []ec2types.TagSpecification{
			{ResourceType: ec2types.ResourceTypeInstance, Tags: tags},
			{ResourceType: ec2types.ResourceTypeVolume, Tags: tags},
		}}
}

func apiErr(code string) error {
	return &smithy.GenericAPIError{Code: code, Message: code + " message"}
}

type fakeRun struct {
	errs  map[string]error // instance type -> error
	tried []string
}

func (f *fakeRun) run(_ context.Context, in *ec2.RunInstancesInput, _ ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
	k := string(in.InstanceType)
	f.tried = append(f.tried, k)
	if in.InstanceMarketOptions != nil || in.InstanceInitiatedShutdownBehavior != ec2types.ShutdownBehaviorTerminate || len(in.TagSpecifications) != 2 {
		return nil, errors.New("unexpected launch request")
	}
	if err := f.errs[k]; err != nil {
		return nil, err
	}
	return &ec2.RunInstancesOutput{Instances: []ec2types.Instance{{InstanceId: aws.String("i-" + k)}}}, nil
}

func TestLaunchFallsBackOnCapacityErrors(t *testing.T) {
	f := &fakeRun{errs: map[string]error{
		"c7a.xlarge": apiErr("InsufficientInstanceCapacity"),
		"c6a.xlarge": apiErr("VcpuLimitExceeded"),
	}}
	out, typ, failed, err := launch(context.Background(), f.run, baseInput(), typedSpec())
	if err != nil || typ != "m7a.xlarge" || aws.ToString(out.Instances[0].InstanceId) != "i-m7a.xlarge" {
		t.Fatalf("want the third type, got %v %v (tried %v)", typ, err, f.tried)
	}
	if len(failed) != 2 || !strings.HasPrefix(failed[1], "c6a.xlarge: VcpuLimitExceeded") {
		t.Fatalf("failed attempts %q", failed)
	}

	// No capacity at all: the launch fails, naming every type.
	f = &fakeRun{errs: map[string]error{
		"c7a.xlarge": apiErr("InsufficientInstanceCapacity"),
		"c6a.xlarge": apiErr("InsufficientInstanceCapacity"),
		"m7a.xlarge": apiErr("InsufficientInstanceCapacity"),
	}}
	_, _, _, err = launch(context.Background(), f.run, baseInput(), typedSpec())
	if err == nil || !strings.Contains(err.Error(), "no machine of size x4") || provider.Classify(err) != provider.ClassCapacity || len(f.tried) != 3 {
		t.Fatalf("want a capacity error after 3 attempts, got %v (tried %v)", err, f.tried)
	}
}

func TestLaunchStopsOnOtherErrors(t *testing.T) {
	f := &fakeRun{errs: map[string]error{"c7a.xlarge": apiErr("UnauthorizedOperation")}}
	_, _, _, err := launch(context.Background(), f.run, baseInput(), typedSpec())
	if err == nil || len(f.tried) != 1 || !strings.Contains(err.Error(), "UnauthorizedOperation") {
		t.Fatalf("a permission error must not be retried: %v (tried %v)", err, f.tried)
	}
	if capacityError(&smithy.GenericAPIError{Code: "InvalidParameterCombination", Message: "invalid combination"}) {
		t.Error("an invalid request is not a capacity error")
	}
	if capacityError(errors.New("network down")) {
		t.Error("plain errors are not capacity errors")
	}
	if !capacityError(&smithy.GenericAPIError{Code: "Unsupported", Message: "The requested configuration is currently not supported."}) {
		t.Error("unsupported type in the zone is a capacity error")
	}
	if !capacityError(&smithy.GenericAPIError{Code: "InvalidParameterValue", Message: "The instance type 'c7a.xlarge' is not supported in your requested Availability Zone."}) {
		t.Error("type not offered is a capacity error")
	}
}

func TestStoppedMachineIsWarm(t *testing.T) {
	stopped := ec2types.Instance{InstanceId: aws.String("i-1"), State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
		InstanceType: "c7a.xlarge", Tags: []ec2types.Tag{{Key: aws.String(provider.LabelMachine), Value: aws.String("m1")}}}
	if m := toMachine(stopped); m.State != provider.Stopped || m.Labels["instance-type"] != "c7a.xlarge" {
		t.Fatalf("stopped machine: %+v", m)
	}
}

func TestSplitPartitionAndRootVolume(t *testing.T) {
	for dev, want := range map[string][2]string{
		"/dev/nvme0n1p1": {"/dev/nvme0n1", "1"},
		"/dev/xvda1":     {"/dev/xvda", "1"},
		"/dev/nvme0n1":   {"/dev/nvme0n1", ""},
		"/dev/mapper/x":  {"/dev/mapper/x", ""},
	} {
		d, p := splitPartition(dev)
		if d != want[0] || p != want[1] {
			t.Errorf("%s: %s %s", dev, d, p)
		}
	}
	i := ec2types.Instance{RootDeviceName: aws.String("/dev/xvda"), BlockDeviceMappings: []ec2types.InstanceBlockDeviceMapping{
		{DeviceName: aws.String("/dev/sdb"), Ebs: &ec2types.EbsInstanceBlockDevice{VolumeId: aws.String("vol-data")}},
		{DeviceName: aws.String("/dev/xvda"), Ebs: &ec2types.EbsInstanceBlockDevice{VolumeId: aws.String("vol-root")}},
	}}
	if rootVolume(i) != "vol-root" {
		t.Fatal(rootVolume(i))
	}
}

func TestGrowDisk(t *testing.T) {
	var calls []string
	defer func(f func(context.Context, string, ...string) ([]byte, error)) { growCmd = f }(growCmd)
	growCmd = func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		switch name {
		case "findmnt":
			return []byte("/dev/nvme0n1p1 xfs\n"), nil
		case "growpart":
			if len(calls) > 3 {
				return []byte("NOCHANGE: partition 1 is size 100"), errors.New("exit status 1")
			}
		}
		return nil, nil
	}
	h := &host{}
	if err := h.GrowDisk(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(calls, "; ") != "findmnt -n -o SOURCE,FSTYPE /; growpart /dev/nvme0n1 1; xfs_growfs -d /" {
		t.Fatalf("calls %v", calls)
	}
	calls = []string{"", "", ""} // the next growpart reports NOCHANGE
	if err := h.GrowDisk(context.Background()); err != nil || len(calls) != 5 {
		t.Fatalf("no change: %v %v", err, calls)
	}
}

func TestQUICSources(t *testing.T) {
	perms := []ec2types.IpPermission{
		{IpProtocol: aws.String("udp"), FromPort: aws.Int32(7443), ToPort: aws.Int32(7443),
			IpRanges: []ec2types.IpRange{{CidrIp: aws.String("1.2.3.4/32")}, {CidrIp: aws.String("10.0.0.0/8")}}},
		{IpProtocol: aws.String("tcp"), FromPort: aws.Int32(22), ToPort: aws.Int32(22),
			IpRanges: []ec2types.IpRange{{CidrIp: aws.String("0.0.0.0/0")}}},
	}
	got := quicSources(perms)
	if len(got) != 2 || got[0] != "1.2.3.4/32" || got[1] != "10.0.0.0/8" {
		t.Fatalf("quicSources = %v", got)
	}
}
