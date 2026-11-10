package awsec2

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/traumventures/cloudsourced/internal/hostagent"
	"github.com/traumventures/cloudsourced/internal/machine"
	"github.com/traumventures/cloudsourced/internal/provider"
)

// shq quotes s for a POSIX shell.
func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'" }

const hostBinary = "/opt/cloudsourced/bin/cloudsourced"

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

// hostSpec is the public machine description handed to the host agent.
// It names the secret parameter but never contains secret values.
func hostSpec(spec provider.MachineSpec, region, stack, bucket, registry, image string) hostagent.Spec {
	return hostagent.Spec{
		Provider: "aws-ec2",
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
			paramRegion:   region,
			paramSecret:   secretParameter(stack, spec.ID),
			paramRegistry: registry,
			// Compiler cache objects of the fleet, shared by its machines.
			paramSccache: bucket + "/cache/" + pathSegment(spec.Owner) + "/sccache",
		},
		CompilerCache: &hostagent.CompilerCache{Port: sccachePort},
	}
}

// userData renders the first boot script: Docker, the server binary, the
// public machine spec and the host agent unit. It contains no secrets
// (the user data is readable through the instance metadata service and
// the EC2 API); the agent fetches them from SSM.
func userData(spec provider.MachineSpec, region, stack string, in *infra, binKey, image string) (string, error) {
	registry := in.repo[:strings.Index(in.repo, "/")]
	hs, err := json.Marshal(hostSpec(spec, region, stack, in.bucket, registry, image))
	if err != nil {
		return "", err
	}
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	w("#!/bin/bash")
	w("set -euo pipefail")
	w("exec >>/var/log/cloudsourced-boot.log 2>&1")
	w("umask 077")
	w("mkdir -p /etc/cloudsourced /opt/cloudsourced/bin %s", hostagent.StateDir)
	w("echo %s | base64 -d > %s", base64.StdEncoding.EncodeToString(hs), hostagent.SpecPath)
	if spec.Swap {
		w("if [ ! -f /swapfile ]; then fallocate -l 8G /swapfile && chmod 600 /swapfile && mkswap /swapfile && echo '/swapfile none swap sw 0 0' >> /etc/fstab; fi; swapon -a || true")
	}
	w("dnf install -y -q docker")
	w(`mkdir -p /etc/docker && echo '{"log-driver":"json-file","log-opts":{"max-size":"50m","max-file":"2"}}' > /etc/docker/daemon.json`)
	w("systemctl enable --now docker")
	w("aws s3 cp --region %s %s %s", shq(region), shq("s3://"+in.bucket+"/"+binKey), hostBinary)
	w("chmod 755 %s", hostBinary)
	// The agent creates the containers, starts them on every boot and
	// enforces expiry and idle stop.
	w(`cat > /etc/systemd/system/cloudsourced-host.service <<'EOF'
[Unit]
Description=cloudsourced host agent
After=docker.service network-online.target
Wants=network-online.target
Requires=docker.service
[Service]
ExecStart=%s host
Restart=always
RestartSec=5
[Install]
WantedBy=multi-user.target
EOF`, hostBinary)
	w("systemctl daemon-reload && systemctl enable --now cloudsourced-host.service")
	w("echo cloudsourced machine %s handed to the host agent", spec.ID)
	return b.String(), nil
}
