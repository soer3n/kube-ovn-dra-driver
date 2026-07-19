# kube-ovn NIC DRA Driver

A [Dynamic Resource Allocation
(DRA)](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/)
driver for **kube-ovn virtual NICs**. It publishes each kube-ovn `Subnet` on a
node as a DRA device, so a workload can request a secondary network interface
(`net1`, `net2`, …) through a `ResourceClaim` instead of a Multus annotation.

> Forked from
> [`kubernetes-sigs/dra-example-driver`](https://github.com/kubernetes-sigs/dra-example-driver)
> and rewritten around a single `nic` device profile.

> 📖 **New here?** Jump to [Documentation](#documentation) for a map of the design
> docs and the "is this the right approach?" decision aids before diving in.

## Status & scope

> **This is an exploration, not a product.** It started from a concrete problem —
> Multus attaching many secondary NICs *serially* makes pod startup latency grow
> linearly with NIC count — and investigates whether **DRA** does better for
> kube-ovn SDN NICs (structurally yes: parallel per-NIC IPAM, scheduler-aware
> placement, IP-pool capacity, clean lifecycle — though the latency *magnitude* is
> still to be measured with `make nic-bench`). It also probes the adjacent,
> still-emerging upstream
> work — **multi-network-api** (SIG-Network), **KNDM/DraNet**, and **KubeVirt
> VEP-183** — to see how an SDN DRA driver fits. See
> [`docs/kndm-dranet-comparison.md`](docs/kndm-dranet-comparison.md) for the
> findings and [`docs/multi-network-api-integration.md`](docs/multi-network-api-integration.md)
> for the target architecture (network API + DRA).

- **IPAM for secondary NICs (committed):** the driver reserves an address/MAC
  from the chosen kube-ovn `Subnet`. Two modes:
  - `pkg/annotation` — IPAM-on-top-of-Multus (writes kube-ovn pod annotations).
  - `pkg/kubeovnip` — Multus-free IPAM via the `ips.kubeovn.io` CRD.
- **Driver-owned netns attach (Multus-free):** `pkg/plumbing` + an NRI sandbox
  hook create the veth/OVS port and move the interface into the pod netns, for
  both VLAN underlay and OVN overlay. Enabled with `kubeletPlugin.nicAttach.enabled`.
  **Validated end-to-end on the kind demo** (overlay + underlay), given the
  kube-ovn controller patch — see
  [`docs/nic-driver.md`](docs/nic-driver.md) for the full design and current state.

## Requirement: kube-ovn with secondary-NIC DRA support

This driver depends on kube-ovn changes that are **not yet in an upstream
release**. Build/run kube-ovn from the fork branch:

```
https://github.com/soer3n/kube-ovn   branch: add-dra-support-for-secondary-nics
```

> The branch will be pushed once driver preparation is complete. Until then the
> kind demo expects a kube-ovn dev image built from that branch (see
> `KUBE_OVN_VERSION` / image-preload steps in the `Makefile`).

**Kubernetes:** the driver uses the stable DRA API (`resource.k8s.io/v1`),
requiring **Kubernetes 1.34+**; it is built and tested against **1.35**
(`k8s.io/*` pinned to `v0.35.x`).

## Quickstart (kind)

A full kube-ovn + (optional Multus) + NIC DRA stack can be brought up in a local
[kind](https://kind.sigs.k8s.io/) cluster. All targets are under the `kind-*` /
`clab-*` prefixes in the `Makefile`.

```bash
# Create cluster, wire the VLAN uplink via containerlab + FRR, deploy kube-ovn
# (+ multus), build & load the driver image, install the chart, apply the NIC
# example.
make kind-demo

# Tear down
make kind-delete
```

Step by step:

```bash
make kind-create             # kind cluster, no CNI, DRA feature-gates on
make clab-deploy             # OPTIONAL: containerlab VLAN uplink + FRR BGP gateway (needs sudo)
make kind-deploy-kube-ovn    # kube-ovn CNI (dev image from the fork branch) -> nodes Ready
make kind-deploy-multus      # OPTIONAL: Multus (only needed for the annotation IPAM mode)
make kind-build-driver       # docker build -> kind load
make kind-deploy-driver      # helm install (deviceProfile=nic)
make kind-deploy-nic-example # subnets, DeviceClass, ResourceClaim, demo pod
make kind-test-vlan          # dual-VLAN traffic + host-routing + isolation checks (needs clab-deploy)
```

See [`docs/nic-driver.md`](docs/nic-driver.md) for the containerlab topology, the VLAN
underlay vs OVN overlay device types, and the tunable `make` variables.

## Install with Helm

```bash
helm install kube-ovn-dra-driver deployments/helm/kube-ovn-dra-driver \
  --namespace kube-system \
  --set deviceProfile=nic
# driverName defaults to "nic.kubeovn.io"; the optional validating webhook is
# behind --set webhook.enabled=true, and the Multus-free attach datapath behind
# --set kubeletPlugin.nicAttach.enabled=true.
```

## Requesting a NIC

The driver publishes one `subnet-<name>` device per kube-ovn `Subnet`, with
attributes under the `nic.kubeovn.io/*` domain (`subnetName`, `subnetType`,
`vlanId`, `provider`, `vpc`). A pod selects the subnet it wants with a CEL
selector on its `ResourceClaim`:

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: my-nic
spec:
  devices:
    requests:
      - name: nic
        exactly:
          deviceClassName: kube-ovn-nic
          selectors:
            - cel:
                expression: "device.attributes['nic.kubeovn.io'].subnetName == 'vlan100-subnet'"
          count: 1
    config:
      - requests: ["nic"]
        opaque:
          driver: nic.kubeovn.io
          parameters:
            apiVersion: nic.resource.kube-ovn.io/v1alpha1
            kind: NicConfig
            interfaceName: net1
```

Worked examples live in [`demo/nic-example/`](demo/nic-example/): a one-underlay
+ one-overlay starting claim, plus scaling fixtures for 2/4/8/16 NICs under
[`demo/nic-example/examples/`](demo/nic-example/examples/) (regenerate with
`examples/generate.py`):

```bash
make nic-example-deploy COUNT=4   # 2 VLAN underlay + 2 OVN overlay NICs
```

## Layout

| Path | Purpose |
|------|---------|
| `cmd/kube-ovn-dra-kubeletplugin/` | DRA kubelet plugin (DaemonSet) — publishes ResourceSlices, prepares claims, NRI hook |
| `cmd/kube-ovn-dra-webhook/` | Validating admission webhook for `NicConfig` opaque config |
| `internal/profiles/nic/` | The `nic` device profile — enumerates kube-ovn Subnets |
| `api/kube-ovn.io/resource/nic/v1alpha1/` | `NicConfig` opaque-config type |
| `pkg/annotation/` | Multus-compatible IPAM (kube-ovn pod annotations) |
| `pkg/kubeovnip/` | Multus-free IPAM via `ips.kubeovn.io` |
| `pkg/nicprepare/` | IPAM reservation lifecycle for a claimed NIC |
| `pkg/plumbing/` | Veth/OVS/netns attach + NRI sandbox hook (validated) |
| `deployments/helm/kube-ovn-dra-driver/` | Helm chart |
| `demo/` | kind + containerlab/FRR demo stack and NIC examples |
| `docs/` | Architecture and design notes |

## Documentation

**Start here** — a map of the repo's docs, by what you're trying to do:

*Understand / run the driver:*
- [`docs/nic-driver.md`](docs/nic-driver.md) — the main design doc: IPAM flow,
  device types, shared subnets, kind/VLAN demo, current status.
- [`docs/architecture.md`](docs/architecture.md) — code walk-through.
- [`docs/benchmarking.md`](docs/benchmarking.md) — `make nic-bench`: DRA vs. Multus
  secondary-NIC spin-up (the attach-timing measurement).

*Evaluate the direction (decision aids — read these if you're asking "is this the
right approach?"):*
- [`docs/kndm-dranet-comparison.md`](docs/kndm-dranet-comparison.md) — this driver
  vs. KNDM/DraNet/SR-IOV/ovn-kubernetes OKEP; "can we just use DraNet/Multus?";
  is SDN in scope for DRA?; hot-plug; strategic options.
- [`docs/multi-network-api-integration.md`](docs/multi-network-api-integration.md)
  — the target architecture: **multi-network-api + DRA**, with this driver as the
  DRA backend (network API owns *which* network; DRA owns *allocate + attach*).

*Upstreaming:*
- [`docs/kube-ovn-issue-draft.md`](docs/kube-ovn-issue-draft.md) — the kube-ovn
  controller changes the driver depends on (issue + PR draft).

## License

Apache 2.0 — see [`LICENSE`](LICENSE).
