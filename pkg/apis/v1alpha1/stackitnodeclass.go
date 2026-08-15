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

package v1alpha1

import (
	"fmt"

	"github.com/awslabs/operatorpkg/status"
	"github.com/mitchellh/hashstructure/v2"
	"github.com/samber/lo"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// StackitNodeClassSpec is the top level specification for the STACKIT Karpenter provider. It holds
// everything Karpenter needs to turn a NodeClaim into a STACKIT IaaS server.
//
// The STACKIT region and project are deliberately not part of this spec: both are operator-level
// settings (--region / --project-id), because the cloud-controller-manager takes its region from
// its own config and stamps it onto every node as topology.kubernetes.io/region. A NodeClass that
// could choose a different region would produce nodes whose region label contradicts the CCM.
type StackitNodeClassSpec struct {
	// availabilityZones restricts the STACKIT availability zones that servers may be launched into,
	// e.g. ["eu01-1", "eu01-2"]. All of them must belong to the region the controller is configured
	// with. When empty, every availability zone the API reports for that region is a candidate.
	// +optional
	// +listType=set
	AvailabilityZones []string `json:"availabilityZones,omitempty"`

	// bootVolume configures the root disk created for every server.
	// +required
	BootVolume BootVolumeSpec `json:"bootVolume"`

	// userData is cloud-init user data passed to the server. This is where the node is joined to
	// the cluster: without it a server boots but never registers, and Karpenter will garbage
	// collect it after the registration TTL.
	// +optional
	UserData *string `json:"userData,omitempty"`

	// networking attaches the server to a network. STACKIT's v2 IaaS API requires a server to be
	// created with either a network or an explicit set of pre-created NICs.
	// +required
	Networking NetworkingSpec `json:"networking"`

	// securityGroups are the IDs of security groups attached to the server.
	// +optional
	// +listType=set
	SecurityGroups []string `json:"securityGroups,omitempty"`

	// keypairName is the name of a STACKIT SSH keypair authorized on the server.
	// +optional
	KeypairName *string `json:"keypairName,omitempty"`

	// affinityGroup is the UUID of a STACKIT affinity group to place servers in. Use a group with
	// an anti-affinity policy to spread nodes across STACKIT hosts.
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`
	// +optional
	AffinityGroup *string `json:"affinityGroup,omitempty"`

	// serviceAccountMails are the mail addresses of STACKIT service accounts attached to the
	// server, which is how a node obtains STACKIT API credentials without a static key.
	// +optional
	// +listType=set
	ServiceAccountMails []string `json:"serviceAccountMails,omitempty"`

	// labels are additional STACKIT server labels applied to every server. Keys reserved by this
	// provider for server discovery (karpenter.sh/managed-by, karpenter.sh/nodepool,
	// karpenter.sh/nodeclaim and karpenter.k8s.stackit/nodeclass) are rejected.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// serverNamePrefix is prepended to the generated name of every server. STACKIT server names
	// must be a DNS-style label sequence and are limited to 63 characters; the generated suffix
	// takes 21 of them.
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?$`
	// +optional
	ServerNamePrefix *string `json:"serverNamePrefix,omitempty"`

	// kubelet defines args used when configuring kubelet on provisioned nodes. They are a subset of
	// the upstream types, recognizing not all options may be supported. Karpenter only uses these
	// to compute allocatable capacity; applying them to the node itself is the responsibility of
	// the bootstrap script in userData.
	// +kubebuilder:validation:XValidation:message="evictionSoft OwnerKey does not have a matching evictionSoftGracePeriod",rule="has(self.evictionSoft) ? self.evictionSoft.all(e, (e in self.evictionSoftGracePeriod)):true"
	// +kubebuilder:validation:XValidation:message="evictionSoftGracePeriod OwnerKey does not have a matching evictionSoft",rule="has(self.evictionSoftGracePeriod) ? self.evictionSoftGracePeriod.all(e, (e in self.evictionSoft)):true"
	// +optional
	Kubelet *KubeletConfiguration `json:"kubelet,omitempty"`
}

// BootVolumeSpec describes the root disk of a provisioned server.
//
// STACKIT creates the boot volume as a separate resource and then boots the server from it. The
// IaaS API's DeleteServer explicitly does not delete volumes, and deleteOnTermination defaults to
// false server-side, so every terminated node would leave a billable volume behind. This provider
// defaults deleteOnTermination to true for that reason.
type BootVolumeSpec struct {
	// source is what the root disk is created from — normally an image.
	// +required
	Source BootVolumeSourceSpec `json:"source"`

	// size is the root disk size in gigabytes. It must be at least as large as the source image.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=8192
	// +required
	Size int64 `json:"size"`

	// performanceClass of the root disk, e.g. "storage_premium_perf1". Available classes are
	// project- and region-specific; list them with the IaaS API's volume-performance-classes
	// endpoint. When unset, STACKIT's project default applies.
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9]+([ /._-]*[A-Za-z0-9]+)*$`
	// +optional
	PerformanceClass *string `json:"performanceClass,omitempty"`

	// deleteOnTermination controls whether the boot volume is deleted along with the server.
	// Defaults to true. Setting it to false leaks a billable volume for every node Karpenter
	// terminates, and those volumes are not tracked or cleaned up by this provider.
	// +kubebuilder:default=true
	// +optional
	DeleteOnTermination *bool `json:"deleteOnTermination,omitempty"`
}

// BootVolumeSourceSpec identifies what a root disk is created from.
type BootVolumeSourceSpec struct {
	// id of the source resource.
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`
	// +required
	ID string `json:"id"`

	// type of the source resource.
	// +kubebuilder:validation:Enum=image;volume;snapshot;backup
	// +kubebuilder:default=image
	// +optional
	Type *string `json:"type,omitempty"`
}

// NetworkingSpec attaches a provisioned server to a network.
type NetworkingSpec struct {
	// networkID is the UUID of the network to attach the server to. STACKIT allocates a NIC on
	// this network automatically. Mutually exclusive with nicIDs.
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`
	// +optional
	NetworkID *string `json:"networkID,omitempty"`

	// nicIDs are UUIDs of pre-created network interfaces to attach. Mutually exclusive with
	// networkID. Since a NIC can only be attached to one server, this is only useful for a
	// NodePool that will ever hold a single node, and is offered mainly for completeness.
	// +optional
	// +listType=set
	NICIDs []string `json:"nicIDs,omitempty"`
}

// KubeletConfiguration defines args to be used when configuring kubelet on provisioned nodes.
// They are a subset of the upstream types, recognizing not all options may be supported.
// Wherever possible, the types and names should reflect the upstream kubelet types.
// https://pkg.go.dev/k8s.io/kubelet/config/v1beta1#KubeletConfiguration
type KubeletConfiguration struct {
	// clusterDNS is a list of IP addresses for the cluster DNS server.
	// +optional
	ClusterDNS []string `json:"clusterDNS,omitempty"`
	// maxPods is an override for the maximum number of pods that can run on a worker node instance.
	// +kubebuilder:validation:Minimum:=0
	// +optional
	MaxPods *int32 `json:"maxPods,omitempty"`
	// podsPerCore is an override for the number of pods that can run on a worker node instance based
	// on the number of cpu cores. This value cannot exceed maxPods, so, if maxPods is a lower value,
	// that value will be used.
	// +kubebuilder:validation:Minimum:=0
	// +optional
	PodsPerCore *int32 `json:"podsPerCore,omitempty"`
	// systemReserved contains resources reserved for OS system daemons and kernel memory.
	// +kubebuilder:validation:XValidation:message="valid keys for systemReserved are ['cpu','memory','ephemeral-storage','pid']",rule="self.all(x, x=='cpu' || x=='memory' || x=='ephemeral-storage' || x=='pid')"
	// +kubebuilder:validation:XValidation:message="systemReserved value cannot be a negative resource quantity",rule="self.all(x, !self[x].startsWith('-'))"
	// +optional
	SystemReserved map[string]string `json:"systemReserved,omitempty"`
	// kubeReserved contains resources reserved for Kubernetes system components.
	// +kubebuilder:validation:XValidation:message="valid keys for kubeReserved are ['cpu','memory','ephemeral-storage','pid']",rule="self.all(x, x=='cpu' || x=='memory' || x=='ephemeral-storage' || x=='pid')"
	// +kubebuilder:validation:XValidation:message="kubeReserved value cannot be a negative resource quantity",rule="self.all(x, !self[x].startsWith('-'))"
	// +optional
	KubeReserved map[string]string `json:"kubeReserved,omitempty"`
	// evictionHard is the map of signal names to quantities that define hard eviction thresholds.
	// +kubebuilder:validation:XValidation:message="valid keys for evictionHard are ['memory.available','nodefs.available','nodefs.inodesFree','imagefs.available','imagefs.inodesFree','pid.available']",rule="self.all(x, x in ['memory.available','nodefs.available','nodefs.inodesFree','imagefs.available','imagefs.inodesFree','pid.available'])"
	// +optional
	EvictionHard map[string]string `json:"evictionHard,omitempty"`
	// evictionSoft is the map of signal names to quantities that define soft eviction thresholds.
	// +kubebuilder:validation:XValidation:message="valid keys for evictionSoft are ['memory.available','nodefs.available','nodefs.inodesFree','imagefs.available','imagefs.inodesFree','pid.available']",rule="self.all(x, x in ['memory.available','nodefs.available','nodefs.inodesFree','imagefs.available','imagefs.inodesFree','pid.available'])"
	// +optional
	EvictionSoft map[string]string `json:"evictionSoft,omitempty"`
	// evictionSoftGracePeriod is the map of signal names to quantities that define grace periods for
	// each eviction signal.
	// +kubebuilder:validation:XValidation:message="valid keys for evictionSoftGracePeriod are ['memory.available','nodefs.available','nodefs.inodesFree','imagefs.available','imagefs.inodesFree','pid.available']",rule="self.all(x, x in ['memory.available','nodefs.available','nodefs.inodesFree','imagefs.available','imagefs.inodesFree','pid.available'])"
	// +optional
	EvictionSoftGracePeriod map[string]metav1.Duration `json:"evictionSoftGracePeriod,omitempty"`
	// evictionMaxPodGracePeriod is the maximum allowed grace period (in seconds) to use when
	// terminating pods in response to soft eviction thresholds being met.
	// +optional
	EvictionMaxPodGracePeriod *int32 `json:"evictionMaxPodGracePeriod,omitempty"`
	// cpuCFSQuota enables CPU CFS quota enforcement for containers that specify CPU limits.
	// +optional
	CPUCFSQuota *bool `json:"cpuCFSQuota,omitempty"`
}

// StackitNodeClass is the Schema for the StackitNodeClass API
// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type==\"Ready\")].status",description=""
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description=""
// +kubebuilder:printcolumn:name="Zones",type="string",JSONPath=".spec.availabilityZones",priority=1,description=""
// +kubebuilder:resource:path=stackitnodeclasses,scope=Cluster,categories=karpenter,shortName={snc,sncs}
// +kubebuilder:storageversion
// +kubebuilder:subresource:status
type StackitNodeClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StackitNodeClassSpec   `json:"spec,omitempty"`
	Status StackitNodeClassStatus `json:"status,omitempty"`
}

// StackitNodeClassHashVersion must be bumped when any of the following happens:
//  1. A hashed field changes its default value.
//  2. A field with an already-set value is added to the hash calculation.
//  3. A field is removed from the hash calculation.
//
// Bumping it prevents Karpenter from mass-replacing existing nodes just because the hash algorithm
// moved underneath them.
const StackitNodeClassHashVersion = "v1"

// Hash generates a hash of the fields of the StackitNodeClass that require a node to be replaced
// when they change. Fields that can be reconciled in place on a running server are excluded.
func (in *StackitNodeClass) Hash() string {
	hashableSpec := in.Spec
	// STACKIT server labels are updated in place on running servers by the labeling controller, so
	// they must not force replacement.
	hashableSpec.Labels = nil
	return fmt.Sprint(lo.Must(hashstructure.Hash([]interface{}{
		hashableSpec,
	}, hashstructure.FormatV2, &hashstructure.HashOptions{
		SlicesAsSets:    true,
		IgnoreZeroValue: true,
		ZeroNil:         true,
	})))
}

func (in *StackitNodeClass) KubeletConfiguration() *KubeletConfiguration {
	return in.Spec.Kubelet
}

// BootVolumeSizeGB is the size of the root disk servers are created with. It is what the kubelet
// reports as ephemeral storage, since STACKIT servers boot from a volume rather than from a disk
// bundled with the machine type.
func (in *StackitNodeClass) BootVolumeSizeGB() int64 {
	return in.Spec.BootVolume.Size
}

// BootVolumeSourceType is the resolved boot volume source type, defaulted to "image".
func (in *StackitNodeClass) BootVolumeSourceType() string {
	return lo.FromPtrOr(in.Spec.BootVolume.Source.Type, "image")
}

// DeleteBootVolumeOnTermination reports whether the boot volume should be torn down with the
// server. It defaults to true — the STACKIT API defaults it to false, which leaks a volume per
// node.
func (in *StackitNodeClass) DeleteBootVolumeOnTermination() bool {
	return lo.FromPtrOr(in.Spec.BootVolume.DeleteOnTermination, true)
}

func (in *StackitNodeClass) GetConditions() []status.Condition {
	return in.Status.Conditions
}

func (in *StackitNodeClass) SetConditions(conditions []status.Condition) {
	in.Status.Conditions = conditions
}

func (in *StackitNodeClass) StatusConditions(opts ...status.ForOption) status.ConditionSet {
	return status.NewReadyConditions(
		ConditionTypeAvailabilityZonesReady,
		ConditionTypeValidationSucceeded,
	).For(in, opts...)
}

// +kubebuilder:object:root=true

// StackitNodeClassList contains a list of StackitNodeClass
type StackitNodeClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StackitNodeClass `json:"items"`
}
