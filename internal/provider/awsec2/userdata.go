package awsec2

import (
	"encoding/base64"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/traumventures/cloudsourced/internal/provider"
)

// shq quotes s for a POSIX shell.
func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'" }

func shjoin(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = shq(a)
	}
	return strings.Join(q, " ")
}

// userData renders the first boot script. Secrets are written to root-only
// files; the instance metadata service (which serves this script) is not
// reachable from containers (hop limit 1).
func userData(spec provider.MachineSpec, region string, in *infra, binKey, image string) (string, error) {
	files := map[string]string{"expires-at": unix(spec.ExpiresAt)}
	for k, v := range spec.SecretEnv {
		files["secret-"+k] = v
	}
	name := "cs-" + spec.ID
	registry := in.repo[:strings.Index(in.repo, "/")]
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	w("#!/bin/bash")
	w("set -euo pipefail")
	w("exec >>/var/log/cloudsourced-boot.log 2>&1")
	w("umask 077")
	w("mkdir -p /etc/cloudsourced /opt/cloudsourced/bin")
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		w("echo %s | base64 -d > /etc/cloudsourced/%s", base64.StdEncoding.EncodeToString([]byte(files[k])), k)
	}
	// Expiry: the instance terminates itself (shutdown behaviour terminate).
	w(`cat > /opt/cloudsourced/expiry.sh <<'EOF'
#!/bin/bash
exp=$(cat /etc/cloudsourced/expires-at)
while [ "$(date +%%s)" -lt "$exp" ]; do sleep 20; done
logger -t cloudsourced "lifetime expired, terminating"
shutdown -h now
EOF`)
	w(`cat > /etc/systemd/system/cloudsourced-expiry.service <<'EOF'
[Unit]
Description=cloudsourced machine lifetime
[Service]
ExecStart=/bin/bash /opt/cloudsourced/expiry.sh
Restart=always
[Install]
WantedBy=multi-user.target
EOF`)
	w("systemctl daemon-reload && systemctl enable --now cloudsourced-expiry.service")
	if spec.Swap {
		w("if [ ! -f /swapfile ]; then fallocate -l 8G /swapfile && chmod 600 /swapfile && mkswap /swapfile && echo '/swapfile none swap sw 0 0' >> /etc/fstab; fi; swapon -a || true")
	}
	w("dnf install -y -q docker")
	w(`mkdir -p /etc/docker && echo '{"log-driver":"json-file","log-opts":{"max-size":"50m","max-file":"2"}}' > /etc/docker/daemon.json`)
	w("systemctl enable --now docker")
	w("for i in $(seq 30); do docker info >/dev/null 2>&1 && break; sleep 1; done")
	w("aws s3 cp --region %s %s /opt/cloudsourced/bin/cloudsourced", shq(region), shq("s3://"+in.bucket+"/"+binKey))
	w("chmod 755 /opt/cloudsourced/bin/cloudsourced")
	w("aws ecr get-login-password --region %s | docker login --username AWS --password-stdin %s", shq(region), shq(registry))
	w("docker pull %s", shq(image))
	w("docker volume create cs-cache >/dev/null && docker volume create cs-deps >/dev/null")

	run := []string{"create", "--name", name, "--hostname", name,
		"--device", "/dev/fuse", "--cap-add", "SYS_ADMIN", "--security-opt", "apparmor=unconfined",
		"--cgroupns", "private", "-p", strconv.Itoa(serverPort) + ":" + strconv.Itoa(serverPort) + "/udp",
		"-v", "/opt/cloudsourced/bin/cloudsourced:/opt/cloudsourced/bin/cloudsourced:ro",
		"-v", "cs-cache:/cache", "-v", "cs-deps:/deps",
		"--entrypoint", "/opt/cloudsourced/bin/cloudsourced",
	}
	for _, k := range provider.SortedKeys(spec.Env) {
		run = append(run, "-e", k+"="+spec.Env[k])
	}
	if spec.Swap {
		run = append(run, "--memory-swap", "-1")
	}
	// Secrets come from the root-only files, not from this script's text.
	w("docker %s \\", shjoin(run))
	for _, k := range provider.SortedKeys(spec.SecretEnv) {
		w(`  -e %s="$(cat /etc/cloudsourced/secret-%s)" \`, k, k)
	}
	w("  %s server", shq(image))
	var svcNames []string
	for _, svc := range spec.Services {
		sn := name + "-svc-" + svc.Name
		svcNames = append(svcNames, sn)
		args := []string{"create", "--name", sn, "--network", "container:" + name}
		envKeys := make([]string, 0, len(svc.Env))
		for k := range svc.Env {
			envKeys = append(envKeys, k)
		}
		sort.Strings(envKeys)
		for _, k := range envKeys {
			args = append(args, "-e", k+"="+svc.Env[k])
		}
		args = append(args, svc.Image)
		args = append(args, svc.Command...)
		w("docker pull %s", shq(svc.Image))
		w("docker %s", shjoin(args))
	}
	// The boot unit starts the machine and then its services (which join
	// the machine's network namespace) on every boot, including restarts
	// after a stop.
	w("cat > /opt/cloudsourced/boot.sh <<'EOF'")
	w("#!/bin/bash")
	w("set -e")
	w("docker start %s", shq(name))
	for _, sn := range svcNames {
		w("docker start %s", shq(sn))
	}
	w("EOF")
	w(`cat > /etc/systemd/system/cloudsourced-boot.service <<'EOF'
[Unit]
Description=cloudsourced machine containers
After=docker.service
Requires=docker.service
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/bash /opt/cloudsourced/boot.sh
[Install]
WantedBy=multi-user.target
EOF`)
	w("systemctl daemon-reload && systemctl enable --now cloudsourced-boot.service")
	w("echo cloudsourced machine %s ready", spec.ID)
	return b.String(), nil
}
