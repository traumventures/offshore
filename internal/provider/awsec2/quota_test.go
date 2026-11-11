package awsec2

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/servicequotas"
	sqtypes "github.com/aws/aws-sdk-go-v2/service/servicequotas/types"
)

type fakeQuota float64

func (f fakeQuota) GetServiceQuota(_ context.Context, in *servicequotas.GetServiceQuotaInput, _ ...func(*servicequotas.Options)) (*servicequotas.GetServiceQuotaOutput, error) {
	if aws.ToString(in.QuotaCode) != standardQuota {
		panic("wrong quota code")
	}
	return &servicequotas.GetServiceQuotaOutput{Quota: &sqtypes.ServiceQuota{Value: aws.Float64(float64(f))}}, nil
}

type fakeInstances []types.Instance

func (f fakeInstances) DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	return &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{Instances: f}}}, nil
}

func inst(t string, cores, threads int32, spot bool) types.Instance {
	i := types.Instance{InstanceType: types.InstanceType(t), CpuOptions: &types.CpuOptions{CoreCount: aws.Int32(cores), ThreadsPerCore: aws.Int32(threads)}}
	if spot {
		i.InstanceLifecycle = types.InstanceLifecycleTypeSpot
	}
	return i
}

// The pool may hold the quota minus the vCPUs of the account's other
// On-Demand standard instances (spot and other families do not count).
func TestAccountQuota(t *testing.T) {
	is := fakeInstances{
		inst("m8a.2xlarge", 8, 1, false),  // the pool's own machine
		inst("c7a.4xlarge", 16, 1, false), // another workload
		inst("c7a.xlarge", 4, 1, true),    // spot: own quota
		inst("g5.xlarge", 2, 2, false),    // G family: own quota
	}
	got, ok, err := accountQuota(context.Background(), fakeQuota(64), is, 8)
	if err != nil || !ok || got != 48 {
		t.Fatalf("got %v %v, want 64 - 16 = 48", got, err)
	}
	if got, _, _ := accountQuota(context.Background(), fakeQuota(8), is, 8); got != -8 {
		t.Fatalf("others exceed the quota: got %v, want -8", got)
	}
}
