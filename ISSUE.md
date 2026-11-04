## Problem statement

OCI Flexible shapes let customers tailor OCPUs and memory to a workload. KPO currently requires users to manually enumerate exact `shapeConfigs` such as `2 OCPUs / 8 GiB` and `4 OCPUs / 16 GiB` in an `OCINodeClass`.

Customers should be able to select one or more Flex shape families, define safety and cost boundaries, and have KPO choose the smallest valid OCPU and memory configuration for each new node from workload demand. Manual presets create configuration debt, can cause overprovisioning, and may leave gaps where a workload does not fit an available preset.

## Current behavior and technical context

KPO already launches OCI Flex instances with exact OCPU and memory values. It represents each configured Flex pair as a separate synthetic Karpenter instance type, for example `VM.Standard.E4.Flex.4o.32g.1_1b`. Each offering has static allocatable capacity and price, allowing Karpenter to simulate scheduling and select an offering before KPO launches the instance.

OCI Compute supports exact OCPUs and memory in `LaunchInstanceShapeConfigDetails`. The challenge is the Karpenter scheduling contract: instance capacity and price are needed before scheduling, while the final aggregate resource demand is known when KPO creates a NodeClaim. A provider-only unbounded dynamic configuration therefore risks incorrect bin packing, pricing, NodePool limit accounting, and consolidation.

## Proposed solution

Deliver this in two tracks.

### Track 1: Production-ready bounded dynamic Flex catalog

Add an opt-in `dynamicFlex` policy to `OCINodeClass` that defines valid bounds rather than requiring users to list every exact pair. KPO expands those bounds into a capped catalog of OCI-valid synthetic offerings, preserving exact capacity and price for Karpenter.

Illustrative API only:

```yaml
spec:
  dynamicFlex:
    enabled: true
    ocpus:
      min: 1
      max: 16
      step: 1
    memoryInGbs:
      min: 8
      max: 128
      step: 1
    maxConfigurations: 128
    selectionPolicy: lowest-price-that-fits
```

Required behavior:

- Validate OCI shape-specific OCPU, memory, ratio, baseline, and image constraints.
- Generate only launchable configurations and cap expansion deterministically.
- Publish each configuration with correct allocatable resources, overhead, networking/pod limits, and price.
- Preserve NodePool requirements, availability-domain/fault-domain placement, on-demand/spot/reserved behavior, capacity reservations, drift, and unavailable-offering cache behavior.
- Make selected OCPU/memory visible through generated names, labels, events, metrics, and logs.
- Retain explicit `shapeConfigs` as an advanced mode and define validation/precedence when both modes are present.
- Document Kubernetes/OKE compatibility, including the multi-Flex-config constraint.

This provides substantially better UX while retaining correct Karpenter scheduling and optimization semantics.

### Track 2: Fully dynamic per-NodeClaim sizing

Propose an upstream Karpenter provider extension that lets a provider materialize a concrete, priced offering from a tentative pod group before final scheduling and NodeClaim creation. That resulting offering must be immutable for the decision.

The design must preserve correctness for pod packing; topology and affinity; NodePool resource limits and status; price comparison; capacity-type fallback; consolidation; instance-type requirements and labels; DaemonSet, kubelet, VNIC, and system-reserved overhead; capacity reservations; drift; disruption; and observability.

A provider-only virtual maximum-capacity Flex type may be useful as a gated experiment, but should not be considered production-correct until those semantics are resolved.

## Acceptance criteria

1. Users can configure bounded Flex ranges without manually listing each exact OCPU/memory pair.
2. KPO never offers or launches a configuration outside OCI limits or configured guardrails.
3. Karpenter schedules pods only to a generated offering whose actual allocatable capacity fits requests plus required overhead.
4. Price ordering and capacity-type fallback use the concrete configuration price.
5. NodePool limits, NodeClaim status, labels, drift, and consolidation remain correct.
6. Unit tests cover bounds, OCI ratio validation, price ordering, overhead, availability fallback, capacity reservations, and the generated-offering cap.
7. E2E tests prove mixed CPU/memory workloads choose the smallest fitting configuration and that oversized workloads scale to the next valid offering.
8. Documentation includes migration from `shapeConfigs`, guardrail guidance, generated-offering limits, and the distinction between bounded-catalog and fully dynamic modes.

## Alternatives considered

- Continue requiring manual `shapeConfigs`: correct today, but creates a poor user experience.
- Enumerate every possible Flex combination: compatible with the current model but can create an impractically large catalog and scheduler overhead.
- Calculate a configuration only during `Create`: OCI launch succeeds, but earlier scheduling, price, limits, and consolidation decisions rely on a misleading virtual offering.
- Wait for upstream support before any improvement: delays a safe, bounded-catalog enhancement.

## References

- OCI launch configuration: https://docs.oracle.com/en-us/iaas/tools/python/latest/api/core/models/oci.core.models.LaunchInstanceShapeConfigDetails.html
- Current KPO `shapeConfigs` implementation: `pkg/apis/v1beta1/ocinodeclass_types.go`, `pkg/providers/instancetype/provider.go`, and `pkg/cloudprovider/cloud_provider.go`.