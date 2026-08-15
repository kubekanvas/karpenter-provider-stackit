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

// Package labeling reconciles the STACKIT labels on running servers with the labels their NodeClass
// currently asks for, so that label-only changes do not require replacing nodes.
package labeling

import (
	"context"
	"fmt"
	"maps"

	"github.com/awslabs/operatorpkg/reasonable"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/operator/injection"

	"github.com/kubekanvas/karpenter-provider-stackit/pkg/apis/v1alpha1"
	"github.com/kubekanvas/karpenter-provider-stackit/pkg/operator/options"
	"github.com/kubekanvas/karpenter-provider-stackit/pkg/providers/instance"
	"github.com/kubekanvas/karpenter-provider-stackit/pkg/utils"
)

type Controller struct {
	kubeClient       client.Client
	instanceProvider instance.Provider
}

func NewController(kubeClient client.Client, instanceProvider instance.Provider) *Controller {
	return &Controller{
		kubeClient:       kubeClient,
		instanceProvider: instanceProvider,
	}
}

func (c *Controller) Reconcile(ctx context.Context, nodeClaim *karpv1.NodeClaim) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, "nodeclaim.labeling")

	if !nodeClaim.DeletionTimestamp.IsZero() || nodeClaim.Status.ProviderID == "" {
		return reconcile.Result{}, nil
	}
	id, err := utils.ParseInstanceID(nodeClaim.Status.ProviderID)
	if err != nil {
		// A NodeClaim whose providerID is not in STACKIT's format belongs to another cloud provider.
		// Requeueing would retry forever against a NodeClaim that will never be ours, so drop it.
		//nolint:nilerr // deliberately dropped, not swallowed: see above.
		return reconcile.Result{}, nil
	}
	if nodeClaim.Spec.NodeClassRef == nil {
		return reconcile.Result{}, nil
	}
	nodeClass := &v1alpha1.StackitNodeClass{}
	if err := c.kubeClient.Get(ctx, client.ObjectKey{Name: nodeClaim.Spec.NodeClassRef.Name}, nodeClass); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	inst, err := c.instanceProvider.Get(ctx, id)
	if err != nil {
		return reconcile.Result{}, cloudprovider.IgnoreNodeClaimNotFoundError(err)
	}

	desired := utils.ManagedLabels(nodeClass, nodeClaim, options.FromContext(ctx).ClusterName)
	// STACKIT has no per-label API: the server PATCH replaces the whole label set, and the whole set
	// is exactly what this controller owns. Comparing the maps in full avoids rewriting identical
	// labels on every NodeClaim event, which would otherwise be one API call per event per node.
	if maps.Equal(desired, inst.Labels) {
		return reconcile.Result{}, nil
	}
	if err := c.instanceProvider.UpdateLabels(ctx, id, desired); err != nil {
		return reconcile.Result{}, fmt.Errorf("updating labels on %q, %w", id, cloudprovider.IgnoreNodeClaimNotFoundError(err))
	}
	return reconcile.Result{}, nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("nodeclaim.labeling").
		For(&karpv1.NodeClaim{}, builder.WithPredicates(
			// Only NodeClaims that have actually been launched carry a STACKIT server to label.
			predicate.NewPredicateFuncs(func(o client.Object) bool {
				return o.(*karpv1.NodeClaim).Status.ProviderID != ""
			}),
		)).
		WithOptions(controller.Options{
			RateLimiter:             reasonable.RateLimiter(),
			MaxConcurrentReconciles: 10,
		}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}
