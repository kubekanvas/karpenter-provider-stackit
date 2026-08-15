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

// Package operator wires the STACKIT providers together and hands them to the Karpenter operator.
package operator

import (
	"context"
	"fmt"

	"github.com/patrickmn/go-cache"
	"github.com/stackitcloud/stackit-sdk-go/core/config"
	"sigs.k8s.io/karpenter/pkg/operator"

	stackitcache "github.com/kubekanvas/karpenter-provider-stackit/pkg/cache"
	"github.com/kubekanvas/karpenter-provider-stackit/pkg/operator/options"
	"github.com/kubekanvas/karpenter-provider-stackit/pkg/providers/instance"
	"github.com/kubekanvas/karpenter-provider-stackit/pkg/providers/instancetype"
	"github.com/kubekanvas/karpenter-provider-stackit/pkg/providers/pricing"
	"github.com/kubekanvas/karpenter-provider-stackit/pkg/stackit"
)

const userAgent = "karpenter-provider-stackit"

// Operator holds everything the STACKIT CloudProvider and its controllers are built from.
type Operator struct {
	*operator.Operator

	StackitClient             stackit.API
	UnavailableOfferingsCache *stackitcache.UnavailableOfferings
	ValidationCache           *cache.Cache
	InstanceTypesProvider     *instancetype.DefaultProvider
	InstanceProvider          instance.Provider
	PricingProvider           *pricing.DefaultProvider
}

// NewOperator builds the STACKIT operator. Pass a non-nil client to inject a fake in tests;
// otherwise credentials are discovered by the SDK from STACKIT_SERVICE_ACCOUNT_KEY,
// STACKIT_SERVICE_ACCOUNT_KEY_PATH or STACKIT_SERVICE_ACCOUNT_TOKEN.
func NewOperator(ctx context.Context, op *operator.Operator, stackitClient stackit.API) (*Operator, error) {
	opts := options.FromContext(ctx)

	if stackitClient == nil {
		client, err := stackit.NewClient(opts.ProjectID, opts.Region, config.WithUserAgent(userAgent))
		if err != nil {
			return nil, fmt.Errorf("creating stackit client, %w", err)
		}
		stackitClient = client
	}

	unavailableOfferings := stackitcache.NewUnavailableOfferings()
	validationCache := cache.New(stackitcache.ValidationTTL, stackitcache.DefaultCleanupInterval)

	instanceTypeProvider := instancetype.NewDefaultProvider(
		stackitClient,
		instancetype.NewDefaultResolver(opts.Region),
		opts.Region,
		cache.New(stackitcache.InstanceTypesAndOfferingsTTL, stackitcache.DefaultCleanupInterval),
		cache.New(stackitcache.InstanceTypesAndOfferingsTTL, stackitcache.DefaultCleanupInterval),
		cache.New(stackitcache.DiscoveredCapacityCacheTTL, stackitcache.DefaultCleanupInterval),
		unavailableOfferings,
	)
	// The two providers are mutually dependent: pricing models a price from the machine type
	// catalog, and the catalog needs prices to build offerings. The catalog owns the API call, so
	// pricing reads from it and is injected back afterwards.
	pricingProvider := pricing.NewDefaultProvider(instanceTypeProvider, opts.PricingOverrides)
	instanceTypeProvider.SetPricingProvider(pricingProvider)

	// Machine types and prices must be loaded before any controller that depends on them starts,
	// otherwise the first scheduling pass sees an empty instance type list and reports the cluster
	// as unable to scale. Refreshes after this point are handled asynchronously by controllers.
	if err := instanceTypeProvider.UpdateMachineTypes(ctx); err != nil {
		return nil, fmt.Errorf("hydrating stackit machine types, %w", err)
	}
	if err := pricingProvider.UpdatePrices(ctx); err != nil {
		return nil, fmt.Errorf("hydrating stackit prices, %w", err)
	}

	instanceProvider := instance.NewDefaultProvider(
		opts.ClusterName,
		stackitClient,
		unavailableOfferings,
		cache.New(stackitcache.DefaultTTL, stackitcache.DefaultCleanupInterval),
	)

	return &Operator{
		Operator:                  op,
		StackitClient:             stackitClient,
		UnavailableOfferingsCache: unavailableOfferings,
		ValidationCache:           validationCache,
		InstanceTypesProvider:     instanceTypeProvider,
		InstanceProvider:          instanceProvider,
		PricingProvider:           pricingProvider,
	}, nil
}
