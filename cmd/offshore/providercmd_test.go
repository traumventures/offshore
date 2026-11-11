package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/traumventures/offshore/internal/orchestrator"
)

func TestProviderHealthOutput(t *testing.T) {
	now := time.Now()
	ps := []orchestrator.ProviderHealth{
		{Name: "aws", State: "ok", BootSeconds: 75},
		{Name: "oci", State: "disabled", ErrorClass: "account_limit", Error: "LimitExceeded: please upgrade", Since: now},
		{Name: "gcp", State: "cooldown", Until: now.Add(2 * time.Minute)},
	}
	var b bytes.Buffer
	printProviders(&b, ps, now)
	if !strings.Contains(b.String(), "DISABLED") || !strings.Contains(b.String(), "offshore provider reset oci") {
		t.Fatal(b.String())
	}
	if w := providerWarnings(ps, now); len(w) != 2 || !strings.Contains(w[0], "PROVIDER oci DISABLED") {
		t.Fatal(w)
	}
	if c := checkProviders(ps, nil, now); c.Status != checkFail {
		t.Fatalf("%+v", c)
	}
	if c := checkProviders(ps[:1], nil, now); c.Status != checkOK {
		t.Fatalf("%+v", c)
	}
	if c := checkProviders(nil, errors.New("x"), now); c.Status != checkSkip {
		t.Fatalf("%+v", c)
	}
}
