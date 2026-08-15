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

import "time"

const (
	// DefaultTTL bounds the QPS this controller drives against the STACKIT IaaS API when reading back
	// resources it just created. It is also the window in which the controller's view of a server
	// may lag the API's. DO NOT CHANGE THIS VALUE WITHOUT DUE CONSIDERATION.
	DefaultTTL = time.Minute
	// UnavailableOfferingsTTL is how long an offering stays out of rotation after STACKIT reported
	// it had no capacity.
	UnavailableOfferingsTTL = 3 * time.Minute
	// InstanceTypesAndOfferingsTTL is how long machine type, zone and price data is reused before being
	// refreshed. STACKIT machine types change on the order of months, so this is deliberately generous.
	InstanceTypesAndOfferingsTTL = 5 * time.Minute
	// DiscoveredCapacityCacheTTL is how long the memory capacity observed on a real node is kept
	// per instance type. It outlives controller restarts' usefulness on purpose: the value only
	// changes when the boot image's kernel changes.
	DiscoveredCapacityCacheTTL = 60 * 24 * time.Hour
	// ValidationTTL is how long a successful NodeClass validation is trusted before being re-run.
	ValidationTTL = 30 * time.Minute
)

const (
	// DefaultCleanupInterval triggers cache cleanup (lazy eviction) at this interval.
	DefaultCleanupInterval = time.Minute
	// UnavailableOfferingsCleanupInterval is shorter than DefaultCleanupInterval so that offerings
	// become schedulable again promptly once their TTL expires.
	UnavailableOfferingsCleanupInterval = 10 * time.Second
)
