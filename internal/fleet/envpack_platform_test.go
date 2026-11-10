package fleet

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/traumventures/cloudsourced/internal/provider"
)

// The image platform survives the upload to the control plane: an arm64
// environment must not be built for amd64 there.
func TestPackEnvKeepsPlatform(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := PackEnv(&buf, provider.ImageSpec{Tag: "t", ContextDir: dir, Dockerfile: filepath.Join(dir, "Dockerfile"), Platform: "linux/arm64"}); err != nil {
		t.Fatal(err)
	}
	spec, err := UnpackEnv(bytes.NewReader(buf.Bytes()), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if spec.Platform != "linux/arm64" {
		t.Fatalf("platform lost: %+v", spec)
	}
}
