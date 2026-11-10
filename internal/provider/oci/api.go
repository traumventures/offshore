package oci

import (
	"context"

	"net/http"

	"github.com/oracle/oci-go-sdk/v65/artifacts"
	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/core"
	"github.com/oracle/oci-go-sdk/v65/identity"
	"github.com/oracle/oci-go-sdk/v65/objectstorage"
)

// The parts of the OCI SDK clients the provider uses (fakes in tests).

type computeAPI interface {
	LaunchInstance(ctx context.Context, req core.LaunchInstanceRequest) (core.LaunchInstanceResponse, error)
	ListInstances(ctx context.Context, req core.ListInstancesRequest) (core.ListInstancesResponse, error)
	GetInstance(ctx context.Context, req core.GetInstanceRequest) (core.GetInstanceResponse, error)
	InstanceAction(ctx context.Context, req core.InstanceActionRequest) (core.InstanceActionResponse, error)
	TerminateInstance(ctx context.Context, req core.TerminateInstanceRequest) (core.TerminateInstanceResponse, error)
	ListVnicAttachments(ctx context.Context, req core.ListVnicAttachmentsRequest) (core.ListVnicAttachmentsResponse, error)
	ListImages(ctx context.Context, req core.ListImagesRequest) (core.ListImagesResponse, error)
}

type networkAPI interface {
	GetVnic(ctx context.Context, req core.GetVnicRequest) (core.GetVnicResponse, error)
	ListVcns(ctx context.Context, req core.ListVcnsRequest) (core.ListVcnsResponse, error)
	CreateVcn(ctx context.Context, req core.CreateVcnRequest) (core.CreateVcnResponse, error)
	ListInternetGateways(ctx context.Context, req core.ListInternetGatewaysRequest) (core.ListInternetGatewaysResponse, error)
	CreateInternetGateway(ctx context.Context, req core.CreateInternetGatewayRequest) (core.CreateInternetGatewayResponse, error)
	GetRouteTable(ctx context.Context, req core.GetRouteTableRequest) (core.GetRouteTableResponse, error)
	UpdateRouteTable(ctx context.Context, req core.UpdateRouteTableRequest) (core.UpdateRouteTableResponse, error)
	GetSecurityList(ctx context.Context, req core.GetSecurityListRequest) (core.GetSecurityListResponse, error)
	UpdateSecurityList(ctx context.Context, req core.UpdateSecurityListRequest) (core.UpdateSecurityListResponse, error)
	ListSubnets(ctx context.Context, req core.ListSubnetsRequest) (core.ListSubnetsResponse, error)
	CreateSubnet(ctx context.Context, req core.CreateSubnetRequest) (core.CreateSubnetResponse, error)
	GetSubnet(ctx context.Context, req core.GetSubnetRequest) (core.GetSubnetResponse, error)
}

type objectAPI interface {
	GetNamespace(ctx context.Context, req objectstorage.GetNamespaceRequest) (objectstorage.GetNamespaceResponse, error)
	GetBucket(ctx context.Context, req objectstorage.GetBucketRequest) (objectstorage.GetBucketResponse, error)
	CreateBucket(ctx context.Context, req objectstorage.CreateBucketRequest) (objectstorage.CreateBucketResponse, error)
	PutObjectLifecyclePolicy(ctx context.Context, req objectstorage.PutObjectLifecyclePolicyRequest) (objectstorage.PutObjectLifecyclePolicyResponse, error)
	HeadObject(ctx context.Context, req objectstorage.HeadObjectRequest) (objectstorage.HeadObjectResponse, error)
	PutObject(ctx context.Context, req objectstorage.PutObjectRequest) (objectstorage.PutObjectResponse, error)
	GetObject(ctx context.Context, req objectstorage.GetObjectRequest) (objectstorage.GetObjectResponse, error)
	DeleteObject(ctx context.Context, req objectstorage.DeleteObjectRequest) (objectstorage.DeleteObjectResponse, error)
	CreatePreauthenticatedRequest(ctx context.Context, req objectstorage.CreatePreauthenticatedRequestRequest) (objectstorage.CreatePreauthenticatedRequestResponse, error)
}

type identityAPI interface {
	ListCompartments(ctx context.Context, req identity.ListCompartmentsRequest) (identity.ListCompartmentsResponse, error)
	CreateCompartment(ctx context.Context, req identity.CreateCompartmentRequest) (identity.CreateCompartmentResponse, error)
	GetCompartment(ctx context.Context, req identity.GetCompartmentRequest) (identity.GetCompartmentResponse, error)
	ListAvailabilityDomains(ctx context.Context, req identity.ListAvailabilityDomainsRequest) (identity.ListAvailabilityDomainsResponse, error)
	ListDynamicGroups(ctx context.Context, req identity.ListDynamicGroupsRequest) (identity.ListDynamicGroupsResponse, error)
	CreateDynamicGroup(ctx context.Context, req identity.CreateDynamicGroupRequest) (identity.CreateDynamicGroupResponse, error)
	ListPolicies(ctx context.Context, req identity.ListPoliciesRequest) (identity.ListPoliciesResponse, error)
	CreatePolicy(ctx context.Context, req identity.CreatePolicyRequest) (identity.CreatePolicyResponse, error)
	UpdatePolicy(ctx context.Context, req identity.UpdatePolicyRequest) (identity.UpdatePolicyResponse, error)
}

type artifactsAPI interface {
	ListContainerRepositories(ctx context.Context, req artifacts.ListContainerRepositoriesRequest) (artifacts.ListContainerRepositoriesResponse, error)
	CreateContainerRepository(ctx context.Context, req artifacts.CreateContainerRepositoryRequest) (artifacts.CreateContainerRepositoryResponse, error)
	ListContainerImages(ctx context.Context, req artifacts.ListContainerImagesRequest) (artifacts.ListContainerImagesResponse, error)
}

// notFound reports a 404 service error (OCI answers 404 also for
// resources the caller may not see).
func notFound(err error) bool {
	se, ok := common.IsServiceError(err)
	return ok && se.GetHTTPStatusCode() == http.StatusNotFound
}

// conflict reports a 409 service error (e.g. a name already in use).
func conflict(err error) bool {
	se, ok := common.IsServiceError(err)
	return ok && se.GetHTTPStatusCode() == http.StatusConflict
}
