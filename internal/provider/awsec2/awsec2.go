// Package awsec2 runs machines as EC2 instances. Each instance runs Docker
// and the host agent ("cloudsourced host"), which applies the same machine
// container layout as the local-docker backend (machine.Layout: server with
// FUSE, services in its network namespace, cache volumes on the instance
// disk), so behaviour matches the reference backend. User data carries only
// the public machine spec; the secret server environment travels through
// an SSM SecureString parameter that the agent reads once and deletes.
//
// Shared infrastructure (security group, instance role, artifact bucket,
// image repository, reaper) is a CloudFormation stack, created on first use.
// Environment images are pushed to the stack's ECR repository by the image
// pipeline (internal/image) before an instance is launched.
package awsec2

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"

	"github.com/traumventures/cloudsourced/internal/image"
	"github.com/traumventures/cloudsourced/internal/provider"
)

//go:embed stack.yaml
var stackTemplate string

// templateVersion must match the TemplateVersion output of stack.yaml.
const templateVersion = "6"

const (
	serverPort = 7443
	tagProject = "project"
	project    = "cloudsourced"
)

func init() {
	provider.Register("aws-ec2", open)
}

// EC2 is the aws-ec2 provider.
type EC2 struct {
	region     string
	stack      string
	allow      []string // CIDRs allowed to reach machines
	diskGB     int32
	amis       map[string]string // optional fixed AMI per architecture
	docker     []string
	binaries   map[string]string // server binary per architecture
	subnet     string
	spotMax    string // optional spot max price (USD per hour)
	cfg        aws.Config
	ec2        *ec2.Client
	ecr        *ecr.Client
	s3         *s3.Client
	cf         *cloudformation.Client
	ssm        *ssm.Client
	infraMu    sync.Mutex
	infra      *infra
	binMu      sync.Mutex
	binKeys    map[string]string // architecture -> uploaded binary key
	ingressSet bool

	builderOnce sync.Once
	builder     *image.DockerBuilder
}

var _ provider.ImageHost = (*EC2)(nil)

type infra struct {
	sg, profile, bucket, repo string
}

func open(opts map[string]any) (provider.Provider, error) {
	e := &EC2{stack: project, diskGB: 100, docker: []string{"docker"}, amis: map[string]string{}, binKeys: map[string]string{}}
	e.region = provider.String(opts, "region", "")
	if v := provider.String(opts, "stack", ""); v != "" {
		e.stack = v
	}
	e.allow = provider.StringList(opts["allow_cidrs"])
	e.amis[archAMD64] = provider.String(opts, "ami", "")
	e.amis[archARM64] = provider.String(opts, "ami_arm64", "")
	e.subnet = provider.String(opts, "subnet", "")
	e.spotMax = provider.String(opts, "spot_max_price", "")
	if v := provider.StringList(opts["docker_command"]); len(v) > 0 {
		e.docker = v
	}
	switch v := opts["disk_gb"].(type) {
	case int64:
		e.diskGB = int32(v)
	case float64:
		e.diskGB = int32(v)
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	e.binaries = serverBinaries(opts, self, runtime.GOOS, runtime.GOARCH)
	var lopts []func(*awsconfig.LoadOptions) error
	if p := provider.String(opts, "profile", ""); p != "" {
		lopts = append(lopts, awsconfig.WithSharedConfigProfile(p))
	}
	if e.region != "" {
		lopts = append(lopts, awsconfig.WithRegion(e.region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(), lopts...)
	if err != nil {
		return nil, fmt.Errorf("aws-ec2: %w", err)
	}
	if cfg.Region == "" {
		cfg.Region = "eu-central-1"
	}
	e.region = cfg.Region
	e.cfg = cfg
	e.ec2 = ec2.NewFromConfig(cfg)
	e.ecr = ecr.NewFromConfig(cfg)
	e.s3 = s3.NewFromConfig(cfg)
	e.cf = cloudformation.NewFromConfig(cfg)
	e.ssm = ssm.NewFromConfig(cfg)
	return e, nil
}

func (e *EC2) Name() string { return "aws-ec2" }

func (e *EC2) Caps() provider.Caps {
	// Instances boot in tens of seconds: poll readiness slowly, backing off.
	return provider.Caps{FUSE: true, StopKeepsDisk: true, PollInterval: 2 * time.Second, PollMax: 10 * time.Second}
}

// ensureInfra creates or updates the shared stack and reads its outputs.
func (e *EC2) ensureInfra(ctx context.Context) (*infra, error) {
	e.infraMu.Lock()
	defer e.infraMu.Unlock()
	if e.infra != nil {
		return e.infra, nil
	}
	st, err := e.describeStack(ctx)
	if err != nil {
		return nil, err
	}
	if st == nil || outputs(st)["TemplateVersion"] != templateVersion {
		if err := e.deployStack(ctx, st); err != nil {
			return nil, err
		}
		if st, err = e.describeStack(ctx); err != nil {
			return nil, err
		}
	}
	out := outputs(st)
	in := &infra{sg: out["SecurityGroupId"], profile: out["InstanceProfileArn"], bucket: out["BucketName"], repo: out["RepositoryUri"]}
	if in.sg == "" || in.profile == "" || in.bucket == "" || in.repo == "" {
		return nil, fmt.Errorf("aws-ec2: stack %s lacks outputs (status %s)", e.stack, st.StackStatus)
	}
	if err := e.ensureIngress(ctx, in.sg); err != nil {
		return nil, err
	}
	e.infra = in
	return in, nil
}

func outputs(st *cftypes.Stack) map[string]string {
	m := map[string]string{}
	if st == nil {
		return m
	}
	for _, o := range st.Outputs {
		m[aws.ToString(o.OutputKey)] = aws.ToString(o.OutputValue)
	}
	return m
}

func (e *EC2) describeStack(ctx context.Context) (*cftypes.Stack, error) {
	out, err := e.cf.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{StackName: aws.String(e.stack)})
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") {
			return nil, nil
		}
		return nil, fmt.Errorf("aws-ec2: describe stack: %w", err)
	}
	if len(out.Stacks) == 0 {
		return nil, nil
	}
	return &out.Stacks[0], nil
}

func (e *EC2) defaultVPC(ctx context.Context) (string, error) {
	out, err := e.ec2.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{Filters: []ec2types.Filter{{Name: aws.String("is-default"), Values: []string{"true"}}}})
	if err != nil {
		return "", err
	}
	if len(out.Vpcs) == 0 {
		return "", errors.New("aws-ec2: no default VPC; set the subnet option")
	}
	return aws.ToString(out.Vpcs[0].VpcId), nil
}

func (e *EC2) vpcOfSubnet(ctx context.Context) (string, error) {
	if e.subnet == "" {
		return e.defaultVPC(ctx)
	}
	out, err := e.ec2.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{SubnetIds: []string{e.subnet}})
	if err != nil {
		return "", err
	}
	if len(out.Subnets) == 0 {
		return "", fmt.Errorf("aws-ec2: subnet %s not found", e.subnet)
	}
	return aws.ToString(out.Subnets[0].VpcId), nil
}

func (e *EC2) deployStack(ctx context.Context, st *cftypes.Stack) error {
	vpc, err := e.vpcOfSubnet(ctx)
	if err != nil {
		return err
	}
	params := []cftypes.Parameter{{ParameterKey: aws.String("VpcId"), ParameterValue: aws.String(vpc)}}
	tags := []cftypes.Tag{{Key: aws.String(tagProject), Value: aws.String(project)}}
	caps := []cftypes.Capability{cftypes.CapabilityCapabilityIam}
	if st == nil {
		out, err := e.cf.CreateStack(ctx, &cloudformation.CreateStackInput{
			StackName: aws.String(e.stack), TemplateBody: aws.String(stackTemplate),
			Parameters: params, Tags: tags, Capabilities: caps, OnFailure: cftypes.OnFailureDelete,
		})
		if err != nil {
			return fmt.Errorf("aws-ec2: create stack: %w", err)
		}
		w := cloudformation.NewStackCreateCompleteWaiter(e.cf)
		if err := w.Wait(ctx, &cloudformation.DescribeStacksInput{StackName: aws.String(e.stack)}, 20*time.Minute); err != nil {
			return fmt.Errorf("aws-ec2: creating stack %s failed: %s", e.stack, e.failureReason(ctx, out.StackId))
		}
		return nil
	}
	_, err = e.cf.UpdateStack(ctx, &cloudformation.UpdateStackInput{
		StackName: aws.String(e.stack), TemplateBody: aws.String(stackTemplate),
		Parameters: params, Tags: tags, Capabilities: caps,
	})
	if err != nil {
		if strings.Contains(err.Error(), "No updates are to be performed") {
			return nil
		}
		return fmt.Errorf("aws-ec2: update stack: %w", err)
	}
	w := cloudformation.NewStackUpdateCompleteWaiter(e.cf)
	if err := w.Wait(ctx, &cloudformation.DescribeStacksInput{StackName: aws.String(e.stack)}, 20*time.Minute); err != nil {
		return fmt.Errorf("aws-ec2: updating stack %s failed: %s", e.stack, e.failureReason(ctx, st.StackId))
	}
	return nil
}

// failureReason returns the first resource failure of a stack operation
// that is not merely a consequence of another failure.
func (e *EC2) failureReason(ctx context.Context, stackID *string) string {
	out, err := e.cf.DescribeStackEvents(ctx, &cloudformation.DescribeStackEventsInput{StackName: stackID})
	if err != nil {
		return err.Error()
	}
	reason := "unknown reason (see the stack events)"
	for _, ev := range out.StackEvents { // newest first; keep the oldest real cause
		r := aws.ToString(ev.ResourceStatusReason)
		if strings.HasSuffix(string(ev.ResourceStatus), "FAILED") && r != "" && !strings.Contains(r, "Resource creation cancelled") {
			reason = aws.ToString(ev.LogicalResourceId) + ": " + r
		}
	}
	return reason
}

// ensureIngress allows QUIC from the configured CIDRs; by default from the
// public address of this orchestrator.
func (e *EC2) ensureIngress(ctx context.Context, sg string) error {
	cidrs := e.allow
	if len(cidrs) == 0 {
		ip, err := publicIP(ctx)
		if err != nil {
			return fmt.Errorf("aws-ec2: detecting the public address for allow_cidrs: %w", err)
		}
		cidrs = []string{ip + "/32"}
	}
	var ranges []ec2types.IpRange
	for _, c := range cidrs {
		ranges = append(ranges, ec2types.IpRange{CidrIp: aws.String(c), Description: aws.String("cloudsourced clients")})
	}
	for _, r := range ranges {
		_, err := e.ec2.AuthorizeSecurityGroupIngress(ctx, &ec2.AuthorizeSecurityGroupIngressInput{
			GroupId: aws.String(sg),
			IpPermissions: []ec2types.IpPermission{{
				IpProtocol: aws.String("udp"), FromPort: aws.Int32(serverPort), ToPort: aws.Int32(serverPort),
				IpRanges: []ec2types.IpRange{r},
			}},
		})
		if err != nil && !strings.Contains(err.Error(), "InvalidPermission.Duplicate") {
			return fmt.Errorf("aws-ec2: security group ingress %s: %w", aws.ToString(r.CidrIp), err)
		}
	}
	return nil
}

func publicIP(ctx context.Context) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://checkip.amazonaws.com", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// --- images ---

// ImagePlatform is the platform of instances of a size whose architecture
// is unknown here: amd64. The fleet knows the architecture of its sizes
// (fleet.size arch) and uses that instead.
func (e *EC2) ImagePlatform(string) image.Platform { return image.LinuxAMD64 }

// ImageRegistry is the ECR repository of the stack.
func (e *EC2) ImageRegistry() image.Registry {
	return &image.ECR{Client: e.ecr, CLI: e.docker, Repository: func(ctx context.Context) (string, error) {
		in, err := e.ensureInfra(ctx)
		if err != nil {
			return "", err
		}
		return in.repo, nil
	}}
}

// ImageBuilder builds with the configured Docker CLI (docker_command).
func (e *EC2) ImageBuilder() image.Builder {
	e.builderOnce.Do(func() { e.builder = &image.DockerBuilder{CLI: e.docker} })
	return e.builder
}

// machineImage returns the ECR reference of spec's image: the pipeline
// passes it resolved; a bare environment tag is mapped to the repository.
func machineImage(repo string, spec provider.MachineSpec) string {
	if strings.HasPrefix(spec.Image, repo+":") {
		return spec.Image
	}
	return image.RemoteTag(repo, spec.Image, image.Platform("linux/"+archOf(spec.Arch)))
}

// ensureBinary uploads the server binary of arch (content addressed) once.
func (e *EC2) ensureBinary(ctx context.Context, bucket, arch string) (string, error) {
	e.binMu.Lock()
	defer e.binMu.Unlock()
	if k := e.binKeys[arch]; k != "" {
		return k, nil
	}
	path := e.binaries[arch]
	if err := checkBinary(path, arch); err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	key := binaryKey(arch, data)
	if _, err := e.s3.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}); err != nil {
		if _, err := e.s3.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(data)}); err != nil {
			return "", fmt.Errorf("aws-ec2: upload server binary: %w", err)
		}
	}
	e.binKeys[arch] = key
	return key, nil
}

// binaryKey is the content addressed S3 key of a server binary.
func binaryKey(arch string, data []byte) string {
	sum := sha256.Sum256(data)
	return "bin/cloudsourced-linux-" + arch + "-" + hex.EncodeToString(sum[:])[:16]
}

func (e *EC2) amiID(ctx context.Context, arch string) (string, error) {
	if a := e.amis[arch]; a != "" {
		return a, nil
	}
	out, err := e.ssm.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(amiParameter(arch))})
	if err != nil {
		return "", fmt.Errorf("aws-ec2: resolve Amazon Linux 2023 %s AMI: %w", arch, err)
	}
	return aws.ToString(out.Parameter.Value), nil
}

func unix(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }

// Create launches an instance whose user data installs Docker and starts
// the machine container.
func (e *EC2) Create(ctx context.Context, spec provider.MachineSpec) (provider.Machine, error) {
	in, err := e.ensureInfra(ctx)
	if err != nil {
		return provider.Machine{}, err
	}
	arch := archOf(spec.Arch)
	key, err := e.ensureBinary(ctx, in.bucket, arch)
	if err != nil {
		return provider.Machine{}, err
	}
	ami, err := e.amiID(ctx, arch)
	if err != nil {
		return provider.Machine{}, err
	}
	ud, err := userData(spec, e.region, e.stack, in, key, machineImage(in.repo, spec))
	if err != nil {
		return provider.Machine{}, err
	}
	if len(ud) > 16000 {
		return provider.Machine{}, fmt.Errorf("aws-ec2: user data too large (%d bytes)", len(ud))
	}
	tags := []ec2types.Tag{
		{Key: aws.String("Name"), Value: aws.String("cloudsourced-" + spec.ID)},
		{Key: aws.String(tagProject), Value: aws.String(project)},
		{Key: aws.String(provider.LabelOwner), Value: aws.String(spec.Owner)},
		{Key: aws.String(provider.LabelMachine), Value: aws.String(spec.ID)},
		{Key: aws.String(provider.LabelKind), Value: aws.String("machine")},
		{Key: aws.String(provider.LabelEnvHash), Value: aws.String(spec.EnvHash)},
		{Key: aws.String(provider.LabelCreatedAt), Value: aws.String(unix(time.Now()))},
		{Key: aws.String(provider.LabelExpiresAt), Value: aws.String(unix(spec.ExpiresAt))},
		{Key: aws.String(provider.LabelCPU), Value: aws.String(strconv.FormatFloat(spec.CPU, 'f', -1, 64))},
		{Key: aws.String(provider.LabelMemory), Value: aws.String(strconv.FormatInt(spec.Memory, 10))},
		{Key: aws.String(provider.LabelFinger), Value: aws.String(spec.Finger)},
		{Key: aws.String(provider.LabelSize), Value: aws.String(spec.Size)},
		{Key: aws.String(provider.LabelArch), Value: aws.String(arch)},
	}
	input := &ec2.RunInstancesInput{
		ImageId:  aws.String(ami),
		MinCount: aws.Int32(1), MaxCount: aws.Int32(1),
		UserData:           aws.String(base64.StdEncoding.EncodeToString([]byte(ud))),
		IamInstanceProfile: &ec2types.IamInstanceProfileSpecification{Arn: aws.String(in.profile)},
		// Shutting down at expiry terminates an on-demand instance (spot:
		// see applyAttempt).
		InstanceInitiatedShutdownBehavior: ec2types.ShutdownBehaviorTerminate,
		MetadataOptions: &ec2types.InstanceMetadataOptionsRequest{
			HttpTokens: ec2types.HttpTokensStateRequired,
			// 1: only the host reaches the metadata service, never containers.
			HttpPutResponseHopLimit: aws.Int32(1),
			HttpEndpoint:            ec2types.InstanceMetadataEndpointStateEnabled,
		},
		BlockDeviceMappings: []ec2types.BlockDeviceMapping{{
			DeviceName: aws.String("/dev/xvda"),
			Ebs: &ec2types.EbsBlockDevice{VolumeSize: aws.Int32(e.diskGB), VolumeType: ec2types.VolumeTypeGp3,
				DeleteOnTermination: aws.Bool(true), Encrypted: aws.Bool(true)},
		}},
		NetworkInterfaces: []ec2types.InstanceNetworkInterfaceSpecification{{
			DeviceIndex: aws.Int32(0), AssociatePublicIpAddress: aws.Bool(true),
			Groups: []string{in.sg}, DeleteOnTermination: aws.Bool(true),
		}},
		TagSpecifications: []ec2types.TagSpecification{
			{ResourceType: ec2types.ResourceTypeInstance, Tags: tags},
			{ResourceType: ec2types.ResourceTypeVolume, Tags: tags},
			{ResourceType: ec2types.ResourceTypeNetworkInterface, Tags: tags},
		},
	}
	if e.subnet != "" {
		input.NetworkInterfaces[0].SubnetId = aws.String(e.subnet)
	}
	// The secret environment goes through SSM: the host agent reads it
	// with the instance role and deletes it.
	if err := e.putSecrets(ctx, spec); err != nil {
		return provider.Machine{}, err
	}
	out, how, failed, err := launch(ctx, e.ec2.RunInstances, input, spec, e.spotMax, time.Now())
	if err != nil {
		e.deleteSecrets(context.Background(), spec.ID)
		return provider.Machine{}, err
	}
	if len(failed) > 0 && how.spot {
		slog.Info("aws-ec2: launched on a later spot instance type", "machine", spec.ID, "size", spec.Size, "instance_type", how.instanceType, "failed", strings.Join(failed, "; "))
	}
	if spec.Spot && !how.spot {
		slog.Warn("aws-ec2: no spot capacity, launched on-demand", "machine", spec.ID, "size", spec.Size, "instance_type", how.instanceType, "spot_attempts", strings.Join(failed, "; "))
	}
	id := aws.ToString(out.Instances[0].InstanceId)
	if err := e.waitRunning(ctx, id); err != nil {
		e.terminate(context.Background(), out.Instances)
		e.deleteSecrets(context.Background(), spec.ID)
		return provider.Machine{}, err
	}
	return e.byMachine(ctx, spec.ID)
}

func (e *EC2) putSecrets(ctx context.Context, spec provider.MachineSpec) error {
	b, err := json.Marshal(spec.SecretEnv)
	if err != nil {
		return err
	}
	in := &ssm.PutParameterInput{
		Name: aws.String(secretParameter(e.stack, spec.ID)), Type: ssmtypes.ParameterTypeSecureString,
		Value:       aws.String(string(b)),
		Description: aws.String("cloudsourced machine secrets, deleted by the machine after boot"),
		Tags:        []ssmtypes.Tag{{Key: aws.String(tagProject), Value: aws.String(project)}, {Key: aws.String(provider.LabelMachine), Value: aws.String(spec.ID)}},
	}
	if len(b) > 4096 {
		in.Tier = ssmtypes.ParameterTierAdvanced
	}
	if _, err := e.ssm.PutParameter(ctx, in); err != nil {
		return fmt.Errorf("aws-ec2: store machine secrets: %w", err)
	}
	return nil
}

func (e *EC2) deleteSecrets(ctx context.Context, id string) error {
	_, err := e.ssm.DeleteParameter(ctx, &ssm.DeleteParameterInput{Name: aws.String(secretParameter(e.stack, id))})
	var nf *ssmtypes.ParameterNotFound
	if err != nil && !errors.As(err, &nf) {
		return fmt.Errorf("aws-ec2: delete machine secrets: %w", err)
	}
	return nil
}

func (e *EC2) waitRunning(ctx context.Context, instanceID string) error {
	w := ec2.NewInstanceRunningWaiter(e.ec2)
	return w.Wait(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{instanceID}}, 10*time.Minute)
}

// instances returns the EC2 instances of owner (optionally one machine).
func (e *EC2) instances(ctx context.Context, owner, machine string) ([]ec2types.Instance, error) {
	filters := []ec2types.Filter{
		{Name: aws.String("tag:" + provider.LabelKind), Values: []string{"machine"}},
		{Name: aws.String("instance-state-name"), Values: []string{"pending", "running", "stopping", "stopped", "shutting-down"}},
	}
	if owner != "" {
		filters = append(filters, ec2types.Filter{Name: aws.String("tag:" + provider.LabelOwner), Values: []string{owner}})
	}
	if machine != "" {
		filters = append(filters, ec2types.Filter{Name: aws.String("tag:" + provider.LabelMachine), Values: []string{machine}})
	}
	var out []ec2types.Instance
	p := ec2.NewDescribeInstancesPaginator(e.ec2, &ec2.DescribeInstancesInput{Filters: filters})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("aws-ec2: describe instances: %w", err)
		}
		for _, r := range page.Reservations {
			out = append(out, r.Instances...)
		}
	}
	return out, nil
}

func toMachine(i ec2types.Instance) provider.Machine {
	l := map[string]string{}
	for _, t := range i.Tags {
		l[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	m := provider.Machine{ID: l[provider.LabelMachine], EnvHash: l[provider.LabelEnvHash], Labels: l, Fingerprint: l[provider.LabelFinger]}
	l["instance-id"] = aws.ToString(i.InstanceId)
	l["instance-type"] = string(i.InstanceType)
	if i.InstanceLifecycle == ec2types.InstanceLifecycleTypeSpot && l[provider.LabelMarket] == "" {
		l[provider.LabelMarket] = provider.MarketSpot
	}
	m.CPU, _ = strconv.ParseFloat(l[provider.LabelCPU], 64)
	m.Memory, _ = strconv.ParseInt(l[provider.LabelMemory], 10, 64)
	if n, err := strconv.ParseInt(l[provider.LabelCreatedAt], 10, 64); err == nil {
		m.CreatedAt = time.Unix(n, 0)
	}
	if n, err := strconv.ParseInt(l[provider.LabelExpiresAt], 10, 64); err == nil {
		m.ExpiresAt = time.Unix(n, 0)
	}
	switch i.State.Name {
	case ec2types.InstanceStateNameRunning:
		m.State = provider.Running
	case ec2types.InstanceStateNamePending:
		m.State = provider.Starting
	case ec2types.InstanceStateNameStopping, ec2types.InstanceStateNameStopped:
		m.State = provider.Stopped
		if spotInterrupted(i) {
			// Only EC2 can restart it: the orchestrator destroys it and
			// places the work elsewhere.
			m.State = provider.Failed
			l["interrupted"] = "spot"
		}
	default:
		m.State = provider.Failed
	}
	if m.State == provider.Running && i.PublicIpAddress != nil {
		m.Addr = aws.ToString(i.PublicIpAddress) + ":" + strconv.Itoa(serverPort)
	}
	return m
}

func (e *EC2) byMachine(ctx context.Context, id string) (provider.Machine, error) {
	is, err := e.instances(ctx, "", id)
	if err != nil {
		return provider.Machine{}, err
	}
	if len(is) == 0 {
		return provider.Machine{}, fmt.Errorf("aws-ec2: machine %s not found", id)
	}
	return toMachine(is[0]), nil
}

func (e *EC2) instanceID(ctx context.Context, id string) (string, error) {
	m, err := e.byMachine(ctx, id)
	if err != nil {
		return "", err
	}
	return m.Labels["instance-id"], nil
}

// Start starts a stopped instance; a boot unit restarts the containers.
func (e *EC2) Start(ctx context.Context, id string) (provider.Machine, error) {
	iid, err := e.instanceID(ctx, id)
	if err != nil {
		return provider.Machine{}, err
	}
	// A machine that is still stopping cannot be started yet.
	if err := ec2.NewInstanceStoppedWaiter(e.ec2).Wait(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{iid}}, 5*time.Minute); err != nil {
		m, _ := e.byMachine(ctx, id)
		if m.State != provider.Running && m.State != provider.Starting {
			return provider.Machine{}, err
		}
	}
	if _, err := e.ec2.StartInstances(ctx, &ec2.StartInstancesInput{InstanceIds: []string{iid}}); err != nil {
		return provider.Machine{}, fmt.Errorf("aws-ec2: start %s: %w", id, err)
	}
	if err := e.waitRunning(ctx, iid); err != nil {
		return provider.Machine{}, err
	}
	return e.byMachine(ctx, id)
}

// Stop stops the instance; its disk (and with it the caches) is kept.
func (e *EC2) Stop(ctx context.Context, id string) error {
	iid, err := e.instanceID(ctx, id)
	if err != nil {
		return err
	}
	_, err = e.ec2.StopInstances(ctx, &ec2.StopInstancesInput{InstanceIds: []string{iid}})
	return err
}

// Destroy terminates the instance; its disk is deleted with it.
func (e *EC2) Destroy(ctx context.Context, id string) error {
	is, err := e.instances(ctx, "", id)
	if err != nil {
		return err
	}
	if err := e.terminate(ctx, is); err != nil {
		return err
	}
	// Normally the agent has deleted it already.
	return e.deleteSecrets(ctx, id)
}

// terminate cancels the spot requests of instances, so that persistent
// requests do not launch replacements, and terminates the instances.
func (e *EC2) terminate(ctx context.Context, is []ec2types.Instance) error {
	ids, reqs := instanceIDs(is)
	if len(reqs) > 0 {
		_, err := e.ec2.CancelSpotInstanceRequests(ctx, &ec2.CancelSpotInstanceRequestsInput{SpotInstanceRequestIds: reqs})
		if err != nil && !strings.Contains(err.Error(), "InvalidSpotInstanceRequestID.NotFound") {
			return fmt.Errorf("aws-ec2: cancel spot request: %w", err)
		}
	}
	if len(ids) > 0 {
		if _, err := e.ec2.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: ids}); err != nil {
			return err
		}
	}
	return nil
}

// instanceIDs returns the instance and spot request IDs of instances.
func instanceIDs(is []ec2types.Instance) (ids, spotRequests []string) {
	for _, i := range is {
		ids = append(ids, aws.ToString(i.InstanceId))
		if r := aws.ToString(i.SpotInstanceRequestId); r != "" {
			spotRequests = append(spotRequests, r)
		}
	}
	return ids, spotRequests
}

// List returns the machines of owner.
func (e *EC2) List(ctx context.Context, owner string) ([]provider.Machine, error) {
	is, err := e.instances(ctx, owner, "")
	if err != nil {
		return nil, err
	}
	var ms []provider.Machine
	for _, i := range is {
		ms = append(ms, toMachine(i))
	}
	return ms, nil
}

// Volumes: caches live on the instance disk and die with the instance.
func (e *EC2) Volumes(context.Context, string) ([]provider.Volume, error) { return nil, nil }

func (e *EC2) DeleteVolume(context.Context, string) error { return nil }
