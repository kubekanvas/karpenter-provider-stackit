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

package utils_test

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	"github.com/kubekanvas/karpenter-provider-stackit/pkg/apis/v1alpha1"
	"github.com/kubekanvas/karpenter-provider-stackit/pkg/utils"
)

// ccmProviderIDRegexp is the exact expression cloud-provider-stackit uses to parse a providerID
// (pkg/ccm/instances.go: providerIDRegexp). Every providerID this controller emits has to satisfy
// it, or the CCM will reject the node it is attached to.
var ccmProviderIDRegexp = regexp.MustCompile(`^stackit://([^/]+)$`)

const (
	testZone = "eu01-1"
	testTeam = "platform"
	testKey  = "team"
)

func TestParseInstanceID(t *testing.T) {
	t.Parallel()

	// "stackit://<id>" is what cloud-provider-stackit's makeInstanceID writes. A NodeClaim whose
	// providerID uses any other shape would silently fail to match its Node.
	for _, tc := range []struct {
		name       string
		providerID string
		want       string
		wantErr    bool
	}{
		{name: "ccm format", providerID: "stackit://00fa1b2c-3d4e-5f60-7182-93a4b5c6d7e8", want: "00fa1b2c-3d4e-5f60-7182-93a4b5c6d7e8"},
		{name: "three slashes is not what the ccm writes", providerID: "stackit:///00fa1b2c", wantErr: true},
		{name: "legacy openstack regional form", providerID: "openstack://eu01/00fa1b2c", wantErr: true},
		{name: "other provider", providerID: "aws:///us-east-1a/i-0123", wantErr: true},
		{name: "prefix with no id", providerID: "stackit://", wantErr: true},
		{name: "bare id", providerID: "00fa1b2c", wantErr: true},
		{name: "empty", providerID: "", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := utils.ParseInstanceID(tc.providerID)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseInstanceID(%q) = %q, want error", tc.providerID, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseInstanceID(%q) returned unexpected error: %v", tc.providerID, err)
			}
			if got != tc.want {
				t.Errorf("ParseInstanceID(%q) = %q, want %q", tc.providerID, got, tc.want)
			}
		})
	}
}

func TestProviderIDRoundTrips(t *testing.T) {
	t.Parallel()

	for _, id := range []string{
		"00fa1b2c-3d4e-5f60-7182-93a4b5c6d7e8",
		"ffffffff-ffff-ffff-ffff-ffffffffffff",
	} {
		providerID := utils.ProviderID(id)
		got, err := utils.ParseInstanceID(providerID)
		if err != nil {
			t.Fatalf("ParseInstanceID(ProviderID(%q)) returned error: %v", id, err)
		}
		if got != id {
			t.Errorf("ParseInstanceID(ProviderID(%q)) = %q, want %q", id, got, id)
		}
	}
}

// TestProviderIDMatchesCCMFormat is the check that actually protects NodeClaim-to-Node binding: the
// string this controller writes must be byte-identical to what the CCM produces, not merely
// parseable by it.
func TestProviderIDMatchesCCMFormat(t *testing.T) {
	t.Parallel()

	const id = "00fa1b2c-3d4e-5f60-7182-93a4b5c6d7e8"
	got := utils.ProviderID(id)

	// This is literally fmt.Sprintf("%s://%s", ProviderName, server.GetId()) from
	// cloud-provider-stackit's Instances.makeInstanceID.
	want := fmt.Sprintf("%s://%s", "stackit", id)
	if got != want {
		t.Fatalf("ProviderID(%q) = %q, want %q (cloud-provider-stackit makeInstanceID)", id, got, want)
	}
	matches := ccmProviderIDRegexp.FindStringSubmatch(got)
	if len(matches) != 2 {
		t.Fatalf("ProviderID(%q) = %q does not match the CCM's providerIDRegexp", id, got)
	}
	if matches[1] != id {
		t.Errorf("CCM regexp extracted %q from %q, want %q", matches[1], got, id)
	}
}

// TestSanitizeZone pins this provider's sanitizer to labels.Sanitize in cloud-provider-stackit
// (pkg/labels/labels.go). If the two diverge, an offering's topology.kubernetes.io/zone requirement
// stops matching the label the CCM writes and no node ever registers.
func TestSanitizeZone(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{name: "real stackit zone is unchanged", in: testZone, want: testZone},
		{name: "underscores and dots survive", in: "eu01_1.a", want: "eu01_1.a"},
		{name: "spaces collapse to a dash", in: "eu01 1", want: testZone},
		{name: "runs of invalid characters collapse to one dash", in: "eu01///1", want: testZone},
		{name: "leading and trailing separators are trimmed", in: "-_.eu01-1._-", want: testZone},
		{name: "empty stays empty", in: "", want: ""},
		{name: "only invalid characters", in: "///", want: ""},
		{name: "over 63 characters is truncated", in: strings.Repeat("a", 70), want: strings.Repeat("a", 63)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := utils.SanitizeZone(tc.in); got != tc.want {
				t.Errorf("SanitizeZone(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestValidateLabels(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		labels  map[string]string
		wantErr bool
	}{
		{name: "nil is fine", labels: nil},
		{name: "ordinary labels", labels: map[string]string{testKey: testTeam, "env": "prod"}},
		{name: "slashes are allowed", labels: map[string]string{"example.com/team": testTeam}},
		{name: "empty value is allowed", labels: map[string]string{testKey: ""}},
		{name: "empty key", labels: map[string]string{"": "x"}, wantErr: true},
		{name: "key with a space", labels: map[string]string{"my team": "x"}, wantErr: true},
		{name: "key with a comma breaks the label selector", labels: map[string]string{"a,b": "x"}, wantErr: true},
		{name: "key with an equals breaks the label selector", labels: map[string]string{"a=b": "x"}, wantErr: true},
		{name: "value with a comma breaks the label selector", labels: map[string]string{testKey: "a,b"}, wantErr: true},
		{name: "non-ascii key", labels: map[string]string{"tëam": "x"}, wantErr: true},
		{name: "over-long key", labels: map[string]string{strings.Repeat("k", 256): "x"}, wantErr: true},
		{name: "over-long value", labels: map[string]string{testKey: strings.Repeat("v", 256)}, wantErr: true},
		{name: "reserved managed-by", labels: map[string]string{v1alpha1.LabelKeyManagedBy: "other"}, wantErr: true},
		{name: "reserved nodepool", labels: map[string]string{v1alpha1.LabelKeyNodePool: "other"}, wantErr: true},
		{name: "reserved nodeclaim", labels: map[string]string{v1alpha1.LabelKeyNodeClaim: "other"}, wantErr: true},
		{name: "reserved nodeclass", labels: map[string]string{v1alpha1.LabelKeyNodeClass: "other"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := utils.ValidateLabels(tc.labels)
			if tc.wantErr && err == nil {
				t.Errorf("ValidateLabels(%v) = nil, want error", tc.labels)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("ValidateLabels(%v) returned unexpected error: %v", tc.labels, err)
			}
		})
	}
}

// TestManagedLabelsPassOwnValidation is the check the brief calls for explicitly: the keys the
// controller writes at launch must themselves satisfy the constraints it enforces on users. A
// managed key that failed validation would mean every CreateServer call is rejected.
func TestManagedLabelsPassOwnValidation(t *testing.T) {
	t.Parallel()

	nodeClass := &v1alpha1.StackitNodeClass{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	nodeClaim := &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "default-abc12",
			Labels: map[string]string{karpv1.NodePoolLabelKey: "default"},
		},
	}
	labels := utils.ManagedLabels(nodeClass, nodeClaim, "my-cluster")

	for key, value := range labels {
		// ValidateLabels rejects reserved keys by design, so each managed key is checked against
		// the format rules alone by validating it under a non-reserved name.
		if err := utils.ValidateLabels(map[string]string{"probe/" + strings.TrimPrefix(key, "karpenter.sh/"): value}); err != nil {
			t.Errorf("managed label %q=%q fails this provider's own value rules: %v", key, value, err)
		}
		if strings.ContainsAny(key, ", =") {
			t.Errorf("managed label key %q contains a label-selector delimiter", key)
		}
		if strings.Contains(value, ",") {
			t.Errorf("managed label value %q for key %q contains a label-selector delimiter", value, key)
		}
		if key == "" || len(key) > 255 {
			t.Errorf("managed label key %q has an unusable length %d", key, len(key))
		}
	}

	for _, want := range []string{
		v1alpha1.LabelKeyManagedBy,
		v1alpha1.LabelKeyNodeClass,
		v1alpha1.LabelKeyNodeClaim,
		v1alpha1.LabelKeyNodePool,
	} {
		if _, ok := labels[want]; !ok {
			t.Errorf("ManagedLabels is missing %q; servers without it are invisible to List and leak", want)
		}
	}
}

func TestManagedLabelsDoNotLetUserLabelsWin(t *testing.T) {
	t.Parallel()

	nodeClass := &v1alpha1.StackitNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: v1alpha1.StackitNodeClassSpec{
			Labels: map[string]string{
				v1alpha1.LabelKeyManagedBy: "someone-elses-cluster",
				testKey:                    testTeam,
			},
		},
	}
	nodeClaim := &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "default-abc12"}}

	labels := utils.ManagedLabels(nodeClass, nodeClaim, "my-cluster")
	if labels[v1alpha1.LabelKeyManagedBy] != "my-cluster" {
		t.Errorf("managed-by = %q, want %q; a user label must never be able to hide a server from List",
			labels[v1alpha1.LabelKeyManagedBy], "my-cluster")
	}
	if labels[testKey] != testTeam {
		t.Errorf("user label team = %q, want %q", labels[testKey], testTeam)
	}
}

func TestManagedLabelsOmitsNodePoolWhenAbsent(t *testing.T) {
	t.Parallel()

	nodeClass := &v1alpha1.StackitNodeClass{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	nodeClaim := &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "standalone"}}

	labels := utils.ManagedLabels(nodeClass, nodeClaim, "my-cluster")
	if _, ok := labels[v1alpha1.LabelKeyNodePool]; ok {
		t.Error("nodepool label was set for a NodeClaim with no nodepool label")
	}
}

// TestLabelSelector pins the "k=v,k=v" form ListServers filters on, and its ordering: an unstable
// selector would defeat any caching in front of the list call.
func TestLabelSelector(t *testing.T) {
	t.Parallel()

	got := utils.LabelSelector(map[string]string{
		"zeta":                     "last",
		v1alpha1.LabelKeyManagedBy: "my-cluster",
		"alpha":                    "first",
	})
	want := "alpha=first,karpenter.sh/managed-by=my-cluster,zeta=last"
	if got != want {
		t.Errorf("LabelSelector = %q, want %q", got, want)
	}
	if utils.LabelSelector(nil) != "" {
		t.Errorf("LabelSelector(nil) = %q, want empty", utils.LabelSelector(nil))
	}
}

func TestLabelsAPIRoundTrip(t *testing.T) {
	t.Parallel()

	in := map[string]string{"team": "platform", v1alpha1.LabelKeyManagedBy: "my-cluster"}
	out := utils.LabelsFromAPI(utils.LabelsToAPI(in))
	if len(out) != len(in) {
		t.Fatalf("round trip produced %d labels, want %d", len(out), len(in))
	}
	for k, v := range in {
		if out[k] != v {
			t.Errorf("round trip lost %q: got %q, want %q", k, out[k], v)
		}
	}

	if utils.LabelsToAPI(nil) != nil {
		t.Error("LabelsToAPI(nil) should stay nil so the API omits the field entirely")
	}
	if got := utils.LabelsFromAPI(nil); len(got) != 0 {
		t.Errorf("LabelsFromAPI(nil) = %v, want empty", got)
	}
	// The generated model types label values as interface{}; anything non-string is not a label
	// this provider wrote and must not be surfaced as one.
	if got := utils.LabelsFromAPI(map[string]interface{}{"n": 1, "s": "ok"}); len(got) != 1 || got["s"] != "ok" {
		t.Errorf("LabelsFromAPI dropped the wrong entries: %v", got)
	}
}
