# Bounded dynamic OCI Flex catalog

Implementation design for issue #65: an exact grid with overflow rejection.

## Intent and agreed scope

Let an operator specify OCPU and memory bounds instead of enumerating individual
Flex configurations. Publish concrete, immutable capacities and prices before
Karpenter schedules workloads, and launch exactly one of those configurations.

The scope is Track 1 plus documentation distinguishing Track 2. This change
does not implement an upstream extension, virtual maximum-capacity offering,
post-scheduling resizing, or in-place node resizing.

OCI validation uses isolated test infrastructure. Record resource identifiers and
delete resources after testing so cleanup can be independently verified.

## Approaches

1. **Representative bounded catalog (not selected).** Generate
   a deterministic subset spanning the valid CPU and memory ranges when the full
   grid exceeds the cap. Broad ranges remain useful. A workload may need the next
   larger retained configuration when a closer intermediate size is omitted.
2. **Exact grid with overflow rejection (selected).** Offer every valid configured grid point,
   but reject an oversized catalog and ask operators to increase steps or narrow
   ranges. Predictable exact coverage requires more operator tuning.

Exact coverage uses overflow rejection. Keeping only the cheapest prefix was rejected as a
design option because it can exclude all sizes needed by large workloads.

## API and compatibility

Add an optional `spec.dynamicFlex` object to `OCINodeClass`:

```yaml
spec:
  dynamicFlex:
    enabled: true
    ocpus: {min: 1, max: 8, step: 1}
    memoryInGbs: {min: 8, max: 64, step: 8}
    maxConfigurations: 128
    baselineOcpuUtilization: BASELINE_1_1
    selectionPolicy: lowest-price-that-fits
```

Implementation choices:

- `enabled` defaults to true when the object is present. Absent or disabled policies
  keep existing explicit/global `shapeConfigs` behavior.
- Both ranges require positive integer `min` and `max`, with `min <= max`; `step`
  defaults to 1 and must be positive. Grid points are `min + n * step <= max`;
  an unaligned maximum is a bound, not an extra grid point.
- Use defensive API ceilings of 4096 OCPUs and 1048576 GB memory. These are
  arithmetic limits, not promises of OCI availability. OCI limits further constrain
  every offering. Avoid allocating a Cartesian product from these bounds.
- `maxConfigurations` defaults to 128, with allowed values 1 through 512. It caps
  configurations **per shape family**, before multiplication by zones/capacity
  types. Document that total catalog size also depends on supported shape families.
- One baseline applies to the policy; default `BASELINE_1_1`, with the existing
  three-value baseline enum. Unsupported shape/baseline combinations are omitted.
- `selectionPolicy` defaults to and accepts only `lowest-price-that-fits`. This
  describes existing Karpenter/provider selection, not a new scheduler policy.
- Reject enabled `dynamicFlex` combined with nonempty per-class `shapeConfigs`.
  Dynamic mode overrides global Flex defaults rather than combining catalogs.
- Enabled dynamic mode offers only supported flexible VM families. Existing
  NodePool `oci.oraclecloud.com/instance-shape` requirements select families.
  Static shapes remain available through NodeClasses without dynamic mode.
- Initially support standard and optimized Flex VMs with complete continuous-range
  metadata. DenseIO, GPU, bare-metal, and other preset-only shapes remain in explicit
  mode; their special launch combinations cannot safely be inferred from intervals.
- Require a known Kubernetes version of at least 1.31 for dynamic mode. Return an
  actionable error for older/unknown versions rather than silently reducing the
  policy to one configuration. Preserve legacy mode's existing behavior.

Generate and verify CRDs, deepcopy code, and API documentation. Admission tests must
exercise raw objects so missing/pruned fields and defaulting errors are observable.

## Validating a concrete configuration

For each eligible shape, intersect configured ranges with current OCI shape limits,
memory-per-OCPU ratios, and the selected image's compatibility constraints. Keep
all arithmetic aligned to the user's original grid. Invalid or incomplete shape
metadata produces no dynamic offerings for that shape.

Resolve the same first compatible image that the current image provider would use.
Retain the OCPU and memory constraints currently discarded by its compatibility
cache. Fetch/cache compatibility once per image, never once per generated pair.
Propagate OCI lookup errors; an error must not be treated as unconstrained
compatibility. A shape absent from the image's compatibility entries is ineligible.

For burstable policies, validate the advertised baseline, documented family-specific
burstable CPU/memory ceilings, image minimum baseline CPU requirements, and required
paravirtualized networking. Do not offer spot or reserved capacity for burstable
instances. Do not infer burstable limits from ordinary extended-memory limits.
Unsupported families fail closed. Keep ordinary dynamic mode at full baseline by
default. Validate extended-memory/NUMA restrictions using supported metadata; omit
combinations whose launchability cannot be established.

Revalidate the selected concrete configuration against the image used for launch.
Reuse the resolved image/compatibility result within that provisioning attempt or
verify the freshly resolved image. Never resize a selected offering during Create.
If refreshed metadata invalidates it, select another fitting concrete offering or
let Karpenter retry.

## Exact bounded expansion

The algorithm operates on CPU rows with valid memory
intervals rather than materializing the full Cartesian grid:

1. Build increasing grid-aligned OCPU rows after all shape/image constraints. For
   each row, derive its valid grid-aligned minimum and maximum memory and count.
2. If the total valid count fits the cap, emit every point in ascending CPU/memory order.
3. Otherwise reject the entire policy for this NodeClass with an actionable error
   naming the overflowing shape and cap and recommending coarser steps or narrower bounds.
   Do not publish a partially generated catalog or silently omit a family.

Every point must be a valid original grid point. The result depends on policy,
shape, and image constraints, never Go map order, current prices, transient host
availability, or unavailable-offering cache contents. Configuration names retain
the existing `Shape.<ocpus>o.<memory>g.<baseline>b` convention.

The promise is the lowest-priced fitting **valid configured offering**, subject to existing
capacity-type priorities, NodePool requirements, overlays, and Karpenter optimization.
It is not the globally optimal pair from every OCI-permitted combination. Changing
steps or bounds can change the set and cause normal drift. Changing only the cap
does not alter an accepted catalog.

## Integrating with existing provider behavior

Feed generated concrete pairs through the existing capacity, memory overhead,
kubelet/DaemonSet reservation, VNIC/pod limit, platform-config, pricing, placement,
and offering decoration paths. Preserve per-configuration unavailable-offering keys
and exact capacity-reservation matching. Do not invent virtual capacities/prices.

Acquire provider snapshots under their existing synchronization, but do not hold a
provider-wide lock across new OCI image lookups. Keep catalog generation bounded;
add a benchmark for the maximum supported cap and large input ranges.

Repair concrete Flex identity matching in `Get`/`List`: the current lookup compares
the OCI base shape to the synthetic instance name. Match shape, OCPU, memory, and
baseline so returned NodeClaims preserve concrete identity and capacity. If a
running node's pair has left the current catalog, preserve its actual identity for
tracking/deletion; do not substitute a different size or incorrectly declare it
missing. Drift must detect that its original offering is no longer allowed.

Treat dynamic bounds as eligibility constraints in drift rather than static launch
configuration hash inputs: removing an offering causes `ShapeNotFound`; widening a
policy alone does not replace nodes whose offering remains. Preserve hashes for
legacy NodeClasses and cover migration effects in tests/documentation.

## Observability

- Add well-known numeric OCPU and memory labels to concrete Flex instance types,
  NodeClaims, and Nodes, with consistent values after restart/readback.
- Log selected shape, OCPUs, memory, baseline, and concrete instance type on launch.
- Publish a successful selection event on the original NodeClaim, avoiding duplicate
  events from readback or retries that did not launch anything.
- Reuse concrete instance-type price/availability metrics and add bounded selection
  or size metrics only where needed to expose actual launched sizes. Avoid OCID,
  NodeClaim-name, or unbounded per-request metric labels.
- Surface invalid-policy, unsupported-version, and empty-catalog errors with enough
  context to correct the policy. Document overflow rejection explicitly.

## Verification and delivery

Unit/integration coverage must establish range/default validation, grid alignment,
exact grid expansion and cap boundaries, OCI/image ratio limits, baseline rules,
pricing, overhead, networking/pod limits, capacity-type fallback, exact reservations,
unavailable cache isolation, readback identity, and drift/migration behavior.

Run the repository's relevant generation, unit/envtest, vet/lint, and build checks.
`make verify` also upgrades dependencies/tools/action pins; avoid unrelated upgrades
in this feature branch and run its relevant checks at pinned versions. Verify the
required Go toolchain is available before attributing any failure to the feature.

Create isolated OKE test infrastructure and deploy the built provider. Cover both
Flannel and OCI VCN-native networking when the supported OKE versions permit it.
Test mixed CPU/memory workloads, a request requiring the next larger configured size,
and a request outside all guardrails. Compare launched OCI shape config, generated
name, labels, NodeClaim capacity/allocatable, and actual node allocatable. Exercise
scale-down/consolidation and restart/readback; use unit tests for scarce capacity
and reservation failure cases. Record exact commands, versions, results, and any
coverage limitations for the PR.

Use unique task names/tags and an explicit resource inventory. Delete task-created
workloads, instances, volumes, node pools, clusters, network components, registry
artifacts, and temporary IAM resources in dependency order. Verify termination and
absence of remaining task resources. Never broadly delete unrelated resources.

Document migration, mode precedence, examples, family/cluster compatibility,
guardrails, cap semantics, cost-selection semantics, observability, and troubleshooting.
Explain Track 2's need for an upstream pre-scheduling materialization contract and
why resizing only in Create cannot preserve packing, limits, prices, and consolidation.

The API lives in `pkg/apis/v1beta1/dynamicflex.go`; catalog generation and OCI/image
intersection live in `pkg/providers/instancetype/dynamicflex.go`. The image provider
retains compatibility constraints, and cloud-provider launch/readback integration
preserves concrete identity. See the [operator guide](../guide/dynamic-flex.md) and
[validation record](../testing/dynamic-flex.md).

## Sources

- [Issue #65](https://github.com/oracle/karpenter-provider-oci/issues/65).
- [OCI shape constraints](https://docs.oracle.com/en-us/iaas/Content/Compute/References/computeshapes.htm).
- [Shape memory options](https://docs.oracle.com/en-us/iaas/tools/python/latest/api/core/models/oci.core.models.ShapeMemoryOptions.html).
- [Image shape compatibility](https://docs.oracle.com/en-us/iaas/tools/python/latest/api/core/models/oci.core.models.ImageShapeCompatibilitySummary.html).
- [Burstable constraints](https://docs.oracle.com/en-us/iaas/Content/Compute/References/burstable-instances.htm).
- [Launch shape configuration](https://docs.oracle.com/en-us/iaas/tools/python/latest/api/core/models/oci.core.models.LaunchInstanceShapeConfigDetails.html).
