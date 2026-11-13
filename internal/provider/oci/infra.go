package oci

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/oracle/oci-go-sdk/v65/artifacts"
	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/core"
	"github.com/oracle/oci-go-sdk/v65/identity"
	"github.com/oracle/oci-go-sdk/v65/objectstorage"
)

// Shared infrastructure, created on first use and found again by name:
//
//   - a compartment (default "offshore" below the tenancy root) that
//     holds everything else;
//   - a VCN 10.77.0.0/16 with an internet gateway, a default route to it
//     and one regional public subnet; the VCN's default security list
//     allows UDP 7443 (QUIC) from allow_cidrs only (no SSH);
//   - a private Object Storage bucket for server binaries, machine
//     secrets (secrets/, deleted by the supervisor after boot) and
//     compiler cache objects (cache/, deleted after a day);
//   - a container repository for environment images;
//   - a dynamic group of the compartment's instances and a policy that
//     lets them read the bucket and the repository and stop or terminate
//     instances of the compartment (iam = false skips both; an
//     administrator then creates them).
type infra struct {
	tenancy     string
	compartment string
	ad          string // availability domain of machines
	subnet      string
	namespace   string // Object Storage namespace
	bucket      string
	repo        string // registry/namespace/repository
}

const (
	vcnCIDR    = "10.77.0.0/16"
	subnetCIDR = "10.77.0.0/20"
	allCIDR    = "0.0.0.0/0"
)

func freeform() map[string]string { return map[string]string{tagProject: project} }

// ensureInfra creates or finds the shared infrastructure once.
func (o *OCI) ensureInfra(ctx context.Context) (*infra, error) {
	o.infraMu.Lock()
	defer o.infraMu.Unlock()
	if o.infra != nil {
		return o.infra, nil
	}
	tenancy, err := o.cfg.TenancyOCID()
	if err != nil {
		return nil, fmt.Errorf("oci: tenancy: %w", err)
	}
	in := &infra{tenancy: tenancy}
	if in.compartment, err = o.ensureCompartment(ctx, tenancy); err != nil {
		return nil, err
	}
	if in.ad, err = o.availabilityDomain(ctx, in.compartment); err != nil {
		return nil, err
	}
	if in.subnet, err = o.ensureNetwork(ctx, in.compartment); err != nil {
		return nil, err
	}
	if in.namespace, err = o.ensureBucket(ctx, in.compartment); err != nil {
		return nil, err
	}
	in.bucket = o.bucket
	if in.repo, err = o.ensureRepository(ctx, in.compartment, in.namespace); err != nil {
		return nil, err
	}
	if o.iam {
		if err := o.ensureIAM(ctx, tenancy, in); err != nil {
			return nil, err
		}
	}
	o.ensureLifecycle(ctx, in)
	o.infra = in
	return in, nil
}

// ensureLifecycle expires cache objects after the pool's [storage]
// retention (whole days, default one) and left-over secrets after a day.
// It needs the objectstorage service policy of ensureIAM, which may take a
// while to apply: failures are not fatal (the cache store never serves
// objects older than the retention), it is retried by the next process.
func (o *OCI) ensureLifecycle(ctx context.Context, in *infra) {
	_, err := o.objects.PutObjectLifecyclePolicy(ctx, objectstorage.PutObjectLifecyclePolicyRequest{
		NamespaceName: common.String(in.namespace), BucketName: common.String(o.bucket),
		PutObjectLifecyclePolicyDetails: objectstorage.PutObjectLifecyclePolicyDetails{Items: lifecycleRules(o.retentionDays)},
	})
	if err != nil {
		o.log.Warn("oci: bucket lifecycle policy (cache objects are not deleted automatically)", "err", err)
	}
}

// lifecycleRules are the bucket's lifecycle rules: cache/ objects expire
// after the retention, secrets/ after a day.
func lifecycleRules(retentionDays int64) []objectstorage.ObjectLifecycleRule {
	rule := func(name string, days int64, prefix string) objectstorage.ObjectLifecycleRule {
		return objectstorage.ObjectLifecycleRule{
			Name: common.String(name), Action: common.String("DELETE"), TimeAmount: common.Int64(days),
			TimeUnit: objectstorage.ObjectLifecycleRuleTimeUnitDays, IsEnabled: common.Bool(true),
			ObjectNameFilter: &objectstorage.ObjectNameFilter{InclusionPrefixes: []string{prefix}},
		}
	}
	return []objectstorage.ObjectLifecycleRule{rule("expire-cache", max(retentionDays, 1), "cache/"), rule("expire-secrets", 1, "secrets/")}
}

// ensureCompartment returns the compartment OCID: the compartment option
// if it is an OCID, else the compartment of that name below the tenancy
// root, created if missing.
func (o *OCI) ensureCompartment(ctx context.Context, tenancy string) (string, error) {
	if strings.HasPrefix(o.compartment, "ocid1.") {
		return o.compartment, nil
	}
	resp, err := o.identity.ListCompartments(ctx, identity.ListCompartmentsRequest{
		CompartmentId: common.String(tenancy), Name: common.String(o.compartment),
		LifecycleState: identity.CompartmentLifecycleStateActive})
	if err != nil {
		return "", fmt.Errorf("oci: list compartments: %w", err)
	}
	if len(resp.Items) > 0 {
		return *resp.Items[0].Id, nil
	}
	cr, err := o.identity.CreateCompartment(ctx, identity.CreateCompartmentRequest{CreateCompartmentDetails: identity.CreateCompartmentDetails{
		CompartmentId: common.String(tenancy), Name: common.String(o.compartment),
		Description: common.String("offshore machines and their infrastructure"), FreeformTags: freeform(),
	}})
	if err != nil {
		return "", fmt.Errorf("oci: create compartment %s: %w", o.compartment, err)
	}
	id := *cr.Id
	// A new compartment takes a while to become usable everywhere.
	err = o.poll(ctx, 5*time.Minute, func() (bool, error) {
		r, err := o.identity.GetCompartment(ctx, identity.GetCompartmentRequest{CompartmentId: common.String(id)})
		if err != nil {
			return false, err
		}
		return r.LifecycleState == identity.CompartmentLifecycleStateActive, nil
	})
	if err != nil {
		return "", fmt.Errorf("oci: compartment %s: %w", o.compartment, err)
	}
	return id, nil
}

func (o *OCI) availabilityDomain(ctx context.Context, compartment string) (string, error) {
	resp, err := o.identity.ListAvailabilityDomains(ctx, identity.ListAvailabilityDomainsRequest{CompartmentId: common.String(compartment)})
	if err != nil {
		return "", fmt.Errorf("oci: availability domains: %w", err)
	}
	var names []string
	for _, ad := range resp.Items {
		names = append(names, *ad.Name)
	}
	return pickAD(names, o.ad)
}

// pickAD returns the availability domain named (or suffixed, e.g. "AD-2")
// by want, default the first one.
func pickAD(names []string, want string) (string, error) {
	if len(names) == 0 {
		return "", fmt.Errorf("oci: no availability domains")
	}
	if want == "" {
		return names[0], nil
	}
	for _, n := range names {
		if n == want || strings.HasSuffix(n, ":"+want) || strings.HasSuffix(n, want) {
			return n, nil
		}
	}
	return "", fmt.Errorf("oci: availability domain %q not found (%s)", want, strings.Join(names, ", "))
}

// ensureNetwork returns the machine subnet (the subnet option, or the
// offshore VCN's public subnet, created with the VCN) and makes sure
// its security list admits QUIC from allow_cidrs.
func (o *OCI) ensureNetwork(ctx context.Context, compartment string) (string, error) {
	cidrs, err := o.allowCIDRs(ctx)
	if err != nil {
		return "", err
	}
	if o.subnet != "" {
		sr, err := o.network.GetSubnet(ctx, core.GetSubnetRequest{SubnetId: common.String(o.subnet)})
		if err != nil {
			return "", fmt.Errorf("oci: subnet %s: %w", o.subnet, err)
		}
		if len(sr.SecurityListIds) == 0 {
			return "", fmt.Errorf("oci: subnet %s has no security list", o.subnet)
		}
		return o.subnet, o.ensureIngress(ctx, sr.SecurityListIds[0], cidrs, false, o.ingress.Explicit())
	}
	var vcn core.Vcn
	created := false
	err = o.poll(ctx, 2*time.Minute, func() (bool, error) {
		// Right after creating the compartment, it may not be visible yet.
		vr, err := o.network.ListVcns(ctx, core.ListVcnsRequest{CompartmentId: common.String(compartment), DisplayName: common.String(project)})
		if notFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		for _, v := range vr.Items {
			if v.LifecycleState == core.VcnLifecycleStateAvailable || v.LifecycleState == core.VcnLifecycleStateProvisioning {
				vcn = v
			}
		}
		return true, nil
	})
	if err != nil {
		return "", fmt.Errorf("oci: list VCNs: %w", err)
	}
	if vcn.Id == nil {
		cr, err := o.network.CreateVcn(ctx, core.CreateVcnRequest{CreateVcnDetails: core.CreateVcnDetails{
			CompartmentId: common.String(compartment), CidrBlocks: []string{vcnCIDR},
			DisplayName: common.String(project), DnsLabel: common.String(project), FreeformTags: freeform(),
		}})
		if err != nil {
			return "", fmt.Errorf("oci: create VCN: %w", err)
		}
		vcn, created = cr.Vcn, true
	}
	if err := o.ensureGateway(ctx, compartment, vcn); err != nil {
		return "", err
	}
	if err := o.ensureIngress(ctx, *vcn.DefaultSecurityListId, cidrs, created, o.ingress.Explicit()); err != nil {
		return "", err
	}
	sr, err := o.network.ListSubnets(ctx, core.ListSubnetsRequest{CompartmentId: common.String(compartment), VcnId: vcn.Id, DisplayName: common.String(project)})
	if err != nil {
		return "", fmt.Errorf("oci: list subnets: %w", err)
	}
	for _, s := range sr.Items {
		if s.LifecycleState == core.SubnetLifecycleStateAvailable || s.LifecycleState == core.SubnetLifecycleStateProvisioning {
			return *s.Id, nil
		}
	}
	cs, err := o.network.CreateSubnet(ctx, core.CreateSubnetRequest{CreateSubnetDetails: core.CreateSubnetDetails{
		CompartmentId: common.String(compartment), VcnId: vcn.Id, CidrBlock: common.String(subnetCIDR),
		DisplayName: common.String(project), DnsLabel: common.String("machines"),
		RouteTableId: vcn.DefaultRouteTableId, SecurityListIds: []string{*vcn.DefaultSecurityListId},
		ProhibitPublicIpOnVnic: common.Bool(false), FreeformTags: freeform(),
	}})
	if err != nil {
		return "", fmt.Errorf("oci: create subnet: %w", err)
	}
	return *cs.Id, nil
}

// ensureGateway gives the VCN an internet gateway and its default route
// table a default route through it.
func (o *OCI) ensureGateway(ctx context.Context, compartment string, vcn core.Vcn) error {
	gr, err := o.network.ListInternetGateways(ctx, core.ListInternetGatewaysRequest{CompartmentId: common.String(compartment), VcnId: vcn.Id})
	if err != nil {
		return fmt.Errorf("oci: list internet gateways: %w", err)
	}
	var igw string
	for _, g := range gr.Items {
		if g.LifecycleState != core.InternetGatewayLifecycleStateTerminated && g.LifecycleState != core.InternetGatewayLifecycleStateTerminating {
			igw = *g.Id
		}
	}
	if igw == "" {
		cr, err := o.network.CreateInternetGateway(ctx, core.CreateInternetGatewayRequest{CreateInternetGatewayDetails: core.CreateInternetGatewayDetails{
			CompartmentId: common.String(compartment), VcnId: vcn.Id, IsEnabled: common.Bool(true),
			DisplayName: common.String(project), FreeformTags: freeform(),
		}})
		if err != nil {
			return fmt.Errorf("oci: create internet gateway: %w", err)
		}
		igw = *cr.Id
	}
	rt, err := o.network.GetRouteTable(ctx, core.GetRouteTableRequest{RtId: vcn.DefaultRouteTableId})
	if err != nil {
		return fmt.Errorf("oci: route table: %w", err)
	}
	for _, r := range rt.RouteRules {
		if r.Destination != nil && *r.Destination == allCIDR {
			return nil
		}
	}
	rules := append(slices.Clone(rt.RouteRules), core.RouteRule{
		NetworkEntityId: common.String(igw), Destination: common.String(allCIDR),
		DestinationType: core.RouteRuleDestinationTypeCidrBlock, Description: common.String("internet"),
	})
	if _, err := o.network.UpdateRouteTable(ctx, core.UpdateRouteTableRequest{RtId: vcn.DefaultRouteTableId,
		UpdateRouteTableDetails: core.UpdateRouteTableDetails{RouteRules: rules}}); err != nil {
		return fmt.Errorf("oci: default route: %w", err)
	}
	return nil
}

// ensureIngress makes a security list admit QUIC from cidrs. On a security
// list of a VCN created here (fresh), the default rules (SSH from
// anywhere, ICMP) are replaced: machines take only QUIC, plus the ICMP
// messages path MTU discovery needs. Egress is open. With an authoritative
// (explicit) allow_cidrs, QUIC rules for other sources are removed.
func (o *OCI) ensureIngress(ctx context.Context, list string, cidrs []string, fresh, authoritative bool) error {
	sl, err := o.network.GetSecurityList(ctx, core.GetSecurityListRequest{SecurityListId: common.String(list)})
	if err != nil {
		return fmt.Errorf("oci: security list: %w", err)
	}
	ingress, egress, changed := securityRules(sl.SecurityList, cidrs, fresh, authoritative)
	if !changed {
		return nil
	}
	_, err = o.network.UpdateSecurityList(ctx, core.UpdateSecurityListRequest{SecurityListId: common.String(list),
		UpdateSecurityListDetails: core.UpdateSecurityListDetails{IngressSecurityRules: ingress, EgressSecurityRules: egress}})
	if err != nil {
		return fmt.Errorf("oci: security list ingress: %w", err)
	}
	return nil
}

func quicRule(cidr string) core.IngressSecurityRule {
	return core.IngressSecurityRule{
		Protocol: common.String("17"), Source: common.String(cidr), SourceType: core.IngressSecurityRuleSourceTypeCidrBlock,
		UdpOptions:  &core.UdpOptions{DestinationPortRange: &core.PortRange{Min: common.Int(serverPort), Max: common.Int(serverPort)}},
		Description: common.String("offshore clients"),
	}
}

func isQUIC(r core.IngressSecurityRule, cidr string) bool {
	return quicRuleSource(r) == cidr
}

// quicRuleSource returns the source of a QUIC (UDP 7443) rule, or "".
func quicRuleSource(r core.IngressSecurityRule) string {
	if r.Protocol != nil && *r.Protocol == "17" && r.Source != nil &&
		r.UdpOptions != nil && r.UdpOptions.DestinationPortRange != nil &&
		r.UdpOptions.DestinationPortRange.Min != nil && *r.UdpOptions.DestinationPortRange.Min == serverPort {
		return *r.Source
	}
	return ""
}

// securityRules returns the rules a security list should have and whether
// they differ from its current ones. authoritative removes QUIC rules for
// sources not in cidrs.
func securityRules(sl core.SecurityList, cidrs []string, fresh, authoritative bool) (in []core.IngressSecurityRule, out []core.EgressSecurityRule, changed bool) {
	in = slices.Clone(sl.IngressSecurityRules)
	out = slices.Clone(sl.EgressSecurityRules)
	if fresh {
		in = []core.IngressSecurityRule{{
			// Path MTU discovery: ICMP type 3 code 4 from anywhere.
			Protocol: common.String("1"), Source: common.String(allCIDR), SourceType: core.IngressSecurityRuleSourceTypeCidrBlock,
			IcmpOptions: &core.IcmpOptions{Type: common.Int(3), Code: common.Int(4)},
		}}
		changed = true
	}
	if authoritative {
		n := len(in)
		in = slices.DeleteFunc(in, func(r core.IngressSecurityRule) bool {
			src := quicRuleSource(r)
			return src != "" && !slices.Contains(cidrs, src)
		})
		changed = changed || len(in) != n
	}
	for _, c := range cidrs {
		if !slices.ContainsFunc(in, func(r core.IngressSecurityRule) bool { return isQUIC(r, c) }) {
			in = append(in, quicRule(c))
			changed = true
		}
	}
	if !slices.ContainsFunc(out, func(r core.EgressSecurityRule) bool {
		return r.Protocol != nil && *r.Protocol == "all" && r.Destination != nil && *r.Destination == allCIDR
	}) {
		out = append(out, core.EgressSecurityRule{Protocol: common.String("all"), Destination: common.String(allCIDR),
			DestinationType: core.EgressSecurityRuleDestinationTypeCidrBlock})
		changed = true
	}
	return in, out, changed
}

func (o *OCI) allowCIDRs(ctx context.Context) ([]string, error) {
	if o.ingress.Explicit() {
		return o.ingress.CIDRs, nil
	}
	ip, err := publicIP(ctx)
	if err != nil {
		return nil, fmt.Errorf("oci: detecting the public address for allow_cidrs: %w", err)
	}
	return []string{ip + "/32"}, nil
}

// ensureBucket creates the private bucket with its lifecycle rules and
// returns the Object Storage namespace.
func (o *OCI) ensureBucket(ctx context.Context, compartment string) (string, error) {
	nr, err := o.objects.GetNamespace(ctx, objectstorage.GetNamespaceRequest{})
	if err != nil {
		return "", fmt.Errorf("oci: object storage namespace: %w", err)
	}
	ns := *nr.Value
	_, err = o.objects.GetBucket(ctx, objectstorage.GetBucketRequest{NamespaceName: common.String(ns), BucketName: common.String(o.bucket)})
	if err == nil {
		return ns, nil
	}
	if !notFound(err) {
		return "", fmt.Errorf("oci: bucket %s: %w", o.bucket, err)
	}
	_, err = o.objects.CreateBucket(ctx, objectstorage.CreateBucketRequest{NamespaceName: common.String(ns), CreateBucketDetails: objectstorage.CreateBucketDetails{
		Name: common.String(o.bucket), CompartmentId: common.String(compartment),
		PublicAccessType: objectstorage.CreateBucketDetailsPublicAccessTypeNopublicaccess, FreeformTags: freeform(),
	}})
	if err != nil && !conflict(err) {
		return "", fmt.Errorf("oci: create bucket %s: %w", o.bucket, err)
	}
	return ns, nil
}

// ensureRepository finds or creates the container repository and returns
// its reference (registry/namespace/name).
func (o *OCI) ensureRepository(ctx context.Context, compartment, namespace string) (string, error) {
	ref := o.registry + "/" + namespace + "/" + o.repository
	rr, err := o.artifacts.ListContainerRepositories(ctx, artifacts.ListContainerRepositoriesRequest{
		CompartmentId: common.String(compartment), DisplayName: common.String(o.repository)})
	if err != nil {
		return "", fmt.Errorf("oci: list container repositories: %w", err)
	}
	if len(rr.Items) > 0 {
		return ref, nil
	}
	_, err = o.artifacts.CreateContainerRepository(ctx, artifacts.CreateContainerRepositoryRequest{CreateContainerRepositoryDetails: artifacts.CreateContainerRepositoryDetails{
		CompartmentId: common.String(compartment), DisplayName: common.String(o.repository),
		IsPublic: common.Bool(false), IsImmutable: common.Bool(false), FreeformTags: freeform(),
	}})
	if err != nil && !conflict(err) {
		return "", fmt.Errorf("oci: create container repository %s: %w", o.repository, err)
	}
	return ref, nil
}

// iamName names the dynamic group and policy of a compartment.
func iamName(compartment string) string {
	sum := sha256.Sum256([]byte(compartment))
	return "offshore-" + hex.EncodeToString(sum[:])[:10]
}

// policyStatements are the permissions of the compartment's instances
// (the supervisors) and of Object Storage (lifecycle rules). Machines may
// stop and terminate instances of the compartment (themselves at idle
// stop and expiry; termination deletes the boot volume), read and delete
// objects of the bucket (secrets, cache) and pull images. The compartment
// holds nothing but offshore machines, and containers cannot reach
// the metadata service: only supervisors hold the instance principal.
func policyStatements(group, compartment, bucket, region string) []string {
	c := "compartment id " + compartment
	return []string{
		"Allow dynamic-group " + group + " to manage instance-family in " + c,
		"Allow dynamic-group " + group + " to manage volume-family in " + c,
		"Allow dynamic-group " + group + " to manage objects in " + c + " where target.bucket.name = '" + bucket + "'",
		"Allow dynamic-group " + group + " to read repos in " + c,
		"Allow service objectstorage-" + region + " to manage object-family in " + c,
	}
}

// ensureIAM creates the dynamic group of the compartment's instances and
// the policy of policyStatements (updated when they change).
func (o *OCI) ensureIAM(ctx context.Context, tenancy string, in *infra) error {
	name := iamName(in.compartment)
	gr, err := o.identity.ListDynamicGroups(ctx, identity.ListDynamicGroupsRequest{CompartmentId: common.String(tenancy), Name: common.String(name)})
	if err != nil {
		return fmt.Errorf("oci: list dynamic groups (set iam = false and create the dynamic group and policy yourself if this account may not): %w", err)
	}
	if len(gr.Items) == 0 {
		_, err := o.identity.CreateDynamicGroup(ctx, identity.CreateDynamicGroupRequest{CreateDynamicGroupDetails: identity.CreateDynamicGroupDetails{
			CompartmentId: common.String(tenancy), Name: common.String(name),
			MatchingRule: common.String("ALL {instance.compartment.id = '" + in.compartment + "'}"),
			Description:  common.String("offshore machines (instances of compartment " + o.compartment + ")"), FreeformTags: freeform(),
		}})
		if err != nil && !conflict(err) {
			return fmt.Errorf("oci: create dynamic group: %w", err)
		}
	}
	want := policyStatements(name, in.compartment, in.bucket, o.region)
	pr, err := o.identity.ListPolicies(ctx, identity.ListPoliciesRequest{CompartmentId: common.String(tenancy), Name: common.String(name)})
	if err != nil {
		return fmt.Errorf("oci: list policies: %w", err)
	}
	if len(pr.Items) > 0 {
		if slices.Equal(pr.Items[0].Statements, want) {
			return nil
		}
		_, err := o.identity.UpdatePolicy(ctx, identity.UpdatePolicyRequest{PolicyId: pr.Items[0].Id,
			UpdatePolicyDetails: identity.UpdatePolicyDetails{Statements: want}})
		if err != nil {
			return fmt.Errorf("oci: update policy: %w", err)
		}
		return nil
	}
	_, err = o.identity.CreatePolicy(ctx, identity.CreatePolicyRequest{CreatePolicyDetails: identity.CreatePolicyDetails{
		CompartmentId: common.String(tenancy), Name: common.String(name), Statements: want,
		Description: common.String("offshore machines of compartment " + o.compartment), FreeformTags: freeform(),
	}})
	if err != nil && !conflict(err) {
		return fmt.Errorf("oci: create policy: %w", err)
	}
	return nil
}

// poll calls f until it reports done, an error or the timeout passes.
func (o *OCI) poll(ctx context.Context, timeout time.Duration, f func() (bool, error)) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		done, err := f()
		if err != nil || done {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(o.pollEvery):
		}
	}
}
