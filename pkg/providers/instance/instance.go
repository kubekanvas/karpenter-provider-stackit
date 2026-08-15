/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package instance launches, inspects and terminates STACKIT IaaS servers on behalf of NodeClaims.
package instance

import (
	"context"
	"fmt"

	"github.com/awslabs/operatorpkg/option"
	"github.com/patrickmn/go-cache"
	"github.com/samber/lo"
	iaas "github.com/stackitcloud/stackit-sdk-go/services/iaas/v2api"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/kubekanvas/karpenter-provider-stackit/pkg/apis/v1alpha1"
	stackitcache "github.com/kubekanvas/karpenter-provider-stackit/pkg/cache"
	"github.com/kubekanvas/karpenter-provider-stackit/pkg/stackit"
	"github.com/kubekanvas/karpenter-provider-stackit/pkg/utils"
)

// SkipCache forces Get to bypass the instance cache. Use it whenever a stale answer would be acted
// on destructively, such as before terminating a server.
var SkipCache = func(opts *options) {
	opts.SkipCache = true
}

type options struct {
	SkipCache bool
}

// Options is the functional option type accepted by Get.
type Options = option.Function[options]

type Provider interface {
	Create(ctx context.Context, nodeClass *v1alpha1.StackitNodeClass, nodeClaim *karpv1.NodeClaim, labels map[string]string, instanceTypes []*cloudprovider.InstanceType) (*Instance, error)
	Get(ctx context.Context, id string, opts ...Options) (*Instance, error)
	List(ctx context.Context) ([]*Instance, error)
	Delete(ctx context.Context, id string) error
	UpdateLabels(ctx context.Context, id string, labels map[string]string) error
}

type DefaultProvider struct {
	clusterName          string
	client               stackit.API
	unavailableOfferings *stackitcache.UnavailableOfferings
	instanceCache        *cache.Cache
}

func NewDefaultProvider(
	clusterName string,
	client stackit.API,
	unavailableOfferings *stackitcache.UnavailableOfferings,
	instanceCache *cache.Cache,
) *DefaultProvider {
	return &DefaultProvider{
		clusterName:          clusterName,
		client:               client,
		unavailableOfferings: unavailableOfferings,
		instanceCache:        instanceCache,
	}
}

func (p *DefaultProvider) Create(
	ctx context.Context,
	nodeClass *v1alpha1.StackitNodeClass,
	nodeClaim *karpv1.NodeClaim,
	labels map[string]string,
	instanceTypes []*cloudprovider.InstanceType,
) (*Instance, error) {
	instanceType, zone, err := cheapestOffering(instanceTypes, nodeClaim)
	if err != nil {
		return nil, err
	}
	payload, err := p.createPayload(nodeClass, nodeClaim, instanceType, zone, labels)
	if err != nil {
		return nil, cloudprovider.NewCreateError(err, "CreateRequestFailed", "Failed to build STACKIT server request")
	}

	server, err := p.client.CreateServer(ctx, payload)
	if err != nil {
		p.recordLaunchFailure(ctx, err, instanceType.Name, zone)
		return nil, cloudprovider.NewCreateError(fmt.Errorf("creating server, %w", err),
			"ServerCreationFailed", fmt.Sprintf("Failed to create STACKIT server: %s", err))
	}

	instance := NewInstance(server)
	log.FromContext(ctx).WithValues(
		"id", instance.ID,
		"machine-type", instance.MachineType,
		"availability-zone", instance.AvailabilityZone).V(1).Info("launched server")

	p.instanceCache.SetDefault(instance.ID, instance)
	return instance, nil
}

func (p *DefaultProvider) Get(ctx context.Context, id string, opts ...Options) (*Instance, error) {
	if !option.Resolve(opts...).SkipCache {
		if cached, ok := p.instanceCache.Get(id); ok {
			return cached.(*Instance), nil
		}
	}

	server, err := p.client.GetServer(ctx, id)
	if stackit.IsNotFound(err) {
		p.instanceCache.Delete(id)
		return nil, cloudprovider.NewNodeClaimNotFoundError(err)
	}
	if err != nil {
		return nil, fmt.Errorf("getting server %q, %w", id, err)
	}

	instance := NewInstance(server)
	p.instanceCache.SetDefault(id, instance)
	return instance, nil
}

// List returns every server this controller manages for the configured cluster.
//
// The managed-by filter is applied server-side, so servers belonging to other clusters — or created
// by hand — are never returned and therefore never garbage collected. STACKIT's list endpoint
// returns labels when details are requested, so unlike the UpCloud provider there is no per-server
// detail lookup to fan out and bound here.
func (p *DefaultProvider) List(ctx context.Context) ([]*Instance, error) {
	servers, err := p.client.ListServers(ctx, utils.LabelSelector(map[string]string{
		v1alpha1.LabelKeyManagedBy: p.clusterName,
	}))
	if err != nil {
		return nil, fmt.Errorf("listing servers, %w", err)
	}

	instances := make([]*Instance, 0, len(servers))
	for i := range servers {
		instance := NewInstance(&servers[i])
		p.instanceCache.SetDefault(instance.ID, instance)
		instances = append(instances, instance)
	}
	return instances, nil
}

// Delete terminates a server.
//
// Karpenter retries Delete until it returns NodeClaimNotFoundError, which is what makes a
// multi-step teardown expressible: the first call issues the delete and reports that it is in
// progress, subsequent calls observe DELETING, and the call that finally sees a 404 reports the
// NodeClaim gone.
//
// The boot volume is torn down by the API itself, because Create sets deleteOnTermination on it.
// DeleteServer explicitly does not delete volumes and the flag defaults to false server-side, so a
// server created outside this provider — or by an older version of it — can still leave a volume
// behind; that case is logged rather than guessed at, since deleting a volume this controller did
// not create is not recoverable.
func (p *DefaultProvider) Delete(ctx context.Context, id string) error {
	instance, err := p.Get(ctx, id, SkipCache)
	if err != nil {
		// Already a NodeClaimNotFoundError when the server is gone.
		return err
	}

	if instance.Terminating() {
		return fmt.Errorf("server %q is %s, deletion will be retried", id, instance.Status)
	}

	if err := p.client.DeleteServer(ctx, id); err != nil {
		if stackit.IsNotFound(err) {
			p.instanceCache.Delete(id)
			return cloudprovider.NewNodeClaimNotFoundError(err)
		}
		if stackit.IsConflict(err) {
			// The server is mid-transition; the next retry will find it in a deletable state.
			return fmt.Errorf("server %q is busy, deletion will be retried, %w", id, err)
		}
		return fmt.Errorf("deleting server %q, %w", id, err)
	}
	p.instanceCache.Delete(id)

	log.FromContext(ctx).WithValues("id", id, "boot-volume", instance.BootVolumeID).V(1).Info("requested server deletion")
	// Not yet gone: report progress and let Karpenter call again until the server 404s.
	return fmt.Errorf("server %q deletion requested, waiting for it to disappear", id)
}

// UpdateLabels replaces the labels on a running server. The IaaS API's server PATCH replaces the
// whole label set, so callers must pass the complete desired set rather than a delta.
func (p *DefaultProvider) UpdateLabels(ctx context.Context, id string, labels map[string]string) error {
	if err := p.client.UpdateServerLabels(ctx, id, labels); err != nil {
		if stackit.IsNotFound(err) {
			return cloudprovider.NewNodeClaimNotFoundError(fmt.Errorf("updating server labels, %w", err))
		}
		return fmt.Errorf("updating labels on server %q, %w", id, err)
	}
	p.instanceCache.Delete(id)
	return nil
}

func (p *DefaultProvider) createPayload(
	nodeClass *v1alpha1.StackitNodeClass,
	nodeClaim *karpv1.NodeClaim,
	instanceType *cloudprovider.InstanceType,
	zone string,
	labels map[string]string,
) (*iaas.CreateServerPayload, error) {
	networking, err := networking(nodeClass)
	if err != nil {
		return nil, err
	}
	name, err := serverName(nodeClass, nodeClaim)
	if err != nil {
		return nil, err
	}

	payload := &iaas.CreateServerPayload{
		Name:        name,
		MachineType: instanceType.Name,
		// zone is the sanitized form used in offerings and node labels. STACKIT's own availability
		// zone ids are already sanitizer-safe (e.g. "eu01-1"), so this round-trips unchanged; the
		// sanitized value is used deliberately so that a zone that would be rewritten by the CCM
		// can never be launched into under a name that will not match its node label.
		AvailabilityZone: lo.ToPtr(zone),
		Labels:           utils.LabelsToAPI(labels),
		Networking:       networking,
		BootVolume: &iaas.BootVolume{
			Size: lo.ToPtr(nodeClass.Spec.BootVolume.Size),
			// The IaaS API defaults this to false and DeleteServer never removes volumes, so
			// leaving it unset bills the account for one orphaned root disk per terminated node.
			DeleteOnTermination: lo.ToPtr(nodeClass.DeleteBootVolumeOnTermination()),
			PerformanceClass:    nodeClass.Spec.BootVolume.PerformanceClass,
			Source: &iaas.BootVolumeSource{
				Id:   nodeClass.Spec.BootVolume.Source.ID,
				Type: nodeClass.BootVolumeSourceType(),
			},
		},
	}
	if nodeClass.Spec.UserData != nil {
		payload.UserData = nodeClass.Spec.UserData
	}
	if nodeClass.Spec.KeypairName != nil {
		payload.KeypairName = nodeClass.Spec.KeypairName
	}
	if nodeClass.Spec.AffinityGroup != nil {
		payload.AffinityGroup = nodeClass.Spec.AffinityGroup
	}
	if len(nodeClass.Spec.SecurityGroups) > 0 {
		payload.SecurityGroups = nodeClass.Spec.SecurityGroups
	}
	if len(nodeClass.Spec.ServiceAccountMails) > 0 {
		payload.ServiceAccountMails = nodeClass.Spec.ServiceAccountMails
	}
	return payload, nil
}

// networking renders the NodeClass networking into the API's one-of. STACKIT's v2 CreateServer
// requires exactly one of networkId or nicIds.
func networking(nodeClass *v1alpha1.StackitNodeClass) (iaas.CreateServerPayloadAllOfNetworking, error) {
	spec := nodeClass.Spec.Networking
	hasNetwork := spec.NetworkID != nil && *spec.NetworkID != ""
	hasNICs := len(spec.NICIDs) > 0

	switch {
	case hasNetwork && hasNICs:
		return iaas.CreateServerPayloadAllOfNetworking{}, fmt.Errorf("networking must set exactly one of networkID or nicIDs, not both")
	case hasNetwork:
		return iaas.CreateServerNetworkingAsCreateServerPayloadAllOfNetworking(
			&iaas.CreateServerNetworking{NetworkId: spec.NetworkID}), nil
	case hasNICs:
		return iaas.CreateServerNetworkingWithNicsAsCreateServerPayloadAllOfNetworking(
			&iaas.CreateServerNetworkingWithNics{NicIds: spec.NICIDs}), nil
	default:
		return iaas.CreateServerPayloadAllOfNetworking{}, fmt.Errorf("networking must set one of networkID or nicIDs")
	}
}

// maxServerNameLength is the longest name STACKIT accepts for a server. The IaaS API's name pattern
// is a DNS-style label sequence and the field is capped at 63 characters.
const maxServerNameLength = 63

// serverName derives a stable name from the NodeClaim. NodeClaim names are already valid DNS labels
// and unique for the lifetime of the node, which is exactly what the IaaS API's name pattern wants.
//
// An over-long name is reported rather than truncated: truncating would eventually collide two
// NodeClaims onto one server name, and STACKIT does not enforce name uniqueness, so the collision
// would surface much later as two NodeClaims fighting over one server.
func serverName(nodeClass *v1alpha1.StackitNodeClass, nodeClaim *karpv1.NodeClaim) (string, error) {
	name := nodeClaim.Name
	if prefix := lo.FromPtr(nodeClass.Spec.ServerNamePrefix); prefix != "" {
		name = fmt.Sprintf("%s-%s", prefix, nodeClaim.Name)
	}
	if len(name) > maxServerNameLength {
		return "", fmt.Errorf("server name %q is %d characters, exceeding STACKIT's limit of %d; shorten spec.serverNamePrefix or the NodePool name",
			name, len(name), maxServerNameLength)
	}
	return name, nil
}

// recordLaunchFailure takes the offering that just failed out of rotation, so the next scheduling
// pass picks a different machine type or zone instead of hammering the same one.
func (p *DefaultProvider) recordLaunchFailure(ctx context.Context, err error, machineType, zone string) {
	logger := log.FromContext(ctx).WithValues("machine-type", machineType, "availability-zone", zone)
	switch {
	case stackit.IsUnauthorized(err):
		// Retrying will never fix credentials, and pulling offerings out of rotation would hide the
		// real problem behind a capacity story.
		logger.Error(err, "STACKIT rejected the controller's credentials")
	case stackit.IsInsufficientCapacity(err):
		p.unavailableOfferings.MarkUnavailable(ctx, err.Error(), machineType, zone)
	case stackit.IsRetryable(err):
		// A 5xx is about STACKIT, not about this machine type, so pull the whole zone rather than
		// convincing ourselves that one machine type is short on capacity.
		p.unavailableOfferings.MarkZoneUnavailable(ctx, err.Error(), zone)
	default:
		logger.Error(err, "failed to launch server")
	}
}

// cheapestOffering picks the lowest-priced available offering that satisfies the NodeClaim. STACKIT
// exposes no batch or fleet launch API, so unlike on AWS there is no way to hand it a list of
// acceptable machine types and let it choose — the choice has to be made here.
func cheapestOffering(instanceTypes []*cloudprovider.InstanceType, nodeClaim *karpv1.NodeClaim) (*cloudprovider.InstanceType, string, error) {
	reqs := scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaim.Spec.Requirements...)

	var bestType *cloudprovider.InstanceType
	var bestOffering *cloudprovider.Offering
	for _, it := range instanceTypes {
		if err := it.Requirements.Compatible(reqs, scheduling.AllowUndefinedWellKnownLabels); err != nil {
			continue
		}
		if !resourcesFit(it, nodeClaim) {
			continue
		}
		offering := it.Offerings.Available().Compatible(reqs).Cheapest()
		if offering == nil {
			continue
		}
		if bestOffering == nil || offering.Price < bestOffering.Price {
			bestType, bestOffering = it, offering
		}
	}
	if bestOffering == nil {
		return nil, "", cloudprovider.NewInsufficientCapacityError(
			fmt.Errorf("no available stackit offering satisfies the nodeclaim"))
	}
	return bestType, bestOffering.Requirements.Get(corev1.LabelTopologyZone).Any(), nil
}

func resourcesFit(it *cloudprovider.InstanceType, nodeClaim *karpv1.NodeClaim) bool {
	allocatable := it.Allocatable()
	for name, quantity := range nodeClaim.Spec.Resources.Requests {
		available, ok := allocatable[name]
		if !ok || available.Cmp(quantity) < 0 {
			return false
		}
	}
	return true
}
