# Bounded dynamic Flex sizing

`OCINodeClass.spec.dynamicFlex` generates concrete OCI Flex sizes from OCPU and
memory ranges. Karpenter can compare their prices and allocatable resources before
scheduling workloads. A node launches with exactly the configuration selected by
the scheduler; the provider does not resize it after scheduling.

## Configure an exact grid

Keep your existing image and network configuration and replace `shapeConfigs`
with a policy such as:

```yaml
apiVersion: oci.oraclecloud.com/v1beta1
kind: OCINodeClass
metadata:
  name: dynamic-workers
spec:
  dynamicFlex:
    enabled: true
    ocpus:
      min: 1
      max: 8
      step: 1
    memoryInGbs:
      min: 8
      max: 64
      step: 8
    maxConfigurations: 128
    baselineOcpuUtilization: BASELINE_1_1
    selectionPolicy: lowest-price-that-fits
  volumeConfig:
    bootVolumeConfig:
      imageConfig:
        imageId: <compatible-oke-image-ocid>
        imageType: OKEImage
  networkConfig:
    primaryVnicConfig:
      subnetConfig:
        subnetId: <worker-subnet-ocid>
```

Choose shape families using the existing NodePool label:

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: dynamic-workers
spec:
  template:
    spec:
      nodeClassRef:
        group: oci.oraclecloud.com
        kind: OCINodeClass
        name: dynamic-workers
      requirements:
        - key: oci.oraclecloud.com/instance-shape
          operator: In
          values: [VM.Standard.E5.Flex]
        - key: kubernetes.io/arch
          operator: In
          values: [amd64]
        - key: karpenter.sh/capacity-type
          operator: In
          values: [on-demand]
  limits:
    cpu: "64"
    memory: 256Gi
  disruption:
    consolidationPolicy: WhenEmptyOrUnderutilized
    consolidateAfter: 1m
```

The example defines 64 pairs per compatible family before OCI/image filtering.
OCPUs are OCI physical OCPUs; NodePool CPU limits and pod CPU requests are Kubernetes
CPUs. Conversion follows the provider's shape metadata and platform configuration.
Memory uses OCI's `memoryInGbs` convention. Set NodePool limits as an aggregate
guardrail as well as per-instance policy bounds.

## Bounds, steps, and the cap

Every range contains `min + n * step <= max`. An unaligned maximum is not an extra
configuration. For example, `min: 3, max: 10, step: 4` selects 3 and 7.

The provider intersects the grid with OCI OCPU and memory limits, memory/OCPU ratios,
baseline restrictions, and the selected image's OCPU/memory constraints. Raising a
lower bound through filtering preserves the original grid origin. Unavailable
host capacity changes offering availability, not the grid or its cap accounting.

`maxConfigurations` counts valid pairs **per shape family**, before availability
domains and capacity types. Its default is 128 and its allowed range is 1–512.
The provider evaluates compatible regional families before NodePool requirements
filter them. If any such family exceeds the cap, the entire NodeClass catalog is
rejected with an error such as:

```text
dynamicFlex catalog for VM.Standard.E5.Flex exceeds maxConfigurations=128; use coarser steps or narrower bounds
```

Increase the steps, narrow the ranges, or increase the cap within its supported
limit. No configurations are silently sampled, truncated, or omitted to meet the
cap, and no partial catalog is published. Changing only the cap does not alter an
accepted grid. Keep ranges and family selections narrow to limit scheduling work.

Both ranges require positive integer bounds with `min <= max`. Steps default to
1. API ceilings are 4096 OCPUs and 1048576 GB; these are defensive arithmetic limits,
not available OCI sizes. Actual offerings must also satisfy OCI and image limits.
An empty intersection produces no suitable configurations, not an out-of-bounds
fallback. OCI lookup failures also prevent publishing an unvalidated catalog.

## Compatibility and pricing

- Dynamic mode requires a known Kubernetes/OKE version of at least 1.31, because
  earlier OCI cloud-controller-manager versions do not support multiple Flex sizes
  correctly. Older/unknown versions return an error rather than choosing one size.
- Dynamic mode offers standard and optimized Flex VMs with complete range metadata.
  DenseIO, GPU, bare-metal, and other preset-only families require explicit
  `shapeConfigs` or a separate NodeClass. Non-flexible shapes are excluded in
  dynamic mode.
- The default baseline is `BASELINE_1_1`. Burstable policies support the advertised
  baselines of `VM.Standard3.Flex`, `VM.Standard.E3.Flex`, `VM.Standard.E4.Flex`, and
  `VM.Standard.E5.Flex`, subject to their stricter burstable limits. They require
  paravirtualized networking, which the provider sets when not explicitly configured.
  Burstable policies cannot use capacity reservations or spot capacity.
- Extended-memory configurations are supported for those four families when the
  OCPU minimum and OCI/image limits are satisfied. They are offered only on demand.
  Unknown extended-memory families are limited to the metadata's single-NUMA-node
  bounds. See [OCI extended-memory limits](https://docs.oracle.com/en-us/iaas/Content/Compute/References/extended-memory-vm-instances.htm).
- Each generated instance type has a concrete price, capacity, kubelet/system
  overhead, memory overhead, and networking/pod limits. Existing capacity discovery,
  platform configuration, scheduling overlays, availability fallback, placement,
  and exact capacity-reservation matching continue to apply.
- `lowest-price-that-fits` is the only selection policy. Selection uses concrete
  configured offerings and Karpenter's existing capacity-type priorities and
  optimization rules. It does not promise a globally optimal size between grid
  points, nor does it treat a pod's request as the entire node demand: DaemonSets,
  other packed pods, and node overhead also consume resources.

## Migration and drift

For an existing NodeClass, remove `spec.shapeConfigs` and add `spec.dynamicFlex` in
the same update. Enabled dynamic mode and nonempty per-class presets are mutually
exclusive. Enabled dynamic mode overrides global Flex defaults. Omitting the policy
or setting `enabled: false` restores the existing explicit/global configuration path.
Keep the range fields when disabling an existing policy.

Use the same baseline, bounds, and steps as existing configurations when you want
those sizes to remain eligible. Generated names retain the existing format, for
example `VM.Standard.E5.Flex.2o.16g.1_1b`.

Dynamic range changes are eligibility changes: removing a running node's offering
causes normal drift; widening bounds does not drift a node whose offering remains.
Changing the step can remove sizes even when they remain between the bounds.
Migrating away from explicit presets changes the existing static NodeClass hash and
can trigger replacement. Use NodePool disruption budgets to control rollout.

## Inspect selected sizes

Nodes and NodeClaims expose:

| Label | Meaning |
| --- | --- |
| `node.kubernetes.io/instance-type` | Concrete generated name |
| `oci.oraclecloud.com/instance-shape` | OCI base shape |
| `oci.oraclecloud.com/instance-ocpus` | Configured OCPUs |
| `oci.oraclecloud.com/instance-memory-in-gbs` | Configured memory |

Successful dynamic launches emit a `DynamicFlexSelected` NodeClaim event. Launch
logs include shape, OCPUs, memory, baseline, and instance type. Existing offering
price/availability metrics use the concrete instance type, while
`karpenter_cloudprovider_dynamic_flex_launches_total` counts successful launches
by shape, OCPUs, memory, baseline, and capacity type.

If a workload stays pending, check the NodeClass readiness, controller errors,
Kubernetes version, image compatibility, range/ratio intersection, catalog overflow,
NodePool requirements and limits, and unavailable offerings. A request outside the
largest valid configuration remains pending rather than exceeding your guardrails.

## Track 2: fully dynamic sizing

This feature is Track 1: a bounded, exact catalog. Fully dynamic per-NodeClaim sizing
would require an upstream Karpenter extension to materialize an immutable, priced
offering from tentative workload demand **before** the final scheduling decision.

Calculating a smaller OCI size only inside `Create` would leave earlier packing,
topology, NodePool limit accounting, price comparisons, and consolidation decisions
based on a different capacity. A virtual maximum-capacity type does not solve that
contract. Track 2 is not implemented or enabled by this policy.

A proposed upstream contract would need to:

- Accept a tentative pod group together with NodePool requirements, topology,
  affinity, DaemonSet demand, and kubelet/system reservations during simulation.
- Materialize a concrete shape, OCPU/memory pair, image, allocatable resources,
  networking/VNIC and pod limits, and price before admitting the scheduling decision.
- Assign an immutable offering identity that survives NodeClaim creation and
  readback, with instance-type labels describing the actual configuration.
- Account for that exact capacity in NodePool limits and status, and use the same
  prices and capacities when comparing consolidation and replacement candidates.
- Preserve capacity-type priority, reservation matching, placement restrictions,
  availability feedback, and fallback. A failed launch must trigger a new decision
  if the next candidate changes the resources or price used by the scheduler.
- Bound materialization cost and define cache invalidation when OCI limits, image
  constraints, reservations, or prices change. Existing static providers should
  remain supported without implementing the new extension.

## Focused end-to-end validation

The focused test uses a ready template NodeClass for image/network settings and
creates its own NodeClass, NodePool, workloads, and nodes. It checks CPU-heavy,
memory-heavy, mixed, and next-size requests against actual OCI instance dimensions,
then verifies scale-down and guardrails. Run it only on an isolated test cluster:

```sh
KUBECONFIG=/path/to/test-kubeconfig \
DYNAMIC_FLEX_E2E=true \
DYNAMIC_FLEX_TEMPLATE_NODECLASS=test-template \
OCI_CLI_PROFILE=DEFAULT OCI_CLI_REGION=us-ashburn-1 \
go test -v ./test/e2e -run '^TestDynamicFlexE2E$' -timeout=90m
```

The test cleans up its Kubernetes resources. Cluster, networking, registry, IAM,
and other environment resources remain the responsibility of the test environment
owner. The same test can run with Flannel or OCI VCN-native networking by changing
the kubeconfig and template NodeClass.

See the [validation record](../testing/dynamic-flex.md) for tested versions,
workloads, and coverage limits, and the [design](../design/dynamic-flex.md) for the
catalog and scheduling contract.
