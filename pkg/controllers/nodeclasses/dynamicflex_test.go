/*
** Karpenter Provider OCI
**
** Copyright (c) 2026 Oracle and/or its affiliates.
** Licensed under the Universal Permissive License v 1.0 as shown at https://oss.oracle.com/licenses/upl/
 */

package nodeclasses

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/oracle/karpenter-provider-oci/pkg/apis/v1beta1"
	"github.com/oracle/karpenter-provider-oci/pkg/fakes"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ = Describe("DynamicFlex API", func() {
	create := func(policy map[string]any, presets bool) (*unstructured.Unstructured, error) {
		nc := fakes.CreateBasicOciNodeClass()
		raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&nc)
		Expect(err).NotTo(HaveOccurred())
		obj := &unstructured.Unstructured{Object: raw}
		Expect(unstructured.SetNestedMap(obj.Object, policy, "spec", "dynamicFlex")).To(Succeed())
		if presets {
			Expect(unstructured.SetNestedSlice(obj.Object, []any{map[string]any{"ocpus": int64(2)}},
				"spec", "shapeConfigs")).To(Succeed())
		}
		err = k8sClient.Create(ctx, obj)
		if err == nil {
			DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed()) })
		}
		return obj, err
	}
	policy := func() map[string]any {
		return map[string]any{
			"ocpus":       map[string]any{"min": int64(1), "max": int64(4)},
			"memoryInGbs": map[string]any{"min": int64(8), "max": int64(32)},
		}
	}
	It("persists and defaults the policy without pruning its fields", func() {
		obj, err := create(policy(), false)
		Expect(err).NotTo(HaveOccurred())
		stored := &v1beta1.OCINodeClass{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), stored)).To(Succeed())
		p := stored.Spec.DynamicFlex
		Expect(p).NotTo(BeNil())
		Expect(*p.Enabled).To(BeTrue())
		Expect(*p.Ocpus.Step).To(Equal(int32(1)))
		Expect(*p.MemoryInGbs.Step).To(Equal(int32(1)))
		Expect(*p.MaxConfigurations).To(Equal(int32(128)))
		Expect(*p.BaselineOcpuUtilization).To(Equal(v1beta1.BASELINE_1_1))
		Expect(p.SelectionPolicy).To(Equal(v1beta1.LowestPriceThatFits))
		copy := stored.DeepCopy()
		*copy.Spec.DynamicFlex.Ocpus.Step = 2
		Expect(*p.Ocpus.Step).To(Equal(int32(1)))
	})
	It("rejects enabled dynamic mode with explicit presets", func() {
		_, err := create(policy(), true)
		Expect(apierrors.IsInvalid(err)).To(BeTrue())
		p := policy()
		p["enabled"] = false
		_, err = create(p, true)
		Expect(err).NotTo(HaveOccurred())
	})
	DescribeTable("rejects invalid policies", func(mutate func(map[string]any)) {
		p := policy()
		mutate(p)
		_, err := create(p, false)
		Expect(apierrors.IsInvalid(err)).To(BeTrue())
	},
		Entry("zero step", func(p map[string]any) { p["ocpus"].(map[string]any)["step"] = int64(0) }),
		Entry("reversed bounds", func(p map[string]any) { p["ocpus"].(map[string]any)["min"] = int64(5) }),
		Entry("fractional CPU", func(p map[string]any) { p["ocpus"].(map[string]any)["min"] = 1.5 }),
		Entry("zero memory", func(p map[string]any) { p["memoryInGbs"].(map[string]any)["min"] = int64(0) }),
		Entry("missing range", func(p map[string]any) { delete(p, "memoryInGbs") }),
		Entry("CPU ceiling", func(p map[string]any) { p["ocpus"].(map[string]any)["max"] = int64(4097) }),
		Entry("zero cap", func(p map[string]any) { p["maxConfigurations"] = int64(0) }),
		Entry("excessive cap", func(p map[string]any) { p["maxConfigurations"] = int64(513) }),
		Entry("unknown baseline", func(p map[string]any) { p["baselineOcpuUtilization"] = "BASELINE_1_4" }),
		Entry("unknown selection", func(p map[string]any) { p["selectionPolicy"] = "largest" }),
	)
})
