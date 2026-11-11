package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/traumventures/offshore/internal/daemon"
	"github.com/traumventures/offshore/internal/orchestrator"
)

// providerCmd lists provider health or resets a disabled provider.
func providerCmd(args []string) error {
	sub := "list"
	if len(args) > 0 {
		sub = args[0]
	}
	ctx := context.Background()
	if err := daemon.EnsureDaemon(ctx); err != nil {
		return err
	}
	switch {
	case sub == "list" && len(args) <= 1:
		ps, err := fetchProviders(ctx)
		if err != nil {
			return err
		}
		printProviders(os.Stdout, ps, time.Now())
		return nil
	case sub == "reset" && len(args) == 2:
		if _, err := daemon.Call(ctx, daemon.Request{Op: "provider-reset", Provider: args[1]}, os.Stderr); err != nil {
			return err
		}
		fmt.Printf("provider %s re-enabled\n", args[1])
		return nil
	}
	return errors.New("usage: offshore provider [list] | reset <name>")
}

func fetchProviders(ctx context.Context) ([]orchestrator.ProviderHealth, error) {
	raw, err := daemon.Call(ctx, daemon.Request{Op: "providers"}, nil)
	if err != nil {
		return nil, err
	}
	var ps []orchestrator.ProviderHealth
	err = json.Unmarshal(raw, &ps)
	return ps, err
}

// printProviders renders provider health; disabled providers are loud.
func printProviders(w io.Writer, ps []orchestrator.ProviderHealth, now time.Time) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PROVIDER\tSTATE\tBOOT\tSTART\tDETAIL")
	for _, p := range ps {
		state, detail := p.State, ""
		switch p.State {
		case "disabled":
			state = "DISABLED"
			detail = fmt.Sprintf("%s since %s: %s (fix the account/credentials, then `offshore provider reset %s` or reload the config)",
				p.ErrorClass, p.Since.Local().Format("Jan 2 15:04"), trim(p.Error, 160), p.Name)
		case "cooldown":
			detail = "no new machines for " + p.Until.Sub(now).Round(time.Second).String()
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", p.Name, state, secsOrDash(p.BootSeconds), secsOrDash(p.StartSeconds), detail)
	}
	tw.Flush()
}

// providerWarnings returns one line per provider that gets no machines.
func providerWarnings(ps []orchestrator.ProviderHealth, now time.Time) []string {
	var out []string
	for _, p := range ps {
		switch p.State {
		case "disabled":
			out = append(out, fmt.Sprintf("PROVIDER %s DISABLED (%s): %s; `offshore provider reset %s` after fixing it", p.Name, p.ErrorClass, trim(p.Error, 160), p.Name))
		case "cooldown":
			out = append(out, fmt.Sprintf("provider %s cooling down for %s (capacity or quota)", p.Name, p.Until.Sub(now).Round(time.Second)))
		}
	}
	return out
}

func secsOrDash(s float64) string {
	if s <= 0 {
		return "-"
	}
	return (time.Duration(s * float64(time.Second))).Round(time.Second).String()
}

func trim(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// checkProviders is the doctor check of provider health.
func checkProviders(ps []orchestrator.ProviderHealth, err error, now time.Time) check {
	c := check{Name: "providers", Status: checkOK}
	if err != nil {
		c.Status, c.Detail = checkSkip, "provider health unavailable: "+err.Error()
		return c
	}
	warn := providerWarnings(ps, now)
	c.Detail = fmt.Sprintf("%d providers", len(ps))
	for _, p := range ps {
		if p.State == "disabled" {
			c.Status, c.Hint = checkFail, "a disabled provider gets no machines until its account/credentials are fixed and it is reset (offshore provider reset NAME) or the config reloaded"
		} else if p.State == "cooldown" && c.Status == checkOK {
			c.Status = checkWarn
		}
	}
	if len(warn) > 0 {
		c.Detail = strings.Join(warn, "; ")
	}
	return c
}
