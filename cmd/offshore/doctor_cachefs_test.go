package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/traumventures/offshore/internal/config"
	"github.com/traumventures/offshore/internal/hostfs"
)

func TestCacheFSCheck(t *testing.T) {
	ext4 := func(opts ...string) hostfs.Info {
		return hostfs.Info{Path: "/var/lib/docker", Type: "ext4", Mount: hostfs.Mount{MountPoint: "/", FSType: "ext4", Options: opts}}
	}
	cases := []struct {
		name   string
		info   hostfs.Info
		err    error
		probed bool
		status string
		hint   []string
	}{
		{"xfs reflink", hostfs.Info{Path: "/d", Type: "xfs", Reflink: true}, nil, true, checkOK, nil},
		{"xfs not probed", hostfs.Info{Path: "/d", Type: "xfs"}, nil, false, checkOK, nil},
		{"xfs without reflink", hostfs.Info{Path: "/d", Type: "xfs"}, nil, true, checkWarn, []string{"copy changed files"}},
		{"ext4", ext4("rw"), nil, false, checkWarn, []string{"mkfs.xfs"}},
		{"ext4 noauto_da_alloc", ext4("rw", "noauto_da_alloc"), nil, false, checkWarn, []string{"mkfs.xfs", "noauto_da_alloc"}},
		{"statfs error", hostfs.Info{}, errors.New("statfs: permission denied"), false, checkSkip, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := cacheFSCheck(c.info, c.err, c.probed)
			if got.Status != c.status {
				t.Fatalf("%+v", got)
			}
			for _, h := range c.hint {
				if !strings.Contains(got.Hint, h) {
					t.Errorf("hint %q lacks %q", got.Hint, h)
				}
			}
			if c.status == checkOK && got.Hint != "" {
				t.Errorf("ok with hint %q", got.Hint)
			}
		})
	}
}

func TestLocalDockerCommand(t *testing.T) {
	fc := &config.PoolConfig{Providers: map[string]map[string]any{
		"aws":   {"type": "aws-ec2"},
		"local": {"type": "local-docker", "command": []any{"sudo", "-n", "docker"}},
	}}
	if got := strings.Join(localDockerCommand(fc), " "); got != "sudo -n docker" {
		t.Fatalf("%q", got)
	}
	fc.Providers["local"] = map[string]any{"type": "local-docker"}
	if got := strings.Join(localDockerCommand(fc), " "); got != "docker" {
		t.Fatalf("default %q", got)
	}
	delete(fc.Providers, "local")
	if localDockerCommand(fc) != nil {
		t.Fatal("no local-docker provider")
	}
}
