package awsec2

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"

	"github.com/traumventures/cloudsourced/internal/provider"
)

// launchPlan lists the instance types to try for a machine, in order.
// Trying the types in the configured order lets the operator rank them;
// any capacity error moves on to the next type, so the first type with
// capacity in the region wins.
func launchPlan(spec provider.MachineSpec) []string {
	if len(spec.InstanceTypes) > 0 {
		return spec.InstanceTypes
	}
	return []string{spec.Size}
}

// capacityError reports whether a RunInstances error means "no capacity
// for this type right now", so another type may work.
func capacityError(err error) bool {
	var ae smithy.APIError
	if !errors.As(err, &ae) {
		return false
	}
	switch ae.ErrorCode() {
	case "InsufficientInstanceCapacity", "InsufficientCapacity", "InsufficientHostCapacity",
		"InsufficientReservedInstanceCapacity", "Unsupported", "VcpuLimitExceeded", "InstanceLimitExceeded":
		return true
	case "InvalidParameterValue":
		// An instance type not offered in the region or zone (other
		// invalid parameters are configuration errors).
		m := strings.ToLower(ae.ErrorMessage())
		return strings.Contains(m, "instance type") && (strings.Contains(m, "not supported") || strings.Contains(m, "not available"))
	}
	return false
}

type runFunc func(ctx context.Context, in *ec2.RunInstancesInput, optFns ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error)

// launch runs the instance types of a machine in order until one gets
// capacity. Errors other than capacity errors end the launch at once.
// failed lists the capacity errors of earlier types.
func launch(ctx context.Context, run runFunc, base *ec2.RunInstancesInput, spec provider.MachineSpec) (out *ec2.RunInstancesOutput, instanceType string, failed []string, err error) {
	for _, t := range launchPlan(spec) {
		in := *base
		in.InstanceType = ec2types.InstanceType(t)
		out, err := run(ctx, &in)
		if err == nil {
			return out, t, failed, nil
		}
		if !capacityError(err) {
			return nil, t, failed, fmt.Errorf("aws-ec2: run instance %s: %w", t, err)
		}
		failed = append(failed, fmt.Sprintf("%s: %s", t, apiError(err)))
	}
	return nil, "", failed, fmt.Errorf("aws-ec2: %w for size %s (%s)", provider.ErrCapacity, spec.Size, strings.Join(failed, "; "))
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
