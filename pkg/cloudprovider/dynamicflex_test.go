/*
** Karpenter Provider OCI
**
** Copyright (c) 2026 Oracle and/or its affiliates.
** Licensed under the Universal Permissive License v 1.0 as shown at https://oss.oracle.com/licenses/upl/
 */

package cloudprovider

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	api "github.com/oracle/karpenter-provider-oci/pkg/apis/v1beta1"
	"github.com/oracle/karpenter-provider-oci/pkg/providers/image"
	"github.com/oracle/karpenter-provider-oci/pkg/providers/instance"
	"github.com/oracle/karpenter-provider-oci/pkg/providers/instancetype"
	"github.com/oracle/karpenter-provider-oci/pkg/providers/kms"
	"github.com/oracle/karpenter-provider-oci/pkg/providers/network"
	"github.com/oracle/karpenter-provider-oci/pkg/providers/placement"
	core "github.com/oracle/oci-go-sdk/v65/core"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	karpenterv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/events"
	"sigs.k8s.io/karpenter/pkg/scheduling"
)

type capturedFlexEvents struct{ events []events.Event }

const dynamicFlexTestShape = "VM.Standard.E5.Flex"

func (r *capturedFlexEvents) Publish(events ...events.Event) { r.events = append(r.events, events...) }

var _ = Describe("Dynamic Flex launch and readback", func() {
	It("uses the cheapest fitting concrete configuration and the image validated before scheduling", func() {
		nc := testReadyNodeClass(uniqueName("dynamic"))
		claim := testNodeClaim(nc.Name)
		claim.Spec.Resources.Requests = v1.ResourceList{
			v1.ResourceCPU: resource.MustParse("3800m"), v1.ResourceMemory: resource.MustParse("4Gi"),
		}
		shape := dynamicFlexTestShape
		validatedImage := testImageResolveResult("validated-image")
		var types []*instancetype.OciInstanceType
		for _, cpu := range []float32{4, 2, 1} {
			name := instancetype.FlexInstanceTypeName(shape, cpu, 8, api.BASELINE_1_1)
			it := testFlexConfigInstanceType(shape, name, cpu, 8, float64(cpu)*0.05+0.08)
			it.Capacity[v1.ResourceCPU] = resource.MustParse(fmt.Sprint(cpu * 2))
			it.Overhead.KubeReserved = v1.ResourceList{v1.ResourceCPU: resource.MustParse("500m")}
			it.BaselineOcpuUtilization = lo.ToPtr(api.BASELINE_1_1)
			it.DynamicFlexImage = validatedImage
			it.Requirements.Add(scheduling.NewRequirement(api.OciInstanceOcpus, v1.NodeSelectorOpIn, fmt.Sprint(cpu)))
			it.Requirements.Add(scheduling.NewRequirement(api.OciInstanceMemory, v1.NodeSelectorOpIn, "8"))
			types = append(types, it)
		}
		launches := 0
		cp := newUnitTestCloudProvider(unitTestCloudProviderOptions{
			kubeClient: newFakeKubeClient(nc), instanceTypes: types,
			imageProvider: &FakeImageProvider{ResolveImageForShapeFn: func(context.Context,
				*api.ImageConfig, string) (*image.ImageResolveResult, error) {
				Fail("dynamic launch must not reselect an image after validating its constraints")
				return nil, nil
			}},
			instanceProvider: &FakeInstanceProvider{LaunchInstanceFn: func(_ context.Context, _ *karpenterv1.NodeClaim,
				_ *api.OCINodeClass, it *instancetype.OciInstanceType, img *image.ImageResolveResult,
				_ *network.NetworkResolveResult, _ *kms.KmsKeyResolveResult,
				_ *placement.Proposal) (*instance.InstanceInfo, error) {
				launches++
				Expect(it.Name).To(Equal(shape+".4o.8g.1_1b"), "2 OCPUs cannot fit after kubelet overhead")
				Expect(img).To(BeIdenticalTo(validatedImage))
				actual := testInstanceWithShape(shape, "pool-a")
				actual.ShapeConfig = &core.InstanceShapeConfig{Ocpus: it.Ocpu, MemoryInGBs: it.MemoryInGbs}
				return &instance.InstanceInfo{Instance: actual}, nil
			}},
		})
		recorder := &capturedFlexEvents{}
		cp.recorder = recorder
		created, err := cp.Create(ctx, claim)
		Expect(err).NotTo(HaveOccurred())
		Expect(launches).To(Equal(1))
		Expect(created.Labels[api.OciInstanceOcpus]).To(Equal("4"))
		Expect(created.Labels[api.OciInstanceMemory]).To(Equal("8"))
		Expect(created.Status.Allocatable.Cpu().MilliValue()).To(Equal(int64(7500)))
		Expect(recorder.events).To(HaveLen(1))
		Expect(recorder.events[0].Reason).To(Equal("DynamicFlexSelected"))
		Expect(recorder.events[0].InvolvedObject).To(BeIdenticalTo(claim))
	})

	It("preserves actual Flex identity across readback and removal from the catalog", func() {
		nc := testReadyNodeClass(uniqueName("readback"))
		pool := testNodePool("pool-a", nc.Name)
		shape := dynamicFlexTestShape
		name := shape + ".2o.8g.1_1b"
		it := testFlexConfigInstanceType(shape, name, 2, 8, 0.18)
		it.BaselineOcpuUtilization = lo.ToPtr(api.BASELINE_1_1)
		actual := testInstanceWithShape(shape, pool.Name)
		actual.ShapeConfig = &core.InstanceShapeConfig{Ocpus: it.Ocpu, MemoryInGBs: it.MemoryInGbs}
		provider := NewFakeInstanceTypeProvider([]*instancetype.OciInstanceType{it})
		cp := newUnitTestCloudProvider(unitTestCloudProviderOptions{
			kubeClient: newFakeKubeClient(nc, pool), instanceTypeProvider: provider,
			instanceProvider: &FakeInstanceProvider{GetInstanceFn: func(context.Context,
				string) (*instance.InstanceInfo, error) {
				return &instance.InstanceInfo{Instance: actual}, nil
			}},
		})
		got, err := cp.Get(ctx, *actual.Id)
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Labels[v1.LabelInstanceTypeStable]).To(Equal(name))
		Expect(got.Status.Capacity).To(Equal(it.Capacity))
		provider.InstancesTypes = nil
		got, err = cp.Get(ctx, *actual.Id)
		Expect(err).NotTo(HaveOccurred())
		Expect(got.Labels[v1.LabelInstanceTypeStable]).To(Equal(name))
		Expect(got.Labels[api.OciInstanceOcpus]).To(Equal("2"))
		Expect(got.Status.Capacity).To(BeEmpty(), "do not substitute another size for a removed offering")
		claim := testNodeClaim(nc.Name)
		claim.Labels[v1.LabelInstanceTypeStable] = name
		reason, err := cp.IsDrifted(ctx, claim)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(reason)).To(Equal("ShapeNotFound"))
	})
})
