/*
** Karpenter Provider OCI
**
** Copyright (c) 2026 Oracle and/or its affiliates.
** Licensed under the Universal Permissive License v 1.0 as shown at https://oss.oracle.com/licenses/upl/
 */

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	api "github.com/oracle/karpenter-provider-oci/pkg/apis/v1beta1"
	"github.com/oracle/karpenter-provider-oci/pkg/providers/instancetype"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	karpenterv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
)

// TestDynamicFlexE2E is opt-in and uses a supplied, ready template NodeClass. It
// creates only uniquely named test objects, and never changes the template.
func TestDynamicFlexE2E(t *testing.T) {
	if os.Getenv("DYNAMIC_FLEX_E2E") != "true" {
		t.Skip("set DYNAMIC_FLEX_E2E=true on an isolated OKE test cluster")
	}
	templateName := os.Getenv("DYNAMIC_FLEX_TEMPLATE_NODECLASS")
	require.NotEmpty(t, templateName)
	config, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
	require.NoError(t, err)
	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))
	require.NoError(t, api.AddToScheme(scheme))
	gv := schema.GroupVersion{Group: "karpenter.sh", Version: "v1"}
	metav1.AddToGroupVersion(scheme, gv)
	scheme.AddKnownTypes(gv, &karpenterv1.NodePool{}, &karpenterv1.NodePoolList{},
		&karpenterv1.NodeClaim{}, &karpenterv1.NodeClaimList{})
	kube, err := client.New(config, client.Options{Scheme: scheme})
	require.NoError(t, err)
	ctx := context.Background()
	template := &api.OCINodeClass{}
	require.NoError(t, kube.Get(ctx, client.ObjectKey{Name: templateName}, template))
	name := fmt.Sprintf("dynamic-flex-%d", time.Now().UnixNano())
	shape := os.Getenv("DYNAMIC_FLEX_SHAPE")
	if shape == "" {
		shape = "VM.Standard.E5.Flex"
	}
	nc := &api.OCINodeClass{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: *template.Spec.DeepCopy()}
	nc.Spec.ShapeConfigs = nil
	nc.Spec.DynamicFlex = &api.DynamicFlex{
		Ocpus:             api.FlexRange{Min: 1, Max: 4},
		MemoryInGbs:       api.FlexRange{Min: 4, Max: 32, Step: lo.ToPtr(int32(4))},
		MaxConfigurations: lo.ToPtr(int32(64)),
	}
	nc.Spec.KubeletConfig = &api.KubeletConfiguration{
		MaxPods:        lo.ToPtr(int32(20)),
		KubeReserved:   map[string]string{"cpu": "100m", "memory": "100Mi"},
		SystemReserved: map[string]string{"cpu": "100m", "memory": "100Mi"},
		EvictionHard:   map[string]string{"memory.available": "100Mi"},
	}
	delay := 5 * time.Second
	pool := &karpenterv1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: karpenterv1.NodePoolSpec{
		Template: karpenterv1.NodeClaimTemplate{Spec: karpenterv1.NodeClaimTemplateSpec{
			NodeClassRef: &karpenterv1.NodeClassReference{Group: api.Group, Kind: "OCINodeClass", Name: name},
			Requirements: []karpenterv1.NodeSelectorRequirementWithMinValues{
				{Key: api.OciInstanceShape, Operator: v1.NodeSelectorOpIn, Values: []string{shape}},
				{Key: v1.LabelArchStable, Operator: v1.NodeSelectorOpIn, Values: []string{"amd64"}},
				{Key: karpenterv1.CapacityTypeLabelKey, Operator: v1.NodeSelectorOpIn,
					Values: []string{karpenterv1.CapacityTypeOnDemand}},
			},
		}},
		Limits: karpenterv1.Limits{v1.ResourceCPU: resource.MustParse("8"), v1.ResourceMemory: resource.MustParse("32Gi")},
		Disruption: karpenterv1.Disruption{ConsolidateAfter: karpenterv1.NillableDuration{Duration: &delay},
			ConsolidationPolicy: karpenterv1.ConsolidationPolicyWhenEmptyOrUnderutilized},
	}}
	namespace := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	require.NoError(t, kube.Create(ctx, namespace))
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		for _, obj := range []client.Object{namespace, pool} {
			if err := client.IgnoreNotFound(kube.Delete(cleanup, obj)); err != nil {
				t.Errorf("cleanup %T: %v", obj, err)
			}
		}
		if err := wait.PollUntilContextCancel(cleanup, 5*time.Second, true, func(ctx context.Context) (bool, error) {
			claims := &karpenterv1.NodeClaimList{}
			if err := kube.List(ctx, claims, client.MatchingLabels{karpenterv1.NodePoolLabelKey: name}); err != nil {
				return false, err
			}
			for i := range claims.Items {
				if err := client.IgnoreNotFound(kube.Delete(ctx, &claims.Items[i])); err != nil {
					return false, err
				}
			}
			return len(claims.Items) == 0, nil
		}); err != nil {
			t.Errorf("waiting for test node cleanup: %v", err)
		}
		if err := client.IgnoreNotFound(kube.Delete(cleanup, nc)); err != nil {
			t.Errorf("nodeclass cleanup: %v", err)
		}
	})
	require.NoError(t, kube.Create(ctx, nc))
	require.NoError(t, kube.Create(ctx, pool))
	newPod := func(label, cpu, memory string) *v1.Pod {
		return &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: label, Namespace: name}, Spec: v1.PodSpec{
			NodeSelector:                  map[string]string{karpenterv1.NodePoolLabelKey: name},
			TerminationGracePeriodSeconds: lo.ToPtr(int64(0)),
			Containers: []v1.Container{{Name: "workload", Image: "registry.k8s.io/pause:3.10",
				Resources: v1.ResourceRequirements{Requests: v1.ResourceList{
					v1.ResourceCPU: resource.MustParse(cpu), v1.ResourceMemory: resource.MustParse(memory),
				}}}},
		}}
	}
	for _, tc := range []struct {
		name, cpu, memory string
		ocpus, gbs        int
	}{
		{"cpu-heavy", "2300m", "1Gi", 2, 4},
		{"memory-heavy", "500m", "9Gi", 1, 12},
		{"mixed", "2300m", "9Gi", 2, 12},
		{"next-size", "2300m", "13Gi", 2, 16},
	} {
		if !t.Run(tc.name, func(t *testing.T) {
			pod := newPod(tc.name, tc.cpu, tc.memory)
			require.NoError(t, kube.Create(ctx, pod))
			require.NoError(t, wait.PollUntilContextTimeout(ctx, 5*time.Second, 15*time.Minute, true,
				func(ctx context.Context) (bool, error) {
					if err := kube.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
						return false, err
					}
					return pod.Status.Phase == v1.PodRunning, nil
				}), "workload failed to run; inspect test namespace %s and controller logs", name)
			node := &v1.Node{}
			require.NoError(t, kube.Get(ctx, client.ObjectKey{Name: pod.Spec.NodeName}, node))
			expected := instancetype.FlexInstanceTypeName(shape, float32(tc.ocpus), float32(tc.gbs), api.BASELINE_1_1)
			require.Equal(t, expected, node.Labels[v1.LabelInstanceTypeStable])
			require.Equal(t, strconv.Itoa(tc.ocpus), node.Labels[api.OciInstanceOcpus])
			require.Equal(t, strconv.Itoa(tc.gbs), node.Labels[api.OciInstanceMemory])
			claims := &karpenterv1.NodeClaimList{}
			require.NoError(t, kube.List(ctx, claims, client.MatchingLabels{karpenterv1.NodePoolLabelKey: name}))
			require.Len(t, claims.Items, 1)
			claim := claims.Items[0]
			require.Equal(t, expected, claim.Labels[v1.LabelInstanceTypeStable])
			requests := pod.Spec.Containers[0].Resources.Requests
			require.GreaterOrEqual(t, claim.Status.Allocatable.Cpu().MilliValue(), requests.Cpu().MilliValue())
			require.GreaterOrEqual(t, node.Status.Allocatable.Memory().Value(), requests.Memory().Value())
			require.Equal(t, int64(tc.ocpus*2), node.Status.Capacity.Cpu().Value())
			require.Equal(t, node.Status.Capacity.Pods().Value(), claim.Status.Capacity.Pods().Value())
			require.NoError(t, wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true,
				func(ctx context.Context) (bool, error) {
					if err := kube.Get(ctx, client.ObjectKeyFromObject(pool), pool); err != nil {
						return false, err
					}
					return pool.Status.Resources.Cpu().Value() == node.Status.Capacity.Cpu().Value(), nil
				}))
			limits := v1.ResourceList(pool.Spec.Limits)
			require.LessOrEqual(t, pool.Status.Resources.Cpu().Value(), limits.Cpu().Value())
			require.LessOrEqual(t, pool.Status.Resources.Memory().Value(), limits.Memory().Value())
			instanceID := strings.TrimPrefix(claim.Status.ProviderID, "oci://")
			command := exec.CommandContext(ctx, "oci", "compute", "instance", "get", "--instance-id", instanceID,
				"--query", `data.{shape:shape,ocpus:"shape-config".ocpus,memory:"shape-config"."memory-in-gbs"}`)
			output, err := command.CombinedOutput()
			require.NoError(t, err, string(output))
			var actual struct {
				Shape         string
				Ocpus, Memory float64
			}
			require.NoError(t, json.Unmarshal(output, &actual))
			require.Equal(t, shape, actual.Shape)
			require.Equal(t, float64(tc.ocpus), actual.Ocpus)
			require.Equal(t, float64(tc.gbs), actual.Memory)
			t.Logf("%s: %s; actual OCI %g OCPUs/%g GB; allocatable CPU=%s memory=%s pods=%s",
				tc.name, instanceID, actual.Ocpus, actual.Memory, node.Status.Allocatable.Cpu().String(),
				node.Status.Allocatable.Memory().String(), node.Status.Allocatable.Pods().String())
			require.NoError(t, kube.Delete(ctx, pod))
			require.NoError(t, wait.PollUntilContextTimeout(ctx, 5*time.Second, 10*time.Minute, true,
				func(ctx context.Context) (bool, error) {
					remaining := &karpenterv1.NodeClaimList{}
					if err := kube.List(ctx, remaining, client.MatchingLabels{karpenterv1.NodePoolLabelKey: name}); err != nil {
						return false, err
					}
					return len(remaining.Items) == 0, nil
				}), "empty-node consolidation did not complete")
		}) {
			t.FailNow()
		}
	}
	t.Run("outside-guardrails", func(t *testing.T) {
		pod := newPod("oversized", "20", "64Gi")
		require.NoError(t, kube.Create(ctx, pod))
		defer func() { require.NoError(t, client.IgnoreNotFound(kube.Delete(ctx, pod))) }()
		deadline := time.Now().Add(90 * time.Second)
		require.NoError(t, wait.PollUntilContextTimeout(ctx, 5*time.Second, 2*time.Minute, true,
			func(ctx context.Context) (bool, error) {
				claims := &karpenterv1.NodeClaimList{}
				if err := kube.List(ctx, claims, client.MatchingLabels{karpenterv1.NodePoolLabelKey: name}); err != nil {
					return false, err
				}
				if len(claims.Items) > 0 {
					return false, fmt.Errorf("provider launched a node for a request outside all guardrails")
				}
				if err := kube.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil && !apierrors.IsNotFound(err) {
					return false, err
				}
				return time.Now().After(deadline), nil
			}))
		require.Empty(t, pod.Spec.NodeName)
	})
}
