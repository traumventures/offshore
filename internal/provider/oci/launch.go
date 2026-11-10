package oci

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/core"

	"github.com/traumventures/cloudsourced/internal/provider"
)

// attempt is one way to launch a machine: a shape, preemptible or not.
type attempt struct {
	shape string
	spot  bool // preemptible capacity
}

func (a attempt) market() string {
	if a.spot {
		return provider.MarketSpot
	}
	return provider.MarketOnDemand
}

func (a attempt) String() string { return a.shape + " (" + a.market() + ")" }

// launchPlan lists the attempts for a machine in order: every shape as a
// preemptible instance, then (spot fallback) every shape on regular
// capacity, as on aws-ec2.
func launchPlan(spec provider.MachineSpec) []attempt {
	var out []attempt
	if spec.Spot {
		for _, s := range shapes(spec) {
			out = append(out, attempt{shape: s, spot: true})
		}
		if !spec.SpotFallback {
			return out
		}
	}
	for _, s := range shapes(spec) {
		out = append(out, attempt{shape: s})
	}
	return out
}

// capacityError reports whether a LaunchInstance error means "no capacity
// for this shape right now" (or none allowed by the service limits), so
// another attempt may work.
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

// applyAttempt sets the shape and capacity type of a launch request.
// Preemptible instances terminate on preemption (OCI offers no stop
// action for them); their boot volume goes with them.
func applyAttempt(d *core.LaunchInstanceDetails, a attempt, spec provider.MachineSpec) {
	d.Shape = common.String(a.shape)
	d.ShapeConfig = shapeConfig(a.shape, spec.CPU, spec.Memory)
	d.PreemptibleInstanceConfig = nil
	if a.spot {
		d.PreemptibleInstanceConfig = &core.PreemptibleInstanceConfigDetails{
			PreemptionAction: core.TerminatePreemptionAction{PreserveBootVolume: common.Bool(false)},
		}
	}
	tags := make(map[string]string, len(d.FreeformTags)+2)
	for k, v := range d.FreeformTags {
		tags[k] = v
	}
	setLabel(tags, provider.LabelMarket, a.market())
	setLabel(tags, provider.LabelSpot, strconv.FormatBool(a.spot))
	d.FreeformTags = tags
}

type launchFunc func(ctx context.Context, req core.LaunchInstanceRequest) (core.LaunchInstanceResponse, error)

// launch runs the attempts of a machine in order until one gets capacity.
// Errors other than capacity errors end the launch at once. failed lists
// the capacity errors of earlier attempts.
func launch(ctx context.Context, run launchFunc, base core.LaunchInstanceDetails, spec provider.MachineSpec) (out core.Instance, a attempt, failed []string, err error) {
	for _, a := range launchPlan(spec) {
		d := base
		applyAttempt(&d, a, spec)
		resp, err := run(ctx, core.LaunchInstanceRequest{LaunchInstanceDetails: d})
		if err == nil {
			return resp.Instance, a, failed, nil
		}
		if !capacityError(err) {
			return core.Instance{}, a, failed, fmt.Errorf("oci: launch instance %s: %w", a, err)
		}
		failed = append(failed, fmt.Sprintf("%s: %s", a, apiError(err)))
	}
	return core.Instance{}, attempt{}, failed, fmt.Errorf("oci: no capacity for size %s (%s)", spec.Size, strings.Join(failed, "; "))
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
