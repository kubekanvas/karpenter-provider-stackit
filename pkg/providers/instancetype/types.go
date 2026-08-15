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

package instancetype

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/mitchellh/hashstructure/v2"
	"github.com/samber/lo"
	iaas "github.com/stackitcloud/stackit-sdk-go/services/iaas/v2api"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"
	"sigs.k8s.io/karpenter/pkg/utils/resources"

	"github.com/kubekanvas/karpenter-provider-stackit/pkg/apis/v1alpha1"
	"github.com/kubekanvas/karpenter-provider-stackit/pkg/operator/options"
	"github.com/kubekanvas/karpenter-provider-stackit/pkg/utils"
)

const (
	memoryAvailable = "memory.available"

	// defaultMaxPods matches the kubelet default. STACKIT imposes no per-server pod or NIC limit of
	// its own, so unlike on AWS this is a plain constant rather than a per-machine-type lookup.
	defaultMaxPods = 110

	// osReservedStorageGB is carved out of the boot volume before it is advertised as
	// ephemeral-storage. It covers the image itself plus room for logs and the container runtime's
	// own metadata.
	osReservedStorageGB = 4

	// familyUnknown is the label value for a machine type whose name carries no family prefix.
	familyUnknown = "unknown"
)

// NodeClass is the subset of StackitNodeClass that instance type resolution depends on. Keeping it
// an interface lets the resolver be exercised without constructing a full NodeClass.
type NodeClass interface {
	KubeletConfiguration() *v1alpha1.KubeletConfiguration
	AvailabilityZoneIDs() []string
	BootVolumeSizeGB() int64
}

// Resolver turns a raw STACKIT machine type into a Karpenter InstanceType.
type Resolver interface {
	// CacheKey tells the InstanceType cache what about the NodeClass changes the resulting
	// InstanceTypes. Anything Resolve reads off the NodeClass must be reflected here.
	CacheKey(nodeClass NodeClass) string
	// Resolve generates an InstanceType from a machine type and the NodeClass settings.
	Resolve(ctx context.Context, machineType *iaas.MachineType, nodeClass NodeClass) *cloudprovider.InstanceType
}

// DefaultResolver resolves instance types for one STACKIT region.
//
// The region is a resolver-level value rather than a NodeClass field because
// cloud-provider-stackit takes its region from its own config and writes it to every node as
// topology.kubernetes.io/region (pkg/ccm/instances.go: Region: i.region). A NodeClass free to pick
// a different region would produce NodeClaims whose region requirement no node can ever satisfy.
type DefaultResolver struct {
	region string
}

func NewDefaultResolver(region string) *DefaultResolver {
	return &DefaultResolver{region: region}
}

func (d DefaultResolver) CacheKey(nodeClass NodeClass) string {
	// The zone set changes the instance type's requirements; the boot volume size changes
	// ephemeral-storage capacity; the kubelet configuration changes both capacity and overhead.
	// Anything Resolve reads has to be reflected here.
	hash, _ := hashstructure.Hash([]interface{}{
		d.region,
		nodeClass.AvailabilityZoneIDs(),
		nodeClass.BootVolumeSizeGB(),
		nodeClass.KubeletConfiguration(),
	}, hashstructure.FormatV2, &hashstructure.HashOptions{SlicesAsSets: true})
	return fmt.Sprintf("%016x", hash)
}

func (d DefaultResolver) Resolve(ctx context.Context, machineType *iaas.MachineType, nodeClass NodeClass) *cloudprovider.InstanceType {
	// !!! Important !!!
	// Everything read off the NodeClass here must also be reflected in CacheKey, or stale
	// InstanceTypes will be served after the NodeClass changes.
	// !!! Important !!!
	kc := &v1alpha1.KubeletConfiguration{}
	if resolved := nodeClass.KubeletConfiguration(); resolved != nil {
		kc = resolved
	}
	return NewInstanceType(ctx, machineType, d.region, nodeClass.AvailabilityZoneIDs(), nodeClass.BootVolumeSizeGB(), kc)
}

func NewInstanceType(
	ctx context.Context,
	machineType *iaas.MachineType,
	region string,
	zones []string,
	bootVolumeGB int64,
	kc *v1alpha1.KubeletConfiguration,
) *cloudprovider.InstanceType {
	return &cloudprovider.InstanceType{
		Name:         machineType.Name,
		Requirements: computeRequirements(machineType, region, zones),
		Capacity:     computeCapacity(ctx, machineType, bootVolumeGB, kc.MaxPods, kc.PodsPerCore),
		Overhead: &cloudprovider.InstanceTypeOverhead{
			KubeReserved:      kubeReservedResources(cpu(machineType), pods(machineType, kc.MaxPods, kc.PodsPerCore), kc.KubeReserved),
			SystemReserved:    systemReservedResources(kc.SystemReserved),
			EvictionThreshold: evictionThreshold(memory(ctx, machineType), kc.EvictionHard, kc.EvictionSoft),
		},
	}
}

func computeRequirements(machineType *iaas.MachineType, region string, zones []string) scheduling.Requirements {
	// Zones are sanitized with the same transform cloud-provider-stackit applies before writing
	// topology.kubernetes.io/zone (pkg/labels/labels.go: Sanitize). Skipping this would produce a
	// requirement the node's own label can never satisfy for any zone id containing a character
	// outside [-a-zA-Z0-9_.].
	sanitizedZones := lo.Map(zones, func(zone string, _ int) string { return utils.SanitizeZone(zone) })

	return scheduling.NewRequirements(
		// Well known upstream.
		scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, machineType.Name),
		// STACKIT has a real region/AZ split, and its cloud-controller-manager sets the two
		// topology labels to different things: zone comes from the server's own availabilityZone,
		// region from the CCM's configuration. Mirroring that split here is what lets a launched
		// node's labels satisfy the NodeClaim's requirements and register.
		scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, sanitizedZones...),
		scheduling.NewRequirement(corev1.LabelTopologyRegion, corev1.NodeSelectorOpIn, region),
		// Every STACKIT machine type runs on x86-64 Linux, and STACKIT has no spot or preemptible
		// market: the IaaS API exposes no interruptible instance concept at all, so on-demand is
		// the only capacity type that can ever be offered.
		scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, karpv1.ArchitectureAmd64),
		scheduling.NewRequirement(corev1.LabelOSStable, corev1.NodeSelectorOpIn, string(corev1.Linux)),
		scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
		// Well known to STACKIT.
		scheduling.NewRequirement(v1alpha1.LabelInstanceCPU, corev1.NodeSelectorOpIn, fmt.Sprint(machineType.Vcpus)),
		scheduling.NewRequirement(v1alpha1.LabelInstanceMemory, corev1.NodeSelectorOpIn, fmt.Sprint(machineType.Ram)),
		scheduling.NewRequirement(v1alpha1.LabelInstanceFamily, corev1.NodeSelectorOpIn, MachineTypeFamily(machineType.Name)),
		scheduling.NewRequirement(v1alpha1.LabelInstanceDisk, corev1.NodeSelectorOpIn, fmt.Sprint(machineType.Disk)),
	)
}

// MachineTypeFamily extracts the family prefix from a STACKIT machine type name. Machine types are
// named "<family>.<size>", e.g. "g1.2", "c1a.4", "m2i.8".
func MachineTypeFamily(name string) string {
	prefix, _, ok := strings.Cut(name, ".")
	if !ok || prefix == "" {
		return familyUnknown
	}
	return strings.ToLower(prefix)
}

func computeCapacity(
	ctx context.Context,
	machineType *iaas.MachineType,
	bootVolumeGB int64,
	maxPods, podsPerCore *int32,
) corev1.ResourceList {
	// GPU machine types are not advertised with a gpu extended resource. STACKIT reports GPU
	// details, if at all, inside MachineType.extraSpecs, whose keys the IaaS API does not document
	// and which neither cloud-provider-stackit nor machine-controller-manager-provider-stackit
	// reads. Guessing a key would silently mis-advertise capacity, so GPU machine types are
	// currently schedulable on their CPU and memory only.
	return corev1.ResourceList{
		corev1.ResourceCPU:              *cpu(machineType),
		corev1.ResourceMemory:           *memory(ctx, machineType),
		corev1.ResourcePods:             *pods(machineType, maxPods, podsPerCore),
		corev1.ResourceEphemeralStorage: *ephemeralStorage(bootVolumeGB),
	}
}

func cpu(machineType *iaas.MachineType) *resource.Quantity {
	return resources.Quantity(strconv.FormatInt(machineType.Vcpus, 10))
}

func memory(ctx context.Context, machineType *iaas.MachineType) *resource.Quantity {
	// STACKIT reports machine type memory in MiB. What the guest kernel actually sees is lower,
	// because the hypervisor and the kernel itself take a cut, so subtract a configurable
	// percentage. The capacity controller replaces this estimate with the real value once a node of
	// this machine type has joined the cluster.
	mem := resources.Quantity(fmt.Sprintf("%dMi", machineType.Ram))
	overheadMiB := int64(math.Ceil(float64(mem.Value()) * options.FromContext(ctx).VMMemoryOverheadPercent / 1024 / 1024))
	mem.Sub(resource.MustParse(fmt.Sprintf("%dMi", overheadMiB)))
	return mem
}

// ephemeralStorage is derived from the NodeClass boot volume, not from MachineType.Disk.
//
// STACKIT boots servers from a separately created volume whose size this provider chooses, so the
// root filesystem the kubelet actually reports is the boot volume. MachineType.Disk describes the
// machine type's own included disk and is 0 for most types; using it would advertise a node with
// no ephemeral storage.
func ephemeralStorage(bootVolumeGB int64) *resource.Quantity {
	usable := bootVolumeGB - osReservedStorageGB
	if usable < 1 {
		usable = 1
	}
	return resources.Quantity(fmt.Sprintf("%dG", usable))
}

func pods(machineType *iaas.MachineType, maxPods, podsPerCore *int32) *resource.Quantity {
	count := int64(defaultMaxPods)
	if maxPods != nil {
		count = int64(lo.FromPtr(maxPods))
	}
	if lo.FromPtr(podsPerCore) > 0 {
		count = lo.Min([]int64{int64(lo.FromPtr(podsPerCore)) * machineType.Vcpus, count})
	}
	return resources.Quantity(fmt.Sprint(count))
}

func systemReservedResources(systemReserved map[string]string) corev1.ResourceList {
	return lo.MapEntries(systemReserved, func(k string, v string) (corev1.ResourceName, resource.Quantity) {
		return corev1.ResourceName(k), resource.MustParse(v)
	})
}

// kubeReservedResources mirrors the tiered kube-reserved defaults used by the other Karpenter
// providers, which in turn follow the GKE/Bottlerocket formula. STACKIT ships no opinion of its own
// here, so matching the ecosystem default is the least surprising choice.
func kubeReservedResources(cpus, pods *resource.Quantity, kubeReserved map[string]string) corev1.ResourceList {
	resourceList := corev1.ResourceList{
		corev1.ResourceMemory:           resource.MustParse(fmt.Sprintf("%dMi", (11*pods.Value())+255)),
		corev1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
	}
	for _, cpuRange := range []struct {
		start      int64
		end        int64
		percentage float64
	}{
		{start: 0, end: 1000, percentage: 0.06},
		{start: 1000, end: 2000, percentage: 0.01},
		{start: 2000, end: 4000, percentage: 0.005},
		{start: 4000, end: 1 << 31, percentage: 0.0025},
	} {
		cpuMilli := cpus.MilliValue()
		if cpuMilli < cpuRange.start {
			continue
		}
		r := float64(cpuRange.end - cpuRange.start)
		if cpuMilli < cpuRange.end {
			r = float64(cpuMilli - cpuRange.start)
		}
		cpuOverhead := resourceList.Cpu()
		cpuOverhead.Add(*resource.NewMilliQuantity(int64(r*cpuRange.percentage), resource.DecimalSI))
		resourceList[corev1.ResourceCPU] = *cpuOverhead
	}
	return lo.Assign(resourceList, lo.MapEntries(kubeReserved, func(k string, v string) (corev1.ResourceName, resource.Quantity) {
		return corev1.ResourceName(k), resource.MustParse(v)
	}))
}

func evictionThreshold(memory *resource.Quantity, evictionHard, evictionSoft map[string]string) corev1.ResourceList {
	overhead := corev1.ResourceList{
		corev1.ResourceMemory: resource.MustParse("100Mi"),
	}

	override := corev1.ResourceList{}
	var evictionSignals []map[string]string
	if evictionHard != nil {
		evictionSignals = append(evictionSignals, evictionHard)
	}
	if evictionSoft != nil {
		evictionSignals = append(evictionSignals, evictionSoft)
	}
	for _, m := range evictionSignals {
		temp := corev1.ResourceList{}
		if v, ok := m[memoryAvailable]; ok {
			temp[corev1.ResourceMemory] = computeEvictionSignal(*memory, v)
		}
		override = resources.MaxResources(override, temp)
	}
	// Assign merges left to right, so the override always wins.
	return lo.Assign(overhead, override)
}

// computeEvictionSignal resolves an eviction signal against the node's capacity, handling both
// absolute quantities and the percentage form documented at
// https://kubernetes.io/docs/concepts/scheduling-eviction/node-pressure-eviction/#eviction-signals
func computeEvictionSignal(capacity resource.Quantity, signalValue string) resource.Quantity {
	if strings.HasSuffix(signalValue, "%") {
		p := mustParsePercentage(signalValue)
		return resource.MustParse(fmt.Sprint(math.Ceil(capacity.AsApproximateFloat64() / 100 * p)))
	}
	return resource.MustParse(signalValue)
}

func mustParsePercentage(v string) float64 {
	p, err := strconv.ParseFloat(strings.Trim(v, "%"), 64)
	if err != nil {
		panic(fmt.Sprintf("expected percentage value to be a float but got %s, %v", v, err))
	}
	// 100% disables the threshold.
	// https://kubernetes.io/docs/reference/config-api/kubelet-config.v1beta1/
	if p == 100 {
		p = 0
	}
	return p
}
