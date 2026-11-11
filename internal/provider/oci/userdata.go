package oci

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/traumventures/cloudsourced/internal/machine"
	"github.com/traumventures/cloudsourced/internal/provider"
	"github.com/traumventures/cloudsourced/internal/supervisor"
)

// shq quotes s for a POSIX shell.
func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'" }

const hostBinary = "/opt/cloudsourced/bin/cloudsourced"

// pathSegment makes s safe as one object name segment.
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
// It names the secret object but never contains secret values.
func hostSpec(spec provider.MachineSpec, region string, in *infra, image string) supervisor.Spec {
	registry, _, _ := strings.Cut(in.repo, "/")
	return supervisor.Spec{
		Provider: "oci",
		Layout: machine.Layout{
			ID: spec.ID, Image: image, Binary: hostBinary,
			Publish:     strconv.Itoa(machine.ServerPort) + ":" + machine.ServerPortSpec(),
			CacheVolume: "cs-cache", DepsVolume: "cs-deps",
			StatusDir: "/run/cloudsourced/status",
			// The whole VM is the machine: no container limits.
			Env: spec.Env, SecretKeys: provider.SortedKeys(spec.SecretEnv), Services: spec.Services,
		},
		ExpiresAt: spec.ExpiresAt.Unix(),
		IdleStop:  int64(spec.IdleStop.Seconds()),
		Params: map[string]string{
			paramRegion:    region,
			paramNamespace: in.namespace,
			paramBucket:    in.bucket,
			paramSecret:    secretObject(spec.ID),
			paramRegistry:  registry,
			// Compiler cache objects of the pool, shared by its machines.
		},
	}
}

// userData renders the first boot script: Docker, the server binary, the
// public machine spec and the supervisor unit. It contains no secrets
// (user data is readable through the metadata service and the API; the
// binary URL is a read-only link to the public server binary); the
// supervisor fetches them from Object Storage.
//
// It supports Oracle Linux (default image; Docker CE from Docker's
// repository, as Oracle Linux ships Podman) and Ubuntu images.
func userData(spec provider.MachineSpec, region string, in *infra, binURL, image string) (string, error) {
	hs, err := json.Marshal(hostSpec(spec, region, in, image))
	if err != nil {
		return "", err
	}
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	w("#!/bin/bash")
	w("set -euo pipefail")
	w("exec >>/var/log/cloudsourced-boot.log 2>&1")
	w("umask 077")
	w("mkdir -p /etc/cloudsourced /opt/cloudsourced/bin %s", supervisor.StateDir)
	w("echo %s | base64 -d > %s", base64.StdEncoding.EncodeToString(hs), supervisor.SpecPath)
	// Platform images do not grow the root file system to the boot volume.
	w("if [ -x /usr/libexec/oci-growfs ]; then /usr/libexec/oci-growfs -y || true; fi")
	if spec.Swap {
		w("if [ ! -f /swapfile ]; then fallocate -l 8G /swapfile && chmod 600 /swapfile && mkswap /swapfile && echo '/swapfile none swap sw 0 0' >> /etc/fstab; fi; swapon -a || true")
	}
	// The security list admits QUIC from allow_cidrs only; the host
	// firewall of the images would also drop it.
	w("systemctl disable --now firewalld 2>/dev/null || true")
	w("if command -v dnf >/dev/null; then")
	w("  dnf install -y -q dnf-plugins-core")
	w("  dnf config-manager --add-repo https://download.docker.com/linux/centos/docker-ce.repo")
	w("  dnf install -y -q docker-ce docker-ce-cli containerd.io")
	w("else")
	w("  export DEBIAN_FRONTEND=noninteractive")
	w("  apt-get update -q && apt-get install -y -q docker.io curl")
	w("  iptables -I INPUT -p udp --dport %d -j ACCEPT", serverPort)
	w("fi")
	w(`mkdir -p /etc/docker && echo '{"log-driver":"json-file","log-opts":{"max-size":"50m","max-file":"2"}}' > /etc/docker/daemon.json`)
	w("systemctl enable --now docker")
	w("curl -fsS --retry 5 --retry-delay 2 -o %s %s", hostBinary, shq(binURL))
	w("chmod 755 %s", hostBinary)
	// The supervisor creates the containers, starts them on every boot and
	// enforces expiry and idle stop. Containers must not reach the
	// metadata service (it hands out the instance principal): DOCKER-USER
	// rejects forwarded traffic to link-local addresses.
	w(`cat > /etc/systemd/system/cloudsourced-supervisor.service <<'EOF'
[Unit]
Description=cloudsourced supervisor
After=docker.service network-online.target
Wants=network-online.target
Requires=docker.service
[Service]
ExecStartPre=/bin/sh -c 'iptables -C DOCKER-USER -d 169.254.0.0/16 -j REJECT 2>/dev/null || iptables -I DOCKER-USER -d 169.254.0.0/16 -j REJECT'
ExecStart=%s supervisor
Restart=always
RestartSec=5
[Install]
WantedBy=multi-user.target
EOF`, hostBinary)
	w("systemctl daemon-reload && systemctl enable --now cloudsourced-supervisor.service")
	w("echo cloudsourced machine %s handed to the supervisor", spec.ID)
	return b.String(), nil
}
