/*
** Karpenter Provider OCI
**
** Copyright (c) 2026 Oracle and/or its affiliates.
** Licensed under the Universal Permissive License v 1.0 as shown at https://oss.oracle.com/licenses/upl/
 */

package instancetype

import (
	"math"
	"testing"

	api "github.com/oracle/karpenter-provider-oci/pkg/apis/v1beta1"
	core "github.com/oracle/oci-go-sdk/v65/core"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
)

func dynamicShape() *core.Shape {
	return &core.Shape{
		Shape: lo.ToPtr("VM.Standard.E5.Flex"), IsFlexible: lo.ToPtr(true),
		OcpuOptions: &core.ShapeOcpuOptions{
			Min: lo.ToPtr(float32(1)), Max: lo.ToPtr(float32(126)), MaxPerNumaNode: lo.ToPtr(float32(94)),
		},
		MemoryOptions: &core.ShapeMemoryOptions{
			MinInGBs: lo.ToPtr(float32(1)), MaxInGBs: lo.ToPtr(float32(2098)),
			MinPerOcpuInGBs: lo.ToPtr(float32(1)), MaxPerOcpuInGBs: lo.ToPtr(float32(64)),
			MaxPerNumaNodeInGBs: lo.ToPtr(float32(1049)), DefaultPerOcpuInGBs: lo.ToPtr(float32(12)),
		},
		BaselineOcpuUtilizations: []core.ShapeBaselineOcpuUtilizationsEnum{
			core.ShapeBaselineOcpuUtilizations1, core.ShapeBaselineOcpuUtilizations2, core.ShapeBaselineOcpuUtilizations8,
		},
	}
}

func dynamicPolicy() *api.DynamicFlex {
	return &api.DynamicFlex{
		Ocpus:       api.FlexRange{Min: 1, Max: 4},
		MemoryInGbs: api.FlexRange{Min: 2, Max: 10, Step: lo.ToPtr(int32(4))},
	}
}

func dynamicCompatibility() *core.ImageShapeCompatibilitySummary {
	return &core.ImageShapeCompatibilitySummary{Shape: lo.ToPtr("VM.Standard.E5.Flex")}
}

func TestDynamicFlexExactGrid(t *testing.T) {
	configs, err := generateDynamicFlexConfigs(dynamicPolicy(), dynamicShape(), dynamicCompatibility())
	require.NoError(t, err)
	pairs := make([][2]float32, 0, len(configs))
	for _, cfg := range configs {
		pairs = append(pairs, [2]float32{*cfg.Ocpus, *cfg.MemoryInGbs})
	}
	require.Equal(t, [][2]float32{
		{1, 2}, {1, 6}, {1, 10}, {2, 2}, {2, 6}, {2, 10}, {3, 6}, {3, 10}, {4, 6}, {4, 10},
	}, pairs)
}

func TestDynamicFlexCapRejectsWithoutTruncating(t *testing.T) {
	p := dynamicPolicy()
	p.MaxConfigurations = lo.ToPtr(int32(10))
	configs, err := generateDynamicFlexConfigs(p, dynamicShape(), dynamicCompatibility())
	require.NoError(t, err)
	require.Len(t, configs, 10)
	p.MaxConfigurations = lo.ToPtr(int32(9))
	configs, err = generateDynamicFlexConfigs(p, dynamicShape(), dynamicCompatibility())
	require.ErrorContains(t, err, "VM.Standard.E5.Flex")
	require.ErrorContains(t, err, "maxConfigurations=9")
	require.ErrorContains(t, err, "coarser steps")
	require.Nil(t, configs, "an overflowing policy must not publish a partial catalog")
}

func TestDynamicFlexImageAndRatioConstraintsPreserveGridOrigin(t *testing.T) {
	p := dynamicPolicy()
	p.Ocpus = api.FlexRange{Min: 1, Max: 7, Step: lo.ToPtr(int32(2))}
	p.MemoryInGbs = api.FlexRange{Min: 1, Max: 14, Step: lo.ToPtr(int32(3))}
	s := dynamicShape()
	s.MemoryOptions.MaxPerOcpuInGBs = lo.ToPtr(float32(2))
	i := dynamicCompatibility()
	i.OcpuConstraints = &core.ImageOcpuConstraints{Min: lo.ToPtr(2), Max: lo.ToPtr(5)}
	i.MemoryConstraints = &core.ImageMemoryConstraints{MinInGBs: lo.ToPtr(3), MaxInGBs: lo.ToPtr(9)}
	configs, err := generateDynamicFlexConfigs(p, s, i)
	require.NoError(t, err)
	require.Len(t, configs, 2)
	require.Equal(t, float32(3), *configs[0].Ocpus)
	require.Equal(t, float32(4), *configs[0].MemoryInGbs)
	require.Equal(t, float32(5), *configs[1].Ocpus)
	require.Equal(t, float32(7), *configs[1].MemoryInGbs)
}

func TestDynamicFlexUnknownConstraintsFailClosed(t *testing.T) {
	for _, mutate := range []func(*core.Shape){
		func(s *core.Shape) { s.MemoryOptions = nil },
		func(s *core.Shape) { s.MemoryOptions.MaxPerOcpuInGBs = nil },
		func(s *core.Shape) { s.OcpuOptions.Min = nil },
		func(s *core.Shape) { s.Shape = lo.ToPtr("VM.DenseIO.E5.Flex") },
		func(s *core.Shape) { s.IsFlexible = lo.ToPtr(false) },
	} {
		s := dynamicShape()
		mutate(s)
		configs, err := generateDynamicFlexConfigs(dynamicPolicy(), s, dynamicCompatibility())
		require.NoError(t, err)
		require.Empty(t, configs)
	}
	configs, err := generateDynamicFlexConfigs(dynamicPolicy(), dynamicShape(), nil)
	require.Error(t, err)
	require.Empty(t, configs)
}

func TestDynamicFlexConstraintsAbovePolicyCannotOverflowGridArithmetic(t *testing.T) {
	for _, mutate := range []func(*core.Shape){
		func(s *core.Shape) {
			s.OcpuOptions.Min = lo.ToPtr(float32(math.MaxFloat32))
			s.OcpuOptions.Max = lo.ToPtr(float32(math.MaxFloat32))
		},
		func(s *core.Shape) {
			s.MemoryOptions.MinPerOcpuInGBs = lo.ToPtr(float32(math.MaxFloat32))
			s.MemoryOptions.MaxPerOcpuInGBs = lo.ToPtr(float32(math.MaxFloat32))
		},
	} {
		s := dynamicShape()
		mutate(s)
		configs, err := generateDynamicFlexConfigs(dynamicPolicy(), s, dynamicCompatibility())
		require.NoError(t, err)
		require.Empty(t, configs, "constraints above the policy cannot wrap around to valid grid points")
	}
}

func TestDynamicFlexBurstableAndExtendedLimits(t *testing.T) {
	p := dynamicPolicy()
	p.Ocpus = api.FlexRange{Min: 27, Max: 28}
	p.MemoryInGbs = api.FlexRange{Min: 1049, Max: 1050}
	configs, err := generateDynamicFlexConfigs(p, dynamicShape(), dynamicCompatibility())
	require.NoError(t, err)
	require.Len(t, configs, 3, "extended memory requires at least 28 OCPUs on E5")
	p.BaselineOcpuUtilization = lo.ToPtr(api.BASELINE_1_8)
	configs, err = generateDynamicFlexConfigs(p, dynamicShape(), dynamicCompatibility())
	require.NoError(t, err)
	require.Empty(t, configs, "burstable memory is capped at 768 GB")
	p.Ocpus = api.FlexRange{Min: 93, Max: 95}
	p.MemoryInGbs = api.FlexRange{Min: 768, Max: 769}
	configs, err = generateDynamicFlexConfigs(p, dynamicShape(), dynamicCompatibility())
	require.NoError(t, err)
	require.Len(t, configs, 2)
	i := dynamicCompatibility()
	i.OcpuConstraints = &core.ImageOcpuConstraints{Min: lo.ToPtr(12)}
	configs, err = generateDynamicFlexConfigs(p, dynamicShape(), i)
	require.NoError(t, err)
	require.Empty(t, configs, "baseline CPU must satisfy an image's minimum")
}

func BenchmarkDynamicFlexOverflow(b *testing.B) {
	p := dynamicPolicy()
	p.Ocpus.Max = 4096
	p.MemoryInGbs.Max = 1048576
	s, i := dynamicShape(), dynamicCompatibility()
	b.ResetTimer()
	for range b.N {
		_, _ = generateDynamicFlexConfigs(p, s, i)
	}
}

func BenchmarkDynamicFlexMaximumCatalog(b *testing.B) {
	p := dynamicPolicy()
	p.Ocpus = api.FlexRange{Min: 9, Max: 16}
	p.MemoryInGbs = api.FlexRange{Min: 16, Max: 520, Step: lo.ToPtr(int32(8))}
	p.MaxConfigurations = lo.ToPtr(api.MaxFlexConfigurations)
	s, i := dynamicShape(), dynamicCompatibility()
	configs, err := generateDynamicFlexConfigs(p, s, i)
	require.NoError(b, err)
	require.Len(b, configs, int(api.MaxFlexConfigurations))
	b.ResetTimer()
	for range b.N {
		_, _ = generateDynamicFlexConfigs(p, s, i)
	}
}
