package awsec2

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"

	"github.com/traumventures/cloudsourced/internal/provider"
)

func spotSpec() provider.MachineSpec {
	return provider.MachineSpec{ID: "m1", Size: "x4", InstanceTypes: []string{"c7a.xlarge", "c6a.xlarge", "m7a.xlarge"},
		Spot: true, SpotFallback: true, ExpiresAt: time.Unix(2_000_000_000, 0)}
}

func TestLaunchPlan(t *testing.T) {
	got := func(s provider.MachineSpec) string {
		var out []string
		for _, a := range launchPlan(s) {
			out = append(out, a.String())
		}
		return strings.Join(out, ", ")
	}
	s := spotSpec()
	if g, w := got(s), "c7a.xlarge (spot), c6a.xlarge (spot), m7a.xlarge (spot), c7a.xlarge (on-demand), c6a.xlarge (on-demand), m7a.xlarge (on-demand)"; g != w {
		t.Errorf("spot with fallback:\n got %s\nwant %s", g, w)
	}
	s.SpotFallback = false
	if g, w := got(s), "c7a.xlarge (spot), c6a.xlarge (spot), m7a.xlarge (spot)"; g != w {
		t.Errorf("spot without fallback: got %s", g)
	}
	// Plain sizes keep launching the size name on demand.
	if g := got(provider.MachineSpec{Size: "c7a.xlarge"}); g != "c7a.xlarge (on-demand)" {
		t.Errorf("on-demand: got %s", g)
	}
}

func baseInput() *ec2.RunInstancesInput {
	tags := []ec2types.Tag{{Key: aws.String("project"), Value: aws.String("cloudsourced")}}
	return &ec2.RunInstancesInput{TagSpecifications: []ec2types.TagSpecification{
		{ResourceType: ec2types.ResourceTypeInstance, Tags: tags},
		{ResourceType: ec2types.ResourceTypeVolume, Tags: tags},
	}}
}

func tagValue(tags []ec2types.Tag, key string) string {
	for _, t := range tags {
		if aws.ToString(t.Key) == key {
			return aws.ToString(t.Value)
		}
	}
	return ""
}

func TestSpotRequest(t *testing.T) {
	in := baseInput()
	spec := spotSpec()
	applyAttempt(in, attempt{instanceType: "c6a.xlarge", spot: true}, spec, "0.05", time.Unix(1_900_000_000, 0))
	if in.InstanceType != "c6a.xlarge" {
		t.Fatalf("instance type %s", in.InstanceType)
	}
	mo := in.InstanceMarketOptions
	if mo == nil || mo.MarketType != ec2types.MarketTypeSpot || mo.SpotOptions == nil {
		t.Fatalf("no spot market options: %+v", mo)
	}
	so := mo.SpotOptions
	if so.SpotInstanceType != ec2types.SpotInstanceTypePersistent || so.InstanceInterruptionBehavior != ec2types.InstanceInterruptionBehaviorStop {
		t.Errorf("want a persistent request that stops on interruption, got %s/%s", so.SpotInstanceType, so.InstanceInterruptionBehavior)
	}
	if so.ValidUntil == nil || !so.ValidUntil.Equal(spec.ExpiresAt) {
		t.Errorf("request must end at the machine expiry, got %v", so.ValidUntil)
	}
	if in.InstanceInitiatedShutdownBehavior != ec2types.ShutdownBehaviorStop {
		t.Errorf("EC2 rejects shutdown behaviour terminate with interruption behaviour stop, got %s", in.InstanceInitiatedShutdownBehavior)
	}
	if aws.ToString(so.MaxPrice) != "0.05" {
		t.Errorf("max price %v", so.MaxPrice)
	}
	var kinds []ec2types.ResourceType
	for _, ts := range in.TagSpecifications {
		kinds = append(kinds, ts.ResourceType)
		if tagValue(ts.Tags, provider.LabelMarket) != provider.MarketSpot || tagValue(ts.Tags, provider.LabelSpot) != "true" || tagValue(ts.Tags, "project") != "cloudsourced" {
			t.Errorf("%s tags %v", ts.ResourceType, ts.Tags)
		}
	}
	if len(kinds) != 3 || kinds[2] != ec2types.ResourceTypeSpotInstancesRequest {
		t.Errorf("spot request is not tagged: %v", kinds)
	}

	// The same input reused for an on-demand attempt drops all spot settings.
	applyAttempt(in, attempt{instanceType: "c7a.xlarge"}, spec, "0.05", time.Unix(1_900_000_000, 0))
	if in.InstanceMarketOptions != nil || in.InstanceInitiatedShutdownBehavior != ec2types.ShutdownBehaviorTerminate || len(in.TagSpecifications) != 2 || tagValue(in.TagSpecifications[0].Tags, provider.LabelMarket) != provider.MarketOnDemand {
		t.Errorf("on-demand attempt: %+v %v", in.InstanceMarketOptions, in.TagSpecifications)
	}
	// No ValidUntil for an expiry in the past (EC2 would reject it).
	spec.ExpiresAt = time.Unix(1_000, 0)
	applyAttempt(in, attempt{instanceType: "c7a.xlarge", spot: true}, spec, "", time.Unix(1_900_000_000, 0))
	if in.InstanceMarketOptions.SpotOptions.ValidUntil != nil || in.InstanceMarketOptions.SpotOptions.MaxPrice != nil {
		t.Errorf("unexpected spot options %+v", in.InstanceMarketOptions.SpotOptions)
	}
}

func apiErr(code string) error {
	return &smithy.GenericAPIError{Code: code, Message: code + " message"}
}

type fakeRun struct {
	errs  map[string]error // "type/market" -> error
	tried []string
}

func (f *fakeRun) run(_ context.Context, in *ec2.RunInstancesInput, _ ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
	market := provider.MarketOnDemand
	if in.InstanceMarketOptions != nil {
		market = provider.MarketSpot
	}
	k := string(in.InstanceType) + "/" + market
	f.tried = append(f.tried, k)
	if err := f.errs[k]; err != nil {
		return nil, err
	}
	return &ec2.RunInstancesOutput{Instances: []ec2types.Instance{{InstanceId: aws.String("i-" + k)}}}, nil
}

func TestLaunchFallsBackOnCapacityErrors(t *testing.T) {
	f := &fakeRun{errs: map[string]error{
		"c7a.xlarge/spot": apiErr("InsufficientInstanceCapacity"),
		"c6a.xlarge/spot": apiErr("SpotMaxPriceTooLow"),
	}}
	_, a, _, err := launch(context.Background(), f.run, baseInput(), spotSpec(), "", time.Now())
	if err != nil || a.String() != "m7a.xlarge (spot)" {
		t.Fatalf("want the third type on spot, got %v %v (tried %v)", a, err, f.tried)
	}

	// No spot capacity at all: on-demand, in the configured type order.
	f = &fakeRun{errs: map[string]error{
		"c7a.xlarge/spot":      apiErr("InsufficientInstanceCapacity"),
		"c6a.xlarge/spot":      apiErr("MaxSpotInstanceCountExceeded"),
		"m7a.xlarge/spot":      apiErr("InsufficientInstanceCapacity"),
		"c7a.xlarge/on-demand": apiErr("InsufficientInstanceCapacity"),
	}}
	out, a, failed, err := launch(context.Background(), f.run, baseInput(), spotSpec(), "", time.Now())
	if err != nil || a.String() != "c6a.xlarge (on-demand)" || aws.ToString(out.Instances[0].InstanceId) != "i-c6a.xlarge/on-demand" {
		t.Fatalf("want the on-demand fallback, got %v %v (tried %v)", a, err, f.tried)
	}
	if len(failed) != 4 || !strings.HasPrefix(failed[1], "c6a.xlarge (spot): MaxSpotInstanceCountExceeded") {
		t.Fatalf("failed attempts %q", failed)
	}

	// Without fallback the launch fails, naming every attempt.
	spec := spotSpec()
	spec.SpotFallback = false
	f = &fakeRun{errs: map[string]error{
		"c7a.xlarge/spot": apiErr("InsufficientInstanceCapacity"),
		"c6a.xlarge/spot": apiErr("InsufficientInstanceCapacity"),
		"m7a.xlarge/spot": apiErr("InsufficientInstanceCapacity"),
	}}
	_, _, _, err = launch(context.Background(), f.run, baseInput(), spec, "", time.Now())
	if err == nil || !strings.Contains(err.Error(), "no capacity for size x4") || len(f.tried) != 3 {
		t.Fatalf("want a capacity error after 3 attempts, got %v (tried %v)", err, f.tried)
	}
}

func TestLaunchStopsOnOtherErrors(t *testing.T) {
	f := &fakeRun{errs: map[string]error{"c7a.xlarge/spot": apiErr("UnauthorizedOperation")}}
	_, _, _, err := launch(context.Background(), f.run, baseInput(), spotSpec(), "", time.Now())
	if err == nil || len(f.tried) != 1 || !strings.Contains(err.Error(), "UnauthorizedOperation") {
		t.Fatalf("a permission error must not be retried: %v (tried %v)", err, f.tried)
	}
	if capacityError(&smithy.GenericAPIError{Code: "InvalidParameterCombination", Message: "The request with instanceInitiatedShutdownBehavior 'terminate' is not supported when instanceInterruptionBehavior is set to 'stop'."}) {
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

func TestSpotInterruptedMachineIsNotWarm(t *testing.T) {
	stopped := ec2types.Instance{InstanceId: aws.String("i-1"), State: &ec2types.InstanceState{Name: ec2types.InstanceStateNameStopped},
		InstanceLifecycle: ec2types.InstanceLifecycleTypeSpot, InstanceType: "c7a.xlarge",
		Tags: []ec2types.Tag{{Key: aws.String(provider.LabelMachine), Value: aws.String("m1")}}}
	stopped.StateReason = &ec2types.StateReason{Code: aws.String("Client.UserInitiatedShutdown")}
	if m := toMachine(stopped); m.State != provider.Stopped || m.Labels[provider.LabelMarket] != provider.MarketSpot || m.Labels["instance-type"] != "c7a.xlarge" {
		t.Fatalf("idle-stopped spot machine: %+v", m)
	}
	stopped.StateReason = &ec2types.StateReason{Code: aws.String("Server.SpotInstanceShutdown")}
	if m := toMachine(stopped); m.State != provider.Failed || m.Labels["interrupted"] != "spot" {
		t.Fatalf("interrupted spot machine must be replaced, got %+v", m)
	}
	ids, reqs := instanceIDs([]ec2types.Instance{{InstanceId: aws.String("i-1"), SpotInstanceRequestId: aws.String("sir-1")}, {InstanceId: aws.String("i-2")}})
	if strings.Join(ids, ",") != "i-1,i-2" || strings.Join(reqs, ",") != "sir-1" {
		t.Fatalf("ids %v requests %v", ids, reqs)
	}
}
