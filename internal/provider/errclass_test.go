package provider

import (
	"errors"
	"fmt"
	"testing"
)

func TestClassify(t *testing.T) {
	for msg, want := range map[string]ErrorClass{
		"oci: launch instance: Error returned by Compute Service. Http Status Code: 400. Error Code: LimitExceeded. Message: Out of service limit for VM.Standard.E4.Flex; please upgrade your account": ClassAccountLimit,
		"api error AuthFailure: AWS was not able to validate the provided access credentials":                                                                                                           ClassAuth,
		"VcpuLimitExceeded: You have requested more vCPU capacity than your current vCPU limit":                                                                                                         ClassQuota,
		"InsufficientInstanceCapacity: We currently do not have sufficient c7a.4xlarge capacity":                                                                                                        ClassCapacity,
		"RequestLimitExceeded: Request limit exceeded.":                                                                                                                                                 ClassTransient,
		"InvalidAMIID.NotFound": ClassOther,
	} {
		if got := ClassifyText(msg); got != want {
			t.Errorf("%q: %s, want %s", msg, got, want)
		}
	}
	if Classify(fmt.Errorf("x: %w", ErrCapacity)) != ClassCapacity || Classify(ClassError(ClassAuth, errors.New("e"))) != ClassAuth {
		t.Fatal("sentinels")
	}
}

func TestLaunchError(t *testing.T) {
	err := LaunchError("oci", "m4", []string{"A: LimitExceeded Out of limit, please upgrade", "B: out of host capacity"})
	if Classify(err) != ClassAccountLimit || !errors.Is(err, ErrAccountLimit) {
		t.Fatal(err)
	}
	if c := Classify(LaunchError("aws-ec2", "m4", []string{"a: VcpuLimitExceeded x", "b: InstanceLimitExceeded y"})); c != ClassQuota {
		t.Fatal(c)
	}
	if c := Classify(LaunchError("aws-ec2", "m4", []string{"a: VcpuLimitExceeded x", "b: InsufficientInstanceCapacity"})); c != ClassCapacity {
		t.Fatal(c)
	}
}
