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

package v1alpha1

import (
	"github.com/awslabs/operatorpkg/status"
)

const (
	ConditionTypeAvailabilityZonesReady = "AvailabilityZonesReady"
	ConditionTypeValidationSucceeded    = "ValidationSucceeded"
)

// AvailabilityZone is a resolved STACKIT availability zone that servers may be launched into.
type AvailabilityZone struct {
	// id of the availability zone, e.g. "eu01-1".
	// +required
	ID string `json:"id"`
}

// StackitNodeClassStatus contains the resolved state of the StackitNodeClass.
type StackitNodeClassStatus struct {
	// availabilityZones contains the zones resolved from spec.availabilityZones that STACKIT
	// reports for the configured region. Offerings are only created for these zones.
	// +optional
	AvailabilityZones []AvailabilityZone `json:"availabilityZones,omitempty"`
	// conditions contains signals for health and readiness.
	// +optional
	Conditions []status.Condition `json:"conditions,omitempty"`
}

// AvailabilityZoneIDs returns the resolved zone ids, which is the form the instance type provider
// consumes.
func (in *StackitNodeClass) AvailabilityZoneIDs() []string {
	ids := make([]string, 0, len(in.Status.AvailabilityZones))
	for _, z := range in.Status.AvailabilityZones {
		ids = append(ids, z.ID)
	}
	return ids
}
