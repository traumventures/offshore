package awsec2

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"

	"github.com/traumventures/cloudsourced/internal/provider"
)

// attempt is one way to launch a machine: an instance type on a market.
type attempt struct {
	instanceType string
	spot         bool
}

func (a attempt) market() string {
	if a.spot {
		return provider.MarketSpot
	}
	return provider.MarketOnDemand
}

func (a attempt) String() string { return a.instanceType + " (" + a.market() + ")" }

// launchPlan lists the attempts for a machine in order: every instance
// type on spot, then (spot fallback) every type on-demand. Trying the
// types in the configured order lets the operator rank them; any
// capacity error moves on to the next type, so the first type with
// capacity in the region wins.
func launchPlan(spec provider.MachineSpec) []attempt {
	types := spec.InstanceTypes
	if len(types) == 0 {
		types = []string{spec.Size}
	}
	var out []attempt
	if spec.Spot {
		for _, t := range types {
			out = append(out, attempt{instanceType: t, spot: true})
		}
		if !spec.SpotFallback {
			return out
		}
	}
	for _, t := range types {
		out = append(out, attempt{instanceType: t})
	}
	return out
}

// capacityError reports whether a RunInstances error means "no capacity
// for this type on this market right now", so another attempt may work.
func capacityError(err error) bool {
	var ae smithy.APIError
	if !errors.As(err, &ae) {
		return false
	}
	switch ae.ErrorCode() {
	case "InsufficientInstanceCapacity", "InsufficientCapacity", "InsufficientHostCapacity",
		"InsufficientReservedInstanceCapacity", "SpotMaxPriceTooLow", "MaxSpotInstanceCountExceeded",
		"Unsupported", "VcpuLimitExceeded", "InstanceLimitExceeded":
		return true
	case "InvalidParameterValue":
		// An instance type not offered in the region or zone (other
		// invalid parameters are configuration errors).
		m := strings.ToLower(ae.ErrorMessage())
		return strings.Contains(m, "instance type") && (strings.Contains(m, "not supported") || strings.Contains(m, "not available"))
	}
	return false
}

// applyAttempt sets the instance type and market of a launch request.
// Spot machines use a persistent request with interruption behaviour stop:
// only persistent requests may stop instead of terminate, and stopping
// keeps what makes a machine valuable, its disk with the warm caches, for
// idle stop and warm restart (one-time spot instances cannot be stopped at
// all). The request ends at the machine's expiry so that it never
// relaunches an instance beyond max_lifetime.
func applyAttempt(in *ec2.RunInstancesInput, a attempt, spec provider.MachineSpec, maxPrice string, now time.Time) {
	in.InstanceType = ec2types.InstanceType(a.instanceType)
	in.InstanceMarketOptions = nil
	in.InstanceInitiatedShutdownBehavior = ec2types.ShutdownBehaviorTerminate
	if a.spot {
		// EC2 rejects shutdown behaviour terminate with interruption
		// behaviour stop: a spot machine that shuts itself down at expiry
		// stops, and the orchestrator (or the reaper) terminates it as
		// expired.
		in.InstanceInitiatedShutdownBehavior = ec2types.ShutdownBehaviorStop
		so := &ec2types.SpotMarketOptions{
			SpotInstanceType:             ec2types.SpotInstanceTypePersistent,
			InstanceInterruptionBehavior: ec2types.InstanceInterruptionBehaviorStop,
		}
		if !spec.ExpiresAt.IsZero() && spec.ExpiresAt.After(now) {
			so.ValidUntil = aws.Time(spec.ExpiresAt.UTC())
		}
		if maxPrice != "" {
			so.MaxPrice = aws.String(maxPrice)
		}
		in.InstanceMarketOptions = &ec2types.InstanceMarketOptionsRequest{MarketType: ec2types.MarketTypeSpot, SpotOptions: so}
	}
	var specs []ec2types.TagSpecification
	for _, ts := range in.TagSpecifications {
		if ts.ResourceType == ec2types.ResourceTypeSpotInstancesRequest {
			continue
		}
		tags := withTag(ts.Tags, provider.LabelMarket, a.market())
		specs = append(specs, ec2types.TagSpecification{ResourceType: ts.ResourceType, Tags: tags})
	}
	if a.spot && len(specs) > 0 {
		specs = append(specs, ec2types.TagSpecification{ResourceType: ec2types.ResourceTypeSpotInstancesRequest, Tags: specs[0].Tags})
	}
	in.TagSpecifications = specs
}

func withTag(tags []ec2types.Tag, key, value string) []ec2types.Tag {
	out := make([]ec2types.Tag, 0, len(tags)+1)
	for _, t := range tags {
		if aws.ToString(t.Key) != key {
			out = append(out, t)
		}
	}
	return append(out, ec2types.Tag{Key: aws.String(key), Value: aws.String(value)})
}

type runFunc func(ctx context.Context, in *ec2.RunInstancesInput, optFns ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error)

// launch runs the attempts of a machine in order until one gets capacity.
// Errors other than capacity errors end the launch at once. failed lists
// the capacity errors of earlier attempts.
func launch(ctx context.Context, run runFunc, base *ec2.RunInstancesInput, spec provider.MachineSpec, maxPrice string, now time.Time) (out *ec2.RunInstancesOutput, a attempt, failed []string, err error) {
	for _, a := range launchPlan(spec) {
		in := *base
		applyAttempt(&in, a, spec, maxPrice, now)
		out, err := run(ctx, &in)
		if err == nil {
			return out, a, failed, nil
		}
		if !capacityError(err) {
			return nil, a, failed, fmt.Errorf("aws-ec2: run instance %s: %w", a, err)
		}
		failed = append(failed, fmt.Sprintf("%s: %s", a, apiError(err)))
	}
	return nil, attempt{}, failed, fmt.Errorf("aws-ec2: no capacity for size %s (%s)", spec.Size, strings.Join(failed, "; "))
}

func apiError(err error) string {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		m := ae.ErrorMessage()
		if len(m) > 160 {
			m = m[:160]
		}
		return ae.ErrorCode() + " " + m
	}
	return err.Error()
}

// spotInterrupted reports whether an instance was stopped by a spot
// interruption. Only EC2 can restart such an instance (when capacity
// returns, at a time of its choosing), so it is not a warm machine.
func spotInterrupted(i ec2types.Instance) bool {
	return i.StateReason != nil && aws.ToString(i.StateReason.Code) == "Server.SpotInstanceShutdown"
}
