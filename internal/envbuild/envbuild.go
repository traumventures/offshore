// Package envbuild turns the [environment] section of a repository
// configuration into a reproducible, content addressed container image.
//
// Layers: base image -> system packages -> toolchains -> one layer per setup
// step. The image tag is derived from a hash over the configuration and
// every referenced file, so identical configurations share one image and a
// change only rebuilds from the first affected layer (Docker layer cache).
package envbuild

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/traumventures/cloudsourced/internal/config"
	"github.com/traumventures/cloudsourced/internal/manifest"
	"github.com/traumventures/cloudsourced/internal/provider"
)

// generatorVersion changes whenever the generated Dockerfile changes shape.
const generatorVersion = "1"

// Plan is a resolved environment build.
type Plan struct {
	Hash       string
	Tag        string
	Dockerfile string
	files      map[string]string // context path -> source file
	baseDir    string
	baseFile   string
	baseTag    string
}

// Resolve computes the environment hash and Dockerfile for a repository.
func Resolve(r *config.Repo) (*Plan, error) {
	env := r.Environment
	p := &Plan{files: map[string]string{}}
	var hashParts []string
	add := func(s string) { hashParts = append(hashParts, s) }
	add("generator=" + generatorVersion)
	add(fmt.Sprintf("schema=%d", config.SchemaVersion))
	add("arch=" + r.Resources.Arch)
	envJSON, _ := json.Marshal(env)
	add("env=" + string(envJSON))

	var df strings.Builder
	from := env.Image
	if env.Base == "dockerfile" {
		file := filepath.Join(r.Root, env.Dockerfile)
		dir := filepath.Dir(file)
		h, err := hashDir(dir)
		if err != nil {
			return nil, fmt.Errorf("environment.dockerfile: %w", err)
		}
		add("dockerfile-context=" + h)
		p.baseDir, p.baseFile = dir, file
		p.baseTag = "cloudsourced-base:" + h[:16]
		from = p.baseTag
	}
	fmt.Fprintf(&df, "FROM %s\n", from)
	df.WriteString("SHELL [\"/bin/sh\", \"-ec\"]\n")
	if len(env.Packages.Apt) > 0 {
		pk := append([]string{}, env.Packages.Apt...)
		sort.Strings(pk)
		fmt.Fprintf(&df, "RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends %s && rm -rf /var/lib/apt/lists/*\n", strings.Join(pk, " "))
	}
	if env.Toolchains.Rust != "" {
		src := filepath.Join(r.Root, env.Toolchains.Rust)
		h, _, err := manifest.HashFile(src)
		if err != nil {
			return nil, fmt.Errorf("environment.toolchains.rust: %w", err)
		}
		add("rust-toolchain=" + h)
		name := filepath.Base(src)
		p.files["toolchain/"+name] = src
		fmt.Fprintf(&df, "COPY toolchain/%s /opt/cloudsourced/toolchain/%s\n", name, name)
		df.WriteString("RUN cd /opt/cloudsourced/toolchain && (rustup toolchain install || rustup show) && rustup show active-toolchain\n")
	}
	for i, step := range env.Setup {
		dir := fmt.Sprintf("setup/%d", i)
		for _, in := range step.Inputs {
			src := filepath.Join(r.Root, in)
			fi, err := os.Stat(src)
			if err != nil {
				return nil, fmt.Errorf("environment.setup[%d].inputs: %w", i, err)
			}
			var h string
			if fi.IsDir() {
				h, err = hashDir(src)
			} else {
				h, _, err = manifest.HashFile(src)
			}
			if err != nil {
				return nil, err
			}
			add(fmt.Sprintf("setup[%d].%s=%s", i, in, h))
			p.files[dir+"/"+filepath.ToSlash(filepath.Clean(in))] = src
		}
		if len(step.Inputs) > 0 {
			fmt.Fprintf(&df, "COPY %s/ /opt/cloudsourced/%s/\n", dir, dir)
		}
		fmt.Fprintf(&df, "RUN mkdir -p /opt/cloudsourced/%s && cd /opt/cloudsourced/%s && %s\n", dir, dir, step.Run)
	}
	keys := make([]string, 0, len(env.Env))
	for k := range env.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v, _ := json.Marshal(env.Env[k])
		fmt.Fprintf(&df, "ENV %s=%s\n", k, v)
	}
	p.Hash = manifest.HashBytes([]byte(strings.Join(hashParts, "\n")))
	fmt.Fprintf(&df, "LABEL cloudsourced.env-hash=%s\n", p.Hash)
	p.Dockerfile = df.String()
	p.Tag = "cloudsourced-env:" + p.Hash[:16]
	return p, nil
}

// Context writes the build context to a temporary directory and returns the
// image spec. The caller removes the directory.
func (p *Plan) Context() (provider.ImageSpec, string, error) {
	dir, err := os.MkdirTemp("", "cloudsourced-env-")
	if err != nil {
		return provider.ImageSpec{}, "", err
	}
	for dst, src := range p.files {
		if err := copyTree(src, filepath.Join(dir, filepath.FromSlash(dst))); err != nil {
			os.RemoveAll(dir)
			return provider.ImageSpec{}, "", err
		}
	}
	df := filepath.Join(dir, "Dockerfile")
	if err := os.WriteFile(df, []byte(p.Dockerfile), 0o644); err != nil {
		os.RemoveAll(dir)
		return provider.ImageSpec{}, "", err
	}
	return provider.ImageSpec{
		Tag: p.Tag, ContextDir: dir, Dockerfile: df,
		BaseTag: p.baseTag, BaseDir: p.baseDir, BaseFile: p.baseFile,
	}, dir, nil
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, _ := d.Info()
		return os.WriteFile(target, b, info.Mode().Perm())
	})
}

// hashDir hashes all regular files below dir (paths and contents).
func hashDir(dir string) (string, error) {
	var parts []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if !d.Type().IsRegular() {
			return nil
		}
		h, _, err := manifest.HashFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		parts = append(parts, rel+"="+h)
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(parts)
	return manifest.HashBytes([]byte(strings.Join(parts, "\n"))), nil
}
