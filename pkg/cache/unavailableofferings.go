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

package cache

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/patrickmn/go-cache"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// UnavailableOfferings records machineType-zone combinations that STACKIT reported as being out of
// capacity. Offerings in this cache are marked unavailable in GetInstanceTypes responses, which
// steers the scheduler onto a different machine type or zone instead of retrying a launch that will fail.
type UnavailableOfferings struct {
	// key: <machineType>:<zone>
	offeringCache         *cache.Cache
	offeringCacheSeqNumMu sync.RWMutex
	offeringCacheSeqNum   map[string]uint64

	// A whole zone can be unavailable, e.g. when STACKIT rejects every launch in it. Tracking that
	// separately avoids having to enumerate every machine type to take a zone out of rotation.
	zoneCache       *cache.Cache
	zoneCacheSeqNum atomic.Uint64
}

func NewUnavailableOfferings() *UnavailableOfferings {
	uo := &UnavailableOfferings{
		offeringCache:       cache.New(UnavailableOfferingsTTL, UnavailableOfferingsCleanupInterval),
		offeringCacheSeqNum: map[string]uint64{},
		zoneCache:           cache.New(UnavailableOfferingsTTL, UnavailableOfferingsCleanupInterval),
	}
	uo.offeringCache.OnEvicted(func(k string, _ interface{}) {
		machineType, _, ok := strings.Cut(k, ":")
		if !ok {
			panic("unavailable offerings cache key is not of expected format <machineType>:<zone>")
		}
		uo.offeringCacheSeqNumMu.Lock()
		uo.offeringCacheSeqNum[machineType]++
		uo.offeringCacheSeqNumMu.Unlock()
	})
	uo.zoneCache.OnEvicted(func(_ string, _ interface{}) {
		uo.zoneCacheSeqNum.Add(1)
	})
	return uo
}

// SeqNum returns a number that changes whenever the availability of any offering for the given machine type
// changes. Callers cache offerings keyed on it rather than re-deriving them on every List.
func (u *UnavailableOfferings) SeqNum(machineType string) uint64 {
	u.offeringCacheSeqNumMu.RLock()
	defer u.offeringCacheSeqNumMu.RUnlock()

	return u.offeringCacheSeqNum[machineType] + u.zoneCacheSeqNum.Load()
}

// IsUnavailable returns true if the machine type is currently out of rotation in the zone.
func (u *UnavailableOfferings) IsUnavailable(machineType, zone string) bool {
	_, offeringFound := u.offeringCache.Get(u.key(machineType, zone))
	_, zoneFound := u.zoneCache.Get(zone)
	return offeringFound || zoneFound
}

// MarkUnavailable takes a single machineType-zone offering out of rotation for UnavailableOfferingsTTL.
func (u *UnavailableOfferings) MarkUnavailable(ctx context.Context, reason, machineType, zone string) {
	// Even when the key is already cached we still Set it, to extend the entry's TTL.
	log.FromContext(ctx).WithValues(
		"reason", reason,
		"machine-type", machineType,
		"zone", zone,
		"ttl", UnavailableOfferingsTTL).V(1).Info("removing offering from offerings")
	u.offeringCache.SetDefault(u.key(machineType, zone), struct{}{})
	u.offeringCacheSeqNumMu.Lock()
	u.offeringCacheSeqNum[machineType]++
	u.offeringCacheSeqNumMu.Unlock()
}

// MarkZoneUnavailable takes every offering in a zone out of rotation for UnavailableOfferingsTTL.
func (u *UnavailableOfferings) MarkZoneUnavailable(ctx context.Context, reason, zone string) {
	log.FromContext(ctx).WithValues(
		"reason", reason,
		"zone", zone,
		"ttl", UnavailableOfferingsTTL).V(1).Info("removing zone from offerings")
	u.zoneCache.SetDefault(zone, struct{}{})
	u.zoneCacheSeqNum.Add(1)
}

func (u *UnavailableOfferings) Delete(machineType, zone string) {
	u.offeringCache.Delete(u.key(machineType, zone))
}

func (u *UnavailableOfferings) Flush() {
	u.offeringCache.Flush()
	u.zoneCache.Flush()
}

func (u *UnavailableOfferings) key(machineType, zone string) string {
	return fmt.Sprintf("%s:%s", machineType, zone)
}
