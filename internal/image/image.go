// Package image builds environment images and makes them available to the
// machines of a provider.
//
// A Builder turns a build context into an image in a local image store (the
// Docker CLI); a Registry makes such an image reachable for machines: the
// local Docker daemon needs nothing (machines are containers on it), ECR
// receives a push. Providers no longer build: they expose the registry their
// machines pull from and the platform they run (provider.ImageHost), and the
// Pipeline builds once per environment and platform and pushes once per
// registry, lazily, right before a machine needs the image.
//
// Building needs a Docker CLI where the pipeline runs (the embedded daemon
// or the control plane). A control plane that only serves registries which
// already hold the image never calls Docker: Has is checked first.
package image

import (
	"context"
	"io"
	"runtime"
	"strings"
)

// Platform is an OCI platform such as linux/amd64.
type Platform string

const (
	LinuxAMD64 Platform = "linux/amd64"
	LinuxARM64 Platform = "linux/arm64"
)

// Native returns the platform of this process, which is also the platform
// the local Docker daemon builds for by default.
func Native() Platform { return Platform("linux/" + runtime.GOARCH) }

// Arch returns the architecture part (amd64 for linux/amd64).
func (p Platform) Arch() string {
	_, a, ok := strings.Cut(string(p), "/")
	if !ok {
		return string(p)
	}
	return strings.ReplaceAll(a, "/", "-")
}

// Spec describes an environment image to build.
type Spec struct {
	Tag        string
	ContextDir string
	Dockerfile string // path inside ContextDir
	BaseTag    string // optional user base image built first
	BaseDir    string
	BaseFile   string
}

// BaseArg is the build argument naming the base image in generated
// Dockerfiles ("ARG OFFSHORE_BASE=<tag>" / "FROM ${OFFSHORE_BASE}"),
// so a base built for another platform can be substituted.
const BaseArg = "OFFSHORE_BASE"

// LocalRef returns the local tag of an image built for platform. Images of
// the native platform keep their tag; others get an architecture suffix so
// both can live in one image store.
func LocalRef(tag string, p Platform) string {
	if p == "" || p == Native() {
		return tag
	}
	return tag + "-" + p.Arch()
}

// Builder builds environment images into a local image store.
type Builder interface {
	// ID identifies the image store, e.g. the CLI invocation: builds are
	// shared between providers whose builders have the same ID.
	ID() string
	// Build makes spec's image for platform available locally (reusing an
	// existing image and the build cache) and returns its local reference.
	Build(ctx context.Context, spec Spec, platform Platform, log io.Writer) (string, error)
}

// Registry makes local images available to machines.
type Registry interface {
	// Local reports whether the registry is the builder's own image store
	// (no push; building is all that is needed).
	Local() bool
	// Ref returns the reference machines use for the image tag on platform.
	Ref(ctx context.Context, tag string, platform Platform) (string, error)
	// Has reports whether the registry holds ref.
	Has(ctx context.Context, ref string) (bool, error)
	// Push copies a local image into the registry and returns its reference.
	Push(ctx context.Context, localRef string, platform Platform, log io.Writer) (string, error)
}
