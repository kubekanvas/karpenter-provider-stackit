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

// Package garbagecollection terminates STACKIT servers that no longer have a NodeClaim backing them.
package garbagecollection

import (
	"context"
	"fmt"
	"time"

	"github.com/awslabs/operatorpkg/reconciler"
	"github.com/awslabs/operatorpkg/singleton"
	"github.com/samber/lo"
	"go.uber.org/multierr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/operator/injection"
	nodeclaimutils "sigs.k8s.io/karpenter/pkg/utils/nodeclaim"
)

const (
	// gcGracePeriod protects servers that were launched moments ago and whose NodeClaim has not been
	// observed yet. Without it, a launch racing this controller would be reaped immediately.
	//
	// The age it is compared against comes from the server's own createdAt, which STACKIT returns
	// on both GetServer and ListServers, so no launch-time bookkeeping of our own is involved.
	gcGracePeriod = 2 * time.Minute

	// gcParallelism bounds concurrent terminations. It is generous because each termination is a couple of
	// STACKIT calls at most and the reconcile is not on any hot path.
	gcParallelism = 20
)

type Controller struct {
	kubeClient    client.Client
	cloudProvider cloudprovider.CloudProvider
	// successfulCount drives a faster requeue for the first few passes, so a controller that starts
	// up next to a pile of orphans clears them promptly rather than over the next hour.
	successfulCount uint64
}

func NewController(kubeClient client.Client, cloudProvider cloudprovider.CloudProvider) *Controller {
	return &Controller{
		kubeClient:    kubeClient,
		cloudProvider: cloudProvider,
	}
}

func (c *Controller) Reconcile(ctx context.Context) (reconciler.Result, error) {
	ctx = injection.WithControllerName(ctx, "instance.garbagecollection")

	// List from STACKIT first. Servers are reaped based on whether a NodeClaim exists, so reading
	// the cloud side first and the cluster side second can only ever make this more conservative:
	// a NodeClaim created in between is seen, and the server is left alone.
	cloudNodeClaims, err := c.cloudProvider.List(ctx)
	if err != nil {
		return reconciler.Result{}, fmt.Errorf("listing cloudprovider nodeclaims, %w", err)
	}
	cloudNodeClaims = lo.Filter(cloudNodeClaims, func(nc *karpv1.NodeClaim, _ int) bool {
		return nc.DeletionTimestamp.IsZero()
	})

	clusterNodeClaims, err := nodeclaimutils.ListManaged(ctx, c.kubeClient, c.cloudProvider)
	if err != nil {
		return reconciler.Result{}, err
	}
	clusterProviderIDs := sets.New(lo.FilterMap(clusterNodeClaims, func(nc *karpv1.NodeClaim, _ int) (string, bool) {
		return nc.Status.ProviderID, nc.Status.ProviderID != ""
	})...)

	nodeList := &corev1.NodeList{}
	if err := c.kubeClient.List(ctx, nodeList); err != nil {
		return reconciler.Result{}, err
	}

	errs := make([]error, len(cloudNodeClaims))
	workqueue.ParallelizeUntil(ctx, gcParallelism, len(cloudNodeClaims), func(i int) {
		nc := cloudNodeClaims[i]
		if clusterProviderIDs.Has(nc.Status.ProviderID) {
			return
		}
		// A missing createdAt must never read as "infinitely old". STACKIT populates it on every
		// server, so reaching this branch means the API returned something unexpected — and
		// deleting a live server on the strength of a field we failed to read is not recoverable.
		// Skip it and say so; the next pass retries once the field is readable.
		if nc.CreationTimestamp.Time.IsZero() {
			log.FromContext(ctx).WithValues("provider-id", nc.Status.ProviderID).
				Info("skipping garbage collection for server with no creation timestamp")
			return
		}
		if time.Since(nc.CreationTimestamp.Time) < gcGracePeriod {
			return
		}
		errs[i] = c.garbageCollect(ctx, nc, nodeList)
	})
	if err := multierr.Combine(errs...); err != nil {
		return reconciler.Result{}, err
	}

	c.successfulCount++
	return reconciler.Result{RequeueAfter: lo.Ternary(c.successfulCount <= 20, 10*time.Second, 2*time.Minute)}, nil
}

func (c *Controller) garbageCollect(ctx context.Context, nodeClaim *karpv1.NodeClaim, nodeList *corev1.NodeList) error {
	ctx = log.IntoContext(ctx, log.FromContext(ctx).WithValues("provider-id", nodeClaim.Status.ProviderID))
	if err := c.cloudProvider.Delete(ctx, nodeClaim); err != nil {
		return cloudprovider.IgnoreNodeClaimNotFoundError(err)
	}
	log.FromContext(ctx).V(1).Info("garbage collected cloudprovider instance")

	// Removing the Node too lets the scheduler forget about it immediately instead of waiting for
	// the node lifecycle controller to notice the instance is gone.
	if node, ok := lo.Find(nodeList.Items, func(n corev1.Node) bool {
		return n.Spec.ProviderID == nodeClaim.Status.ProviderID
	}); ok {
		if err := c.kubeClient.Delete(ctx, &node); err != nil {
			return client.IgnoreNotFound(err)
		}
		log.FromContext(ctx).WithValues("Node", klog.KObj(&node)).V(1).Info("garbage collected node")
	}
	return nil
}

func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named("instance.garbagecollection").
		WatchesRawSource(singleton.Source()).
		Complete(singleton.AsReconciler(c))
}
