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

// Package hash keeps the StackitNodeClass hash annotation current, which is what drift detection
// compares NodeClaims against.
package hash

import (
	"context"

	"github.com/awslabs/operatorpkg/reasonable"
	"github.com/samber/lo"
	"go.uber.org/multierr"
	"k8s.io/apimachinery/pkg/api/equality"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	nodeclaimutils "sigs.k8s.io/karpenter/pkg/utils/nodeclaim"

	"github.com/kubekanvas/karpenter-provider-stackit/pkg/apis/v1alpha1"
)

type Controller struct {
	kubeClient client.Client
}

func NewController(kubeClient client.Client) *Controller {
	return &Controller{kubeClient: kubeClient}
}

func (c *Controller) Reconcile(ctx context.Context, nodeClass *v1alpha1.StackitNodeClass) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, "nodeclass.hash")

	stored := nodeClass.DeepCopy()
	if nodeClass.Annotations[v1alpha1.AnnotationStackitNodeClassHashVersion] != v1alpha1.StackitNodeClassHashVersion {
		if err := c.updateNodeClaimHash(ctx, nodeClass); err != nil {
			return reconcile.Result{}, err
		}
	}
	nodeClass.Annotations = lo.Assign(nodeClass.Annotations, map[string]string{
		v1alpha1.AnnotationStackitNodeClassHash:        nodeClass.Hash(),
		v1alpha1.AnnotationStackitNodeClassHashVersion: v1alpha1.StackitNodeClassHashVersion,
	})

	if !equality.Semantic.DeepEqual(stored, nodeClass) {
		if err := c.kubeClient.Patch(ctx, nodeClass, client.MergeFrom(stored)); err != nil {
			return reconcile.Result{}, client.IgnoreNotFound(err)
		}
	}
	return reconcile.Result{}, nil
}

// updateNodeClaimHash re-stamps existing NodeClaims when the hashing algorithm changes version.
// Without this, every node in the cluster would look drifted immediately after a controller upgrade
// and be replaced for no reason.
func (c *Controller) updateNodeClaimHash(ctx context.Context, nodeClass *v1alpha1.StackitNodeClass) error {
	nodeClaims := &karpv1.NodeClaimList{}
	if err := c.kubeClient.List(ctx, nodeClaims, nodeclaimutils.ForNodeClass(nodeClass)); err != nil {
		return err
	}

	errs := make([]error, len(nodeClaims.Items))
	for i := range nodeClaims.Items {
		nc := &nodeClaims.Items[i]
		if nc.Annotations[v1alpha1.AnnotationStackitNodeClassHashVersion] == v1alpha1.StackitNodeClassHashVersion {
			continue
		}
		stored := nc.DeepCopy()
		nc.Annotations = lo.Assign(nc.Annotations, map[string]string{
			v1alpha1.AnnotationStackitNodeClassHashVersion: v1alpha1.StackitNodeClassHashVersion,
		})
		// A NodeClaim that is already known to be drifted keeps its old hash, so that the drift it
		// was flagged for is not silently forgiven by the version bump.
		if nc.StatusConditions().Get(karpv1.ConditionTypeDrifted) == nil {
			nc.Annotations = lo.Assign(nc.Annotations, map[string]string{
				v1alpha1.AnnotationStackitNodeClassHash: nodeClass.Hash(),
			})
		}
		if !equality.Semantic.DeepEqual(stored, nc) {
			if err := c.kubeClient.Patch(ctx, nc, client.MergeFrom(stored)); err != nil {
				errs[i] = client.IgnoreNotFound(err)
			}
		}
	}
	return multierr.Combine(errs...)
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("nodeclass.hash").
		For(&v1alpha1.StackitNodeClass{}).
		WithOptions(controller.Options{
			RateLimiter:             reasonable.RateLimiter(),
			MaxConcurrentReconciles: 10,
		}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}
