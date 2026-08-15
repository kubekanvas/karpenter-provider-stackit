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

// Package nodeclass reconciles StackitNodeClasses against the STACKIT API, resolving the
// availability zones a NodeClass names and reporting readiness.
package nodeclass

import (
	"context"
	"fmt"
	"time"

	"github.com/awslabs/operatorpkg/reasonable"
	"github.com/patrickmn/go-cache"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/util/sets"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/karpenter/pkg/operator/injection"

	"github.com/kubekanvas/karpenter-provider-stackit/pkg/apis/v1alpha1"
	"github.com/kubekanvas/karpenter-provider-stackit/pkg/stackit"
	"github.com/kubekanvas/karpenter-provider-stackit/pkg/utils"
)

// resyncPeriod bounds how stale a NodeClass's resolved availability zones can be. STACKIT adds
// zones rarely, so this only needs to be faster than a human noticing.
const resyncPeriod = time.Minute * 5

// availabilityZonesCacheKey caches the region's zone list, which is identical for every NodeClass
// and changes on the order of years.
const availabilityZonesCacheKey = "availability-zones"

type Controller struct {
	kubeClient      client.Client
	client          stackit.API
	region          string
	validationCache *cache.Cache
	disableDryRun   bool
}

func NewController(
	kubeClient client.Client,
	stackitClient stackit.API,
	region string,
	validationCache *cache.Cache,
	disableDryRun bool,
) *Controller {
	return &Controller{
		kubeClient:      kubeClient,
		client:          stackitClient,
		region:          region,
		validationCache: validationCache,
		disableDryRun:   disableDryRun,
	}
}

func (c *Controller) Reconcile(ctx context.Context, nodeClass *v1alpha1.StackitNodeClass) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, "nodeclass.status")

	if !nodeClass.DeletionTimestamp.IsZero() {
		return reconcile.Result{}, nil
	}
	stored := nodeClass.DeepCopy()

	// Each step records its own condition and does not short-circuit the others, so a NodeClass
	// with both a bad zone and a bad label reports both problems at once instead of one per
	// reconcile.
	zoneErr := c.resolveAvailabilityZones(ctx, nodeClass)
	validationErr := c.validate(nodeClass)

	if !equality.Semantic.DeepEqual(stored, nodeClass) {
		if err := c.kubeClient.Status().Patch(ctx, nodeClass, client.MergeFrom(stored)); err != nil {
			return reconcile.Result{}, client.IgnoreNotFound(err)
		}
	}
	// Transient STACKIT failures are worth retrying with backoff; a NodeClass that simply names a
	// zone that does not exist is not, and is left to be corrected by the user.
	for _, err := range []error{zoneErr, validationErr} {
		if err != nil && stackit.IsRetryable(err) {
			return reconcile.Result{}, err
		}
	}
	return reconcile.Result{RequeueAfter: resyncPeriod}, nil
}

// resolveAvailabilityZones intersects the zones the NodeClass asks for with the zones STACKIT
// reports for the configured region, so that a typo surfaces as a NotReady NodeClass rather than as
// failing launches.
//
// An empty spec.availabilityZones means "every zone in the region", unlike the UpCloud provider's
// single default zone: STACKIT's zones are within one region and are interchangeable for
// scheduling, so spreading across all of them is the useful default.
func (c *Controller) resolveAvailabilityZones(ctx context.Context, nodeClass *v1alpha1.StackitNodeClass) error {
	available, err := c.listAvailabilityZones(ctx)
	if err != nil {
		nodeClass.StatusConditions().SetUnknownWithReason(v1alpha1.ConditionTypeAvailabilityZonesReady,
			"AvailabilityZoneResolutionFailed", fmt.Sprintf("Failed to list STACKIT availability zones, %s", err))
		return fmt.Errorf("listing availability zones, %w", err)
	}
	if len(available) == 0 {
		nodeClass.Status.AvailabilityZones = nil
		nodeClass.StatusConditions().SetFalse(v1alpha1.ConditionTypeAvailabilityZonesReady,
			"AvailabilityZonesNotFound", fmt.Sprintf("STACKIT reported no availability zones in region %q", c.region))
		return fmt.Errorf("no availability zones in region %q", c.region)
	}

	requested := nodeClass.Spec.AvailabilityZones
	if len(requested) == 0 {
		requested = available
	}

	known := sets.New(available...)
	resolved := make([]v1alpha1.AvailabilityZone, 0, len(requested))
	var unknown []string
	for _, id := range requested {
		if !known.Has(id) {
			unknown = append(unknown, id)
			continue
		}
		resolved = append(resolved, v1alpha1.AvailabilityZone{ID: id})
	}
	if len(unknown) > 0 {
		nodeClass.Status.AvailabilityZones = nil
		nodeClass.StatusConditions().SetFalse(v1alpha1.ConditionTypeAvailabilityZonesReady,
			"AvailabilityZonesNotFound", fmt.Sprintf("Availability zones not present in region %q: %s",
				c.region, utils.PrettySlice(unknown, 5)))
		return fmt.Errorf("unknown availability zones %v in region %q", unknown, c.region)
	}

	nodeClass.Status.AvailabilityZones = resolved
	nodeClass.StatusConditions().SetTrue(v1alpha1.ConditionTypeAvailabilityZonesReady)
	return nil
}

func (c *Controller) listAvailabilityZones(ctx context.Context) ([]string, error) {
	if cached, ok := c.validationCache.Get(availabilityZonesCacheKey); ok {
		return cached.([]string), nil
	}
	zones, err := c.client.ListAvailabilityZones(ctx)
	if err != nil {
		return nil, err
	}
	c.validationCache.SetDefault(availabilityZonesCacheKey, zones)
	return zones, nil
}

// validate checks the parts of the NodeClass that CRD validation cannot express on its own, either
// because they depend on other fields or because they depend on STACKIT's own constraints.
func (c *Controller) validate(nodeClass *v1alpha1.StackitNodeClass) error {
	if err := utils.ValidateLabels(nodeClass.Spec.Labels); err != nil {
		nodeClass.StatusConditions().SetFalse(v1alpha1.ConditionTypeValidationSucceeded,
			"LabelValidationFailed", err.Error())
		return err
	}
	if err := validateNetworking(nodeClass); err != nil {
		nodeClass.StatusConditions().SetFalse(v1alpha1.ConditionTypeValidationSucceeded,
			"NetworkValidationFailed", err.Error())
		return err
	}
	nodeClass.StatusConditions().SetTrue(v1alpha1.ConditionTypeValidationSucceeded)
	return nil
}

// validateNetworking enforces the one-of that STACKIT's CreateServer requires but that the CRD
// schema cannot express across two sibling fields.
func validateNetworking(nodeClass *v1alpha1.StackitNodeClass) error {
	spec := nodeClass.Spec.Networking
	hasNetwork := spec.NetworkID != nil && *spec.NetworkID != ""
	hasNICs := len(spec.NICIDs) > 0

	switch {
	case hasNetwork && hasNICs:
		return fmt.Errorf("networking: set exactly one of networkID or nicIDs, not both")
	case !hasNetwork && !hasNICs:
		return fmt.Errorf("networking: one of networkID or nicIDs is required")
	}
	if hasNICs && len(spec.NICIDs) > 1 {
		// A NIC belongs to at most one server, so a NodePool that could scale past one node would
		// fail every launch after the first.
		return fmt.Errorf("networking.nicIDs: only one NIC may be given, since a NIC cannot be shared between servers")
	}
	return nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("nodeclass.status").
		For(&v1alpha1.StackitNodeClass{}).
		WithOptions(controller.Options{
			RateLimiter:             reasonable.RateLimiter(),
			MaxConcurrentReconciles: 10,
		}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}
