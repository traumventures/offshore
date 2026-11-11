package oci

import (
	"context"
	"fmt"
	"strings"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/core"

	"github.com/traumventures/cloudsourced/internal/provider"
)

// launchPlan lists the shapes to try for a machine, in order, as on
// aws-ec2: any capacity error moves on to the next shape.
func launchPlan(spec provider.MachineSpec) []string { return shapes(spec) }

// capacityError reports whether a LaunchInstance error means "no capacity
// for this shape right now" (or none allowed by the service limits), so
// another shape may work.
func capacityError(err error) bool {
	se, ok := common.IsServiceError(err)
	if !ok {
		return false
	}
	m := strings.ToLower(se.GetMessage())
	switch {
	case strings.Contains(m, "out of host capacity"), strings.Contains(m, "out of capacity"):
		return true
	case se.GetCode() == "LimitExceeded", se.GetCode() == "QuotaExceeded":
		return true
	case se.GetCode() == "InvalidParameter" && strings.Contains(m, "shape") &&
		(strings.Contains(m, "not supported") || strings.Contains(m, "not available") || strings.Contains(m, "not valid")):
		// A shape not offered in the region or availability domain.
		return true
	}
	return false
}

// applyShape sets the shape of a launch request.
func applyShape(d *core.LaunchInstanceDetails, shape string, spec provider.MachineSpec) {
	d.Shape = common.String(shape)
	d.ShapeConfig = shapeConfig(shape, spec.CPU, spec.Memory)
}

type launchFunc func(ctx context.Context, req core.LaunchInstanceRequest) (core.LaunchInstanceResponse, error)

// launch runs the shapes of a machine in order until one gets capacity.
// Errors other than capacity errors end the launch at once. failed lists
// the capacity errors of earlier shapes.
func launch(ctx context.Context, run launchFunc, base core.LaunchInstanceDetails, spec provider.MachineSpec) (out core.Instance, shape string, failed []string, err error) {
	for _, s := range launchPlan(spec) {
		d := base
		applyShape(&d, s, spec)
		resp, err := run(ctx, core.LaunchInstanceRequest{LaunchInstanceDetails: d})
		if err == nil {
			return resp.Instance, s, failed, nil
		}
		if !capacityError(err) {
			return core.Instance{}, s, failed, fmt.Errorf("oci: launch instance %s: %w", s, err)
		}
		failed = append(failed, fmt.Sprintf("%s: %s", s, apiError(err)))
	}
	return core.Instance{}, "", failed, fmt.Errorf("oci: %w for size %s (%s)", provider.ErrCapacity, spec.Size, strings.Join(failed, "; "))
}

func apiError(err error) string {
	if se, ok := common.IsServiceError(err); ok {
		m := se.GetMessage()
		if len(m) > 160 {
			m = m[:160]
		}
		return se.GetCode() + " " + m
	}
	return err.Error()
}
