/*
** Karpenter Provider OCI
**
** Copyright (c) 2026 Oracle and/or its affiliates.
** Licensed under the Universal Permissive License v 1.0 as shown at https://oss.oracle.com/licenses/upl/
 */

package instancetype

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	api "github.com/oracle/karpenter-provider-oci/pkg/apis/v1beta1"
	"github.com/oracle/karpenter-provider-oci/pkg/providers/image"
	core "github.com/oracle/oci-go-sdk/v65/core"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
)

// OCI does not include burstable ceilings or the extended-memory minimum OCPUs
// in ListShapes. Keep these exceptions explicit and fail closed for unknown ones.
// https://docs.oracle.com/en-us/iaas/Content/Compute/References/burstable-instances.htm
// https://docs.oracle.com/en-us/iaas/Content/Compute/References/extended-memory-vm-instances.htm
type flexFamilyLimit struct {
	normalCPU, normalMemory, burstCPU, burstMemory, extendedMinCPU float64
}

var flexFamilyLimits = map[string]flexFamilyLimit{
	"VM.Standard3.Flex":   {32, 512, 32, 384, 14},
	"VM.Standard.E3.Flex": {64, 1024, 64, 768, 28},
	"VM.Standard.E4.Flex": {64, 1024, 64, 768, 28},
	"VM.Standard.E5.Flex": {94, 1049, 94, 768, 28},
}

func supportsDynamicFlex(shape *core.Shape) bool {
	return shape != nil && shape.Shape != nil && lo.FromPtr(shape.IsFlexible) &&
		(strings.HasPrefix(*shape.Shape, "VM.Standard") || strings.HasPrefix(*shape.Shape, "VM.Optimized")) &&
		!IsGpuShape(*shape) && lo.FromPtr(shape.LocalDisksTotalSizeInGBs) == 0
}

func finitePositive(value *float32) bool {
	return value != nil && *value > 0 && !math.IsNaN(float64(*value)) && !math.IsInf(float64(*value), 0)
}

func hasValidFlexRanges(shape *core.Shape) bool {
	c, m := shape.OcpuOptions, shape.MemoryOptions
	return c != nil && m != nil && finitePositive(c.Min) && finitePositive(c.Max) &&
		finitePositive(m.MinInGBs) && finitePositive(m.MaxInGBs) &&
		finitePositive(m.MinPerOcpuInGBs) && finitePositive(m.MaxPerOcpuInGBs) &&
		*c.Min <= *c.Max && *m.MinInGBs <= *m.MaxInGBs && *m.MinPerOcpuInGBs <= *m.MaxPerOcpuInGBs
}

func normalFlexLimits(shape *core.Shape) (float64, float64) {
	limits := flexFamilyLimits[lo.FromPtr(shape.Shape)]
	cpu, memory := limits.normalCPU, limits.normalMemory
	if finitePositive(shape.OcpuOptions.MaxPerNumaNode) {
		cpu = float64(*shape.OcpuOptions.MaxPerNumaNode)
	}
	if finitePositive(shape.MemoryOptions.MaxPerNumaNodeInGBs) {
		memory = float64(*shape.MemoryOptions.MaxPerNumaNodeInGBs)
	}
	if cpu == 0 {
		cpu = float64(*shape.OcpuOptions.Max)
	}
	if memory == 0 {
		memory = float64(*shape.MemoryOptions.MaxInGBs)
	}
	return cpu, memory
}

func isExtendedFlex(shape *core.Shape, cpu, memory float32) bool {
	normalCPU, normalMemory := normalFlexLimits(shape)
	return float64(cpu) > normalCPU || float64(memory) > normalMemory
}

// gridStart retains the user's grid origin when OCI/image constraints raise the minimum.
func gridStart(r api.FlexRange, minimum float64) int64 {
	origin, step := int64(r.Min), int64(r.Increment())
	minimumInteger := int64(math.Ceil(minimum))
	if minimumInteger <= origin {
		return origin
	}
	return origin + ((minimumInteger-origin+step-1)/step)*step
}

func generateDynamicFlexConfigs(policy *api.DynamicFlex, shape *core.Shape,
	compatibility *core.ImageShapeCompatibilitySummary) ([]*api.ShapeConfig, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	if !policy.IsEnabled() || !supportsDynamicFlex(shape) {
		return nil, nil
	}
	if compatibility == nil || lo.FromPtr(compatibility.Shape) != *shape.Shape {
		return nil, fmt.Errorf("dynamicFlex requires image compatibility constraints for %s", *shape.Shape)
	}
	c, m := shape.OcpuOptions, shape.MemoryOptions
	if !hasValidFlexRanges(shape) {
		return nil, nil
	}
	baseline := policy.Baseline()
	minCPU, maxCPU := float64(*c.Min), math.Min(float64(policy.Ocpus.Max), float64(*c.Max))
	minMemory, maxMemory := float64(*m.MinInGBs), math.Min(float64(policy.MemoryInGbs.Max), float64(*m.MaxInGBs))
	factor := float64(1)
	limits := flexFamilyLimits[*shape.Shape]
	if baseline != api.BASELINE_1_1 {
		if limits.burstCPU == 0 ||
			!lo.Contains(shape.BaselineOcpuUtilizations, core.ShapeBaselineOcpuUtilizationsEnum(baseline)) {
			return nil, nil
		}
		maxCPU = math.Min(maxCPU, limits.burstCPU)
		maxMemory = math.Min(maxMemory, limits.burstMemory)
		if baseline == api.BASELINE_1_2 {
			factor = 2
		} else {
			factor = 8
		}
	}
	if imageCPU := compatibility.OcpuConstraints; imageCPU != nil {
		if imageCPU.Min != nil {
			minCPU = math.Max(minCPU, float64(*imageCPU.Min)*factor)
		}
		if imageCPU.Max != nil {
			maxCPU = math.Min(maxCPU, float64(*imageCPU.Max))
		}
	}
	if imageMemory := compatibility.MemoryConstraints; imageMemory != nil {
		if imageMemory.MinInGBs != nil {
			minMemory = math.Max(minMemory, float64(*imageMemory.MinInGBs))
		}
		if imageMemory.MaxInGBs != nil {
			maxMemory = math.Min(maxMemory, float64(*imageMemory.MaxInGBs))
		}
	}
	normalCPU, normalMemory := normalFlexLimits(shape)
	if limits.extendedMinCPU == 0 {
		// Unknown extended-memory families may have additional constraints not in ListShapes.
		maxCPU, maxMemory = math.Min(maxCPU, normalCPU), math.Min(maxMemory, normalMemory)
	}
	if minCPU > maxCPU || minMemory > maxMemory {
		return nil, nil
	}
	var configs []*api.ShapeConfig
	for cpu := gridStart(policy.Ocpus, minCPU); cpu <= int64(math.Floor(maxCPU)); cpu += int64(policy.Ocpus.Increment()) {
		low := math.Max(minMemory, float64(cpu)*float64(*m.MinPerOcpuInGBs))
		high := math.Min(maxMemory, float64(cpu)*float64(*m.MaxPerOcpuInGBs))
		if float64(cpu) < limits.extendedMinCPU {
			high = math.Min(high, normalMemory)
		}
		if low > high {
			continue
		}
		first, last := gridStart(policy.MemoryInGbs, low), int64(math.Floor(high))
		if first > last {
			continue
		}
		count := (last-first)/int64(policy.MemoryInGbs.Increment()) + 1
		if int64(len(configs))+count > int64(policy.ConfigurationLimit()) {
			return nil, fmt.Errorf("dynamicFlex catalog for %s exceeds maxConfigurations=%d; "+
				"use coarser steps or narrower bounds",
				*shape.Shape, policy.ConfigurationLimit())
		}
		for memory := first; memory <= last; memory += int64(policy.MemoryInGbs.Increment()) {
			configs = append(configs, &api.ShapeConfig{
				Ocpus: lo.ToPtr(float32(cpu)), MemoryInGbs: lo.ToPtr(float32(memory)),
				BaselineOcpuUtilization: lo.ToPtr(baseline),
			})
		}
	}
	return configs, nil
}

func FlexInstanceTypeName(shape string, cpu, memory float32, baseline api.BaselineOcpuUtilization) string {
	suffix := "1_1b"
	if baseline == api.BASELINE_1_2 {
		suffix = "1_2b"
	} else if baseline == api.BASELINE_1_8 {
		suffix = "1_8b"
	}
	return fmt.Sprintf("%s.%.0fo.%.0fg.%s", shape, cpu, memory, suffix)
}

// listDynamicFlexTypes resolves images outside the provider lock, then decorates
// the complete validated catalog under that lock. An overflow publishes no catalog.
func (p *DefaultProvider) listDynamicFlexTypes(ctx context.Context, nodeClass *api.OCINodeClass,
	taints []v1.Taint) ([]*OciInstanceType, error) {
	policy := nodeClass.Spec.DynamicFlex
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	if len(nodeClass.Spec.ShapeConfigs) != 0 {
		return nil, fmt.Errorf("enabled dynamicFlex and shapeConfigs are mutually exclusive")
	}
	if policy.Baseline() != api.BASELINE_1_1 {
		if options := nodeClass.Spec.LaunchOptions; options != nil && options.NetworkType != nil &&
			*options.NetworkType != api.NetworkTypeParavirtualized {
			return nil, fmt.Errorf("burstable dynamicFlex requires PARAVIRTUALIZED networking")
		}
		if len(nodeClass.Spec.CapacityReservationConfigs) > 0 {
			return nil, fmt.Errorf("burstable dynamicFlex does not support capacity reservations")
		}
	}
	p.lock.RLock()
	version := p.k8sVersion
	shapes := lo.Filter(lo.Values(p.shapeAdMap), func(sa *ShapeAndAd, _ int) bool {
		return sa != nil && supportsDynamicFlex(sa.Shape)
	})
	p.lock.RUnlock()
	if version == nil || version.Major < 1 || (version.Major == 1 && version.Minor < minK8sMinorForMultiFlex) {
		return nil, fmt.Errorf("dynamicFlex requires a known Kubernetes version >= 1.31")
	}
	if p.imageProvider == nil || nodeClass.Spec.VolumeConfig == nil ||
		nodeClass.Spec.VolumeConfig.BootVolumeConfig == nil {
		return nil, fmt.Errorf("dynamicFlex requires an image provider and boot volume image configuration")
	}
	sort.Slice(shapes, func(i, j int) bool {
		return lo.FromPtr(shapes[i].Shape.Shape) < lo.FromPtr(shapes[j].Shape.Shape)
	})
	type preparedShape struct {
		shape *ShapeAndAd
		types []*OciInstanceType
	}
	prepared := make([]preparedShape, 0, len(shapes))
	imageConfig := nodeClass.Spec.VolumeConfig.BootVolumeConfig.ImageConfig
	for _, sa := range shapes {
		if !supportsDynamicFlex(sa.Shape) {
			continue
		}
		resolved, err := p.imageProvider.ResolveImageForShape(ctx, imageConfig, *sa.Shape.Shape)
		if errors.Is(err, image.ErrNoCompatibleImage) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("dynamicFlex image constraints for %s: %w", *sa.Shape.Shape, err)
		}
		if resolved == nil || len(resolved.Images) != 1 || resolved.Images[0].Id == nil {
			return nil, fmt.Errorf("dynamicFlex image resolution for %s returned no concrete image", *sa.Shape.Shape)
		}
		configs, err := generateDynamicFlexConfigs(policy, sa.Shape, resolved.ShapeCompatibility)
		if err != nil {
			return nil, err
		}
		var candidates []*OciInstanceType
		for _, cfg := range configs {
			candidates = append(candidates, &OciInstanceType{
				InstanceType: cloudprovider.InstanceType{
					Name: FlexInstanceTypeName(*sa.Shape.Shape, *cfg.Ocpus, *cfg.MemoryInGbs, policy.Baseline()),
				},
				Shape: *sa.Shape.Shape, Ocpu: cfg.Ocpus, MemoryInGbs: cfg.MemoryInGbs,
				BaselineOcpuUtilization: cfg.BaselineOcpuUtilization, SupportShapeConfig: true,
				DynamicFlexImage: resolved, ExtendedMemory: isExtendedFlex(sa.Shape, *cfg.Ocpus, *cfg.MemoryInGbs),
			})
		}
		prepared = append(prepared, preparedShape{sa, candidates})
	}
	p.lock.RLock()
	defer p.lock.RUnlock()
	var result []*OciInstanceType
	for _, entry := range prepared {
		types, err := p.decorateInstanceTypes(ctx, entry.shape, nodeClass, taints, entry.types)
		if err != nil {
			return nil, err
		}
		result = append(result, types...)
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("dynamicFlex: no configurations satisfy the policy, OCI shape limits, "+
			"and image constraints for NodeClass %s", nodeClass.Name)
	}
	return result, nil
}
