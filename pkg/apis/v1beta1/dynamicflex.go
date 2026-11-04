/*
** Karpenter Provider OCI
**
** Copyright (c) 2026 Oracle and/or its affiliates.
** Licensed under the Universal Permissive License v 1.0 as shown at https://oss.oracle.com/licenses/upl/
 */

package v1beta1

import "fmt"

const (
	DefaultMaxFlexConfigurations int32 = 128
	MaxFlexConfigurations        int32 = 512
	LowestPriceThatFits                = "lowest-price-that-fits"
)

// DynamicFlex describes an exact grid, intersected with OCI shape and image limits.
// A catalog exceeding MaxConfigurations is rejected; configurations are never sampled.
type DynamicFlex struct {
	// Enabled activates the policy. Disabling it restores explicit/global shapeConfigs.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// Ocpus bounds the integer OCPU grid. These are physical OCPUs, not Kubernetes CPUs.
	// +kubebuilder:validation:XValidation:rule="self.max <= 4096",message="ocpus.max must be at most 4096"
	Ocpus FlexRange `json:"ocpus"`

	// MemoryInGbs bounds the integer memory grid in OCI gigabytes.
	MemoryInGbs FlexRange `json:"memoryInGbs"`

	// MaxConfigurations caps the number of valid CPU/memory pairs per shape family,
	// before zones and capacity types. Use coarser steps or narrower bounds on overflow.
	// +kubebuilder:default=128
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=512
	// +optional
	MaxConfigurations *int32 `json:"maxConfigurations,omitempty"`

	// BaselineOcpuUtilization applies to every generated configuration.
	// Burstable policies support only OCI families with known burstable limits.
	// +kubebuilder:default=BASELINE_1_1
	// +optional
	BaselineOcpuUtilization *BaselineOcpuUtilization `json:"baselineOcpuUtilization,omitempty"`

	// SelectionPolicy uses concrete prices and allocatable resources through Karpenter's
	// existing scheduling and capacity-type priorities.
	// +kubebuilder:default=lowest-price-that-fits
	// +kubebuilder:validation:Enum=lowest-price-that-fits
	// +optional
	SelectionPolicy string `json:"selectionPolicy,omitempty"`
}

// FlexRange contains grid points Min + n*Step <= Max. An unaligned Max is not added.
// +kubebuilder:validation:XValidation:rule="self.min <= self.max",message="min must not exceed max"
type FlexRange struct {
	// Min is the inclusive lower bound and grid origin.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1048576
	Min int32 `json:"min"`
	// Max is the inclusive upper bound.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1048576
	Max int32 `json:"max"`
	// Step is the positive increment from Min.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1048576
	// +optional
	Step *int32 `json:"step,omitempty"`
}

func (d *DynamicFlex) IsEnabled() bool {
	return d != nil && (d.Enabled == nil || *d.Enabled)
}

func (r FlexRange) Increment() int32 {
	if r.Step == nil {
		return 1
	}
	return *r.Step
}

func (d *DynamicFlex) ConfigurationLimit() int32 {
	if d.MaxConfigurations == nil {
		return DefaultMaxFlexConfigurations
	}
	return *d.MaxConfigurations
}

func (d *DynamicFlex) Baseline() BaselineOcpuUtilization {
	if d.BaselineOcpuUtilization == nil {
		return BASELINE_1_1
	}
	return *d.BaselineOcpuUtilization
}

// Validate also protects callers that construct NodeClasses without API admission.
func (d *DynamicFlex) Validate() error {
	if !d.IsEnabled() {
		return nil
	}
	for _, field := range []struct {
		name       string
		rangeValue FlexRange
		ceiling    int32
	}{{"ocpus", d.Ocpus, 4096}, {"memoryInGbs", d.MemoryInGbs, 1048576}} {
		r := field.rangeValue
		if r.Min < 1 || r.Max < r.Min || r.Max > field.ceiling || r.Increment() < 1 || r.Increment() > 1048576 {
			return fmt.Errorf("dynamicFlex.%s requires 1 <= min <= max <= %d and a positive step <= 1048576", field.name, field.ceiling)
		}
	}
	if d.ConfigurationLimit() < 1 || d.ConfigurationLimit() > MaxFlexConfigurations {
		return fmt.Errorf("dynamicFlex.maxConfigurations must be between 1 and %d", MaxFlexConfigurations)
	}
	if d.SelectionPolicy != "" && d.SelectionPolicy != LowestPriceThatFits {
		return fmt.Errorf("dynamicFlex.selectionPolicy must be %q", LowestPriceThatFits)
	}
	switch d.Baseline() {
	case BASELINE_1_1, BASELINE_1_2, BASELINE_1_8:
	default:
		return fmt.Errorf("unsupported dynamicFlex.baselineOcpuUtilization %q", d.Baseline())
	}
	return nil
}
