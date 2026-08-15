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

package instance_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	gocache "github.com/patrickmn/go-cache"
	"github.com/samber/lo"
	"github.com/stackitcloud/stackit-sdk-go/core/oapierror"
	iaas "github.com/stackitcloud/stackit-sdk-go/services/iaas/v2api"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	"github.com/kubekanvas/karpenter-provider-stackit/pkg/apis/v1alpha1"
	stackitcache "github.com/kubekanvas/karpenter-provider-stackit/pkg/cache"
	"github.com/kubekanvas/karpenter-provider-stackit/pkg/providers/instance"
	"github.com/kubekanvas/karpenter-provider-stackit/pkg/utils"
)

// fakeAPI records what the provider asked STACKIT to do.
type fakeAPI struct {
	created      []iaas.CreateServerPayload
	deleted      []string
	listSelector string

	server    *iaas.Server
	getErr    error
	deleteErr error
	createErr error
}

func notFound() error {
	return &oapierror.GenericOpenAPIError{StatusCode: http.StatusNotFound}
}

func (f *fakeAPI) CreateServer(_ context.Context, payload *iaas.CreateServerPayload) (*iaas.Server, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.created = append(f.created, *payload)
	return &iaas.Server{
		Id:               lo.ToPtr("00fa1b2c-3d4e-5f60-7182-93a4b5c6d7e8"),
		Name:             payload.Name,
		MachineType:      payload.MachineType,
		AvailabilityZone: payload.AvailabilityZone,
		Status:           lo.ToPtr(instance.StatusCreating),
		CreatedAt:        lo.ToPtr(time.Now()),
		Labels:           payload.Labels,
	}, nil
}

func (f *fakeAPI) GetServer(_ context.Context, _ string) (*iaas.Server, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.server, nil
}

func (f *fakeAPI) DeleteServer(_ context.Context, serverID string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, serverID)
	return nil
}

func (f *fakeAPI) ListServers(_ context.Context, labelSelector string) ([]iaas.Server, error) {
	f.listSelector = labelSelector
	if f.server == nil {
		return nil, nil
	}
	return []iaas.Server{*f.server}, nil
}

func (f *fakeAPI) UpdateServerLabels(_ context.Context, _ string, _ map[string]string) error {
	return nil
}
func (f *fakeAPI) ListMachineTypes(_ context.Context) ([]iaas.MachineType, error) { return nil, nil }
func (f *fakeAPI) ListAvailabilityZones(_ context.Context) ([]string, error)      { return nil, nil }
func (f *fakeAPI) DeleteVolume(_ context.Context, _ string) error                 { return nil }

func newProvider(api *fakeAPI) *instance.DefaultProvider {
	return instance.NewDefaultProvider(
		"my-cluster",
		api,
		stackitcache.NewUnavailableOfferings(),
		gocache.New(time.Minute, time.Minute),
	)
}

func nodeClass() *v1alpha1.StackitNodeClass {
	return &v1alpha1.StackitNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: v1alpha1.StackitNodeClassSpec{
			BootVolume: v1alpha1.BootVolumeSpec{
				Size:   50,
				Source: v1alpha1.BootVolumeSourceSpec{ID: "11111111-2222-3333-4444-555555555555"},
			},
			Networking: v1alpha1.NetworkingSpec{
				NetworkID: lo.ToPtr("66666666-7777-8888-9999-aaaaaaaaaaaa"),
			},
		},
	}
}

func nodeClaim(name string) *karpv1.NodeClaim {
	return &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func instanceTypes() []*cloudprovider.InstanceType {
	return []*cloudprovider.InstanceType{{
		Name: "g1.2",
		Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, "g1.2"),
			scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "eu01-1"),
			scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
		),
		Offerings: cloudprovider.Offerings{{
			Requirements: scheduling.NewRequirements(
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, "eu01-1"),
				scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
			),
			Price:     0.05,
			Available: true,
		}},
		Capacity: corev1.ResourceList{},
		Overhead: &cloudprovider.InstanceTypeOverhead{},
	}}
}

// TestCreateSetsDeleteOnTermination is the single most expensive invariant to get wrong. STACKIT's
// DeleteServer explicitly does not delete volumes, and BootVolume.deleteOnTermination defaults to
// false server-side, so omitting it leaks a billable root disk for every node Karpenter ever
// terminates.
func TestCreateSetsDeleteOnTermination(t *testing.T) {
	t.Parallel()

	api := &fakeAPI{}
	p := newProvider(api)

	if _, err := p.Create(context.Background(), nodeClass(), nodeClaim("default-abc12"),
		map[string]string{v1alpha1.LabelKeyManagedBy: "my-cluster"}, instanceTypes()); err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if len(api.created) != 1 {
		t.Fatalf("got %d CreateServer calls, want 1", len(api.created))
	}

	bootVolume := api.created[0].BootVolume
	if bootVolume == nil {
		t.Fatal("CreateServer payload has no boot volume")
	}
	if bootVolume.DeleteOnTermination == nil {
		t.Fatal("deleteOnTermination was not set; the API defaults it to false and leaks the volume")
	}
	if !*bootVolume.DeleteOnTermination {
		t.Error("deleteOnTermination = false by default; every terminated node would leak its root disk")
	}
}

// TestCreateHonoursExplicitDeleteOnTermination confirms the default is a default, not a hardcode:
// an operator who deliberately wants to keep volumes can still say so.
func TestCreateHonoursExplicitDeleteOnTermination(t *testing.T) {
	t.Parallel()

	api := &fakeAPI{}
	p := newProvider(api)
	nc := nodeClass()
	nc.Spec.BootVolume.DeleteOnTermination = lo.ToPtr(false)

	if _, err := p.Create(context.Background(), nc, nodeClaim("default-abc12"), nil, instanceTypes()); err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if got := api.created[0].BootVolume.DeleteOnTermination; got == nil || *got {
		t.Errorf("deleteOnTermination = %v, want an explicit false", got)
	}
}

func TestCreateBuildsBootVolumeAndNetworking(t *testing.T) {
	t.Parallel()

	api := &fakeAPI{}
	p := newProvider(api)

	if _, err := p.Create(context.Background(), nodeClass(), nodeClaim("default-abc12"), nil, instanceTypes()); err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	payload := api.created[0]

	if payload.MachineType != "g1.2" {
		t.Errorf("machineType = %q, want g1.2", payload.MachineType)
	}
	if lo.FromPtr(payload.AvailabilityZone) != "eu01-1" {
		t.Errorf("availabilityZone = %q, want eu01-1", lo.FromPtr(payload.AvailabilityZone))
	}
	if got := lo.FromPtr(payload.BootVolume.Size); got != 50 {
		t.Errorf("boot volume size = %d, want 50", got)
	}
	if payload.BootVolume.Source == nil || payload.BootVolume.Source.Type != "image" {
		t.Errorf("boot volume source = %+v, want type image", payload.BootVolume.Source)
	}
	// STACKIT's v2 CreateServer requires exactly one of networkId or nicIds.
	if payload.Networking.CreateServerNetworking == nil {
		t.Fatal("networking one-of was not set to a network")
	}
	if payload.Networking.CreateServerNetworkingWithNics != nil {
		t.Error("both networking variants were set; the API rejects that")
	}
}

func TestCreateRejectsAmbiguousNetworking(t *testing.T) {
	t.Parallel()

	api := &fakeAPI{}
	p := newProvider(api)
	nc := nodeClass()
	nc.Spec.Networking.NICIDs = []string{"bbbbbbbb-cccc-dddd-eeee-ffffffffffff"}

	if _, err := p.Create(context.Background(), nc, nodeClaim("default-abc12"), nil, instanceTypes()); err == nil {
		t.Fatal("Create accepted both networkID and nicIDs")
	}
	if len(api.created) != 0 {
		t.Error("a server was created despite invalid networking")
	}
}

func TestCreateRejectsOverlongServerName(t *testing.T) {
	t.Parallel()

	api := &fakeAPI{}
	p := newProvider(api)
	nc := nodeClass()
	nc.Spec.ServerNamePrefix = lo.ToPtr("a-fairly-long-prefix-here")

	// A 25-char prefix plus a separator plus a 50-char NodeClaim name is past STACKIT's 63-char
	// limit, so the launch has to be refused rather than silently truncated into a name collision.
	longName := "default-" + strings.Repeat("x", 42)
	if _, err := p.Create(context.Background(), nc, nodeClaim(longName), nil, instanceTypes()); err == nil {
		t.Fatal("Create accepted a server name longer than STACKIT allows")
	}
	if len(api.created) != 0 {
		t.Error("a server was created despite an invalid name")
	}
}

func TestCreateAppliesServerNamePrefix(t *testing.T) {
	t.Parallel()

	api := &fakeAPI{}
	p := newProvider(api)
	nc := nodeClass()
	nc.Spec.ServerNamePrefix = lo.ToPtr("worker")

	if _, err := p.Create(context.Background(), nc, nodeClaim("default-abc12"), nil, instanceTypes()); err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if got := api.created[0].Name; got != "worker-default-abc12" {
		t.Errorf("server name = %q, want worker-default-abc12", got)
	}
}

// TestDeleteFollowsKarpenterRetryContract pins the multi-step teardown. Karpenter retries Delete
// until it returns NodeClaimNotFoundError, so an in-progress deletion must report an error rather
// than nil — returning nil would tell Karpenter the node was gone while the server still ran.
func TestDeleteFollowsKarpenterRetryContract(t *testing.T) {
	t.Parallel()

	const id = "00fa1b2c-3d4e-5f60-7182-93a4b5c6d7e8"

	t.Run("first call issues the delete and reports progress", func(t *testing.T) {
		t.Parallel()
		api := &fakeAPI{server: &iaas.Server{
			Id:     lo.ToPtr(id),
			Status: lo.ToPtr(instance.StatusActive),
		}}
		p := newProvider(api)

		err := p.Delete(context.Background(), id)
		if err == nil {
			t.Fatal("Delete returned nil while the server still exists; Karpenter would stop retrying")
		}
		if cloudprovider.IsNodeClaimNotFoundError(err) {
			t.Error("Delete reported NodeClaimNotFound on the call that issued the deletion")
		}
		if len(api.deleted) != 1 || api.deleted[0] != id {
			t.Errorf("DeleteServer calls = %v, want exactly [%s]", api.deleted, id)
		}
	})

	t.Run("while deleting it does not re-issue the delete", func(t *testing.T) {
		t.Parallel()
		api := &fakeAPI{server: &iaas.Server{
			Id:     lo.ToPtr(id),
			Status: lo.ToPtr(instance.StatusDeleting),
		}}
		p := newProvider(api)

		err := p.Delete(context.Background(), id)
		if err == nil {
			t.Fatal("Delete returned nil for a server still in DELETING")
		}
		if len(api.deleted) != 0 {
			t.Errorf("DeleteServer was called again for a server already deleting: %v", api.deleted)
		}
	})

	t.Run("once gone it reports NodeClaimNotFound", func(t *testing.T) {
		t.Parallel()
		api := &fakeAPI{getErr: notFound()}
		p := newProvider(api)

		err := p.Delete(context.Background(), id)
		if !cloudprovider.IsNodeClaimNotFoundError(err) {
			t.Fatalf("Delete returned %v, want a NodeClaimNotFoundError so Karpenter stops retrying", err)
		}
	})

	t.Run("a delete that 404s reports NodeClaimNotFound", func(t *testing.T) {
		t.Parallel()
		api := &fakeAPI{
			server:    &iaas.Server{Id: lo.ToPtr(id), Status: lo.ToPtr(instance.StatusActive)},
			deleteErr: notFound(),
		}
		p := newProvider(api)

		err := p.Delete(context.Background(), id)
		if !cloudprovider.IsNodeClaimNotFoundError(err) {
			t.Fatalf("Delete returned %v, want a NodeClaimNotFoundError", err)
		}
	})

	t.Run("a conflict is retried rather than reported as gone", func(t *testing.T) {
		t.Parallel()
		api := &fakeAPI{
			server:    &iaas.Server{Id: lo.ToPtr(id), Status: lo.ToPtr(instance.StatusActive)},
			deleteErr: &oapierror.GenericOpenAPIError{StatusCode: http.StatusConflict},
		}
		p := newProvider(api)

		err := p.Delete(context.Background(), id)
		if err == nil || cloudprovider.IsNodeClaimNotFoundError(err) {
			t.Fatalf("Delete returned %v for a 409, want a retryable error", err)
		}
	})
}

// TestListFiltersServerSideByCluster confirms the managed-by filter reaches the API. Without it,
// garbage collection would consider every server in the STACKIT project — including other
// clusters' nodes — an orphan.
func TestListFiltersServerSideByCluster(t *testing.T) {
	t.Parallel()

	api := &fakeAPI{server: &iaas.Server{
		Id:               lo.ToPtr("00fa1b2c-3d4e-5f60-7182-93a4b5c6d7e8"),
		Status:           lo.ToPtr(instance.StatusActive),
		CreatedAt:        lo.ToPtr(time.Now()),
		AvailabilityZone: lo.ToPtr("eu01-1"),
		Labels:           utils.LabelsToAPI(map[string]string{v1alpha1.LabelKeyManagedBy: "my-cluster"}),
	}}
	p := newProvider(api)

	instances, err := p.List(context.Background())
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	want := v1alpha1.LabelKeyManagedBy + "=my-cluster"
	if api.listSelector != want {
		t.Errorf("label selector = %q, want %q", api.listSelector, want)
	}
	if len(instances) != 1 {
		t.Fatalf("got %d instances, want 1", len(instances))
	}
	// The list response carries labels, which is what makes a per-server detail call unnecessary.
	if instances[0].Labels[v1alpha1.LabelKeyManagedBy] != "my-cluster" {
		t.Error("labels were not recovered from the list response")
	}
}

// TestInstanceCreatedAtComesFromAPI guards the garbage-collection grace period: a zero creation
// time would make a freshly launched server look infinitely old.
func TestInstanceCreatedAtComesFromAPI(t *testing.T) {
	t.Parallel()

	created := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	inst := instance.NewInstance(&iaas.Server{
		Id:        lo.ToPtr("00fa1b2c-3d4e-5f60-7182-93a4b5c6d7e8"),
		CreatedAt: lo.ToPtr(created),
		Status:    lo.ToPtr(instance.StatusActive),
	})
	if !inst.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v, want %v", inst.CreatedAt, created)
	}

	// A server with no timestamp must surface as the zero time so the caller can refuse to reap it,
	// rather than being silently backfilled with something plausible.
	inst = instance.NewInstance(&iaas.Server{
		Id:     lo.ToPtr("00fa1b2c-3d4e-5f60-7182-93a4b5c6d7e8"),
		Status: lo.ToPtr(instance.StatusActive),
	})
	if !inst.CreatedAt.IsZero() {
		t.Errorf("CreatedAt = %v for a server with no createdAt, want the zero time", inst.CreatedAt)
	}
}

func TestInstanceZoneIsSanitized(t *testing.T) {
	t.Parallel()

	inst := instance.NewInstance(&iaas.Server{
		Id:               lo.ToPtr("00fa1b2c-3d4e-5f60-7182-93a4b5c6d7e8"),
		AvailabilityZone: lo.ToPtr("eu01 1"),
		Status:           lo.ToPtr(instance.StatusActive),
	})
	if got := inst.Zone(); got != "eu01-1" {
		t.Errorf("Zone() = %q, want the CCM-sanitized eu01-1", got)
	}
}

func TestInstanceProviderIDMatchesCCM(t *testing.T) {
	t.Parallel()

	const id = "00fa1b2c-3d4e-5f60-7182-93a4b5c6d7e8"
	inst := instance.NewInstance(&iaas.Server{Id: lo.ToPtr(id), Status: lo.ToPtr(instance.StatusActive)})
	if got, want := inst.ProviderID(), "stackit://"+id; got != want {
		t.Errorf("ProviderID() = %q, want %q", got, want)
	}
}

func TestInstanceTerminating(t *testing.T) {
	t.Parallel()

	for status, want := range map[string]bool{
		instance.StatusActive:   false,
		instance.StatusCreating: false,
		instance.StatusError:    false,
		instance.StatusDeleting: true,
		instance.StatusDeleted:  true,
	} {
		inst := instance.NewInstance(&iaas.Server{
			Id:     lo.ToPtr("00fa1b2c-3d4e-5f60-7182-93a4b5c6d7e8"),
			Status: lo.ToPtr(status),
		})
		if got := inst.Terminating(); got != want {
			t.Errorf("Terminating() for status %q = %v, want %v", status, got, want)
		}
	}
}

func TestCreateWithNoAvailableOfferingIsInsufficientCapacity(t *testing.T) {
	t.Parallel()

	api := &fakeAPI{}
	p := newProvider(api)

	its := instanceTypes()
	its[0].Offerings[0].Available = false

	_, err := p.Create(context.Background(), nodeClass(), nodeClaim("default-abc12"), nil, its)
	if err == nil {
		t.Fatal("Create succeeded with no available offering")
	}
	if !cloudprovider.IsInsufficientCapacityError(err) {
		t.Errorf("Create returned %v, want an InsufficientCapacityError so the scheduler backs off", err)
	}
	if len(api.created) != 0 {
		t.Error("a server was created despite no available offering")
	}
}

func TestCreateSurfacesAPIErrors(t *testing.T) {
	t.Parallel()

	api := &fakeAPI{createErr: errors.New("stackit said no")}
	p := newProvider(api)

	if _, err := p.Create(context.Background(), nodeClass(), nodeClaim("default-abc12"), nil, instanceTypes()); err == nil {
		t.Fatal("Create swallowed an API error")
	}
}
