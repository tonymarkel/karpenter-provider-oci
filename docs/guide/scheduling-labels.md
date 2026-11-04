# Scheduling Labels

KPO supports the following scheduling labels.

Flex configurations also expose `oci.oraclecloud.com/instance-ocpus` (for example,
`2`) and `oci.oraclecloud.com/instance-memory-in-gbs` (for example, `16`). These
describe physical OCI OCPUs and configured memory, before kubelet overhead. They
can constrain explicit presets or [dynamic Flex configurations](dynamic-flex.md)
using NodePool requirements such as `In`, `Gt`, and `Lt`.

| Label | Example | Description                                                                                                                                                                                                          |
|---|---|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `topology.kubernetes.io/zone` | `Uocm:PHX-AD-1` | The availability domain.                                                                                                                                                                                             |
| `node.kubernetes.io/instance-type` | `VM.Standard.B1.4` | OCI shape name. For flexible shapes, use `shapename.Xo.Yg.burstableRatio`, for example: `VM.Standard.E5.Flex.2o.32g.1_1b`.                                                                                           |
| `kubernetes.io/os` | `linux` | Operating system as defined by Go `GOOS` [values](https://github.com/golang/go/blob/master/src/internal/syslist/syslist.go) (KnownOS) on the instance                                                                |
| `kubernetes.io/arch` | `amd64` | Architecture as defined by Go `GOARCH` [values](https://github.com/golang/go/blob/master/src/internal/syslist/syslist.go ) (KnownArch) on the instance                                                                                                                                        |
| `karpenter.sh/capacity-type` | `spot` | Capacity types include `reserved`, `spot`, and `on-demand`.                                                                                                                                                          |
| `oci.oraclecloud.com/instance-shape` | `[ VM.Standard.E5.Flex, VM.Standard.E6.Flex ]` | **OCI-specific.** Available on all shapes.                                                                                                                                                                           |
| `oci.oraclecloud.com/gpu-shape` | `true` | **OCI-specific.** Available on all GPU shapes.                                                                                                                                                                       |
| `oci.oraclecloud.com/baremetal-shape` | `true` | **OCI-specific.** Available on all bare metal shapes.                                                                                                                                                                |
| `oci.oraclecloud.com/denseio-shape` | `true` | **OCI-specific.** Available on all DenseIO shapes.                                                                                                                                                                   |
| `oci.oraclecloud.com/flex-shape` | `true` | **OCI-specific.** Available on all flexible shapes.                                                                                                                                                                  |
| `oci.oraclecloud.com/fault-domain` | `FAULT-DOMAIN-1` | **OCI-specific.** Fault domain within the selected availability domain. Use this label to constrain placement to a specific fault domain, such as `FAULT-DOMAIN-1`, `FAULT-DOMAIN-2`, or `FAULT-DOMAIN-3`. |
| `oci.oraclecloud.com/capacity-reservation-id` | `the last part of capacity reservation OCID` | **OCI-specific.** Capacity reservation ID; only present on reserved offering. Since Kubernetes limits label values to **63 characters**, trim everything before the last dot (`.`) of the capacity reservation OCID. |
