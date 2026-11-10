package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/traumventures/cloudsourced/internal/client"
)

// metricsCmd prints the daemon's metrics (Prometheus text format) together
// with those of its fleet: the embedded orchestrator and its machines, or
// the shared control plane in remote mode.
func metricsCmd(args []string) error {
	if len(args) > 0 {
		return errors.New("usage: cloudsourced metrics")
	}
	ctx := context.Background()
	if err := client.EnsureDaemon(ctx); err != nil {
		return err
	}
	raw, err := client.Call(ctx, client.Request{Op: "metrics"}, nil)
	if err != nil {
		return err
	}
	var m client.Metrics
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	fmt.Print(m.Text)
	return nil
}
