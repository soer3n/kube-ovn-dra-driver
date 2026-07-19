# Architecture: kube-ovn NIC DRA Driver

This document describes the design of a Dynamic Resource Allocation (DRA) driver
that bridges Kubernetes DRA (KEP-3063) with the kube-ovn CNI to provide
scheduler-managed secondary network interfaces for pods.

It is intended as a reference implementation for the intersection of:
- [kubernetes-sigs/multi-network-api](https://github.com/kubernetes-sigs/multi-network-api)
- [KEP-4815 DRA Partitionable Devices](https://github.com/kubernetes/enhancements/issues/4815)
- [kube-ovn](https://github.com/kubeovn/kube-ovn) subnet-backed IPAM

---

## Problem Statement

Kubernetes today has no first-class API for attaching a pod to a secondary network
with scheduler-visible resource allocation. The existing solutions (Multus + NAD,
kube-ovn annotations) work but share a common gap:

- **No scheduler visibility**: secondary NIC assignment happens in the CNI plugin,
  after the pod is already scheduled. The scheduler cannot consider NIC availability.
- **No hotplug**: secondary NICs are wired at pod start, not dynamically after.
- **No declarative resource lifecycle**: there is no Kubernetes object tracking
  "this pod owns IP 10.0.0.5 on subnet ovn-net".

DRA solves the scheduler visibility and lifecycle problems. This driver shows how
to back DRA ResourceSlices with real kube-ovn subnets and plumb the interfaces
using NRI.

---

## Key Design Constraint: Two-Phase Plumbing

The central constraint driving the architecture is:

| Phase | Trigger | Constraints |
|---|---|---|
| **Phase 1** — IPAM | `PrepareResourceClaims` (kubelet→driver gRPC) | Slow OK, but pod netns does **not exist yet** |
| **Phase 2** — Plumbing | NRI `RunPodSandbox` hook | Pod netns **exists**, but timeout is ~2s |

A naive single-phase approach fails: if you try to create the veth pair in
`PrepareResourceClaims`, the pod network namespace doesn't exist yet. If you do
IPAM in `RunPodSandbox`, the 2-second NRI deadline is too tight for a round-trip
to the kube-ovn controller.

The solution is a clean split across these two phases.

---

## Architecture Overview

```
┌─────────────────────────────────────────────────────────────────────────┐
│                        Kubernetes Control Plane                         │
│                                                                         │
│  kube-scheduler  ──────────────────────────────────────────────────────┐│
│  (reads ResourceSlice,                                                  ││
│   allocates devices to claims)                                          ││
│                                                                         ││
│  kube-ovn-controller ◄── Pod annotations ──── write IP/MAC/GW ────────┐││
│  (manages OVN logical                                                   │││
│   switches + IPAM)                                                      │││
└─────────────────────────────────────────────────────────────────────────┘││
                                                                           │││
┌─────────────────────────────────────────────────────────────────────────┘││
│                           Node (kubelet)                                  ││
│                                                                           ││
│  kubelet                                                                  ││
│    │                                                                      ││
│    ├─ PrepareResourceClaims ──► driver.go ──► state.go ──────────────────┘│
│    │                                              │                        │
│    │                            nicprepare.RequestIPAM()                  │
│    │                              │ annotate pod  │                        │
│    │                              └──────────────►│                        │
│    │                                              │ wait for kube-ovn      │
│    │                                              │ to write back IP/MAC   │
│    │                                              │◄──────────────────────┘
│    │                            store NicDeviceConfig(s) keyed by podUID
│    │
│    └─ (pod scheduled, containerd starts sandbox)
│                                                │
│  containerd ──► NRI ──► driver.RunPodSandbox ─┘
│                              │
│                     nicprepare.PlumbNIC()
│                       ├─ ovn:  veth pair + OVS port + netns config
│                       └─ vlan: veth pair + OVS VLAN bridge + netns config
│
│  containerd ──► NRI ──► driver.StopPodSandbox
│                              │
│                     nicprepare.UnplumbNIC() + ReleaseIPAM()
└───────────────────────────────────────────────────────────────────────────┘
```

---

## Component Map

### `cmd/kube-ovn-dra-kubeletplugin/driver.go`

The main driver struct. Implements:

- **`PrepareResourceClaims`** — Phase 1 entry point. Iterates claims, calls
  `state.Prepare()` which calls `nicprepare.RequestIPAM()` per device. Stores
  `NicDeviceConfig` in `state.podNICConfigs[podUID]`.
- **`RunPodSandbox`** (NRI hook) — Phase 2 entry point. Retrieves stored configs
  via `state.TakePodNICConfigs(podUID)`, calls `nicprepare.PlumbNIC()` with the
  real netns path from the NRI `PodSandbox` struct.
- **`StopPodSandbox`** (NRI hook) — Teardown. Calls `nicprepare.UnplumbNIC()` +
  `nicprepare.ReleaseIPAM()`.

### `cmd/kube-ovn-dra-kubeletplugin/state.go`

Manages per-node device state:

- **`NewDeviceState`** — enumerates kube-ovn subnets at startup by calling the
  kube-ovn CRD API (`subnets.kubeovn.io`). Each subnet becomes a `resourceapi.Device`
  in the `ResourceSlice`. Attributes published per device:

  | Attribute | Type | Example |
  |---|---|---|
  | `subnetName` | string | `"ovn-subnet"` |
  | `subnetType` | string | `"ovn"` or `"vlan"` |
  | `vlanId` | int | `100` |
  | `provider` | string | `"external"` |
  | `cidr` | string | `"10.200.0.0/24"` |

- **`podNICConfigs`** — `map[types.UID][]*NicDeviceConfig`. Written by
  `PrepareResourceClaims`, consumed (and deleted) by `RunPodSandbox`.
- **Checkpoint** — prepared claim state is persisted via kubelet's checkpoint
  manager so it survives driver restarts.

### `pkg/nicprepare/nicprepare.go`

The two-phase lifecycle implementation:

```
RequestIPAM(ctx, client, claim, result, device, ifaceName) → NicDeviceConfig
  1. Resolve pod name from claim.Status.ReservedFor
  2. Annotate pod: ovn.kubernetes.io/logical_switch_<ifaceName> = <subnetName>
  3. Poll pod annotations until kube-ovn writes back IP/MAC/GW (up to 30s)
  4. Return NicDeviceConfig (stored, not used yet)

PlumbNIC(cfg, netNS) → error
  switch cfg.SubnetType:
  "vlan" → PlumbVLAN: veth pair + OVS VLAN bridge port + ip/mac in netns
  "ovn"  → PlumbOVN:  veth pair + OVS port on br-int + ip/mac in netns

UnplumbNIC(cfg)
  Remove OVS port + veth pair

ReleaseIPAM(ctx, client, cfg)
  Remove kube-ovn annotation → controller frees the IP
```

### `pkg/plumbing/`

Low-level kernel + OVS plumbing:
- Creates veth pairs
- Attaches host-side to OVS bridge (`br-int` for OVN, `br-<provider>` for VLAN)
- Moves pod-side veth into pod netns and configures IP/MAC/GW/routes

---

## ResourceSlice → kube-ovn Subnet Mapping

Each kube-ovn `Subnet` CRD becomes a `Device` in the driver's `ResourceSlice`:

```yaml
# kube-ovn Subnet CRD
apiVersion: kubeovn.io/v1
kind: Subnet
metadata:
  name: ovn-subnet
spec:
  cidrBlock: 10.200.0.0/24
  protocol: IPv4
  vpc: ovn-cluster

# Becomes this Device in ResourceSlice
name: subnet-ovn-subnet
attributes:
  nic.kubeovn.io/subnetName:  "ovn-subnet"
  nic.kubeovn.io/subnetType:  "ovn"
  nic.kubeovn.io/cidr:        "10.200.0.0/24"
```

Users select devices via CEL in their `ResourceClaim`:

```yaml
requests:
  - name: nic0
    exactly:
      deviceClassName: kube-ovn-nic
      selectors:
        - cel:
            expression: >
              device.attributes['nic.kubeovn.io'].subnetName.stringValue == 'ovn-subnet'
```

---

## IPAM Model: Flat, CNI-Owned

The driver does **not** split subnets per node. The kube-ovn OVN logical switch
spans all nodes — any pod on any node can receive any IP from the subnet's CIDR.
The scheduler selects a node; the driver then requests an IP from kube-ovn's
flat pool after scheduling.

This is the **flat multi-network model**: one `/24` across all nodes, no sub-CIDR
allocation per node. The OVN fabric handles inter-node forwarding transparently.

This contrasts with the default Kubernetes pod CIDR model where each node owns a
sub-CIDR (e.g. `/27` per node from a `/16` cluster CIDR).

```
Flat model (this driver):
  Node A: pod gets 10.200.0.5  ─┐
  Node A: pod gets 10.200.0.6  ─┤── all from 10.200.0.0/24
  Node B: pod gets 10.200.0.7  ─┘   kube-ovn OVN handles routing

Sub-CIDR model (default NodeIpam):
  Node A: owns 10.244.0.0/27  → pods .1-.30
  Node B: owns 10.244.0.32/27 → pods .33-.62
```

---

## NRI Integration

NRI (Node Resource Interface) is the containerd plugin API used to intercept
container lifecycle events. The driver registers as an NRI plugin that handles:

- **`Synchronize`** — called on (re)connect; returns existing containers to sync state.
- **`RunPodSandbox`** — called by containerd just before the pod sandbox network
  namespace is handed to the CNI. The netns path is available here. This is
  Phase 2 plumbing.
- **`StopPodSandbox`** — called when the sandbox stops. Triggers teardown.

NRI requires containerd ≥ v2.0 with NRI enabled. The driver registers with:
```
plugin name: <driverName>   (e.g. "nic.kubeovn.io")
plugin index: "00"           (runs before other NRI plugins)
```

The NRI socket path `/var/run/nri/nri.sock` must be mounted into the driver pod.

---

## What This Driver Does NOT Do

- **No scheduler extension**: the flat IPAM model means any node is valid. A
  future improvement using KEP-4815 Partitionable Devices could let the scheduler
  track IP pool exhaustion.
- **No NetworkPolicy on secondary interfaces**: out of scope, tracked by
  `multi-network-api`.
- **No Service on secondary interfaces**: out of scope, tracked by `multi-network-api`.
- **No IPv6**: only IPv4 subnets are currently handled.

---

## Relation to Upstream Proposals

### kubernetes-sigs/multi-network-api

`multi-network-api` defines the `Network` CRD (identity layer) and explicitly
defers interface attachment to DRA:

> *"We do not list basic use cases that just add network interfaces to a pod,
> since those are currently handled by Dynamic Resource Allocation."*

This driver implements exactly that DRA-based attachment layer. A future
integration would:
1. Watch `Network` objects
2. Map each `Network` to a kube-ovn `Subnet`
3. Publish the subnet as a `ResourceSlice` device
4. Accept `ResourceClaim` selectors referencing the `Network` name

### KEP-4815 DRA Partitionable Devices (alpha k8s 1.35, beta 1.36)

Today the driver uses flat IPAM — any IP from the pool can go to any claim.
With KEP-4815, the IP pool itself would become a partitionable device:

```
ResourceSlice device: subnet-ovn-subnet
  pool capacity: 254 IPs
  partition shape: 1 IP per claim
```

The scheduler would track pool exhaustion and refuse to schedule pods when the
subnet is full — something the current driver cannot express. The driver would
receive the pre-allocated IP in the `AllocationResult` rather than calling
kube-ovn's annotation API.

---

## Demo Setup

The full demo uses:

- **kind** (v1.35+) — local multi-node cluster, no default CNI
- **Containerlab** — injects `eth1` into kind node containers to simulate
  a VLAN-capable physical NIC for kube-ovn ProviderNetwork
- **kube-ovn** (v1.14+) — installed via Helm, provides OVN overlay + VLAN subnets
- **Multus** (v4.2+) — thick mode, provides the multi-NIC pod annotation interface
- **This driver** — DRA kubelet plugin + NRI plugin

```bash
make kind-demo          # full end-to-end setup
make kind-demo-hotplug  # attach a third NIC to the running pod
make kind-delete        # teardown
```

See [`demo/kind/kind-no-cni.yaml`](../demo/kind/kind-no-cni.yaml) and
[`demo/containerlab/vlan-topology.yaml`](../demo/containerlab/vlan-topology.yaml).
