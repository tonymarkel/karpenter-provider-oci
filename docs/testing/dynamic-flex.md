# Dynamic Flex validation

Validation for issue #65, performed on 2026-11-04 in isolated OCI test infrastructure
in `us-ashburn-1`. This covers the bounded exact catalog (Track 1).

## Local checks

The provider/controller suite, envtest API admission tests, vet, repository lint,
license-header checks, and Linux amd64 build passed. The tests cover exact grids,
overflow rejection without partial results, OCI/image constraints, burstable and
extended-memory restrictions, capacity and prices, VNIC limits, reservations,
availability fallback, image pinning, readback, and drift.

Toolchain: Go 1.26.10, envtest Kubernetes 1.36.2, controller-gen 0.23.0,
golangci-lint 1.64.8 (built with Go 1.26.10), and addlicense 1.2.0. Generated CRDs,
deepcopy code, and the API reference were refreshed without dependency upgrades.

```sh
KUBEBUILDER_ASSETS="$PWD/bin/k8s/1.36.2-darwin-arm64" go test ./pkg/... ./cmd/...
go vet ./...
bin/golangci-lint run --timeout=5m
go test -race ./pkg/providers/instancetype -run TestDynamicFlex -count=1
go test ./pkg/providers/instancetype -run '^$' -bench DynamicFlex -benchmem
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/operator-issue65 ./cmd/main.go
bin/addlicense -check -f hack/upl-header.tmpl -ignore 'vendor/**' -ignore 'bin/**' .
git diff --check
```

On an Apple M2 Pro, generating the maximum accepted 512-pair catalog took about
28 microseconds and 34 KB per operation. An oversized range was rejected in about
6 microseconds and 7 KB. These measure grid generation, excluding API lookup,
offering decoration, and scheduling.

## OKE workload checks

The environments use enhanced OKE clusters running Kubernetes 1.36.4, Oracle Linux
8.10 OKE images, and `VM.Standard.E5.Flex`. The controller image uses the repository's
pinned Oracle Linux runtime base. Tests use a 1–4 OCPU grid and 4–32 GB memory grid
in 4 GB steps, with a per-family cap of 64 and NodePool limits of 8 CPUs/32 GiB.

| Pod requests | Expected and observed OCI size | Flannel | VCN-native |
| --- | --- | --- | --- |
| 2300m CPU, 1 GiB memory | 2 OCPUs / 4 GB | Passed | Passed |
| 500m CPU, 9 GiB memory | 1 OCPU / 12 GB | Passed | Passed |
| 2300m CPU, 9 GiB memory | 2 OCPUs / 12 GB | Passed | Passed |
| 2300m CPU, 13 GiB memory | 2 OCPUs / 16 GB | Passed | Passed |
| 20 CPUs, 64 GiB memory | Pending pod; no NodeClaim | Passed | Passed |

Each sizing case checks actual OCI dimensions, synthetic instance-type names,
OCPU/memory labels, node allocatable resources, and automatic consolidation after
workload deletion. The native run also checks NodePool resource accounting.
The VCN-native CNI image version was 3.4.0-2. Its template has one secondary VNIC
with 16 pod IPs; native nodes advertised 16 pod slots, while the Flannel nodes
advertised the configured 20.
The selection event, launch log, and concrete-size counter were observed live.
Controller restarts retained the identity of already-launched Flex nodes.

A separate live overflow check used the same range with `maxConfigurations: 1`.
The controller rejected the catalog with `exceeds maxConfigurations=1; use coarser
steps or narrower bounds`, left a fitting pod pending, and created zero NodeClaims.
Its test objects were removed after verification.

Run the focused test with the instructions in
[Bounded Dynamic Flex Sizing](../guide/dynamic-flex.md#focused-end-to-end-validation).
The full existing E2E suite needs additional reservation, compute-cluster, and KMS
fixtures and was not run. Burstable/extended-memory launches, capacity exhaustion,
and capacity-reservation matching are covered by local tests, not live launches.

## Cleanup

Both test runs removed their Kubernetes workloads, NodePools, NodeClasses, and
NodeClaims. Independent OCI API checks confirmed zero remaining active tracked
resources: 11 instances and their boot volumes, both clusters and managed node
pools, the VCN and four subnets, route tables, internet/NAT gateways, security list,
private registry repository, IAM policy and dynamic group, and console captures.
The temporary registry authentication token was revoked and its local files removed.
Local test image tags were removed and the build VM was restored to its original
stopped state. Terminated/deleted OCI records can remain visible in service history.
