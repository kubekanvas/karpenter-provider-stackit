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

package utils

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/awslabs/operatorpkg/serrors"
	"github.com/samber/lo"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/kubekanvas/karpenter-provider-stackit/pkg/apis/v1alpha1"
)

// ProviderIDPrefix is the prefix the STACKIT cloud-controller-manager writes into
// Node.spec.providerID. Karpenter matches NodeClaims to Nodes on this exact string, so it must stay
// byte-for-byte identical to the CCM's.
//
// cloud-provider-stackit builds it as fmt.Sprintf("%s://%s", "stackit", server.GetId())
// (pkg/ccm/instances.go, makeInstanceID) — two slashes, no region segment.
//
// The CCM's *parser* additionally accepts a bare id and the legacy
// "openstack://<region>/<id>" form, and declares an OS_CCM_REGIONAL env var, but NewInstance
// hardcodes regionProviderID: false and nothing ever reads it back. No supported configuration
// writes the regional form, so emitting anything but the canonical shape below would leave
// NodeClaims permanently unbound even though the CCM could still parse it.
const ProviderIDPrefix = "stackit://"

// STACKIT server label constraints. The IaaS API accepts label keys containing '/' — the
// machine-controller-manager provider uses "kubernetes.io/machine" — and does not publish a
// stricter charset, so this validates the conservative intersection that is safe to send: a
// non-empty printable-ASCII key with no whitespace or comma.
//
// Commas and equals signs matter specifically because ListServers filters via a
// "key=value,key=value" labelSelector string; a label containing either would produce a selector
// that silently parses into something else and make the server undiscoverable.
const (
	maxLabelKeyLength   = 255
	maxLabelValueLength = 255
)

var (
	labelKeyRegex   = regexp.MustCompile(`^[\x21-\x7E]+$`)
	labelValueRegex = regexp.MustCompile(`^[\x20-\x7E]*$`)

	// zoneSanitizeRegex mirrors labels.Sanitize in cloud-provider-stackit
	// (pkg/labels/labels.go), which is applied to the availability zone before it becomes the
	// node's topology.kubernetes.io/zone value. Instance type offerings must use the sanitized
	// form or scheduling requirements will not match the labels the CCM actually writes.
	zoneSanitizeRegex = regexp.MustCompile(`[^-a-zA-Z0-9_.]+`)
)

// ParseInstanceID extracts the STACKIT server UUID from a providerID.
func ParseInstanceID(providerID string) (string, error) {
	id, ok := strings.CutPrefix(providerID, ProviderIDPrefix)
	if !ok || id == "" || strings.Contains(id, "/") {
		return "", serrors.Wrap(fmt.Errorf("provider id does not match known format"), "provider-id", providerID)
	}
	return id, nil
}

// ProviderID renders a STACKIT server UUID as a Kubernetes providerID.
func ProviderID(id string) string {
	return ProviderIDPrefix + id
}

// SanitizeZone reproduces labels.Sanitize from cloud-provider-stackit: non-alphanumeric characters
// other than '-', '_' and '.' collapse to '-', leading and trailing '-_.' are trimmed, and the
// result is capped at 63 characters. Applying the identical transform on this side is what keeps
// an offering's zone equal to the node's topology.kubernetes.io/zone label.
func SanitizeZone(zone string) string {
	sanitized := zoneSanitizeRegex.ReplaceAllString(zone, "-")
	sanitized = strings.Trim(sanitized, "-_.")
	if len(sanitized) > 63 {
		sanitized = sanitized[:63]
	}
	return sanitized
}

// ManagedLabels returns the STACKIT server labels the controller stamps onto every server it
// launches. LabelKeyManagedBy is what List filters on, so a server missing it is invisible to this
// controller and will never be garbage collected.
//
// Unlike the UpCloud provider there is no created-at label: STACKIT's Server carries a real
// createdAt timestamp (model_server.go) that comes back on List, so garbage collection reads the
// API's own value rather than one this controller has to maintain.
func ManagedLabels(nodeClass *v1alpha1.StackitNodeClass, nodeClaim *karpv1.NodeClaim, clusterName string) map[string]string {
	managed := map[string]string{
		v1alpha1.LabelKeyManagedBy: clusterName,
		v1alpha1.LabelKeyNodeClass: nodeClass.Name,
		v1alpha1.LabelKeyNodeClaim: nodeClaim.Name,
	}
	if nodePool, ok := nodeClaim.Labels[karpv1.NodePoolLabelKey]; ok {
		managed[v1alpha1.LabelKeyNodePool] = nodePool
	}
	// User labels are applied first so that the managed keys always win.
	return lo.Assign(nodeClass.Spec.Labels, managed)
}

// ValidateLabels checks user-supplied labels against STACKIT's constraints and against the keys the
// controller reserves for itself.
func ValidateLabels(labels map[string]string) error {
	var problems []string
	reserved := v1alpha1.ManagedLabelKeys()
	for _, key := range sortedKeys(labels) {
		value := labels[key]
		switch {
		case key == "":
			problems = append(problems, "\"\": key must not be empty")
		case len(key) > maxLabelKeyLength:
			problems = append(problems, fmt.Sprintf("%q: key must be at most %d characters", key, maxLabelKeyLength))
		case !labelKeyRegex.MatchString(key):
			problems = append(problems, fmt.Sprintf("%q: key must be printable ASCII without whitespace", key))
		case strings.ContainsAny(key, ",="):
			problems = append(problems, fmt.Sprintf("%q: key must not contain ',' or '=', which delimit the label selector", key))
		case len(value) > maxLabelValueLength:
			problems = append(problems, fmt.Sprintf("%q: value must be at most %d characters", key, maxLabelValueLength))
		case !labelValueRegex.MatchString(value):
			problems = append(problems, fmt.Sprintf("%q: value must be printable ASCII", key))
		case strings.Contains(value, ","):
			problems = append(problems, fmt.Sprintf("%q: value must not contain ',', which delimits the label selector", key))
		}
		if reserved.Has(key) {
			problems = append(problems, fmt.Sprintf("%q: key is managed by Karpenter", key))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return serrors.Wrap(fmt.Errorf("labels failed validation requirements"), "labels", strings.Join(problems, ", "))
}

// LabelSelector renders labels into the "key=value,key=value" form the IaaS API's list endpoints
// accept. Keys are sorted so the request is stable and cacheable.
func LabelSelector(labels map[string]string) string {
	parts := make([]string, 0, len(labels))
	for _, key := range sortedKeys(labels) {
		parts = append(parts, key+"="+labels[key])
	}
	return strings.Join(parts, ",")
}

// LabelsToAPI converts a string map into the map[string]interface{} the generated SDK expects.
func LabelsToAPI(labels map[string]string) map[string]interface{} {
	if labels == nil {
		return nil
	}
	out := make(map[string]interface{}, len(labels))
	for k, v := range labels {
		out[k] = v
	}
	return out
}

// LabelsFromAPI converts the SDK's map[string]interface{} back into a string map, dropping any
// value that is not a string. The IaaS API only ever returns string label values, but the
// generated model types them as interface{}.
func LabelsFromAPI(labels map[string]interface{}) map[string]string {
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := lo.Keys(m)
	// Insertion sort keeps this dependency-free; label maps are tiny.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// PrettySlice truncates a slice after maxItems so that log lines stay readable.
func PrettySlice[T any](s []T, maxItems int) string {
	var sb strings.Builder
	for i, elem := range s {
		if i > maxItems-1 {
			fmt.Fprintf(&sb, " and %d other(s)", len(s)-i)
			break
		} else if i > 0 {
			fmt.Fprint(&sb, ", ")
		}
		fmt.Fprint(&sb, elem)
	}
	return sb.String()
}

// WithDefaultFloat64 returns the float64 value of the supplied environment variable or, if it is
// absent or unparseable, the supplied default.
func WithDefaultFloat64(key string, def float64) float64 {
	val, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	f, err := strconv.ParseFloat(val, 64)
	if err != nil {
		return def
	}
	return f
}
