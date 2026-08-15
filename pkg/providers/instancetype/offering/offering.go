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

// Package offering attaches per-zone availability and price to instance types.
package offering

import (
	"fmt"
	"sync"

	"github.com/mitchellh/hashstructure/v2"
	"github.com/patrickmn/go-cache"
	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	stackitcache "github.com/kubekanvas/karpenter-provider-stackit/pkg/cache"
	"github.com/kubekanvas/karpenter-provider-stackit/pkg/providers/pricing"
	"github.com/kubekanvas/karpenter-provider-stackit/pkg/utils"
)

type Provider interface {
	InjectOfferings(instanceTypes []*cloudprovider.InstanceType, region string, zones []string) []*cloudprovider.InstanceType
}

type DefaultProvider struct {
	pricingProvider      pricing.Provider
	unavailableOfferings *stackitcache.UnavailableOfferings
	// lastUnavailableOfferingsSeqNum maps machine type name to the availability sequence number the
	// cached offerings were built from, so cached entries are dropped the moment availability
	// changes.
	lastUnavailableOfferingsSeqNum sync.Map
	cache                          *cache.Cache
}

func NewDefaultProvider(
	pricingProvider pricing.Provider,
	unavailableOfferings *stackitcache.UnavailableOfferings,
	offeringCache *cache.Cache,
) *DefaultProvider {
	return &DefaultProvider{
		pricingProvider:      pricingProvider,
		unavailableOfferings: unavailableOfferings,
		cache:                offeringCache,
	}
}

// InjectOfferings returns copies of instanceTypes with their offerings resolved against the current
// price list and availability cache. Offerings are deliberately not cached alongside the rest of
// the instance type: availability changes far more often than capacity or requirements do, and a
// stale "available" offering costs a failed launch.
func (p *DefaultProvider) InjectOfferings(instanceTypes []*cloudprovider.InstanceType, region string, zones []string) []*cloudprovider.InstanceType {
	its := make([]*cloudprovider.InstanceType, 0, len(instanceTypes))
	for _, it := range instanceTypes {
		// The copy is one level deep on purpose: it lets us swap offerings without mutating the
		// instance types handed out by earlier GetInstanceTypes calls.
		its = append(its, &cloudprovider.InstanceType{
			Name:         it.Name,
			Requirements: it.Requirements,
			Offerings:    p.createOfferings(it, region, zones),
			Capacity:     it.Capacity,
			Overhead:     it.Overhead,
		})
	}
	return its
}

func (p *DefaultProvider) createOfferings(it *cloudprovider.InstanceType, region string, zones []string) cloudprovider.Offerings {
	key := p.cacheKey(it, region, zones)
	seqNum := p.unavailableOfferings.SeqNum(it.Name)
	lastSeqNum, ok := p.lastUnavailableOfferingsSeqNum.Load(it.Name)
	if !ok {
		lastSeqNum = uint64(0)
	}
	if cached, ok := p.cache.Get(key); ok && lastSeqNum == seqNum {
		return cached.(cloudprovider.Offerings)
	}

	// STACKIT prices a machine type per region, not per availability zone, so the price is looked
	// up once and applied to every zone's offering.
	price, priced := p.pricingProvider.Price(it.Name)

	offerings := cloudprovider.Offerings{}
	if priced {
		for _, zone := range zones {
			// The zone must be in the form the cloud-controller-manager writes to
			// topology.kubernetes.io/zone, or the offering's requirement will not match the node.
			sanitized := utils.SanitizeZone(zone)
			offerings = append(offerings, &cloudprovider.Offering{
				Requirements: scheduling.NewRequirements(
					scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, sanitized),
					scheduling.NewRequirement(corev1.LabelTopologyRegion, corev1.NodeSelectorOpIn, region),
					// STACKIT sells capacity one way only: the IaaS API has no spot or preemptible
					// market and no reservation concept, so every offering is on-demand.
					scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
				),
				Price:     price,
				Available: !p.unavailableOfferings.IsUnavailable(it.Name, sanitized),
			})
		}
	}

	p.cache.SetDefault(key, offerings)
	p.lastUnavailableOfferingsSeqNum.Store(it.Name, seqNum)
	return offerings
}

func (p *DefaultProvider) cacheKey(it *cloudprovider.InstanceType, region string, zones []string) string {
	zonesHash, _ := hashstructure.Hash(zones, hashstructure.FormatV2, &hashstructure.HashOptions{SlicesAsSets: true})
	return fmt.Sprintf("%s-%s-%016x", it.Name, region, zonesHash)
}
