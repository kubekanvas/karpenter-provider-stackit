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

// Package pricing supplies the per-hour prices Karpenter uses to rank offerings and to compute
// consolidation savings.
//
// STACKIT publishes no price list API. Verified on 2026-08-15 against stackit-sdk-go v1.14.0: the
// IaaS MachineType model carries only name, vcpus, ram, disk, description and extraSpecs, and the
// `cost` service (v3api) reports spend that has already been incurred per project — it cannot price
// a machine type that is not already running.
//
// Karpenter does not need absolute prices; it needs prices whose *ratios* are right, because they
// decide which instance type it picks and whether consolidating two nodes into one saves money.
// Emitting 0.0 would make every node free and consolidation nonsensical, so this provider derives a
// synthetic price from each machine type's resources instead. Operators who care about absolute
// accuracy override it per machine type with --pricing-overrides.
package pricing

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/karpenter/pkg/utils/pretty"
)

// Resource coefficients for the synthetic price, in euros per hour.
//
// These are anchored to STACKIT's published pay-as-you-go list pricing for general purpose (g1)
// flavors as of 2026-08-15, decomposed into a per-vCPU and a per-GiB-of-RAM component. They are a
// modeling choice, not a quote: STACKIT's actual invoice depends on the contract, and this
// provider has no way to read it. What matters for Karpenter is that a 4 vCPU / 16 GiB machine
// costs about twice a 2 vCPU / 8 GiB one, which any reasonable coefficient pair gets right.
const (
	// EurPerVCPUHour is the modeled hourly cost of one vCPU.
	EurPerVCPUHour = 0.0246
	// EurPerGiBHour is the modeled hourly cost of one GiB of RAM.
	EurPerGiBHour = 0.0033
	// EurPerGiBDiskHour is the modeled hourly cost of one GiB of local disk. Most STACKIT machine
	// types report 0 here because their storage is a separate boot volume, in which case this term
	// drops out.
	EurPerGiBDiskHour = 0.00007
)

const mibPerGiB = 1024.0

// Provider resolves the hourly price of a machine type.
type Provider interface {
	Price(machineType string) (float64, bool)
	UpdatePrices(ctx context.Context) error
}

// MachineTypeSpec is the subset of a STACKIT machine type the price model reads.
type MachineTypeSpec struct {
	Name    string
	VCPUs   int64
	RAMMiB  int64
	DiskGiB int64
}

// MachineTypeLister supplies the machine types to price. It is satisfied by the instance type
// provider, which already lists them for its own purposes.
type MachineTypeLister interface {
	MachineTypeSpecs(ctx context.Context) ([]MachineTypeSpec, error)
}

type DefaultProvider struct {
	lister    MachineTypeLister
	overrides map[string]float64

	mu sync.RWMutex
	// prices is keyed by machine type name. STACKIT prices a machine type identically across the
	// availability zones of one region, so unlike the UpCloud provider there is no zone dimension.
	prices map[string]float64

	cm *pretty.ChangeMonitor
}

func NewDefaultProvider(lister MachineTypeLister, overrides map[string]float64) *DefaultProvider {
	return &DefaultProvider{
		lister:    lister,
		overrides: overrides,
		prices:    map[string]float64{},
		cm:        pretty.NewChangeMonitor(),
	}
}

// Price returns the modeled hourly price in euros for a machine type. The second return value is
// false for a machine type that has not been seen yet, which is how the instance type provider
// avoids publishing an offering it cannot rank.
func (p *DefaultProvider) Price(machineType string) (float64, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	price, ok := p.prices[machineType]
	return price, ok
}

// UpdatePrices recomputes prices for every machine type currently offered.
func (p *DefaultProvider) UpdatePrices(ctx context.Context) error {
	specs, err := p.lister.MachineTypeSpecs(ctx)
	if err != nil {
		return fmt.Errorf("listing machine types for pricing, %w", err)
	}
	if len(specs) == 0 {
		return fmt.Errorf("no stackit machine types found to price")
	}

	prices := make(map[string]float64, len(specs))
	for _, spec := range specs {
		if override, ok := p.overrides[spec.Name]; ok {
			prices[spec.Name] = override
			continue
		}
		prices[spec.Name] = ModelPrice(spec)
	}

	p.mu.Lock()
	p.prices = prices
	p.mu.Unlock()

	if p.cm.HasChanged("pricing", prices) {
		log.FromContext(ctx).WithValues(
			"machine-types", len(prices),
			"overridden", len(p.overrides),
		).V(1).Info("computed modeled pricing")
	}
	return nil
}

// ModelPrice computes the synthetic hourly price of a machine type from its resources.
//
// The result is always strictly positive: a machine type that reported no resources at all would
// otherwise price at zero and look free to the consolidation controller.
func ModelPrice(spec MachineTypeSpec) float64 {
	price := float64(spec.VCPUs)*EurPerVCPUHour +
		(float64(spec.RAMMiB)/mibPerGiB)*EurPerGiBHour +
		float64(spec.DiskGiB)*EurPerGiBDiskHour
	if price <= 0 {
		// Not expected — every machine type the API returns has vcpus and ram — but a zero price
		// would make consolidation believe this node costs nothing to keep.
		return EurPerVCPUHour
	}
	return price
}

// Reset drops all cached prices. Used by tests.
func (p *DefaultProvider) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.prices = map[string]float64{}
}

// ParseOverrides reads the --pricing-overrides flag, a comma-separated list of
// "<machine-type>=<euros-per-hour>" pairs, e.g. "g1.2=0.0716,g1.4=0.1432".
func ParseOverrides(raw string) (map[string]float64, error) {
	overrides := map[string]float64{}
	if strings.TrimSpace(raw) == "" {
		return overrides, nil
	}
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		name, value, ok := strings.Cut(pair, "=")
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if !ok || name == "" || value == "" {
			return nil, fmt.Errorf("pricing override %q is not of the form <machine-type>=<euros-per-hour>", pair)
		}
		price, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return nil, fmt.Errorf("pricing override %q has an unparseable price, %w", pair, err)
		}
		if price <= 0 {
			return nil, fmt.Errorf("pricing override %q must be greater than zero", pair)
		}
		overrides[name] = price
	}
	return overrides, nil
}
