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

// Package cloudprovider implements sigs.k8s.io/karpenter's CloudProvider interface for STACKIT.
package cloudprovider

import (
	"context"
	stderrors "errors"
	"fmt"
	"time"

	"github.com/awslabs/operatorpkg/status"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	coreapis "sigs.k8s.io/karpenter/pkg/apis"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/utils/resources"

	"github.com/kubekanvas/karpenter-provider-stackit/pkg/apis"
	"github.com/kubekanvas/karpenter-provider-stackit/pkg/apis/v1alpha1"
	cloudproviderevents "github.com/kubekanvas/karpenter-provider-stackit/pkg/cloudprovider/events"
	"github.com/kubekanvas/karpenter-provider-stackit/pkg/operator/options"
	"github.com/kubekanvas/karpenter-provider-stackit/pkg/providers/instance"
	"github.com/kubekanvas/karpenter-provider-stackit/pkg/providers/instancetype"
	"github.com/kubekanvas/karpenter-provider-stackit/pkg/utils"
)

var _ cloudprovider.CloudProvider = (*CloudProvider)(nil)

type CloudProvider struct {
	kubeClient client.Client
	recorder   events.Recorder
	region     string

	instanceTypeProvider instancetype.Provider
	instanceProvider     instance.Provider
}

func New(
	instanceTypeProvider instancetype.Provider,
	instanceProvider instance.Provider,
	recorder events.Recorder,
	kubeClient client.Client,
	region string,
) *CloudProvider {
	return &CloudProvider{
		instanceTypeProvider: instanceTypeProvider,
		instanceProvider:     instanceProvider,
		kubeClient:           kubeClient,
		recorder:             recorder,
		region:               region,
	}
}

// Create launches a server for the NodeClaim and returns the NodeClaim hydrated with the labels
// resolved from the machine type and availability zone that were actually chosen.
func (c *CloudProvider) Create(ctx context.Context, nodeClaim *karpv1.NodeClaim) (*karpv1.NodeClaim, error) {
	nodeClass, err := c.resolveNodeClassFromNodeClaim(ctx, nodeClaim)
	if err != nil {
		if errors.IsNotFound(err) {
			// Without a NodeClass there is no capacity that could ever satisfy this NodeClaim, so
			// report it as an ICE rather than a transient failure the scheduler should retry.
			c.recorder.Publish(cloudproviderevents.NodeClaimFailedToResolveNodeClass(nodeClaim))
			return nil, cloudprovider.NewInsufficientCapacityError(fmt.Errorf("resolving nodeclass, %w", err))
		}
		return nil, fmt.Errorf("resolving nodeclass, %w", err)
	}

	nodeClassReady := nodeClass.StatusConditions().Get(status.ConditionReady)
	if nodeClassReady.IsFalse() {
		return nil, cloudprovider.NewNodeClassNotReadyError(stderrors.New(nodeClassReady.Message))
	}
	if nodeClassReady.IsUnknown() {
		return nil, cloudprovider.NewCreateError(
			fmt.Errorf("resolving nodeclass readiness, nodeclass is in Ready=Unknown, %s", nodeClassReady.Message),
			"NodeClassReadinessUnknown", "NodeClass is in Ready=Unknown")
	}
	if nodeClassReady.ObservedGeneration != nodeClass.Generation {
		return nil, cloudprovider.NewNodeClassNotReadyError(
			fmt.Errorf("nodeclass status has not been reconciled against the latest spec"))
	}

	instanceTypes, err := c.instanceTypeProvider.List(ctx, nodeClass)
	if err != nil {
		return nil, cloudprovider.NewCreateError(fmt.Errorf("resolving instance types, %w", err),
			"InstanceTypeResolutionFailed", "Error resolving instance types")
	}

	labels := utils.ManagedLabels(nodeClass, nodeClaim, options.FromContext(ctx).ClusterName)
	inst, err := c.instanceProvider.Create(ctx, nodeClass, nodeClaim, labels, instanceTypes)
	if err != nil {
		return nil, fmt.Errorf("creating instance, %w", err)
	}

	instanceType, _ := lo.Find(instanceTypes, func(i *cloudprovider.InstanceType) bool { return i.Name == inst.MachineType })
	created := c.instanceToNodeClaim(inst, instanceType)
	created.Annotations = lo.Assign(created.Annotations, map[string]string{
		v1alpha1.AnnotationStackitNodeClassHash:        nodeClass.Hash(),
		v1alpha1.AnnotationStackitNodeClassHashVersion: v1alpha1.StackitNodeClassHashVersion,
	})
	return created, nil
}

func (c *CloudProvider) List(ctx context.Context) ([]*karpv1.NodeClaim, error) {
	instances, err := c.instanceProvider.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing instances, %w", err)
	}

	nodeClaims := make([]*karpv1.NodeClaim, 0, len(instances))
	for _, inst := range instances {
		instanceType, err := c.resolveInstanceTypeFromInstance(ctx, inst)
		if err != nil {
			return nil, fmt.Errorf("resolving instance type, %w", err)
		}
		nodeClaims = append(nodeClaims, c.instanceToNodeClaim(inst, instanceType))
	}
	return nodeClaims, nil
}

func (c *CloudProvider) Get(ctx context.Context, providerID string) (*karpv1.NodeClaim, error) {
	id, err := utils.ParseInstanceID(providerID)
	if err != nil {
		return nil, fmt.Errorf("getting instance ID, %w", err)
	}
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("id", id))

	inst, err := c.instanceProvider.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	instanceType, err := c.resolveInstanceTypeFromInstance(ctx, inst)
	if err != nil {
		return nil, fmt.Errorf("resolving instance type, %w", err)
	}
	return c.instanceToNodeClaim(inst, instanceType), nil
}

func (c *CloudProvider) Delete(ctx context.Context, nodeClaim *karpv1.NodeClaim) error {
	id, err := utils.ParseInstanceID(nodeClaim.Status.ProviderID)
	if err != nil {
		return fmt.Errorf("getting instance ID, %w", err)
	}
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("id", id))

	return c.instanceProvider.Delete(ctx, id)
}

// GetInstanceTypes returns every machine type the NodePool's NodeClass can launch, whether or not
// capacity is currently available for it.
func (c *CloudProvider) GetInstanceTypes(ctx context.Context, nodePool *karpv1.NodePool) ([]*cloudprovider.InstanceType, error) {
	nodeClass, err := c.resolveNodeClassFromNodePool(ctx, nodePool)
	if err != nil {
		if errors.IsNotFound(err) {
			c.recorder.Publish(cloudproviderevents.NodePoolFailedToResolveNodeClass(nodePool))
		}
		// Returning an empty list here would surface as "nothing schedulable" with no hint as to
		// why, so the error is propagated instead.
		return nil, fmt.Errorf("resolving node class, %w", err)
	}
	return c.instanceTypeProvider.List(ctx, nodeClass)
}

func (c *CloudProvider) IsDrifted(ctx context.Context, nodeClaim *karpv1.NodeClaim) (cloudprovider.DriftReason, error) {
	nodePoolName, ok := nodeClaim.Labels[karpv1.NodePoolLabelKey]
	if !ok {
		return "", nil
	}
	nodePool := &karpv1.NodePool{}
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: nodePoolName}, nodePool); err != nil {
		return "", client.IgnoreNotFound(err)
	}
	if nodePool.Spec.Template.Spec.NodeClassRef == nil {
		return "", nil
	}
	nodeClass, err := c.resolveNodeClassFromNodePool(ctx, nodePool)
	if err != nil {
		if errors.IsNotFound(err) {
			// Drift cannot be evaluated against a NodeClass that no longer exists. Reporting no
			// drift keeps existing nodes alive rather than replacing them all at once.
			c.recorder.Publish(cloudproviderevents.NodePoolFailedToResolveNodeClass(nodePool))
			return "", nil
		}
		return "", fmt.Errorf("resolving nodeclass, %w", err)
	}
	return c.isNodeClassDrifted(ctx, nodeClaim, nodeClass), nil
}

// Name returns the CloudProvider implementation name.
func (c *CloudProvider) Name() string {
	return "stackit"
}

func (c *CloudProvider) GetSupportedNodeClasses() []status.Object {
	return []status.Object{&v1alpha1.StackitNodeClass{}}
}

// RepairPolicies lists the node conditions Karpenter will replace a node for. STACKIT publishes no
// instance health signal of its own — there is no equivalent of EC2 instance status checks — so
// every policy here is driven by the kubelet's own view of the node.
func (c *CloudProvider) RepairPolicies() []cloudprovider.RepairPolicy {
	return []cloudprovider.RepairPolicy{
		{
			ConditionType:      corev1.NodeReady,
			ConditionStatus:    corev1.ConditionFalse,
			TolerationDuration: 30 * time.Minute,
		},
		{
			ConditionType:      corev1.NodeReady,
			ConditionStatus:    corev1.ConditionUnknown,
			TolerationDuration: 30 * time.Minute,
		},
		// Node Monitoring Agent conditions, if that agent is installed.
		{
			ConditionType:      "StorageReady",
			ConditionStatus:    corev1.ConditionFalse,
			TolerationDuration: 30 * time.Minute,
		},
		{
			ConditionType:      "NetworkingReady",
			ConditionStatus:    corev1.ConditionFalse,
			TolerationDuration: 30 * time.Minute,
		},
		{
			ConditionType:      "KernelReady",
			ConditionStatus:    corev1.ConditionFalse,
			TolerationDuration: 30 * time.Minute,
		},
		{
			ConditionType:      "ContainerRuntimeReady",
			ConditionStatus:    corev1.ConditionFalse,
			TolerationDuration: 30 * time.Minute,
		},
	}
}

func (c *CloudProvider) resolveNodeClassFromNodeClaim(ctx context.Context, nodeClaim *karpv1.NodeClaim) (*v1alpha1.StackitNodeClass, error) {
	if nodeClaim.Spec.NodeClassRef == nil {
		return nil, errors.NewNotFound(schema.GroupResource{Group: apis.Group, Resource: "stackitnodeclasses"}, "")
	}
	nodeClass := &v1alpha1.StackitNodeClass{}
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: nodeClaim.Spec.NodeClassRef.Name}, nodeClass); err != nil {
		return nil, err
	}
	if !nodeClass.DeletionTimestamp.IsZero() {
		return nil, newTerminatingNodeClassError(nodeClass.Name)
	}
	return nodeClass, nil
}

func (c *CloudProvider) resolveNodeClassFromNodePool(ctx context.Context, nodePool *karpv1.NodePool) (*v1alpha1.StackitNodeClass, error) {
	if nodePool.Spec.Template.Spec.NodeClassRef == nil {
		return nil, errors.NewNotFound(schema.GroupResource{Group: apis.Group, Resource: "stackitnodeclasses"}, "")
	}
	nodeClass := &v1alpha1.StackitNodeClass{}
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: nodePool.Spec.Template.Spec.NodeClassRef.Name}, nodeClass); err != nil {
		return nil, err
	}
	if !nodeClass.DeletionTimestamp.IsZero() {
		return nil, newTerminatingNodeClassError(nodeClass.Name)
	}
	return nodeClass, nil
}

// resolveInstanceTypeFromInstance recovers the instance type of an existing server. Failing to
// resolve it is not fatal: the NodeClaim is still returned, only without capacity information.
func (c *CloudProvider) resolveInstanceTypeFromInstance(ctx context.Context, inst *instance.Instance) (*cloudprovider.InstanceType, error) {
	nodePool, err := c.resolveNodePoolFromInstance(ctx, inst)
	if err != nil {
		return nil, client.IgnoreNotFound(fmt.Errorf("resolving nodepool, %w", err))
	}
	instanceTypes, err := c.GetInstanceTypes(ctx, nodePool)
	if err != nil {
		return nil, client.IgnoreNotFound(fmt.Errorf("resolving instance type, %w", err))
	}
	instanceType, _ := lo.Find(instanceTypes, func(i *cloudprovider.InstanceType) bool { return i.Name == inst.MachineType })
	return instanceType, nil
}

func (c *CloudProvider) resolveNodePoolFromInstance(ctx context.Context, inst *instance.Instance) (*karpv1.NodePool, error) {
	nodePoolName, ok := inst.Labels[v1alpha1.LabelKeyNodePool]
	if !ok {
		return nil, errors.NewNotFound(schema.GroupResource{Group: coreapis.Group, Resource: "nodepools"}, "")
	}
	nodePool := &karpv1.NodePool{}
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Name: nodePoolName}, nodePool); err != nil {
		return nil, err
	}
	return nodePool, nil
}

func (c *CloudProvider) instanceToNodeClaim(inst *instance.Instance, instanceType *cloudprovider.InstanceType) *karpv1.NodeClaim {
	nodeClaim := &karpv1.NodeClaim{}
	labels := map[string]string{}

	if instanceType != nil {
		for key, req := range instanceType.Requirements {
			// A requirement only becomes a node label when it resolves to exactly one value: a
			// requirement compatible with three zones says nothing about which zone this server is in.
			if req.Len() == 1 {
				labels[key] = req.Values()[0]
			}
		}
		nonZero := func(_ corev1.ResourceName, v resource.Quantity) bool { return !resources.IsZero(v) }
		nodeClaim.Status.Capacity = lo.PickBy(instanceType.Capacity, nonZero)
		nodeClaim.Status.Allocatable = lo.PickBy(instanceType.Allocatable(), nonZero)
	}

	// Zone comes from the server itself rather than from the instance type, because the instance
	// type spans every zone the NodeClass allows while the server is in exactly one of them.
	// Instance.Zone() applies the same sanitizer cloud-provider-stackit applies before writing
	// topology.kubernetes.io/zone, so this value matches the node's label byte for byte.
	labels[corev1.LabelTopologyZone] = inst.Zone()
	// Region is the controller's configured region, mirroring the CCM, which writes its own
	// configured region to every node rather than reading one off the server.
	labels[corev1.LabelTopologyRegion] = c.region
	// STACKIT sells no interruptible capacity, so every node is on-demand.
	labels[karpv1.CapacityTypeLabelKey] = karpv1.CapacityTypeOnDemand

	nodeClaim.Labels = labels
	nodeClaim.Status.ProviderID = inst.ProviderID()
	// STACKIT returns a real creation timestamp on the server, including from the list endpoint, so
	// garbage collection can tell a just-launched server from an orphan without this provider
	// having to record the launch time itself.
	nodeClaim.CreationTimestamp = metav1.Time{Time: inst.CreatedAt}
	// Note that a stopped or errored server is deliberately NOT surfaced as terminating here. Doing
	// so would make garbage collection skip it, and a stopped orphan is exactly what garbage
	// collection exists to remove — it keeps billing for its boot volume until something deletes it.
	return nodeClaim
}

// newTerminatingNodeClassError reports a NodeClass that is being deleted as NotFound, so that every
// caller's existing NotFound handling applies, while still saying what actually happened.
func newTerminatingNodeClassError(name string) *errors.StatusError {
	qualifiedResource := schema.GroupResource{Group: apis.Group, Resource: "stackitnodeclasses"}
	err := errors.NewNotFound(qualifiedResource, name)
	err.ErrStatus.Message = fmt.Sprintf("%s %q is terminating, treating as not found", qualifiedResource.String(), name)
	return err
}
