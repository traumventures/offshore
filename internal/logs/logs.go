// Package logs configures structured logging for the daemon, the control
// plane and workers from the environment:
//
//	OFFSHORE_LOG_FORMAT=text|json  (default text)
//	OFFSHORE_LOG_LEVEL=debug|info|warn|error  (default info;
//	                                  OFFSHORE_DEBUG=1 means debug)
//
// The orchestrator forwards both variables to the machines it creates, so
// a pool logs in one format.
package logs

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// Environment variables.
const (
	EnvFormat = "OFFSHORE_LOG_FORMAT"
	EnvLevel  = "OFFSHORE_LOG_LEVEL"
)

// Options is a parsed logging configuration.
type Options struct {
	JSON  bool
	Level slog.Level
}

// Parse reads the options from getenv. Invalid values are reported and
// replaced by the defaults.
func Parse(getenv func(string) string) (Options, error) {
	o := Options{Level: slog.LevelInfo}
	var errs []string
	switch f := strings.ToLower(strings.TrimSpace(getenv(EnvFormat))); f {
	case "", "text":
	case "json":
		o.JSON = true
	default:
		errs = append(errs, fmt.Sprintf("%s=%q: want text or json", EnvFormat, f))
	}
	if getenv("OFFSHORE_DEBUG") != "" {
		o.Level = slog.LevelDebug
	}
	if l := strings.TrimSpace(getenv(EnvLevel)); l != "" {
		var lv slog.Level
		if err := lv.UnmarshalText([]byte(l)); err != nil {
			errs = append(errs, fmt.Sprintf("%s=%q: want debug, info, warn or error", EnvLevel, l))
		} else {
			o.Level = lv
		}
	}
	if len(errs) > 0 {
		return o, fmt.Errorf("logging: %s", strings.Join(errs, "; "))
	}
	return o, nil
}

// New returns a logger writing to w.
func (o Options) New(w io.Writer) *slog.Logger {
	ho := &slog.HandlerOptions{Level: o.Level}
	if o.JSON {
		return slog.New(slog.NewJSONHandler(w, ho))
	}
	return slog.New(slog.NewTextHandler(w, ho))
}

// FromEnv returns a logger to stderr configured from the environment.
func FromEnv() *slog.Logger {
	o, err := Parse(os.Getenv)
	l := o.New(os.Stderr)
	if err != nil {
		l.Warn("invalid logging configuration, using defaults", "err", err)
	}
	return l
}

// Forward returns the logging variables set in this process, for the
// environment of child services (workers).
func Forward() map[string]string {
	out := map[string]string{}
	for _, k := range []string{EnvFormat, EnvLevel} {
		if v := os.Getenv(k); v != "" {
			out[k] = v
		}
	}
	return out
}
