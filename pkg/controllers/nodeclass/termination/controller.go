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

// Package termination holds a StackitNodeClass open until the NodeClaims launched from it are gone.
package termination

import (
	"context"
	"fmt"
	"time"

	"github.com/awslabs/operatorpkg/reasonable"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	nodeclaimutils "sigs.k8s.io/karpenter/pkg/utils/nodeclaim"

	"github.com/kubekanvas/karpenter-provider-stackit/pkg/apis/v1alpha1"
)

// requeueWhileInUse is how often a NodeClass with live NodeClaims is re-checked while it waits to
// be deleted.
const requeueWhileInUse = 10 * time.Second

type Controller struct {
	kubeClient client.Client
	recorder   record.EventRecorder
}

func NewController(kubeClient client.Client, recorder record.EventRecorder) *Controller {
	return &Controller{kubeClient: kubeClient, recorder: recorder}
}

func (c *Controller) Reconcile(ctx context.Context, nodeClass *v1alpha1.StackitNodeClass) (reconcile.Result, error) {
	ctx = injection.WithControllerName(ctx, "nodeclass.termination")

	if nodeClass.DeletionTimestamp.IsZero() {
		// Take ownership before any server can be launched from this NodeClass, so that there is no
		// window in which a NodeClass can be deleted out from under running nodes.
		if !controllerutil.ContainsFinalizer(nodeClass, v1alpha1.TerminationFinalizer) {
			stored := nodeClass.DeepCopy()
			controllerutil.AddFinalizer(nodeClass, v1alpha1.TerminationFinalizer)
			if err := c.kubeClient.Patch(ctx, nodeClass, client.MergeFrom(stored)); err != nil {
				return reconcile.Result{}, client.IgnoreNotFound(err)
			}
		}
		return reconcile.Result{}, nil
	}

	nodeClaims := &karpv1.NodeClaimList{}
	if err := c.kubeClient.List(ctx, nodeClaims, nodeclaimutils.ForNodeClass(nodeClass)); err != nil {
		return reconcile.Result{}, fmt.Errorf("listing nodeclaims for nodeclass, %w", err)
	}
	if len(nodeClaims.Items) > 0 {
		// Releasing the finalizer here would leave running servers with no NodeClass to resolve, which
		// disables drift detection and instance type resolution for them until they are drained by
		// hand. Waiting is the safe option; deleting the NodePool is the supported way to drain them.
		c.recorder.Eventf(nodeClass, corev1.EventTypeNormal, "WaitingOnNodeClaims",
			"Waiting on %d NodeClaim(s) to terminate before deleting the StackitNodeClass", len(nodeClaims.Items))
		return reconcile.Result{RequeueAfter: requeueWhileInUse}, nil
	}

	stored := nodeClass.DeepCopy()
	controllerutil.RemoveFinalizer(nodeClass, v1alpha1.TerminationFinalizer)
	if err := c.kubeClient.Patch(ctx, nodeClass, client.MergeFrom(stored)); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	return reconcile.Result{}, nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("nodeclass.termination").
		For(&v1alpha1.StackitNodeClass{}).
		WithOptions(controller.Options{
			RateLimiter:             reasonable.RateLimiter(),
			MaxConcurrentReconciles: 10,
		}).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}
