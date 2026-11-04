/*
** Karpenter Provider OCI
**
** Copyright (c) 2026 Oracle and/or its affiliates.
** Licensed under the Universal Permissive License v 1.0 as shown at https://oss.oracle.com/licenses/upl/
 */

package cloudprovider

import (
	"fmt"

	api "github.com/oracle/karpenter-provider-oci/pkg/apis/v1beta1"
	"github.com/oracle/karpenter-provider-oci/pkg/metrics"
	"github.com/oracle/karpenter-provider-oci/pkg/providers/instancetype"
	v1 "k8s.io/api/core/v1"
	corev1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/events"
)

func (c *CloudProvider) recordDynamicFlexSelection(original, launched *corev1.NodeClaim,
	it *instancetype.OciInstanceType) {
	if it.DynamicFlexImage == nil {
		return
	}
	metrics.DynamicFlexLaunches.Inc(map[string]string{
		metrics.ShapeLabel:        it.Shape,
		"ocpus":                   launched.Labels[api.OciInstanceOcpus],
		"memory_in_gbs":           launched.Labels[api.OciInstanceMemory],
		"baseline":                string(*it.BaselineOcpuUtilization),
		metrics.CapacityTypeLabel: launched.Labels[corev1.CapacityTypeLabelKey],
	})
	if c.recorder != nil {
		c.recorder.Publish(events.Event{
			InvolvedObject: original, Type: v1.EventTypeNormal, Reason: "DynamicFlexSelected",
			Message: fmt.Sprintf("Launched %s with %g OCPUs, %g GB memory and %s", it.Shape,
				*it.Ocpu, *it.MemoryInGbs, *it.BaselineOcpuUtilization),
			DedupeValues: []string{string(original.UID), launched.Status.ProviderID},
		})
	}
}
