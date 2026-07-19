# kube-ovn GitHub Issue: DRA Driver Integration — API Gaps and Enhancement Requests

> **Note**: This is a draft for filing at https://github.com/kubeovn/kube-ovn/issues
> **Labels to add**: `enhancement`, `kind/feature`, `area/api`

---

## Title
`[Enhancement] DRA (Dynamic Resource Allocation) driver integration: API gaps and enhancement requests`

---

## Summary

We have built a Kubernetes DRA (Dynamic Resource Allocation) driver that uses
kube-ovn subnets as the backing resource for scheduler-managed secondary pod
NICs. The driver implements the full path — IPAM via the `ips.kubeovn.io` CRD
plus an NRI-based attach that owns the veth/OVS plumbing — with the goal of
attaching secondary NICs **without Multus**. Getting there surfaced several gaps
and undocumented constraints in the kube-ovn API surface.

This issue documents what worked well, what required workarounds, and what
enhancements to kube-ovn would make DRA integration cleaner and more robust.

**Reference implementation**: https://github.com/soer3n/kube-ovn-dra-driver (NIC profile)

---

## Background: What is a DRA Driver?

DRA (KEP-3063, stable in k8s 1.32) allows device drivers to publish resources
as `ResourceSlice` objects. The scheduler allocates them to `ResourceClaim`
objects. At pod start, the driver receives the allocation and plumbs the device.

For networking, this means:
- kube-ovn subnets → published as `ResourceSlice` devices
- User creates `ResourceClaim` selecting a subnet by attribute
- Scheduler assigns a claim to a node
- Driver allocates an IP from kube-ovn and wires the interface into the pod

This gives the scheduler full visibility into secondary NIC allocation — something
not possible with Multus + NAD today.

---

## What Works Well

### 1. Subnet CRD as device inventory

`subnets.kubeovn.io` provides all information needed to populate a `ResourceSlice`:
`cidrBlock`, `vlan`, `provider`, `protocol`. The driver can enumerate all subnets
at startup and publish them as devices with typed attributes.

### 2. Subnet IP capacity already exposed in status

`Subnet.status` already carries `v4availableIPs` and `v4usingIPs` (and IPv6
equivalents). This is exactly what a DRA driver needs to implement KEP-4815
Partitionable Devices — the subnet can be published as a pool whose capacity
tracks `status.v4availableIPs`, letting the scheduler reject pods when the pool
is exhausted without any new kube-ovn API changes required.

### 3. Per-provider IPAM annotations (Multus flows)

kube-ovn keys every per-network annotation by the network's **provider**, using
the template `<provider>.kubernetes.io/<field>` (see `pkg/util/const.go`
`*AnnotationTemplate`, written by `pkg/controller/pod.go` with
`fmt.Sprintf(template, subnet.Spec.Provider)`):
```
<provider>.kubernetes.io/logical_switch  = <subnetName>   # request
<provider>.kubernetes.io/ip_address      = 172.17.0.100   # written back
<provider>.kubernetes.io/mac_address     = 00:00:00:53:6B:BB
<provider>.kubernetes.io/cidr / gateway  = ...
<provider>.kubernetes.io/allocated       = "true"         # readiness signal
```
`<provider>` is the Subnet's `spec.provider` — `<nad>.<ns>.ovn` for a Multus
NAD, or the sentinel `ovn` for the default network. There is **no
`_<ifaceName>` suffix**; NICs are distinguished by provider. (An earlier version
of this driver and this draft assumed an `ovn.kubernetes.io/logical_switch_<iface>`
form — that does not exist.)

### 4. `ips.kubeovn.io` CRD enables Multus-free IP reservation

Creating an `IP` object makes the controller's reserved-IP reconciler
(`handleAddReservedIP`, `pkg/controller/ip.go`) allocate an address from the
subnet pool **with no NAD and no pod** — exactly what a DRA driver needs to
reserve at `PrepareResourceClaims` time. This is the mechanism our driver now
uses. Constraints we had to reverse-engineer (see Gap #2):
- `metadata.name` must equal `ovs.PodNameToPortName(podName, namespace, provider)`;
- `spec.{subnet,podName,namespace}` are required;
- the controller writes back `spec.ipAddress` / `spec.v4IpAddress` /
  `spec.macAddress`.

---

## Gaps and Enhancement Requests

### 1. Secondary-network IPAM is coupled to Multus/NAD (core gap)

kube-ovn-controller derives the set of providers to allocate for a pod
**exclusively** from the pod's `k8s.v1.cni.cncf.io/networks` annotation → the
referenced `NetworkAttachmentDefinition` (`getPodAttachmentNet`,
`pkg/controller/pod.go`), from which it derives `provider = <nad>.<ns>.ovn`.

Consequence: **writing `<provider>.kubernetes.io/logical_switch` on a pod does
nothing without a NAD** — the controller never adds the provider to `podNets`
and never reconciles it. A DRA driver that wants to avoid Multus therefore
cannot drive secondary IPAM through pod annotations at all; it must either
install a NAD (re-introducing Multus) or bypass the pod path entirely via the
`ips.kubeovn.io` CRD (which is what we do).

**Enhancement request**: a supported way to request secondary-network IPAM for a
provider/subnet **without** a `NetworkAttachmentDefinition` — e.g. a pod-spec or
annotation hook the controller honors for NRI/DRA drivers that don't use Multus.

### 2. `ips.kubeovn.io` reservation works, but with undocumented constraints

The IP-CRD path (What Works Well #4) is the Multus-free reservation mechanism,
but it took source-reading to use correctly, and has rough edges:

- **Silent name coupling**: `metadata.name` must exactly equal
  `ovs.PodNameToPortName(podName, namespace, provider)`. If it doesn't,
  `handleAddReservedIP` returns `nil` and the object is **silently ignored** —
  no event, no status, the reservation just never happens.
  *Enhancement*: surface a status condition/event when an IP object can't be
  reconciled (bad name, unknown subnet, etc.).
- **No gateway/CIDR write-back**: the controller writes `ipAddress`/`macAddress`
  but not the gateway or mask, so the driver must separately read
  `Subnet.spec.{gateway,cidrBlock}` to build a usable address and routes.
  *Enhancement*: include gateway/CIDR in the IP object's status.
- **No LSP creation**: reserving an IP does not create the OVN Logical Switch
  Port (see Gap #4).

### 3. Annotation polling fragility (resolved by the IP-CRD path)

The earlier annotation-based flow polled `pod.metadata.annotations` every 500ms
for up to 30s waiting for `<provider>.kubernetes.io/allocated="true"`. This was
racy and coupled IPAM to pod-object mutation. The `ips.kubeovn.io` path replaces
it: the driver creates the IP object and waits on its `spec.ipAddress` (a
dedicated object, watchable, no pod mutation). Noted here only to document the
migration.

### 4. Reserved IP does not create/bind the OVN Logical Switch Port

For DRA-attached secondary NICs the OVS port only binds and forwards once an LSP
exists on the subnet's logical switch, but `handleAddReservedIP` reserves the
address **without** creating the LSP — LSP creation lives in the pod-processing
path, which is NAD/annotation-driven (Gap #1). So a Multus-free attach via DRA has
no controller-created LSP; the driver would have to create it via OVS directly
(risking stale LSPs on crash). **This affects both OVN overlay and VLAN underlay**
— a live run showed the underlay port also needs a bound LSP to forward; the
earlier assumption that VLAN underlay is pure L2 with no LSP was wrong.

**Enhancement request**: a supported way to have the controller create/bind the
LSP for a reserved IP (pod+provider), or a `LogicalSwitchPort` CRD the controller
owns and garbage-collects — see also Gap #6.

### 5. No per-interface VLAN subnet on non-existent `eth1`

**Context**: For `ProviderNetwork`-backed VLAN subnets, kube-ovn requires the
physical NIC (`eth1`) to already exist on the node. In environments without a
dedicated secondary NIC (e.g. kind clusters, VMs with a single interface),
kube-ovn's `ovs-ovn` pod fails to configure the provider bridge.

**Enhancement request**: Allow a `ProviderNetwork` to be in a "pending" state
when the physical NIC doesn't exist, rather than failing. This would make the
VLAN subnet available once `eth1` appears (e.g. after Containerlab wires it in).

### 6. OVN Logical Switch Port lifecycle

**Problem**: Following from Gap #4, for DRA-attached secondary NICs (overlay and
VLAN underlay alike) a Multus-free driver ends up creating/binding the LSP via OVS
directly (`external_ids:iface-id`), bypassing kube-ovn-controller. This can leave
stale LSPs if the driver crashes between creation and cleanup, and duplicates
logic the controller already owns.

**Enhancement request**: A `LogicalSwitchPort` CRD (or extend `Subnet` status
with an `activePorts` list) so that kube-ovn-controller owns LSP lifecycle and
garbage-collects orphaned ports.

---

## Questions

1. Is creating an `ips.kubeovn.io` object (named
   `PodNameToPortName(podName, namespace, provider)`, with
   `spec.{subnet,podName,namespace}`) the supported way to pre-reserve an
   address outside the pod/NAD flow? Is the "name must equal `PodNameToPortName`"
   coupling intentional and stable, and could a mismatch surface an error/event
   instead of being silently ignored?

2. Is there a supported way to drive secondary-network IPAM for a
   provider/subnet **without** a `NetworkAttachmentDefinition`, for NRI/DRA
   drivers that don't use Multus? (See Gap #1.)

3. For a reserved IP, is there (or could there be) a supported path to also
   create/bind the OVN Logical Switch Port, so an **overlay** port comes up
   without the full Multus pod-processing flow? (See Gaps #4/#6.)

4. Is there interest in an official `kube-ovn-dra-driver` integration guide or
   example in the kube-ovn docs/repository?

---

## References

- [DRA reference implementation (NIC profile)](https://github.com/soer3n/kube-ovn-dra-driver)
- [KEP-3063: Dynamic Resource Allocation](https://github.com/kubernetes/enhancements/issues/3063)
- [KEP-4815: DRA Partitionable Devices](https://github.com/kubernetes/enhancements/issues/4815)
- [kubernetes-sigs/multi-network-api](https://github.com/kubernetes-sigs/multi-network-api)
- [Architecture doc](https://github.com/soer3n/kube-ovn-dra-driver/blob/main/docs/architecture.md)
