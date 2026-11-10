package image

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
)

// CLI is a Docker CLI invocation, e.g. ["sudo", "-n", "docker"].
type CLI []string

func (c CLI) cmd() []string {
	if len(c) == 0 {
		return []string{"docker"}
	}
	return c
}

// Run runs a docker subcommand; stdout and stderr go to log, the tail of
// stderr is part of the error.
func (c CLI) Run(ctx context.Context, stdin io.Reader, log io.Writer, args ...string) error {
	cmdv := c.cmd()
	argv := append(append([]string{}, cmdv[1:]...), args...)
	cmd := exec.CommandContext(ctx, cmdv[0], argv...)
	cmd.Stdin = stdin
	if log == nil {
		log = io.Discard
	}
	var stderr bytes.Buffer
	cmd.Stdout = log
	cmd.Stderr = io.MultiWriter(log, &stderr)
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 2000 {
			msg = msg[len(msg)-2000:]
		}
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("docker %s: %s", args[0], msg)
	}
	return nil
}

// Exists reports whether the image store has ref.
func (c CLI) Exists(ctx context.Context, ref string) bool {
	return c.Run(ctx, nil, nil, "image", "inspect", "--format", "{{.Id}}", ref) == nil
}

// DockerBuilder builds with "docker build" into the CLI's daemon. Existing
// images are reused; Docker's layer cache makes rebuilds incremental.
type DockerBuilder struct {
	CLI CLI

	mu   sync.Mutex
	base map[string]*sync.Mutex // base tag -> build lock
}

func (b *DockerBuilder) ID() string { return "docker:" + strings.Join(b.CLI.cmd(), " ") }

func (b *DockerBuilder) lock(key string) func() {
	b.mu.Lock()
	if b.base == nil {
		b.base = map[string]*sync.Mutex{}
	}
	m, ok := b.base[key]
	if !ok {
		m = &sync.Mutex{}
		b.base[key] = m
	}
	b.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// Build builds spec for platform unless the image exists.
func (b *DockerBuilder) Build(ctx context.Context, spec Spec, p Platform, log io.Writer) (string, error) {
	ref := LocalRef(spec.Tag, p)
	if b.CLI.Exists(ctx, ref) {
		return ref, nil
	}
	plat := []string{}
	if p != "" {
		plat = []string{"--platform", string(p)}
	}
	args := append([]string{"build"}, plat...)
	if spec.BaseTag != "" {
		base := LocalRef(spec.BaseTag, p)
		unlock := b.lock(base)
		if !b.CLI.Exists(ctx, base) {
			fmt.Fprintf(log, "building base image %s\n", base)
			bargs := append(append([]string{"build"}, plat...), "-t", base, "-f", spec.BaseFile, spec.BaseDir)
			if err := b.CLI.Run(ctx, nil, log, bargs...); err != nil {
				unlock()
				return "", err
			}
		}
		unlock()
		if base != spec.BaseTag {
			args = append(args, "--build-arg", BaseArg+"="+base)
		}
	}
	fmt.Fprintf(log, "building environment image %s\n", ref)
	args = append(args, "-t", ref, "-f", spec.Dockerfile, spec.ContextDir)
	if err := b.CLI.Run(ctx, nil, log, args...); err != nil {
		return "", err
	}
	return ref, nil
}

// DockerDaemon is the image store of the local Docker daemon used as a
// registry: machines are containers on the same daemon, nothing is pushed.
type DockerDaemon struct{ CLI CLI }

func (DockerDaemon) Local() bool { return true }

func (DockerDaemon) Ref(_ context.Context, tag string, p Platform) (string, error) {
	return LocalRef(tag, p), nil
}

func (d DockerDaemon) Has(ctx context.Context, ref string) (bool, error) {
	return d.CLI.Exists(ctx, ref), nil
}

func (DockerDaemon) Push(_ context.Context, localRef string, _ Platform, _ io.Writer) (string, error) {
	return localRef, nil
}
