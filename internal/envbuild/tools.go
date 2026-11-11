package envbuild

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// tool is a pinned, statically linked release binary installed into
// environment images. The archive is downloaded on the client (cached in
// the user cache directory), verified against its SHA-256 and the binary is
// copied into the build context, so base images need no download tools.
type tool struct {
	Name    string
	Version string
	URL     string
	SHA256  string // of the archive
	Member  string // path of the binary inside the archive
}

// Pinned releases. Changing a version or checksum changes the environment
// hash of repositories that use the tool.
const (
	sccacheVersion = "0.19.0"
	moldVersion    = "3.0.0"
)

var toolReleases = map[string]map[string]tool{
	"sccache": {
		"x86_64":  sccacheTool("x86_64", "aa73c7270c997894038c0d8f0d297865f57569e1687883322bc431713d4bb9dd"),
		"aarch64": sccacheTool("aarch64", "3e47fc4a71b1ddd1b4294a0de9d34498922fdf41e3cd0bda5df3954e3d6deb9d"),
	},
	"mold": {
		"x86_64":  moldTool("x86_64", "6c90d4a474c7c0409dfb575be03a5345878ac14fdba18de8b40fa58c60121189"),
		"aarch64": moldTool("aarch64", "52c759d3689babaea4c42af2f31062a74ab83e17ffb5a8090232534c67b70577"),
	},
}

func sccacheTool(arch, sum string) tool {
	dir := fmt.Sprintf("sccache-v%s-%s-unknown-linux-musl", sccacheVersion, arch)
	return tool{
		Name: "sccache", Version: sccacheVersion, SHA256: sum, Member: dir + "/sccache",
		URL: fmt.Sprintf("https://github.com/mozilla/sccache/releases/download/v%s/%s.tar.gz", sccacheVersion, dir),
	}
}

func moldTool(arch, sum string) tool {
	dir := fmt.Sprintf("mold-%s-%s-linux", moldVersion, arch)
	return tool{
		Name: "mold", Version: moldVersion, SHA256: sum, Member: dir + "/bin/mold",
		URL: fmt.Sprintf("https://github.com/rui314/mold/releases/download/v%s/%s.tar.gz", moldVersion, dir),
	}
}

func releaseFor(name, arch string) (tool, error) {
	t, ok := toolReleases[name][arch]
	if !ok {
		return tool{}, fmt.Errorf("%s: no release for %s", name, arch)
	}
	return t, nil
}

// toolCacheDir holds downloaded archives. OFFSHORE_TOOL_CACHE
// overrides it (e.g. for offline use with pre-seeded archives).
func toolCacheDir() (string, error) {
	if d := os.Getenv("OFFSHORE_TOOL_CACHE"); d != "" {
		return d, nil
	}
	d, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "offshore", "tools"), nil
}

// archive returns the path of the verified archive, downloading it if
// needed.
func (t tool) archive() (string, error) {
	dir, err := toolCacheDir()
	if err != nil {
		return "", err
	}
	file := filepath.Join(dir, t.SHA256+"-"+filepath.Base(t.URL))
	if sum, err := fileSHA256(file); err == nil && sum == t.SHA256 {
		return file, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Get(t.URL)
	if err != nil {
		return "", fmt.Errorf("download %s %s: %w", t.Name, t.Version, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: %s", t.URL, resp.Status)
	}
	tmp, err := os.CreateTemp(dir, "download-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, h), resp.Body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("download %s: %w", t.URL, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != t.SHA256 {
		return "", fmt.Errorf("download %s: checksum mismatch (got %s, want %s)", t.URL, got, t.SHA256)
	}
	if err := os.Rename(tmp.Name(), file); err != nil {
		return "", err
	}
	return file, nil
}

// extract writes the tool's binary to dst (mode 0755).
func (t tool) extract(dst string) error {
	file, err := t.archive()
	if err != nil {
		return err
	}
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("%s: %s not in archive", file, t.Member)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		if h.Name != t.Member || h.Typeflag != tar.TypeReg {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, tr)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		return err
	}
}

func fileSHA256(file string) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
