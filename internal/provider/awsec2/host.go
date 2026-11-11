package awsec2

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/traumventures/offshore/internal/supervisor"
)

// Spec parameters of the supervisor on EC2.
const (
	paramRegion   = "region"
	paramSecret   = "secret_parameter" // SSM SecureString with the secret server environment
	paramRegistry = "registry"         // ECR registry of the environment image
)

func init() {
	supervisor.RegisterPlatform("aws-ec2", openHost)
}

// secretParameter is the SSM parameter holding a machine's secret
// environment until its supervisor has read it.
func secretParameter(stack, machineID string) string {
	return "/offshore/" + stack + "/machines/" + machineID
}

// host is the supervisor side of the provider. It uses the instance role,
// whose credentials come from the instance metadata service; only the host
// reaches it (hop limit 1), never the containers.
type host struct {
	spec supervisor.Spec
	cfg  aws.Config
	ssm  *ssm.Client
	ec2  *ec2.Client
	ecr  *ecr.Client
	imds *imds.Client
}

func openHost(ctx context.Context, spec supervisor.Spec) (supervisor.Platform, error) {
	region := spec.Params[paramRegion]
	if region == "" || spec.Params[paramSecret] == "" {
		return nil, errors.New("aws-ec2 host: spec lacks region or secret parameter")
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, err
	}
	return &host{spec: spec, cfg: cfg, ssm: ssm.NewFromConfig(cfg), ec2: ec2.NewFromConfig(cfg),
		ecr: ecr.NewFromConfig(cfg), imds: imds.NewFromConfig(cfg)}, nil
}

func (h *host) FetchSecrets(ctx context.Context) (map[string]string, error) {
	out, err := h.ssm.GetParameter(ctx, &ssm.GetParameterInput{
		Name: aws.String(h.spec.Params[paramSecret]), WithDecryption: aws.Bool(true)})
	if err != nil {
		return nil, fmt.Errorf("aws-ec2 host: read secret parameter: %w", err)
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(aws.ToString(out.Parameter.Value)), &m); err != nil {
		return nil, fmt.Errorf("aws-ec2 host: secret parameter: %w", err)
	}
	return m, nil
}

func (h *host) DeleteSecrets(ctx context.Context) error {
	_, err := h.ssm.DeleteParameter(ctx, &ssm.DeleteParameterInput{Name: aws.String(h.spec.Params[paramSecret])})
	var nf *ssmtypes.ParameterNotFound
	if errors.As(err, &nf) {
		return nil
	}
	return err
}

// Prepare logs the Docker CLI in to ECR.
func (h *host) Prepare(ctx context.Context, docker supervisor.Runner) error {
	registry := h.spec.Params[paramRegistry]
	if registry == "" {
		return nil
	}
	out, err := h.ecr.GetAuthorizationToken(ctx, &ecr.GetAuthorizationTokenInput{})
	if err != nil || len(out.AuthorizationData) == 0 {
		return fmt.Errorf("aws-ec2 host: ecr token: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(aws.ToString(out.AuthorizationData[0].AuthorizationToken))
	if err != nil {
		return err
	}
	_, pass, _ := strings.Cut(string(raw), ":")
	_, err = docker(ctx, nil, []byte(pass), "login", "--username", "AWS", "--password-stdin", registry)
	return err
}

// StopSelf stops this instance (the instance role may stop machines only).
func (h *host) StopSelf(ctx context.Context) error {
	md, err := h.imds.GetMetadata(ctx, &imds.GetMetadataInput{Path: "instance-id"})
	if err != nil {
		return fmt.Errorf("aws-ec2 host: instance id: %w", err)
	}
	b, err := io.ReadAll(md.Content)
	md.Content.Close()
	if err != nil {
		return err
	}
	_, err = h.ec2.StopInstances(ctx, &ec2.StopInstancesInput{InstanceIds: []string{strings.TrimSpace(string(b))}})
	return err
}

// growCmd runs a command on the host (variable for tests).
var growCmd = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// GrowDisk implements supervisor.DiskGrower: after ResizeDisk enlarged
// the root volume, grow its partition and file system (no-ops otherwise).
func (h *host) GrowDisk(ctx context.Context) error {
	src, err := growCmd(ctx, "findmnt", "-n", "-o", "SOURCE,FSTYPE", "/")
	if err != nil {
		return fmt.Errorf("findmnt: %v: %s", err, src)
	}
	f := strings.Fields(string(src))
	if len(f) != 2 {
		return fmt.Errorf("findmnt: unexpected %q", src)
	}
	dev, fstype := f[0], f[1]
	disk, part := splitPartition(dev)
	if part == "" {
		return nil
	}
	// growpart exits 1 with NOCHANGE when the partition fills the disk.
	if out, err := growCmd(ctx, "growpart", disk, part); err != nil && !strings.Contains(string(out), "NOCHANGE") {
		return fmt.Errorf("growpart: %v: %s", err, out)
	} else if err != nil {
		return nil
	}
	var out []byte
	switch fstype {
	case "xfs":
		out, err = growCmd(ctx, "xfs_growfs", "-d", "/")
	case "ext4":
		out, err = growCmd(ctx, "resize2fs", dev)
	default:
		return fmt.Errorf("cannot grow %s file systems", fstype)
	}
	if err != nil {
		return fmt.Errorf("grow %s: %v: %s", fstype, err, out)
	}
	return nil
}

// splitPartition splits /dev/nvme0n1p1 into /dev/nvme0n1 and 1 (also
// /dev/xvda1).
func splitPartition(dev string) (disk, part string) {
	i := len(dev)
	for i > 0 && dev[i-1] >= '0' && dev[i-1] <= '9' {
		i--
	}
	if i == len(dev) || i == 0 {
		return dev, ""
	}
	disk, part = dev[:i], dev[i:]
	if strings.Contains(disk, "nvme") || strings.Contains(disk, "mmcblk") {
		if !strings.HasSuffix(disk, "p") {
			return dev, "" // a whole disk such as /dev/nvme0n1
		}
		disk = strings.TrimSuffix(disk, "p")
	}
	return disk, part
}
