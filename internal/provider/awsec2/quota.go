package awsec2

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/servicequotas"

	"github.com/traumventures/offshore/internal/provider"
)

// standardQuota is the Service Quotas code of "Running On-Demand Standard
// (A, C, D, H, I, M, R, T, Z) instances", in vCPUs per region.
const standardQuota = "L-1216C47A"

type quotaAPI interface {
	GetServiceQuota(ctx context.Context, in *servicequotas.GetServiceQuotaInput, opts ...func(*servicequotas.Options)) (*servicequotas.GetServiceQuotaOutput, error)
}

var _ provider.Quota = (*EC2)(nil)

// Quota implements provider.Quota: the region's On-Demand standard vCPU
// quota minus the vCPUs of the account's other running and pending
// On-Demand standard instances (the pool's own, own vCPUs, are what the
// pool may hold). It needs servicequotas:GetServiceQuota and
// ec2:DescribeInstances.
func (e *EC2) Quota(ctx context.Context, _ string, own float64) (float64, bool, error) {
	if e.quotas == nil {
		e.quotas = servicequotas.NewFromConfig(e.cfg)
	}
	return accountQuota(ctx, e.quotas, e.api, own)
}

func accountQuota(ctx context.Context, q quotaAPI, api ec2.DescribeInstancesAPIClient, own float64) (float64, bool, error) {
	out, err := q.GetServiceQuota(ctx, &servicequotas.GetServiceQuotaInput{ServiceCode: aws.String("ec2"), QuotaCode: aws.String(standardQuota)})
	if err != nil {
		return 0, false, fmt.Errorf("aws-ec2: vCPU quota: %w", err)
	}
	if out.Quota == nil || out.Quota.Value == nil {
		return 0, false, nil
	}
	used := 0.0
	p := ec2.NewDescribeInstancesPaginator(api, &ec2.DescribeInstancesInput{
		Filters: []types.Filter{{Name: aws.String("instance-state-name"), Values: []string{"pending", "running"}}},
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return 0, false, fmt.Errorf("aws-ec2: vCPU usage: %w", err)
		}
		for _, r := range page.Reservations {
			for _, i := range r.Instances {
				if i.InstanceLifecycle == types.InstanceLifecycleTypeSpot || !standardFamily(string(i.InstanceType)) {
					continue
				}
				used += vcpus(i)
			}
		}
	}
	return *out.Quota.Value - (used - own), true, nil
}

// standardFamily reports whether an instance type counts against the
// standard On-Demand quota (families A, C, D, H, I, M, R, T, Z).
func standardFamily(t string) bool {
	return t != "" && strings.ContainsRune("acdhimrtz", rune(t[0])) && !strings.HasPrefix(t, "dl") && !strings.HasPrefix(t, "inf")
}

func vcpus(i types.Instance) float64 {
	if c := i.CpuOptions; c != nil && c.CoreCount != nil {
		threads := int32(1)
		if c.ThreadsPerCore != nil {
			threads = *c.ThreadsPerCore
		}
		return float64(*c.CoreCount * threads)
	}
	return 0
}
