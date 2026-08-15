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

package pricing_test

import (
	"context"
	"errors"
	"testing"

	"github.com/kubekanvas/karpenter-provider-stackit/pkg/providers/pricing"
)

const (
	smallType  = "g1.1"
	mediumType = "g1.2"
	largeType  = "g1.4"
)

type fakeLister struct {
	specs []pricing.MachineTypeSpec
	err   error
	calls int
}

func (f *fakeLister) MachineTypeSpecs(_ context.Context) ([]pricing.MachineTypeSpec, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.specs, nil
}

func specs() []pricing.MachineTypeSpec {
	return []pricing.MachineTypeSpec{
		{Name: smallType, VCPUs: 1, RAMMiB: 4096},
		{Name: mediumType, VCPUs: 2, RAMMiB: 8192},
		{Name: largeType, VCPUs: 4, RAMMiB: 16384},
	}
}

func TestUpdatePrices(t *testing.T) {
	t.Parallel()

	p := pricing.NewDefaultProvider(&fakeLister{specs: specs()}, nil)
	if err := p.UpdatePrices(context.Background()); err != nil {
		t.Fatalf("UpdatePrices returned error: %v", err)
	}

	for _, name := range []string{smallType, mediumType, largeType} {
		price, ok := p.Price(name)
		if !ok {
			t.Fatalf("Price(%q) not found", name)
		}
		// The whole point of modeling a price is to avoid handing Karpenter a zero, which would
		// make consolidation think the node is free to keep.
		if price <= 0 {
			t.Errorf("Price(%q) = %v, want a strictly positive price", name, price)
		}
	}
	if _, ok := p.Price("does-not-exist"); ok {
		t.Error("Price returned ok for an unknown machine type")
	}
}

// TestPricesAreMonotonicInResources is what actually matters for scheduling: Karpenter compares
// prices to each other, so a bigger machine type must never look cheaper than a smaller one.
func TestPricesAreMonotonicInResources(t *testing.T) {
	t.Parallel()

	p := pricing.NewDefaultProvider(&fakeLister{specs: specs()}, nil)
	if err := p.UpdatePrices(context.Background()); err != nil {
		t.Fatalf("UpdatePrices returned error: %v", err)
	}

	small, _ := p.Price(smallType)
	medium, _ := p.Price(mediumType)
	large, _ := p.Price(largeType)

	if !(small < medium && medium < large) {
		t.Errorf("prices are not monotonic in resources: g1.1=%v g1.2=%v g1.4=%v", small, medium, large)
	}
	// Doubling both cpu and memory should roughly double the price, since the model is linear.
	if ratio := large / medium; ratio < 1.9 || ratio > 2.1 {
		t.Errorf("g1.4/g1.2 price ratio = %v, want approximately 2", ratio)
	}
}

func TestModelPriceIsAlwaysPositive(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		spec pricing.MachineTypeSpec
	}{
		{name: "normal", spec: pricing.MachineTypeSpec{Name: mediumType, VCPUs: 2, RAMMiB: 8192}},
		{name: "no disk", spec: pricing.MachineTypeSpec{Name: mediumType, VCPUs: 2, RAMMiB: 8192, DiskGiB: 0}},
		{name: "all zero", spec: pricing.MachineTypeSpec{Name: "weird"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := pricing.ModelPrice(tc.spec); got <= 0 {
				t.Errorf("ModelPrice(%+v) = %v, want a strictly positive price", tc.spec, got)
			}
		})
	}
}

func TestUpdatePricesAppliesOverrides(t *testing.T) {
	t.Parallel()

	p := pricing.NewDefaultProvider(&fakeLister{specs: specs()}, map[string]float64{mediumType: 0.5})
	if err := p.UpdatePrices(context.Background()); err != nil {
		t.Fatalf("UpdatePrices returned error: %v", err)
	}

	if got, _ := p.Price(mediumType); got != 0.5 {
		t.Errorf("Price(g1.2) = %v, want the override 0.5", got)
	}
	if got, _ := p.Price(largeType); got == 0.5 {
		t.Error("override for g1.2 leaked onto g1.4")
	}
}

func TestUpdatePricesRejectsEmptyCatalog(t *testing.T) {
	t.Parallel()

	p := pricing.NewDefaultProvider(&fakeLister{specs: nil}, nil)
	if err := p.UpdatePrices(context.Background()); err == nil {
		t.Fatal("UpdatePrices accepted an empty machine type catalog")
	}
}

func TestUpdatePricesKeepsLastGoodDataOnError(t *testing.T) {
	t.Parallel()

	lister := &fakeLister{specs: specs()}
	p := pricing.NewDefaultProvider(lister, nil)
	if err := p.UpdatePrices(context.Background()); err != nil {
		t.Fatalf("UpdatePrices returned error: %v", err)
	}
	before, _ := p.Price(mediumType)

	lister.err = errors.New("stackit is having a bad day")
	if err := p.UpdatePrices(context.Background()); err == nil {
		t.Fatal("UpdatePrices swallowed a lister error")
	}

	// A failed refresh must not blank the price table: pricing every node at zero would make the
	// consolidation controller tear the cluster down.
	after, ok := p.Price(mediumType)
	if !ok || after != before {
		t.Errorf("Price(g1.2) = (%v, %v) after a failed refresh, want the previous %v", after, ok, before)
	}
}

func TestParseOverrides(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		raw     string
		want    map[string]float64
		wantErr bool
	}{
		{name: "empty", raw: "", want: map[string]float64{}},
		{name: "whitespace only", raw: "   ", want: map[string]float64{}},
		{name: "single", raw: "g1.2=0.0716", want: map[string]float64{mediumType: 0.0716}},
		{name: "multiple with spaces", raw: "g1.2=0.0716, g1.4=0.1432", want: map[string]float64{mediumType: 0.0716, largeType: 0.1432}},
		{name: "trailing comma", raw: "g1.2=0.0716,", want: map[string]float64{mediumType: 0.0716}},
		{name: "missing equals", raw: "g1.2", wantErr: true},
		{name: "missing value", raw: "g1.2=", wantErr: true},
		{name: "missing name", raw: "=0.5", wantErr: true},
		{name: "unparseable price", raw: "g1.2=cheap", wantErr: true},
		{name: "zero price", raw: "g1.2=0", wantErr: true},
		{name: "negative price", raw: "g1.2=-1", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := pricing.ParseOverrides(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseOverrides(%q) = %v, want error", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseOverrides(%q) returned error: %v", tc.raw, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("ParseOverrides(%q) = %v, want %v", tc.raw, got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("ParseOverrides(%q)[%q] = %v, want %v", tc.raw, k, got[k], v)
				}
			}
		})
	}
}
