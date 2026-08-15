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

// Package instancetype maps STACKIT machine types onto Karpenter instance types.
package instancetype

import (
	"context"
	"fmt"
	"sync"

	"github.com/patrickmn/go-cache"
	"github.com/samber/lo"
	iaas "github.com/stackitcloud/stackit-sdk-go/services/iaas/v2api"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/utils/pretty"

	stackitcache "github.com/kubekanvas/karpenter-provider-stackit/pkg/cache"
	"github.com/kubekanvas/karpenter-provider-stackit/pkg/providers/instancetype/offering"
	"github.com/kubekanvas/karpenter-provider-stackit/pkg/providers/pricing"
	"github.com/kubekanvas/karpenter-provider-stackit/pkg/stackit"
)

type Provider interface {
	Get(ctx context.Context, nodeClass NodeClass, name string) (*cloudprovider.InstanceType, error)
	List(ctx context.Context, nodeClass NodeClass) ([]*cloudprovider.InstanceType, error)
}

type DefaultProvider struct {
	client   stackit.API
	resolver Resolver
	region   string

	muMachineTypes sync.RWMutex
	machineTypes   map[string]iaas.MachineType

	instanceTypesCache      *cache.Cache
	offeringCache           *cache.Cache
	discoveredCapacityCache *cache.Cache
	unavailableOfferings    *stackitcache.UnavailableOfferings
	cm                      *pretty.ChangeMonitor

	offeringProvider *offering.DefaultProvider
}

// NewDefaultProvider builds the instance type provider.
//
// The pricing provider is supplied afterwards via SetPricingProvider rather than here, because the
// two are mutually dependent: STACKIT has no price list API, so prices are modeled from the
// machine type catalog that this provider owns, while this provider needs those prices to build
// offerings. This provider owns the API call, so it is constructed first.
func NewDefaultProvider(
	client stackit.API,
	resolver Resolver,
	region string,
	instanceTypesCache *cache.Cache,
	offeringCache *cache.Cache,
	discoveredCapacityCache *cache.Cache,
	unavailableOfferings *stackitcache.UnavailableOfferings,
) *DefaultProvider {
	return &DefaultProvider{
		client:                  client,
		resolver:                resolver,
		region:                  region,
		machineTypes:            map[string]iaas.MachineType{},
		instanceTypesCache:      instanceTypesCache,
		offeringCache:           offeringCache,
		discoveredCapacityCache: discoveredCapacityCache,
		unavailableOfferings:    unavailableOfferings,
		cm:                      pretty.NewChangeMonitor(),
	}
}

// SetPricingProvider completes construction by attaching the pricing provider. List returns an
// error until it has been called.
func (p *DefaultProvider) SetPricingProvider(pricingProvider pricing.Provider) {
	p.offeringProvider = offering.NewDefaultProvider(pricingProvider, p.unavailableOfferings, p.offeringCache)
}

func (p *DefaultProvider) List(ctx context.Context, nodeClass NodeClass) ([]*cloudprovider.InstanceType, error) {
	if p.offeringProvider == nil {
		return nil, fmt.Errorf("instance type provider was not given a pricing provider")
	}

	p.muMachineTypes.RLock()
	defer p.muMachineTypes.RUnlock()

	if len(p.machineTypes) == 0 {
		return nil, fmt.Errorf("no stackit machine types found")
	}
	zones := nodeClass.AvailabilityZoneIDs()
	if len(zones) == 0 {
		return nil, fmt.Errorf("nodeclass has no resolved availability zones")
	}

	key := p.cacheKey(nodeClass)
	var instanceTypes []*cloudprovider.InstanceType
	if item, ok := p.instanceTypesCache.Get(key); ok {
		instanceTypes = item.([]*cloudprovider.InstanceType)
	} else {
		instanceTypes = lo.FilterMapToSlice(p.machineTypes, func(name string, _ iaas.MachineType) (*cloudprovider.InstanceType, bool) {
			it, err := p.get(ctx, nodeClass, name)
			if err != nil {
				return nil, false
			}
			return it, true
		})
		p.instanceTypesCache.SetDefault(key, instanceTypes)
	}
	return p.offeringProvider.InjectOfferings(instanceTypes, p.region, zones), nil
}

func (p *DefaultProvider) Get(ctx context.Context, nodeClass NodeClass, name string) (*cloudprovider.InstanceType, error) {
	instanceTypes, err := p.List(ctx, nodeClass)
	if err != nil {
		return nil, err
	}
	it, ok := lo.Find(instanceTypes, func(i *cloudprovider.InstanceType) bool { return i.Name == name })
	if !ok {
		return nil, fmt.Errorf("instance type %q not found", name)
	}
	return it, nil
}

func (p *DefaultProvider) get(ctx context.Context, nodeClass NodeClass, name string) (*cloudprovider.InstanceType, error) {
	machineType, ok := p.machineTypes[name]
	if !ok {
		return nil, fmt.Errorf("machine type %q not found in cache", name)
	}
	it := p.resolver.Resolve(ctx, &machineType, nodeClass)
	if it == nil {
		return nil, fmt.Errorf("failed to generate instance type %q", name)
	}
	// Prefer memory capacity observed on a real node over the estimate, since the hypervisor and
	// kernel overhead we subtract is only ever an approximation.
	if cached, ok := p.discoveredCapacityCache.Get(discoveredCapacityCacheKey(it.Name, nodeClass)); ok {
		it.Capacity[corev1.ResourceMemory] = cached.(resource.Quantity)
	}
	InstanceTypeVCPU.Set(float64(machineType.Vcpus), map[string]string{instanceTypeLabel: machineType.Name})
	InstanceTypeMemory.Set(float64(machineType.Ram)*1024*1024, map[string]string{instanceTypeLabel: machineType.Name})
	return it, nil
}

// UpdateMachineTypes refreshes the machine type catalog from STACKIT. It is called once at startup
// and then periodically by the instance type controller.
func (p *DefaultProvider) UpdateMachineTypes(ctx context.Context) error {
	// DO NOT REMOVE THIS LOCK ----------------------------------------------------------------------
	// It serializes concurrent refreshes so that a burst of callers turns into one STACKIT request
	// rather than one per caller.
	p.muMachineTypes.Lock()
	defer p.muMachineTypes.Unlock()

	machineTypes, err := p.client.ListMachineTypes(ctx)
	if err != nil {
		return fmt.Errorf("listing stackit machine types, %w", err)
	}
	if len(machineTypes) == 0 {
		return fmt.Errorf("stackit returned no machine types")
	}

	if p.cm.HasChanged("machine-types", machineTypes) {
		// None of the cached instance types are valid once the machine type catalog moves.
		p.instanceTypesCache.Flush()
		log.FromContext(ctx).WithValues("count", len(machineTypes)).V(1).Info("discovered machine types")
	}
	p.machineTypes = lo.SliceToMap(machineTypes, func(mt iaas.MachineType) (string, iaas.MachineType) {
		return mt.Name, mt
	})
	return nil
}

// MachineTypeSpecs exposes the cached catalog to the pricing provider, which models a price from
// each machine type's resources because STACKIT publishes no price list API.
func (p *DefaultProvider) MachineTypeSpecs(ctx context.Context) ([]pricing.MachineTypeSpec, error) {
	p.muMachineTypes.RLock()
	if len(p.machineTypes) == 0 {
		p.muMachineTypes.RUnlock()
		// Pricing refreshes on its own schedule and may well run before the first catalog refresh.
		if err := p.UpdateMachineTypes(ctx); err != nil {
			return nil, err
		}
		p.muMachineTypes.RLock()
	}
	defer p.muMachineTypes.RUnlock()

	return lo.MapToSlice(p.machineTypes, func(_ string, mt iaas.MachineType) pricing.MachineTypeSpec {
		return pricing.MachineTypeSpec{
			Name:    mt.Name,
			VCPUs:   mt.Vcpus,
			RAMMiB:  mt.Ram,
			DiskGiB: mt.Disk,
		}
	}), nil
}

// UpdateInstanceTypeCapacityFromNode records the memory a real node of this instance type reports.
// Karpenter over-provisions when its estimate is too high, so the smallest observed value wins.
func (p *DefaultProvider) UpdateInstanceTypeCapacityFromNode(ctx context.Context, node *corev1.Node, nodeClass NodeClass) error {
	instanceTypeName, ok := node.Labels[corev1.LabelInstanceTypeStable]
	if !ok {
		return nil
	}
	key := discoveredCapacityCacheKey(instanceTypeName, nodeClass)
	actualCapacity := node.Status.Capacity.Memory()
	cachedCapacity, found := p.discoveredCapacityCache.Get(key)
	if found && actualCapacity.Cmp(cachedCapacity.(resource.Quantity)) > 0 {
		return nil
	}
	// Set even when the values are equal, to refresh the TTL.
	p.discoveredCapacityCache.SetDefault(key, *actualCapacity)
	if !found || actualCapacity.Cmp(cachedCapacity.(resource.Quantity)) < 0 {
		log.FromContext(ctx).WithValues(
			"memory-capacity", actualCapacity,
			"instance-type", instanceTypeName).V(1).Info("updating discovered capacity cache")
	}
	return nil
}

func (p *DefaultProvider) cacheKey(nodeClass NodeClass) string {
	return p.resolver.CacheKey(nodeClass)
}

func discoveredCapacityCacheKey(instanceType string, nodeClass NodeClass) string {
	// The boot volume size is the only NodeClass input that changes what a node of this machine
	// type reports, since the image behind it determines the kernel and therefore how much of the
	// machine type's memory the guest actually sees.
	return fmt.Sprintf("%s-%d", instanceType, nodeClass.BootVolumeSizeGB())
}

// Reset drops all cached machine type and capacity data. Used by tests.
func (p *DefaultProvider) Reset() {
	p.muMachineTypes.Lock()
	defer p.muMachineTypes.Unlock()

	p.machineTypes = map[string]iaas.MachineType{}
	p.instanceTypesCache.Flush()
	p.discoveredCapacityCache.Flush()
}
