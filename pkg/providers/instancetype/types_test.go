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

package instancetype_test

import (
	"context"
	"testing"

	iaas "github.com/stackitcloud/stackit-sdk-go/services/iaas/v2api"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/kubekanvas/karpenter-provider-stackit/pkg/apis/v1alpha1"
	"github.com/kubekanvas/karpenter-provider-stackit/pkg/operator/options"
	"github.com/kubekanvas/karpenter-provider-stackit/pkg/providers/instancetype"
)

// testContext supplies the options the memory calculation reads. A zero overhead keeps the
// arithmetic in the capacity tests exact.
func testContext(overhead float64) context.Context {
	return options.ToContext(context.Background(), &options.Options{
		ClusterName:             "test",
		ProjectID:               "00000000-0000-0000-0000-000000000000",
		Region:                  testRegion,
		VMMemoryOverheadPercent: overhead,
	})
}

const (
	zoneA      = "eu01-1"
	zoneB      = "eu01-2"
	testRegion = "eu01"
)

func machineType(name string, vcpus, ramMiB, diskGiB int64) *iaas.MachineType {
	return &iaas.MachineType{Name: name, Vcpus: vcpus, Ram: ramMiB, Disk: diskGiB}
}

func TestMachineTypeFamily(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		in   string
		want string
	}{
		{in: "g1.2", want: "g1"},
		{in: "g2i.4", want: "g2i"},
		{in: "c1a.8", want: "c1a"},
		{in: "M1.16", want: "m1"},
		{in: "nodot", want: "unknown"},
		{in: "", want: "unknown"},
		{in: ".4", want: "unknown"},
	} {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			if got := instancetype.MachineTypeFamily(tc.in); got != tc.want {
				t.Errorf("MachineTypeFamily(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRequirementsSeparateZoneAndRegion is the STACKIT-specific invariant: unlike UpCloud, the
// cloud-controller-manager writes a per-server availabilityZone to topology.kubernetes.io/zone and
// its own configured region to topology.kubernetes.io/region. Collapsing the two — as the UpCloud
// provider correctly does for UpCloud — would produce requirements no STACKIT node can satisfy.
func TestRequirementsSeparateZoneAndRegion(t *testing.T) {
	t.Parallel()

	it := instancetype.NewInstanceType(testContext(0), machineType("g1.2", 2, 8192, 0),
		testRegion, []string{zoneA, zoneB}, 50, &v1alpha1.KubeletConfiguration{})

	zone := it.Requirements.Get(corev1.LabelTopologyZone)
	if got := zone.Values(); len(got) != 2 {
		t.Fatalf("zone requirement = %v, want two zones", got)
	}
	if !zone.Has(zoneA) || !zone.Has(zoneB) {
		t.Errorf("zone requirement = %v, want eu01-1 and eu01-2", zone.Values())
	}

	regionReq := it.Requirements.Get(corev1.LabelTopologyRegion)
	if got := regionReq.Values(); len(got) != 1 || got[0] != testRegion {
		t.Errorf("region requirement = %v, want exactly [eu01]", got)
	}
	if zone.Has(testRegion) && regionReq.Has(zoneA) {
		t.Error("zone and region requirements are collapsed; STACKIT distinguishes them")
	}
}

// TestRequirementsSanitizeZones makes sure a zone id that the CCM's label sanitizer would rewrite
// is stored in its rewritten form, so the requirement matches the node label.
func TestRequirementsSanitizeZones(t *testing.T) {
	t.Parallel()

	it := instancetype.NewInstanceType(testContext(0), machineType("g1.2", 2, 8192, 0),
		testRegion, []string{"eu01 1"}, 50, &v1alpha1.KubeletConfiguration{})

	zone := it.Requirements.Get(corev1.LabelTopologyZone)
	if !zone.Has("eu01-1") {
		t.Errorf("zone requirement = %v, want the sanitized form eu01-1", zone.Values())
	}
	if zone.Has("eu01 1") {
		t.Error("zone requirement kept the unsanitized value; it can never match a node label")
	}
}

// TestRequirementsCapacityTypeIsOnDemandOnly pins the hardcoded capacity type. STACKIT's IaaS API
// has no spot or preemptible market, so offering anything else would schedule work onto capacity
// that does not exist.
func TestRequirementsCapacityTypeIsOnDemandOnly(t *testing.T) {
	t.Parallel()

	it := instancetype.NewInstanceType(testContext(0), machineType("g1.2", 2, 8192, 0),
		testRegion, []string{zoneA}, 50, &v1alpha1.KubeletConfiguration{})

	capacityType := it.Requirements.Get(karpv1.CapacityTypeLabelKey)
	got := capacityType.Values()
	if len(got) != 1 || got[0] != karpv1.CapacityTypeOnDemand {
		t.Errorf("capacity type requirement = %v, want exactly [%s]", got, karpv1.CapacityTypeOnDemand)
	}
}

func TestRequirementsWellKnownStackitLabels(t *testing.T) {
	t.Parallel()

	it := instancetype.NewInstanceType(testContext(0), machineType("g1.4", 4, 16384, 20),
		testRegion, []string{zoneA}, 50, &v1alpha1.KubeletConfiguration{})

	for label, want := range map[string]string{
		corev1.LabelInstanceTypeStable: "g1.4",
		corev1.LabelArchStable:         karpv1.ArchitectureAmd64,
		corev1.LabelOSStable:           string(corev1.Linux),
		v1alpha1.LabelInstanceCPU:      "4",
		v1alpha1.LabelInstanceMemory:   "16384",
		v1alpha1.LabelInstanceFamily:   "g1",
		v1alpha1.LabelInstanceDisk:     "20",
	} {
		if got := it.Requirements.Get(label).Any(); got != want {
			t.Errorf("requirement %s = %q, want %q", label, got, want)
		}
	}
}

func TestCapacity(t *testing.T) {
	t.Parallel()

	it := instancetype.NewInstanceType(testContext(0), machineType("g1.2", 2, 8192, 0),
		testRegion, []string{zoneA}, 50, &v1alpha1.KubeletConfiguration{})

	if got := it.Capacity.Cpu(); got.Value() != 2 {
		t.Errorf("cpu capacity = %s, want 2", got)
	}
	if got, want := it.Capacity.Memory(), resource.MustParse("8192Mi"); got.Cmp(want) != 0 {
		t.Errorf("memory capacity = %s, want %s at zero overhead", got, &want)
	}
	if got := it.Capacity.Pods(); got.Value() != 110 {
		t.Errorf("pods capacity = %s, want the kubelet default of 110", got)
	}
}

// TestCapacityEphemeralStorageComesFromBootVolume pins the choice not to use MachineType.Disk.
// STACKIT boots from a separately created volume, and Disk is 0 for most machine types, so reading
// it would advertise nodes with no usable ephemeral storage.
func TestCapacityEphemeralStorageComesFromBootVolume(t *testing.T) {
	t.Parallel()

	// Disk is 0, which is what STACKIT reports for volume-booted machine types.
	it := instancetype.NewInstanceType(testContext(0), machineType("g1.2", 2, 8192, 0),
		testRegion, []string{zoneA}, 50, &v1alpha1.KubeletConfiguration{})

	got := it.Capacity[corev1.ResourceEphemeralStorage]
	want := resource.MustParse("46G") // 50G boot volume less the 4G OS reservation.
	if got.Cmp(want) != 0 {
		t.Errorf("ephemeral storage = %s, want %s", got.String(), want.String())
	}
	if got.IsZero() {
		t.Error("ephemeral storage is zero; MachineType.Disk was used instead of the boot volume")
	}
}

func TestCapacityEphemeralStorageNeverGoesNegative(t *testing.T) {
	t.Parallel()

	// A boot volume smaller than the OS reservation must not produce a negative or zero quantity.
	it := instancetype.NewInstanceType(testContext(0), machineType("g1.2", 2, 8192, 0),
		testRegion, []string{zoneA}, 2, &v1alpha1.KubeletConfiguration{})

	got := it.Capacity[corev1.ResourceEphemeralStorage]
	if got.Sign() <= 0 {
		t.Errorf("ephemeral storage = %s, want a positive quantity", got.String())
	}
}

func TestCapacityMemoryOverheadIsSubtracted(t *testing.T) {
	t.Parallel()

	it := instancetype.NewInstanceType(testContext(0.075), machineType("g1.2", 2, 8192, 0),
		testRegion, []string{zoneA}, 50, &v1alpha1.KubeletConfiguration{})

	got := it.Capacity.Memory()
	full := resource.MustParse("8192Mi")
	if got.Cmp(full) >= 0 {
		t.Errorf("memory capacity = %s, want less than the advertised %s", got, &full)
	}
	// 7.5% of 8192Mi is 614.4Mi, rounded up to 615Mi.
	want := resource.MustParse("7577Mi")
	if got.Cmp(want) != 0 {
		t.Errorf("memory capacity = %s, want %s", got, &want)
	}
}

func TestCapacityMaxPodsOverride(t *testing.T) {
	t.Parallel()

	maxPods := int32(58)
	it := instancetype.NewInstanceType(testContext(0), machineType("g1.2", 2, 8192, 0),
		testRegion, []string{zoneA}, 50, &v1alpha1.KubeletConfiguration{MaxPods: &maxPods})

	if got := it.Capacity.Pods(); got.Value() != 58 {
		t.Errorf("pods capacity = %s, want 58", got)
	}
}

func TestCapacityPodsPerCoreCapsMaxPods(t *testing.T) {
	t.Parallel()

	podsPerCore := int32(10)
	it := instancetype.NewInstanceType(testContext(0), machineType("g1.2", 2, 8192, 0),
		testRegion, []string{zoneA}, 50, &v1alpha1.KubeletConfiguration{PodsPerCore: &podsPerCore})

	// 10 pods per core * 2 cores = 20, which is below the default of 110 and therefore wins.
	if got := it.Capacity.Pods(); got.Value() != 20 {
		t.Errorf("pods capacity = %s, want 20", got)
	}
}

func TestOverheadIsNonZero(t *testing.T) {
	t.Parallel()

	it := instancetype.NewInstanceType(testContext(0), machineType("g1.2", 2, 8192, 0),
		testRegion, []string{zoneA}, 50, &v1alpha1.KubeletConfiguration{})

	if it.Overhead == nil {
		t.Fatal("overhead is nil")
	}
	if it.Overhead.KubeReserved.Memory().IsZero() {
		t.Error("kube-reserved memory is zero; allocatable would overstate the node")
	}
	if it.Overhead.KubeReserved.Cpu().IsZero() {
		t.Error("kube-reserved cpu is zero; allocatable would overstate the node")
	}
	if it.Overhead.EvictionThreshold.Memory().IsZero() {
		t.Error("eviction threshold memory is zero")
	}
	// Allocatable must end up strictly below capacity, or Karpenter will pack nodes it cannot fill.
	allocatable := it.Allocatable()
	capacity := it.Capacity
	if allocatable.Memory().Cmp(*capacity.Memory()) >= 0 {
		t.Error("allocatable memory is not below capacity")
	}
}

func TestOverheadEvictionThresholdAcceptsPercentages(t *testing.T) {
	t.Parallel()

	it := instancetype.NewInstanceType(testContext(0), machineType("g1.2", 2, 8192, 0),
		testRegion, []string{zoneA}, 50,
		&v1alpha1.KubeletConfiguration{EvictionHard: map[string]string{"memory.available": "5%"}})

	got := it.Overhead.EvictionThreshold.Memory()
	// 5% of 8192Mi, well above the 100Mi default that would otherwise apply.
	want := resource.MustParse("100Mi")
	if got.Cmp(want) <= 0 {
		t.Errorf("eviction threshold = %s, want more than the %s default", got, &want)
	}
}
