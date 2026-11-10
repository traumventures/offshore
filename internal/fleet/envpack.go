package fleet

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/traumventures/cloudsourced/internal/provider"
)

// packMeta describes a packed environment build context.
type packMeta struct {
	Tag        string `json:"tag"`
	Dockerfile string `json:"dockerfile"` // relative to ctx/
	BaseTag    string `json:"base_tag,omitempty"`
	BaseFile   string `json:"base_file,omitempty"` // relative to base/
}

// PackEnv writes the build context of spec as a gzipped tar: meta.json,
// ctx/ (the context directory) and base/ (the optional base image context).
func PackEnv(w io.Writer, spec provider.ImageSpec) error {
	meta := packMeta{Tag: spec.Tag, BaseTag: spec.BaseTag}
	var err error
	if meta.Dockerfile, err = inside(spec.ContextDir, spec.Dockerfile); err != nil {
		return err
	}
	if spec.BaseTag != "" {
		if meta.BaseFile, err = inside(spec.BaseDir, spec.BaseFile); err != nil {
			return err
		}
	}
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	mb, _ := json.Marshal(meta)
	if err := tw.WriteHeader(&tar.Header{Name: "meta.json", Mode: 0o644, Size: int64(len(mb)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if _, err := tw.Write(mb); err != nil {
		return err
	}
	if err := addTree(tw, spec.ContextDir, "ctx"); err != nil {
		return err
	}
	if spec.BaseTag != "" {
		if err := addTree(tw, spec.BaseDir, "base"); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func inside(dir, file string) (string, error) {
	rel, err := filepath.Rel(dir, file)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("%s is outside its build context %s", file, dir)
	}
	return filepath.ToSlash(rel), nil
}

func addTree(tw *tar.Writer, root, prefix string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		name := path.Join(prefix, filepath.ToSlash(rel))
		info, err := d.Info()
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		} else if !info.Mode().IsRegular() && !info.IsDir() {
			return nil // sockets, devices: not part of a build context
		}
		h, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		h.Name = name
		if info.IsDir() {
			h.Name += "/"
		}
		h.Uname, h.Gname, h.Uid, h.Gid = "", "", 0, 0
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, f)
			f.Close()
			return err
		}
		return nil
	})
}

// UnpackEnv extracts a packed build context into dir and returns its spec.
// Paths are confined to dir; symlinks are created last so that no entry is
// written through one.
func UnpackEnv(r io.Reader, dir string) (provider.ImageSpec, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return provider.ImageSpec{}, err
	}
	tr := tar.NewReader(gz)
	var meta *packMeta
	type link struct{ name, target string }
	var links []link
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return provider.ImageSpec{}, err
		}
		name := path.Clean(h.Name)
		if name == "." || path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return provider.ImageSpec{}, fmt.Errorf("invalid path %q in environment context", h.Name)
		}
		dst := filepath.Join(dir, filepath.FromSlash(name))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return provider.ImageSpec{}, err
			}
		case tar.TypeReg:
			if name == "meta.json" {
				meta = &packMeta{}
				if err := json.NewDecoder(io.LimitReader(tr, 1<<20)).Decode(meta); err != nil {
					return provider.ImageSpec{}, fmt.Errorf("meta.json: %w", err)
				}
				continue
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return provider.ImageSpec{}, err
			}
			f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fs.FileMode(h.Mode)&0o777|0o200)
			if err != nil {
				return provider.ImageSpec{}, err
			}
			_, err = io.Copy(f, tr)
			f.Close()
			if err != nil {
				return provider.ImageSpec{}, err
			}
		case tar.TypeSymlink:
			links = append(links, link{dst, h.Linkname})
		}
	}
	for _, l := range links {
		if err := os.MkdirAll(filepath.Dir(l.name), 0o755); err != nil {
			return provider.ImageSpec{}, err
		}
		if err := os.Symlink(l.target, l.name); err != nil {
			return provider.ImageSpec{}, err
		}
	}
	if meta == nil || meta.Tag == "" {
		return provider.ImageSpec{}, errors.New("environment context without meta.json")
	}
	spec := provider.ImageSpec{
		Tag:        meta.Tag,
		ContextDir: filepath.Join(dir, "ctx"),
		Dockerfile: filepath.Join(dir, "ctx", filepath.FromSlash(meta.Dockerfile)),
		BaseTag:    meta.BaseTag,
	}
	if meta.BaseTag != "" {
		spec.BaseDir = filepath.Join(dir, "base")
		spec.BaseFile = filepath.Join(dir, "base", filepath.FromSlash(meta.BaseFile))
	}
	return spec, nil
}

// fileEnv is an environment whose packed context is stored in a file.
func fileEnv(hash, tag, file string) Env {
	return Env{Hash: hash, Tag: tag, Context: func() (provider.ImageSpec, string, error) {
		f, err := os.Open(file)
		if err != nil {
			return provider.ImageSpec{}, "", err
		}
		defer f.Close()
		dir, err := os.MkdirTemp("", "cloudsourced-env-")
		if err != nil {
			return provider.ImageSpec{}, "", err
		}
		spec, err := UnpackEnv(f, dir)
		if err != nil {
			os.RemoveAll(dir)
			return provider.ImageSpec{}, "", err
		}
		return spec, dir, nil
	}}
}
