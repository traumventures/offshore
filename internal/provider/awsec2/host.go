package awsec2

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/traumventures/cloudsourced/internal/blobcache"
	"github.com/traumventures/cloudsourced/internal/machine"
	"github.com/traumventures/cloudsourced/internal/supervisor"
)

// Spec parameters of the supervisor on EC2.
const (
	paramRegion   = "region"
	paramSecret   = "secret_parameter" // SSM SecureString with the secret server environment
	paramRegistry = "registry"         // ECR registry of the environment image
	paramSccache  = "sccache_s3"       // bucket/prefix of the compiler cache
	// sccachePort is the host's compiler cache port on the Docker bridge.
	sccachePort = 7480
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

// CompilerCacheStore keeps compiler cache objects in the stack's bucket
// below cache/ (deleted after a day by the bucket's lifecycle rule).
func (h *host) CompilerCacheStore(ctx context.Context) (blobcache.Store, error) {
	loc := h.spec.Params[paramSccache]
	if loc == "" {
		return nil, nil
	}
	bucket, prefix, err := blobcache.ParseS3Location(loc)
	if err != nil {
		return nil, err
	}
	return blobcache.NewS3Store(s3.NewFromConfig(h.cfg), bucket, prefix, blobcache.MaxTTL), nil
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

// Interruption reports a pending spot interruption (instance-action) or a
// rebalance recommendation from the instance metadata service; nil when
// there is neither (always for on-demand instances).
func (h *host) Interruption(ctx context.Context) (*machine.DrainNotice, error) {
	return spotNotice(ctx, h.metadata, time.Now())
}

// metadata reads an instance metadata path; found is false on 404.
func (h *host) metadata(ctx context.Context, path string) (b []byte, found bool, err error) {
	md, err := h.imds.GetMetadata(ctx, &imds.GetMetadataInput{Path: path})
	if err != nil {
		var re interface{ HTTPStatusCode() int }
		if errors.As(err, &re) && re.HTTPStatusCode() == http.StatusNotFound {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer md.Content.Close()
	b, err = io.ReadAll(io.LimitReader(md.Content, 1<<16))
	return b, err == nil, err
}

// spotNotice turns the metadata service's spot notices into a drain
// notice. An interruption (two minutes' warning, action stop or
// terminate) wins over a rebalance recommendation (elevated risk, no
// deadline).
func spotNotice(ctx context.Context, get func(context.Context, string) ([]byte, bool, error), now time.Time) (*machine.DrainNotice, error) {
	b, found, err := get(ctx, "spot/instance-action")
	if err != nil {
		return nil, err
	}
	if found {
		var ia struct {
			Action string `json:"action"`
			Time   string `json:"time"`
		}
		if err := json.Unmarshal(b, &ia); err != nil {
			return nil, fmt.Errorf("spot/instance-action: %w", err)
		}
		n := &machine.DrainNotice{Reason: machine.DrainSpotInterruption, Action: ia.Action, Noticed: now.Unix()}
		if t, err := time.Parse(time.RFC3339, ia.Time); err == nil {
			n.Deadline = t.Unix()
		} else {
			n.Deadline = now.Add(2 * time.Minute).Unix()
		}
		return n, nil
	}
	b, found, err = get(ctx, "events/recommendations/rebalance")
	if err != nil || !found {
		return nil, err
	}
	var rb struct {
		NoticeTime string `json:"noticeTime"`
	}
	if err := json.Unmarshal(b, &rb); err != nil {
		return nil, fmt.Errorf("events/recommendations/rebalance: %w", err)
	}
	n := &machine.DrainNotice{Reason: machine.DrainSpotRebalance, Noticed: now.Unix()}
	if t, err := time.Parse(time.RFC3339, rb.NoticeTime); err == nil {
		n.Noticed = t.Unix()
	}
	return n, nil
}
