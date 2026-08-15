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

package main

import (
	"os"

	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/metrics"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/overlay"
	corecontrollers "sigs.k8s.io/karpenter/pkg/controllers"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	coreoperator "sigs.k8s.io/karpenter/pkg/operator"

	"github.com/kubekanvas/karpenter-provider-stackit/pkg/cloudprovider"
	"github.com/kubekanvas/karpenter-provider-stackit/pkg/controllers"
	"github.com/kubekanvas/karpenter-provider-stackit/pkg/operator"
	"github.com/kubekanvas/karpenter-provider-stackit/pkg/operator/options"
)

func main() {
	ctx, coreOp := coreoperator.NewOperator()
	op, err := operator.NewOperator(ctx, coreOp, nil)
	if err != nil {
		log.FromContext(ctx).Error(err, "Failed to create STACKIT operator")
		os.Exit(1)
	}

	stackitCloudProvider := cloudprovider.New(
		op.InstanceTypesProvider,
		op.InstanceProvider,
		op.EventRecorder,
		op.GetClient(),
		options.FromContext(ctx).Region,
	)
	// The metrics decorator has to sit underneath the overlay decorator so that the metrics reflect
	// what the cloud provider actually did, not what NodeOverlays reshaped it into.
	overlayUndecoratedCloudProvider := metrics.Decorate(stackitCloudProvider)
	cloudProvider := overlay.Decorate(overlayUndecoratedCloudProvider, op.GetClient(), op.InstanceTypeStore)
	clusterState := state.NewCluster(op.Clock, op.GetClient(), cloudProvider)

	op.
		WithControllers(ctx, corecontrollers.NewControllers(
			ctx,
			op.Manager,
			op.Clock,
			op.GetClient(),
			op.EventRecorder,
			cloudProvider,
			overlayUndecoratedCloudProvider,
			clusterState,
			op.InstanceTypeStore,
		)...).
		WithControllers(ctx, controllers.NewControllers(
			ctx,
			op.Manager,
			op.GetClient(),
			op.StackitClient,
			op.ValidationCache,
			cloudProvider,
			op.InstanceProvider,
			op.InstanceTypesProvider,
			op.PricingProvider,
		)...).
		Start(ctx)
}
