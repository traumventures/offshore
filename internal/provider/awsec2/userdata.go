package awsec2

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/traumventures/offshore/internal/machine"
	"github.com/traumventures/offshore/internal/provider"
	"github.com/traumventures/offshore/internal/supervisor"
)

// shq quotes s for a POSIX shell.
func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'" }

const hostBinary = "/opt/offshore/bin/offshore"

// pathSegment makes s safe as one S3 key segment.
func pathSegment(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	if b.Len() == 0 {
		return "default"
	}
	return b.String()
}

// hostSpec is the public machine description handed to the supervisor.
// It names the secret parameter but never contains secret values.
func hostSpec(spec provider.MachineSpec, region, stack, bucket, registry, image string) supervisor.Spec {
	return supervisor.Spec{
		Provider: "aws-ec2",
		Layout: machine.Layout{
			ID: spec.ID, Image: image, Binary: hostBinary,
			Publish:     strconv.Itoa(machine.ServerPort) + ":" + machine.ServerPortSpec(),
			CacheVolume: "cs-cache", DepsVolume: "cs-deps",
			StatusDir: "/run/offshore/status",
			// The whole VM is the machine: no container limits.
			Env: spec.Env, SecretKeys: provider.SortedKeys(spec.SecretEnv), Services: spec.Services,
		},
		ExpiresAt: spec.ExpiresAt.Unix(),
		IdleStop:  int64(spec.IdleStop.Seconds()),
		Cache:     cacheSpec(spec),
		Params: map[string]string{
			paramRegion:   region,
			paramSecret:   secretParameter(stack, spec.ID),
			paramRegistry: registry,
			paramBucket:   bucket,
		},
	}
}

// instanceStore matches the instance-store NVMe disks of Nitro instances
// (EBS volumes have their own by-id names).
const instanceStore = "/dev/disk/by-id/nvme-Amazon_EC2_NVMe_Instance_Storage_*"

// cacheSpec places the cache: on the attached EBS volume, else on the
// first instance-store disk if the instance type has one.
func cacheSpec(spec provider.MachineSpec) *supervisor.CacheSpec {
	if spec.Volume != "" {
		return &supervisor.CacheSpec{Medium: supervisor.CacheVolume, Device: VolumeDevice(spec.Volume), TTLSeconds: int64(spec.CacheTTL.Seconds())}
	}
	return &supervisor.CacheSpec{Medium: supervisor.CacheLocal, LocalDevices: instanceStore}
}

// dockerDaemonJSON is the Docker daemon configuration of machines (and
// baked images): bounded container logs, parallel layer downloads.
const dockerDaemonJSON = `{"log-driver":"json-file","log-opts":{"max-size":"50m","max-file":"2"},"max-concurrent-downloads":10}`

// binarySource is where the boot script gets the server binary: a
// presigned HTTPS URL (curl, no AWS CLI start) or else the S3 key.
type binarySource struct {
	Key, URL string
}

// userData renders the first boot script: Docker, the server binary, the
// public machine spec and the supervisor unit. It contains no secrets
// (the user data is readable through the instance metadata service and
// the EC2 API); the supervisor fetches them from SSM. The binary download
// runs while Docker is installed; each step records its boot phase
// (supervisor.BootScript).
func userData(spec provider.MachineSpec, region, stack string, in *infra, bin binarySource, image string) (string, error) {
	registry := in.repo[:strings.Index(in.repo, "/")]
	hs, err := json.Marshal(hostSpec(spec, region, stack, in.bucket, registry, image))
	if err != nil {
		return "", err
	}
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	w("#!/bin/bash")
	w("set -euo pipefail")
	w("exec >>/var/log/offshore-boot.log 2>&1")
	w("umask 077")
	w("mkdir -p /etc/offshore /opt/offshore/bin %s %s", supervisor.StateDir, filepath.Dir(supervisor.BootScript))
	w("%s", supervisor.BootScriptLine(machine.PhaseUserData))
	w("echo %s | base64 -d > %s", base64.StdEncoding.EncodeToString(hs), supervisor.SpecPath)
	if spec.Swap {
		w("if [ ! -f /swapfile ]; then fallocate -l 8G /swapfile && chmod 600 /swapfile && mkswap /swapfile && echo '/swapfile none swap sw 0 0' >> /etc/fstab; fi; swapon -a || true")
	}
	// The server binary downloads while Docker installs.
	fetch := fmt.Sprintf("aws s3 cp --region %s %s %s.tmp", shq(region), shq("s3://"+in.bucket+"/"+bin.Key), hostBinary)
	if bin.URL != "" {
		fetch = fmt.Sprintf("curl -fsS --retry 5 --retry-all-errors -o %s.tmp %s", hostBinary, shq(bin.URL))
	}
	w("( %s && chmod 755 %s.tmp && mv %s.tmp %s && %s ) &", fetch, hostBinary, hostBinary, hostBinary, supervisor.BootScriptLine(machine.PhaseBinary))
	w("binpid=$!")
	// A machine image with Docker (a baked one) skips the installation.
	w("command -v docker >/dev/null || dnf install -y -q docker")
	// A baked image has this configuration (and Docker running) already.
	w(`mkdir -p /etc/docker && echo %s > /etc/docker/daemon.json.new`, shq(dockerDaemonJSON))
	w("if cmp -s /etc/docker/daemon.json.new /etc/docker/daemon.json; then rm /etc/docker/daemon.json.new; systemctl enable --now docker; " +
		"else mv /etc/docker/daemon.json.new /etc/docker/daemon.json; systemctl enable docker && systemctl restart docker; fi")
	w("%s", supervisor.BootScriptLine(machine.PhaseDocker))
	w("wait $binpid")
	// The agent creates the containers, starts them on every boot and
	// enforces expiry and idle stop.
	w(`cat > /etc/systemd/system/offshore-supervisor.service <<'EOF'
[Unit]
Description=offshore supervisor
After=docker.service network-online.target
Wants=network-online.target
Requires=docker.service
[Service]
ExecStart=%s supervisor
OOMScoreAdjust=-1000
Restart=always
RestartSec=5
[Install]
WantedBy=multi-user.target
EOF`, hostBinary)
	w("systemctl daemon-reload && systemctl enable --now offshore-supervisor.service")
	// A baked disk loads its blocks from the snapshot on first read: read
	// the images' files at idle I/O priority before sessions need them.
	w("if [ -e %s ]; then (nice -n 19 ionice -c3 find /var/lib/docker -xdev -type f -exec cat {} + >/dev/null 2>&1 &) ; fi", bakedMarker)
	w("echo offshore machine %s handed to the supervisor", spec.ID)
	return b.String(), nil
}
