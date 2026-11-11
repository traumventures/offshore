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
	"strings"
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
	// Dir, when set, is a directory inside the archive whose whole tree
	// is installed (toolchains) instead of the single binary Member.
	Dir string
}

// Pinned releases. Changing a version or checksum changes the environment
// hash of repositories that use the tool.
const (
	sccacheVersion = "0.19.0"
	moldVersion    = "3.0.0"
	wildVersion    = "0.11.0"
	// headstartVersion is the commit of github.com/PowderworksCode/headstart
	// built by scripts/build-headstart.sh.
	headstartVersion = "02ec0bf314"
)

// artifactBase is where offshore publishes toolchains it builds itself
// (the aws-ec2 artifact bucket, private: downloads are signed with the
// client's AWS credentials, see archive).
const artifactBase = "https://offshore-artifactbucket-z7c0j8lsimso.s3.eu-central-1.amazonaws.com/toolchains/"

var toolReleases = map[string]map[string]tool{
	"sccache": {
		"x86_64":  sccacheTool("x86_64", "aa73c7270c997894038c0d8f0d297865f57569e1687883322bc431713d4bb9dd"),
		"aarch64": sccacheTool("aarch64", "3e47fc4a71b1ddd1b4294a0de9d34498922fdf41e3cd0bda5df3954e3d6deb9d"),
	},
	"mold": {
		"x86_64":  moldTool("x86_64", "6c90d4a474c7c0409dfb575be03a5345878ac14fdba18de8b40fa58c60121189"),
		"aarch64": moldTool("aarch64", "52c759d3689babaea4c42af2f31062a74ab83e17ffb5a8090232534c67b70577"),
	},
	"wild": {
		"x86_64":  wildTool("x86_64", "71abbcb7264a31571f3eeb1b7342b0dd6e0ddbcaf280bcfe7e427c0a2bead79f"),
		"aarch64": wildTool("aarch64", "4b3696f6b030a03d4e903abe267c2c858b22d03df53998d96d4a166a074dede5"),
	},
	// Patched rustc (-Zearly-metadata) and cargo (-Zheadstart), no
	// upstream binary release; x86_64 only.
	"headstart": {
		"x86_64": headstartTool("x86_64", "b94ccd515bf3f88bfcd756f985fb7af4592731d10cc6204fa8f202a9d24ab976"),
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

func wildTool(arch, sum string) tool {
	dir := fmt.Sprintf("wild-linker-%s-%s-unknown-linux-musl", wildVersion, arch)
	return tool{
		Name: "wild", Version: wildVersion, SHA256: sum, Member: dir + "/wild",
		URL: fmt.Sprintf("https://github.com/wild-linker/wild/releases/download/%s/%s.tar.gz", wildVersion, dir),
	}
}

func headstartTool(arch, sum string) tool {
	return tool{
		Name: "headstart", Version: headstartVersion, SHA256: sum, Dir: "headstart-" + headstartVersion,
		URL: fmt.Sprintf("%sheadstart-%s-%s-unknown-linux-gnu.tar.gz", artifactBase, headstartVersion, arch),
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
	body := io.Reader(resp.Body)
	if resp.StatusCode == http.StatusForbidden && strings.HasPrefix(t.URL, artifactBase) {
		// The artifact bucket is private: fetch with the AWS credentials
		// of the client (AWS_PROFILE etc.) instead.
		rc, serr := s3Get(t.URL)
		if serr != nil {
			return "", fmt.Errorf("download %s: %w (needs AWS credentials with read access to the artifact bucket, or a pre-seeded OFFSHORE_TOOL_CACHE)", t.URL, serr)
		}
		defer rc.Close()
		body = rc
	} else if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: %s", t.URL, resp.Status)
	}
	tmp, err := os.CreateTemp(dir, "download-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, h), body)
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
	if t.Dir != "" {
		return extractTree(tr, t.Dir, dst)
	}
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

// extractTree writes the regular files and directories below prefix in
// the archive to dst, keeping their modes.
func extractTree(tr *tar.Reader, prefix, dst string) error {
	prefix = strings.TrimSuffix(prefix, "/") + "/"
	n := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(h.Name, "./")
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		rel := filepath.FromSlash(strings.TrimPrefix(name, prefix))
		if rel == "" {
			continue
		}
		if !filepath.IsLocal(rel) {
			return fmt.Errorf("archive entry %q escapes %s", h.Name, prefix)
		}
		p := filepath.Join(dst, rel)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(h.Mode)&0o755|0o644)
			if err != nil {
				return err
			}
			_, err = io.Copy(out, tr)
			if cerr := out.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
			n++
		case tar.TypeLink:
			// Hard links (tar -h packs identical files once) point at an
			// earlier entry below the same prefix.
			target := filepath.FromSlash(strings.TrimPrefix(strings.TrimPrefix(h.Linkname, "./"), prefix))
			if !strings.HasPrefix(strings.TrimPrefix(h.Linkname, "./"), prefix) || !filepath.IsLocal(target) {
				return fmt.Errorf("archive entry %q links outside %s", h.Name, prefix)
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			os.Remove(p)
			if err := os.Link(filepath.Join(dst, target), p); err != nil {
				return err
			}
			n++
		default:
			return fmt.Errorf("archive entry %q: unsupported type %c", h.Name, h.Typeflag)
		}
	}
	if n == 0 {
		return fmt.Errorf("%s not in archive", prefix)
	}
	return nil
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
