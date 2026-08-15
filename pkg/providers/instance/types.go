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

package instance

import (
	"time"

	iaas "github.com/stackitcloud/stackit-sdk-go/services/iaas/v2api"

	"github.com/kubekanvas/karpenter-provider-stackit/pkg/utils"
)

// Server statuses this provider reacts to. The IaaS API documents the full set on Server.Status as
// ACTIVE, BACKING-UP, CREATING, DEALLOCATED, DEALLOCATING, DELETED, DELETING, ERROR, INACTIVE,
// MIGRATING, PAUSED, REBOOT, REBOOTING, REBUILD, REBUILDING, RESCUE, RESCUING, RESIZING, RESTORING,
// SNAPSHOTTING, STARTING, STOPPING, UNRESCUING and UPDATING.
const (
	StatusActive   = "ACTIVE"
	StatusCreating = "CREATING"
	StatusDeleting = "DELETING"
	StatusDeleted  = "DELETED"
	StatusError    = "ERROR"
)

// Instance is this provider's view of a STACKIT IaaS server.
//
// Unlike the UpCloud provider, this is built straight from a list response: STACKIT's ListServers
// takes a details flag that includes labels and the boot volume, so mapping a server back to its
// NodePool costs no extra call.
type Instance struct {
	ID               string
	Name             string
	AvailabilityZone string
	MachineType      string
	Status           string
	CreatedAt        time.Time
	BootVolumeID     string
	Labels           map[string]string
}

// NewInstance builds an Instance from an API server object.
func NewInstance(server *iaas.Server) *Instance {
	instance := &Instance{
		ID:               server.GetId(),
		Name:             server.GetName(),
		AvailabilityZone: server.GetAvailabilityZone(),
		MachineType:      server.GetMachineType(),
		Status:           server.GetStatus(),
		CreatedAt:        server.GetCreatedAt(),
		Labels:           utils.LabelsFromAPI(server.GetLabels()),
	}
	if server.HasBootVolume() {
		bootVolume := server.GetBootVolume()
		instance.BootVolumeID = bootVolume.GetId()
	}
	return instance
}

// ProviderID renders the instance as a Kubernetes providerID.
func (i *Instance) ProviderID() string {
	return utils.ProviderID(i.ID)
}

// Zone is the availability zone in the form the cloud-controller-manager writes it to the node's
// topology.kubernetes.io/zone label. Offerings and NodeClaim labels must both use this form.
func (i *Instance) Zone() string {
	return utils.SanitizeZone(i.AvailabilityZone)
}

// Terminating reports whether STACKIT has already accepted a deletion for this server.
func (i *Instance) Terminating() bool {
	return i.Status == StatusDeleting || i.Status == StatusDeleted
}
