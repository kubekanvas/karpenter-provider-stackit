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

// Package stackit narrows the generated STACKIT IaaS SDK down to the calls this provider makes.
//
// The generated client threads (projectId, region) through every method and returns a fluent
// request builder. Wrapping it keeps that repetition out of the providers and gives the unit tests
// a small interface to fake.
package stackit

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/stackitcloud/stackit-sdk-go/core/config"
	"github.com/stackitcloud/stackit-sdk-go/core/oapierror"
	iaas "github.com/stackitcloud/stackit-sdk-go/services/iaas/v2api"

	"github.com/kubekanvas/karpenter-provider-stackit/pkg/utils"
)

// API is the subset of the STACKIT IaaS API this provider depends on.
type API interface {
	// CreateServer launches a server and returns it as accepted by the API.
	CreateServer(ctx context.Context, payload *iaas.CreateServerPayload) (*iaas.Server, error)
	// GetServer returns a single server. It returns an error satisfying IsNotFound once the server
	// is gone.
	GetServer(ctx context.Context, serverID string) (*iaas.Server, error)
	// DeleteServer requests deletion of a server. Deleting an already-deleted server returns an
	// error satisfying IsNotFound.
	DeleteServer(ctx context.Context, serverID string) error
	// ListServers returns every server matching labelSelector, with details (labels included)
	// populated.
	ListServers(ctx context.Context, labelSelector string) ([]iaas.Server, error)
	// UpdateServerLabels replaces the labels on a server.
	UpdateServerLabels(ctx context.Context, serverID string, labels map[string]string) error
	// ListMachineTypes returns the machine types available to the project in the region.
	ListMachineTypes(ctx context.Context) ([]iaas.MachineType, error)
	// ListAvailabilityZones returns the availability zone ids in the region.
	ListAvailabilityZones(ctx context.Context) ([]string, error)
	// DeleteVolume removes a volume. Used to clean up boot volumes the API left behind.
	DeleteVolume(ctx context.Context, volumeID string) error
}

// Client is the API implementation backed by the generated SDK.
type Client struct {
	api       iaas.DefaultAPI
	projectID string
	region    string
}

// NewClient builds a client for one project and region.
//
// Passing no config options makes the SDK discover credentials the standard way, from
// STACKIT_SERVICE_ACCOUNT_KEY, STACKIT_SERVICE_ACCOUNT_KEY_PATH or STACKIT_SERVICE_ACCOUNT_TOKEN.
// The Helm chart mounts whichever of those the operator supplies as environment variables.
func NewClient(projectID, region string, opts ...config.ConfigurationOption) (*Client, error) {
	if projectID == "" {
		return nil, errors.New("stackit: project id is required")
	}
	if region == "" {
		return nil, errors.New("stackit: region is required")
	}
	client, err := iaas.NewAPIClient(append([]config.ConfigurationOption{config.WithRegion(region)}, opts...)...)
	if err != nil {
		return nil, fmt.Errorf("creating stackit iaas client, %w", err)
	}
	return &Client{api: client.DefaultAPI, projectID: projectID, region: region}, nil
}

// NewClientWithAPI builds a client around an already-constructed API, for tests.
func NewClientWithAPI(api iaas.DefaultAPI, projectID, region string) *Client {
	return &Client{api: api, projectID: projectID, region: region}
}

func (c *Client) Region() string { return c.region }

func (c *Client) ProjectID() string { return c.projectID }

func (c *Client) CreateServer(ctx context.Context, payload *iaas.CreateServerPayload) (*iaas.Server, error) {
	server, err := c.api.CreateServer(ctx, c.projectID, c.region).CreateServerPayload(*payload).Execute()
	if err != nil {
		return nil, fmt.Errorf("creating server, %w", err)
	}
	return server, nil
}

func (c *Client) GetServer(ctx context.Context, serverID string) (*iaas.Server, error) {
	server, err := c.api.GetServer(ctx, c.projectID, c.region, serverID).Execute()
	if err != nil {
		return nil, fmt.Errorf("getting server %s, %w", serverID, err)
	}
	return server, nil
}

func (c *Client) DeleteServer(ctx context.Context, serverID string) error {
	if err := c.api.DeleteServer(ctx, c.projectID, c.region, serverID).Execute(); err != nil {
		return fmt.Errorf("deleting server %s, %w", serverID, err)
	}
	return nil
}

func (c *Client) ListServers(ctx context.Context, labelSelector string) ([]iaas.Server, error) {
	// Details(true) is what makes the response carry labels, nics and the boot volume. Without it
	// every server would need a follow-up GetServer just to find its NodePool.
	req := c.api.ListServers(ctx, c.projectID, c.region).Details(true)
	if labelSelector != "" {
		req = req.LabelSelector(labelSelector)
	}
	resp, err := req.Execute()
	if err != nil {
		return nil, fmt.Errorf("listing servers, %w", err)
	}
	if resp == nil {
		return nil, nil
	}
	return resp.Items, nil
}

func (c *Client) UpdateServerLabels(ctx context.Context, serverID string, labels map[string]string) error {
	payload := iaas.UpdateServerPayload{Labels: utils.LabelsToAPI(labels)}
	if _, err := c.api.UpdateServer(ctx, c.projectID, c.region, serverID).UpdateServerPayload(payload).Execute(); err != nil {
		return fmt.Errorf("updating labels on server %s, %w", serverID, err)
	}
	return nil
}

func (c *Client) ListMachineTypes(ctx context.Context) ([]iaas.MachineType, error) {
	resp, err := c.api.ListMachineTypes(ctx, c.projectID, c.region).Execute()
	if err != nil {
		return nil, fmt.Errorf("listing machine types, %w", err)
	}
	if resp == nil {
		return nil, nil
	}
	return resp.Items, nil
}

func (c *Client) ListAvailabilityZones(ctx context.Context) ([]string, error) {
	// ListAvailabilityZones is the one call that is region-scoped but not project-scoped.
	resp, err := c.api.ListAvailabilityZones(ctx, c.region).Execute()
	if err != nil {
		return nil, fmt.Errorf("listing availability zones, %w", err)
	}
	if resp == nil {
		return nil, nil
	}
	return resp.Items, nil
}

func (c *Client) DeleteVolume(ctx context.Context, volumeID string) error {
	if err := c.api.DeleteVolume(ctx, c.projectID, c.region, volumeID).Execute(); err != nil {
		return fmt.Errorf("deleting volume %s, %w", volumeID, err)
	}
	return nil
}

// IsNotFound reports whether err is the API's 404. It matches the behavior of
// cloud-provider-stackit's stackiterrors.IsNotFound so that both components agree on when a server
// has really gone away.
func IsNotFound(err error) bool {
	return hasStatus(err, http.StatusNotFound)
}

// IsConflict reports whether err is the API's 409, which a delete returns while the server is
// still transitioning between states.
func IsConflict(err error) bool {
	return hasStatus(err, http.StatusConflict)
}

// IsUnauthorized reports whether the credentials were rejected, which is worth surfacing distinctly
// because it is the one failure retrying will never fix.
func IsUnauthorized(err error) bool {
	return hasStatus(err, http.StatusUnauthorized) || hasStatus(err, http.StatusForbidden)
}

// IsRetryable reports whether err is a server-side fault worth backing off from rather than
// attributing to the machine type that happened to be requested.
func IsRetryable(err error) bool {
	var apiErr *oapierror.GenericOpenAPIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.StatusCode >= http.StatusInternalServerError || apiErr.StatusCode == http.StatusTooManyRequests
}

// capacityMessageFragments are the phrases STACKIT has been observed to use when a machine type is
// exhausted in an availability zone.
//
// The IaaS API does not document a distinct status code or error code for exhausted capacity, so
// this is a message match and is deliberately conservative: a false negative only means Karpenter
// retries the same offering, whereas a false positive would take a healthy offering out of
// rotation. Revisit once STACKIT publishes a machine-readable code.
var capacityMessageFragments = []string{
	"no valid host",
	"not enough hosts available",
	"insufficient capacity",
	"resource quota exceeded",
	"quota exceeded",
}

// IsInsufficientCapacity reports whether err says the requested machine type cannot currently be
// placed in the requested availability zone.
func IsInsufficientCapacity(err error) bool {
	var apiErr *oapierror.GenericOpenAPIError
	if !errors.As(err, &apiErr) {
		return false
	}
	// STACKIT reports placement failures as a 409 and quota failures as a 403 or 422; a plain 500
	// is a fault, not a capacity signal, and is handled by IsRetryable.
	switch apiErr.StatusCode {
	case http.StatusConflict, http.StatusForbidden, http.StatusUnprocessableEntity, http.StatusServiceUnavailable:
	default:
		return false
	}
	body := strings.ToLower(string(apiErr.Body) + " " + apiErr.ErrorMessage)
	for _, fragment := range capacityMessageFragments {
		if strings.Contains(body, fragment) {
			return true
		}
	}
	return false
}

func hasStatus(err error, code int) bool {
	var apiErr *oapierror.GenericOpenAPIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.StatusCode == code
}
