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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/kubekanvas/karpenter-provider-stackit/pkg/apis"
)

func init() {
	// STACKIT, unlike UpCloud, has a genuine region/availability-zone split, and its
	// cloud-controller-manager labels nodes accordingly: topology.kubernetes.io/zone gets the
	// server's own availabilityZone, while topology.kubernetes.io/region gets the region the CCM
	// itself was configured with. Both well known topology labels are therefore meaningful and
	// carry different values. See pkg/providers/instancetype for where they are set.
	//
	// Windows is never a possibility on STACKIT-provisioned Karpenter nodes, so drop the Windows
	// build label from the well known set to keep scheduling requirements minimal.
	unused := []string{
		corev1.LabelWindowsBuild,
	}
	karpv1.RestrictedLabelDomains = karpv1.RestrictedLabelDomains.Insert(RestrictedLabelDomains...)
	karpv1.WellKnownLabels = karpv1.WellKnownLabels.Union(StackitWellKnownLabels)
	karpv1.WellKnownLabels = karpv1.WellKnownLabels.Delete(unused...)
}

var (
	RestrictedLabelDomains = []string{
		apis.Group,
	}

	// StackitWellKnownLabels belong to RestrictedLabelDomains but are allowed: Karpenter is aware
	// of them, so NodePools and pods can narrow scheduling down by any of these dimensions.
	StackitWellKnownLabels = sets.New(
		LabelInstanceCPU,
		LabelInstanceMemory,
		LabelInstanceFamily,
		LabelInstanceDisk,
	)
)

const (
	// LabelNodeClass is set on every provisioned server so that servers can be mapped back to the
	// StackitNodeClass that launched them without consulting the Kubernetes API.
	LabelNodeClass = apis.Group + "/nodeclass"

	LabelInstanceCPU    = apis.Group + "/instance-cpu"
	LabelInstanceMemory = apis.Group + "/instance-memory"
	LabelInstanceFamily = apis.Group + "/instance-family"
	LabelInstanceDisk   = apis.Group + "/instance-disk"

	AnnotationStackitNodeClassHash        = apis.Group + "/stackitnodeclass-hash"
	AnnotationStackitNodeClassHashVersion = apis.Group + "/stackitnodeclass-hash-version"

	// TerminationFinalizer is placed on StackitNodeClasses so that the class cannot disappear while
	// servers launched from it are still running.
	TerminationFinalizer = apis.Group + "/termination"
)

// STACKIT server label keys used for resource discovery.
//
// The IaaS API accepts label keys containing '/' — the machine-controller-manager provider uses
// "kubernetes.io/machine" — so the karpenter.sh/* keys below are valid as written. They are
// validated at launch time all the same (see ValidateLabels), because a rejected CreateServer call
// after the NodeClaim exists is far more expensive than a rejected NodeClass.
//
// Changing any of them is a breaking change: previously launched servers would stop being
// discovered and would leak.
const (
	// LabelKeyManagedBy holds the cluster name, mirroring the semantics of karpenter.sh/managed-by
	// on other providers. It is the key the instance provider filters on when listing servers.
	LabelKeyManagedBy = "karpenter.sh/managed-by"
	// LabelKeyNodePool holds the owning NodePool name.
	LabelKeyNodePool = karpv1.NodePoolLabelKey
	// LabelKeyNodeClaim holds the owning NodeClaim name.
	LabelKeyNodeClaim = "karpenter.sh/nodeclaim"
	// LabelKeyNodeClass holds the owning StackitNodeClass name.
	LabelKeyNodeClass = LabelNodeClass
)

// ManagedLabelKeys are the label keys this provider owns on a STACKIT server. A NodeClass may not
// set them through spec.labels, since doing so would break server discovery or misattribute a
// server to another NodePool.
func ManagedLabelKeys() sets.Set[string] {
	return sets.New(
		LabelKeyManagedBy,
		LabelKeyNodePool,
		LabelKeyNodeClaim,
		LabelKeyNodeClass,
	)
}
