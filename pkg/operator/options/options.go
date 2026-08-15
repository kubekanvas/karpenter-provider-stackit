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

package options

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	coreoptions "sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/utils/env"

	"github.com/kubekanvas/karpenter-provider-stackit/pkg/providers/pricing"
	"github.com/kubekanvas/karpenter-provider-stackit/pkg/utils"
)

func init() {
	coreoptions.Injectables = append(coreoptions.Injectables, &Options{})
}

type optionsKey struct{}

type Options struct {
	// ClusterName identifies this cluster's servers in the STACKIT project. It is written to every
	// server as a label and is the filter used to list them, so two clusters sharing a STACKIT
	// project must not share a name.
	ClusterName string
	// ProjectID is the STACKIT project servers are created in.
	ProjectID string
	// Region is the STACKIT region servers are created in. It must equal the region the
	// cloud-controller-manager is configured with, because the CCM writes its own configured
	// region to every node as topology.kubernetes.io/region; a mismatch leaves NodeClaims
	// permanently unregistered.
	Region string
	// VMMemoryOverheadPercent is subtracted from a machine type's advertised memory to approximate
	// what the guest kernel actually sees, until a real node reports its capacity.
	VMMemoryOverheadPercent float64
	// PricingOverrides maps machine type name to euros per hour, overriding the modeled price.
	PricingOverrides map[string]float64
	// DisableDryRun skips the STACKIT-side validation the NodeClass controller performs.
	DisableDryRun bool

	rawPricingOverrides string
}

func (o *Options) AddFlags(fs *coreoptions.FlagSet) {
	fs.StringVar(&o.ClusterName, "cluster-name", env.WithDefaultString("CLUSTER_NAME", ""),
		"[REQUIRED] The Kubernetes cluster name, used to label and discover this cluster's STACKIT servers.")
	fs.StringVar(&o.ProjectID, "project-id", env.WithDefaultString("STACKIT_PROJECT_ID", ""),
		"[REQUIRED] The STACKIT project ID that servers are created in.")
	fs.StringVar(&o.Region, "region", env.WithDefaultString("STACKIT_REGION", ""),
		"[REQUIRED] The STACKIT region servers are created in, e.g. eu01. Must match the cloud-controller-manager's configured region.")
	fs.Float64Var(&o.VMMemoryOverheadPercent, "vm-memory-overhead-percent",
		utils.WithDefaultFloat64("VM_MEMORY_OVERHEAD_PERCENT", 0.075),
		"The fraction of a machine type's memory assumed to be lost to virtualization and kernel overhead when no node of that type has reported its capacity yet.")
	fs.StringVar(&o.rawPricingOverrides, "pricing-overrides", env.WithDefaultString("PRICING_OVERRIDES", ""),
		"Comma-separated <machine-type>=<euros-per-hour> pairs overriding the modeled price, e.g. g1.2=0.0716,g1.4=0.1432. STACKIT publishes no price list API, so prices are otherwise modeled from each machine type's resources.")
	fs.BoolVarWithEnv(&o.DisableDryRun, "disable-dry-run", "DISABLE_DRY_RUN", false,
		"If true, skip validating StackitNodeClasses against the STACKIT API.")
}

func (o *Options) Parse(fs *coreoptions.FlagSet, args ...string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		return fmt.Errorf("parsing flags, %w", err)
	}
	overrides, err := pricing.ParseOverrides(o.rawPricingOverrides)
	if err != nil {
		return fmt.Errorf("parsing --pricing-overrides, %w", err)
	}
	o.PricingOverrides = overrides
	if err := o.Validate(); err != nil {
		return fmt.Errorf("validating options, %w", err)
	}
	return nil
}

func (o *Options) Validate() error {
	var errs []error
	if o.ClusterName == "" {
		errs = append(errs, fmt.Errorf("missing required --cluster-name (or CLUSTER_NAME)"))
	}
	if o.ProjectID == "" {
		errs = append(errs, fmt.Errorf("missing required --project-id (or STACKIT_PROJECT_ID)"))
	}
	if o.Region == "" {
		errs = append(errs, fmt.Errorf("missing required --region (or STACKIT_REGION)"))
	}
	if o.VMMemoryOverheadPercent < 0 || o.VMMemoryOverheadPercent >= 1 {
		errs = append(errs, fmt.Errorf("--vm-memory-overhead-percent must be in [0, 1), got %v", o.VMMemoryOverheadPercent))
	}
	return errors.Join(errs...)
}

func (o *Options) ToContext(ctx context.Context) context.Context {
	return ToContext(ctx, o)
}

func ToContext(ctx context.Context, opts *Options) context.Context {
	return context.WithValue(ctx, optionsKey{}, opts)
}

func FromContext(ctx context.Context) *Options {
	retval := ctx.Value(optionsKey{})
	if retval == nil {
		return nil
	}
	return retval.(*Options)
}
