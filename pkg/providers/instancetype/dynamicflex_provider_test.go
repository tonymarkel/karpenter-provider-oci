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
	"testing"
	"time"

	"github.com/coreos/go-semver/semver"
	api "github.com/oracle/karpenter-provider-oci/pkg/apis/v1beta1"
	"github.com/oracle/karpenter-provider-oci/pkg/cache"
	"github.com/oracle/karpenter-provider-oci/pkg/fakes"
	"github.com/oracle/karpenter-provider-oci/pkg/providers/capacityreservation"
	"github.com/oracle/karpenter-provider-oci/pkg/providers/image"
	core "github.com/oracle/oci-go-sdk/v65/core"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	corev1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

type dynamicImages struct {
	image.Provider
	calls  int
	err    error
	before func()
}

func (d *dynamicImages) ResolveImageForShape(_ context.Context,
	_ *api.ImageConfig, shape string) (*image.ImageResolveResult, error) {
	d.calls++
	if d.before != nil {
		d.before()
	}
	if d.err != nil {
		return nil, d.err
	}
	return &image.ImageResolveResult{
		Images:             []*core.Image{{Id: lo.ToPtr("image-validated-before-scheduling")}},
		ShapeCompatibility: &core.ImageShapeCompatibilitySummary{Shape: lo.ToPtr(shape)},
	}, nil
}

func dynamicProviderFixture() (*DefaultProvider, *api.OCINodeClass, *dynamicImages) {
	s := dynamicShape()
	s.BillingType = core.ShapeBillingTypePaid
	s.MaxVnicAttachments = lo.ToPtr(32)
	s.MaxVnicAttachmentOptions = &core.ShapeMaxVnicAttachmentOptions{
		Min: lo.ToPtr(2), Max: lo.ToPtr(float32(32)), DefaultPerOcpu: lo.ToPtr(float32(1)),
	}
	images := &dynamicImages{}
	p := &DefaultProvider{
		region: "us-ashburn-1", k8sVersion: semver.New("1.36.4"), imageProvider: images,
		shapeAdMap: map[string]*ShapeAndAd{
			*s.Shape: {Shape: s, Ads: []string{"tenancy:IAD-AD-1", "tenancy:IAD-AD-2"}},
		},
		shapeToPrice:         map[string]*ShapePriceInfo{"VM.STANDARD.E5.FLEX": {OcpuUnitPrice: 0.05, MemoryUnitPrice: 0.01}},
		preemptibleShapes:    PreemptibleShapes{"VM.STANDARD.E5.FLEX": "VM.Standard.E5.Flex"},
		unavailableOfferings: cache.NewUnavailableOfferings(time.Minute),
	}
	nc := fakes.CreateBasicOciNodeClass()
	nc.Spec.DynamicFlex = dynamicPolicy()
	nc.Spec.KubeletConfig = &api.KubeletConfiguration{
		KubeReserved:   map[string]string{"cpu": "100m", "memory": "100Mi"},
		SystemReserved: map[string]string{"cpu": "100m", "memory": "100Mi"},
	}
	return p, &nc, images
}

func TestDynamicFlexProviderConcreteResourcesAndPrices(t *testing.T) {
	p, nc, images := dynamicProviderFixture()
	images.before = func() {
		require.True(t, p.lock.TryLock(), "image lookups must not hold the catalog lock")
		p.lock.Unlock()
	}
	types, err := p.ListInstanceTypes(context.Background(), nc, nil)
	require.NoError(t, err)
	require.Len(t, types, 10)
	require.Equal(t, 1, images.calls, "resolve once per family, not once per configuration")
	for _, it := range types {
		require.NotNil(t, it.DynamicFlexImage)
		require.Equal(t, "image-validated-before-scheduling", *it.DynamicFlexImage.Images[0].Id)
		require.Equal(t, int64(*it.Ocpu*2), it.Capacity.Cpu().Value())
		allocatable := it.Allocatable()
		require.Less(t, allocatable.Cpu().MilliValue(), it.Capacity.Cpu().MilliValue())
		require.Less(t, allocatable.Memory().Value(), int64(*it.MemoryInGbs)*1024*1024*1024)
		require.InDelta(t, float64(*it.Ocpu)*0.05+float64(*it.MemoryInGbs)*0.01, it.Offerings[0].Price, 0.00001)
		require.True(t, it.Offerings[0].Available)
		require.Equal(t, it.Name, it.Requirements.Get(v1.LabelInstanceTypeStable).Any())
		require.NotEmpty(t, it.Requirements.Get(api.OciInstanceOcpus).Any())
		require.NotEmpty(t, it.Requirements.Get(api.OciInstanceMemory).Any())
		require.Same(t, it.DynamicFlexImage, it.CopyAndUpdateOfferings(it.Offerings).DynamicFlexImage)
	}
}

func TestDynamicFlexProviderModeAndVersionGuards(t *testing.T) {
	for _, version := range []*semver.Version{nil, semver.New("1.30.9")} {
		p, nc, _ := dynamicProviderFixture()
		p.k8sVersion = version
		_, err := p.ListInstanceTypes(context.Background(), nc, nil)
		require.ErrorContains(t, err, ">= 1.31")
	}
	p, nc, _ := dynamicProviderFixture()
	p.GlobalShapeConfigs = []*api.ShapeConfig{{Ocpus: lo.ToPtr(float32(8)), MemoryInGbs: lo.ToPtr(float32(32))}}
	types, err := p.ListInstanceTypes(context.Background(), nc, nil)
	require.NoError(t, err)
	require.Len(t, types, 10, "enabled policy overrides global defaults")
	nc.Spec.ShapeConfigs = p.GlobalShapeConfigs
	_, err = p.ListInstanceTypes(context.Background(), nc, nil)
	require.ErrorContains(t, err, "mutually exclusive")
	nc.Spec.DynamicFlex.Enabled = lo.ToPtr(false)
	types, err = p.ListInstanceTypes(context.Background(), nc, nil)
	require.NoError(t, err)
	require.Len(t, types, 1)
	require.Equal(t, "VM.Standard.E5.Flex.8o.32g.1_1b", types[0].Name)
}

func TestDynamicFlexProviderNoPartialCatalogOnFailure(t *testing.T) {
	p, nc, images := dynamicProviderFixture()
	nc.Spec.DynamicFlex.MaxConfigurations = lo.ToPtr(int32(9))
	types, err := p.ListInstanceTypes(context.Background(), nc, nil)
	require.ErrorContains(t, err, "coarser steps")
	require.Nil(t, types)
	nc.Spec.DynamicFlex.MaxConfigurations = nil
	images.err = errors.New("OCI image lookup unavailable")
	types, err = p.ListInstanceTypes(context.Background(), nc, nil)
	require.ErrorContains(t, err, "OCI image lookup unavailable")
	require.Nil(t, types)
	images.err = image.ErrNoCompatibleImage
	types, err = p.ListInstanceTypes(context.Background(), nc, nil)
	require.ErrorContains(t, err, "no configurations satisfy")
	require.Nil(t, types)
}

func TestDynamicFlexProviderAvailabilityIsPerConfigurationAndCapacityType(t *testing.T) {
	p, nc, _ := dynamicProviderFixture()
	p.unavailableOfferings.MarkUnavailable(context.Background(), "VM.Standard.E5.Flex", lo.ToPtr(float32(1)),
		lo.ToPtr(float32(2)), "IAD-AD-1", corev1.CapacityTypeSpot, "")
	taints := []v1.Taint{{Key: PreemptibleTaintKey, Effect: v1.TaintEffectNoSchedule}}
	types, err := p.ListInstanceTypes(context.Background(), nc, taints)
	require.NoError(t, err)
	require.Len(t, types, 10)
	for _, it := range types {
		for _, offering := range it.Offerings {
			unavailable := *it.Ocpu == 1 && *it.MemoryInGbs == 2 &&
				offering.Zone() == "IAD-AD-1" && offering.CapacityType() == corev1.CapacityTypeSpot
			require.Equal(t, !unavailable, offering.Available)
			if offering.CapacityType() == corev1.CapacityTypeSpot {
				require.InDelta(t, (float64(*it.Ocpu)*0.05+float64(*it.MemoryInGbs)*0.01)*0.5, offering.Price, 0.00001)
			}
		}
	}
}

func TestDynamicFlexProviderMatchesExactReservations(t *testing.T) {
	p, nc, _ := dynamicProviderFixture()
	nc.Spec.CapacityReservationConfigs = []*api.CapacityReservationConfig{
		{CapacityReservationId: lo.ToPtr("ocid1.reservation.test")},
	}
	p.capacityReservationProvider = &fakeCapacityReservationProvider{results: []capacityreservation.ResolveResult{{
		Ocid: "ocid1.reservation.test", Ad: "tenancy:IAD-AD-1",
		ShapeAvailabilities: []capacityreservation.ShapeAvailability{{
			Shape: "VM.Standard.E5.Flex", Ad: "tenancy:IAD-AD-1", Total: 3, Used: 1,
			Ocpu: lo.ToPtr(float32(2)), MemoryInGbs: lo.ToPtr(float32(6)),
		}},
	}}}
	types, err := p.ListInstanceTypes(context.Background(), nc, nil)
	require.NoError(t, err)
	reservations := 0
	for _, it := range types {
		for _, offering := range it.Offerings {
			if offering.CapacityType() == corev1.CapacityTypeReserved {
				reservations++
				require.Equal(t, "VM.Standard.E5.Flex.2o.6g.1_1b", it.Name)
				require.Equal(t, 2, offering.ReservationCapacity)
			}
		}
	}
	require.Equal(t, 1, reservations)
}

func TestDynamicFlexProviderVNICLimitsAndDrift(t *testing.T) {
	p, nc, _ := dynamicProviderFixture()
	nc.Spec.NetworkConfig.SecondaryVnicConfigs = []*api.SecondaryVnicConfig{{}, {}, {}}
	types, err := p.ListInstanceTypes(context.Background(), nc, nil)
	require.NoError(t, err)
	for _, it := range types {
		require.Equal(t, *it.Ocpu >= 3, len(it.Offerings.Available()) > 0)
		actual := &core.Instance{Shape: lo.ToPtr(it.Shape), ShapeConfig: &core.InstanceShapeConfig{
			Ocpus: it.Ocpu, MemoryInGBs: it.MemoryInGbs,
			BaselineOcpuUtilization: core.InstanceShapeConfigBaselineOcpuUtilization1,
		}}
		require.True(t, it.MatchesInstance(actual))
		reason, err := IsInstanceDriftedFromInstanceType(actual, it)
		require.NoError(t, err)
		require.Empty(t, reason)
		actual.ShapeConfig.MemoryInGBs = lo.ToPtr(*it.MemoryInGbs + 1)
		require.False(t, it.MatchesInstance(actual))
		reason, err = IsInstanceDriftedFromInstanceType(actual, it)
		require.NoError(t, err)
		require.EqualValues(t, "memoryInGbsMismatch", reason)
	}
}
