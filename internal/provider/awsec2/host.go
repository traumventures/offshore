package awsec2

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/traumventures/cloudsourced/internal/supervisor"
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
	return "/cloudsourced/" + stack + "/machines/" + machineID
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
