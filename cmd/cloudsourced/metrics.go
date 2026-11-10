package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/traumventures/cloudsourced/internal/daemon"
)

// metricsCmd prints the daemon's metrics (Prometheus text format) together
// with those of its pool: the embedded orchestrator and its machines, or
// the shared control plane in remote mode.
func metricsCmd(args []string) error {
	if len(args) > 0 {
		return errors.New("usage: cloudsourced metrics")
	}
	ctx := context.Background()
	if err := daemon.EnsureDaemon(ctx); err != nil {
		return err
	}
	raw, err := daemon.Call(ctx, daemon.Request{Op: "metrics"}, nil)
	if err != nil {
		return err
	}
	var m daemon.Metrics
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	fmt.Print(m.Text)
	return nil
}
