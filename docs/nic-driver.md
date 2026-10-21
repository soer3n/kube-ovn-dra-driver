# kube-ovn NIC DRA Driver

A Dynamic Resource Allocation (DRA) driver for kube-ovn virtual NICs, built on top of the `kubernetes-sigs/dra-example-driver` scaffold. A pod gets secondary NICs (`net1`, `net2`, …) from `ResourceClaim`s, without Multus, a `NetworkAttachmentDefinition` or the `k8s.v1.cni.cncf.io/networks` annotation.

## Architecture

kube-ovn owns the network: addresses, MACs, logical switch ports, ip CRs and their lifecycle. kube-ovn-controller, started with `--enable-dra-nic`, treats every device allocated by this driver (`--dra-nic-driver-name`, default `nic.kubeovn.io`) as a third network source next to the default network and Multus attachments. The driver publishes the subnets and plugs the interfaces.

```
┌──────────────────────────────────────────────────────────────────┐
│  ResourceSlice: one pool per Subnet (central, default),          │
│  or one pool per node (node mode)                                │
│  Device: subnet-<name>   (AllowMultipleAllocations, capacity)    │
│    attributes: subnetName, subnetType (vlan|ovn), vlanId,        │
│                providerNetwork, provider, vpc                    │
└──────────────────────────────────────────────────────────────────┘
        ↑ published by the publisher              ↓ allocated by kube-scheduler
          (or the kubelet plugins)
┌──────────────────────────┐        ┌──────────────────────────────────┐
│ kubelet plugin (DaemonSet)│        │ kube-ovn-controller              │
│ cmd/kube-ovn-dra-         │        │ --enable-dra-nic                 │
│ kubeletplugin             │        │  device → subnet → provider      │
│                           │        │  IPAM, LSP, ip CR, annotations   │
│ NodePrepareResources      │◀──────│  <provider>.kubernetes.io/*      │
│  wait for allocated=true  │ pod    └──────────────────────────────────┘
│  (pkg/nicprepare)         │ annotations
│                           │
│ NRI RunPodSandbox         │        veth into the pod netns, host end on
│  attach (pkg/plumbing) ───┼──────▶ br-int with external_ids:iface-id=<LSP>
│ NRI StopPodSandbox        │        → ovn-controller binds the port
│  detach                   │
└──────────────────────────┘
```

The contract between both sides is written up in kube-ovn's `docs/dra-nic.md`.

## Packages

| Path | Purpose |
|------|---------|
| `api/kube-ovn.io/resource/nic/v1alpha1/` | `NicConfig` opaque config (`interfaceName`) |
| `internal/profiles/nic/` | NIC device profile — enumerates kube-ovn Subnets via the dynamic client (node publish mode) |
| `internal/publisher/` | Central publisher — one pool per Subnet, free addresses as capacity (`kube-ovn-dra-kubeletplugin publisher`) |
| `pkg/annotation/` | Reads kube-ovn-controller's allocation from the pod annotations |
| `pkg/nicprepare/` | Resolves a claimed NIC: pod, subnet, provider key, logical switch port, addresses |
| `pkg/plumbing/` | Veth/OVS/netns attach, KubeVirt bridge + tap + DHCP, NRI sandbox handler, `NICStore` |
| `cmd/kube-ovn-dra-kubeletplugin/` | kubelet plugin: ResourceSlices, prepare/unprepare, NRI plugin |
| `cmd/kube-ovn-dra-webhook/` | Optional validating webhook for `NicConfig` |

## Allocation and attach flow

1. **Devices.** The driver publishes each kube-ovn `Subnet` as device `subnet-<name>` with the attribute `nic.kubeovn.io/subnetName`: by default the publisher, one pool per Subnet (see [Publish modes](#publish-modes)), in node mode the NIC profile of every kubelet plugin. Both watch Subnets and Vlans and republish within seconds of a change. kube-ovn-controller reads that attribute from the newest ResourceSlice generation of the device's pool. The subnet must have a dedicated `spec.provider` (e.g. `<subnet>.<namespace>.ovn`); the default provider `ovn` belongs to `eth0` and is rejected by both sides.
2. **Allocation.** The scheduler allocates the device to the pod's claim and binds the pod. Once the pod is bound and all its claims are allocated, kube-ovn-controller allocates IP and MAC, creates the logical switch port and the ip CR `<name>.<namespace>.<provider key>`, and writes the pod annotations `<provider key>.kubernetes.io/{ip_address,mac_address,cidr,gateway,logical_switch,allocated}`. `<name>` is the pod name, or the VM name for a KubeVirt VM with keep-vm-ip, in which case `<provider key>.kubernetes.io/virtualmachine` is set as well.
3. **Provider key.** A NIC alone on its provider in the pod is keyed by the provider. NICs of one pod sharing a provider are keyed by `<provider>.<interfaceName>`, the rule kube-ovn uses for repeated Multus attachments of one NAD. `interfaceName` comes from the `NicConfig` that applies to the request (default `net1`).
4. **Prepare.** `NodePrepareResources` resolves the pod from the claim's `reservedFor` and rejects an interface name that another NIC of the pod already uses, in the claim or in another claim of the pod; such a pod would otherwise fail later, when the sandbox starts. Then `nicprepare.RequestIPAM` waits up to `--allocation-timeout` (default 30 s) for `allocated="true"` under either provider key and reads the result. Dual-stack values are comma separated; the driver configures all addresses on pod interfaces. The NICs of a claim are resolved concurrently. Each NIC becomes a `plumbing.Spec` in the `NICStore`, keyed by pod and claim, and in the checkpoint, which restores the store after a plugin restart. The Specs stay until the claim is unprepared, so a recreated sandbox of the pod gets its NICs again.
5. **Attach.** The NRI `RunPodSandbox` hook reads the pod's Specs, fills in the netns path and sandbox ID, and runs `Attach` per NIC: create a veth pair (names hashed from sandbox ID and interface, IFNAMSIZ-safe), move the pod end into the netns and rename it, set MAC, MTU, addresses (IPv6 without DAD) and per-NIC routes (never a default route), then add the host end to `br-int` with `external_ids:iface-id=<logical switch port>` plus `vendor=kube-ovn`, `pod_name`, `pod_namespace`, `ip`, `pod_netns` and the owner mark `kube-ovn-dra-driver=nic`. If one NIC fails, the NICs already attached are detached again and the sandbox fails. When the plugin (re)connects, NRI `Synchronize` runs the same attach for every running sandbox, which covers a pod started while the plugin was down; `Attach` is idempotent and skips what exists.
6. **Detach.** The NRI `StopPodSandbox` hook detaches the NICs it attached and then removes every OVS port with the pod's owner mark, which also covers pods attached before a plugin restart. A keep-vm-ip VM's port outlives its pods, so a leftover port would steal the binding from the next pod.
7. **Release.** Nothing: `NodeUnprepareResources` only drops the NICs from the store, the checkpoint and CDI spec. kube-ovn-controller releases the address when the pod is deleted, or keeps it for a VM that still exists, as for Multus attachments.

### Overlay and underlay

Both subnet types take the same path. For a VLAN underlay subnet, kube-ovn-controller creates the logical switch port as well, and the logical switch reaches the VLAN through its localnet port on the provider bridge. The `subnetType`, `vlanId` and `providerNetwork` attributes only help claims select subnets.

> OVN 26.03's northd derives `Logical_Switch_Port.tag` from `tag_request`, and kube-ovn only writes `tag`, so VLAN subnets lose their tag on clusters with that OVN. `make kind-vlan-tag-workaround` (run by `kind-deploy-kube-ovn-fixtures`) sets `tag_request` on the localnet ports until kube-ovn is fixed.

### KubeVirt VMs

QEMU needs a tap device, not a veth. For pods labelled `kubevirt.io=virt-launcher`, `Attach` also renames the veth out of the way, creates a Linux bridge and a tap device named like the NIC (owned by the qemu user, uid 107) and enslaves both to the bridge.

Where the guest gets its addresses from depends on the Subnet:

| Subnet | DHCP for the guest | Families | Routes of the NIC's routes annotation |
| --- | --- | --- | --- |
| `enableDHCP: false` (default) | a single-client DHCPv4 server of the driver on the bridge, with the allocated address, gateway and routes | IPv4; the guest gets the IPv4 address of a dual-stack NIC, an IPv6-only NIC gets no DHCP server | yes, as option 121 |
| `enableDHCP: true` | OVN's DHCP, the driver starts no server | IPv4, and IPv6 by DHCPv6 and router advertisements with `enableIPv6RA: true` | no, OVN's DHCP options are per Subnet (`dhcpV4Options`) |

With OVN's DHCP and router advertisements, the guest config needs `dhcp6`, `accept-ra` and, for networkd, the same `use-routes` override for both families; the router advertisements also give the guest an IPv6 default route via the NIC.

KubeVirt's built-in bindings do not resolve DRA networks yet, so a KubeVirt `Plugin` adds the tap to the VM's domain: [`deployments/kubevirt`](../deployments/kubevirt) registers the `kube-ovn-nic` binding without a sidecar and a CEL hook that appends an `ethernet` interface per such VMI interface, with the MAC the driver writes on the VMI (`kubeletPlugin.annotateVMIMAC=true`). It needs KubeVirt v1.9 or later with the feature gates `NetworkDevicesWithDRA` and `Plugins`; the manifest is in the v1.9 form (`domainHooks`), and `make kind-deploy-kubevirt` converts it for v1.10 (`launcherHooks`). Where the `Plugins` gate cannot be enabled, the [kube-ovn network binding plugin](https://github.com/soer3n/kube-ovn-network-binding-plugin) does the same as a sidecar. kube-ovn names the VM's port after the VM, so IP and MAC survive VM restarts.

The bridge, tap and DHCP server are a workaround as well: once KubeVirt's `managedTap` attachment resolves DRA networks, KubeVirt creates the tap itself, and the driver only needs to hand over the veth. The binding plugin's README describes the expected migration, and why reporting the NIC in the ResourceClaim status (`status.devices[].networkData`) would also remove the interface naming contract between claim templates and VMs.

### Failure behavior

- **NRI fails closed.** The kubelet plugin prepares claims only while the container runtime has synchronized its NRI plugin. Without NRI nobody would attach the NIC when the sandbox starts, so prepare fails and kubelet retries it. The plugin reconnects with a backoff; on reconnect the runtime synchronizes all running sandboxes, which attaches the NICs of sandboxes created in between. Attaches that fail during that sync are retried every 10 s until they succeed or the sandbox stops. The `readiness` health service reports NRI state.
- **Waiting for kube-ovn.** Prepare waits up to `--allocation-timeout` (default 30s) for kube-ovn-controller's allocation and then fails, and kubelet retries.
- **MTU and routes.** The MTU follows kube-ovn-cni: the Subnet's `spec.mtu`, else the ProviderNetwork MTU of an underlay Subnet without a logical gateway, else the MTU of the pod's `eth0`. Extra routes come from the NIC's `<provider key>.kubernetes.io/routes` annotation; a default route is refused.

### Recorded providers

kube-ovn-controller records the subnet and provider of each NIC on its claim when it first resolves it (`ovn.kubernetes.io/dra_providers`, JSON keyed by `<pool>/<device>`), and keeps using them. The driver reads the record too and waits for the kube-ovn annotations of the recorded provider, so a NIC keeps working when an admin changes the subnet's `spec.provider` afterwards; only NICs of new claims use the new provider.

### Shared claims and live migration

A claim stands for one NIC with one address, so only the virt-launcher pods of one KubeVirt VM may share it: the source and the target pod of a live migration, which kube-ovn gives the VM's address (keep-vm-ip). Each node prepares the claim for the pod scheduled on it. Any other sharing, or two consumers on one node, is rejected.

With a `ResourceClaimTemplate`, KubeVirt creates a new claim for the migration target pod. The scheduler counts it against the pool's capacity although the VM keeps its address, so a full pool blocks the migration until it is retried with a free address. A named `ResourceClaim` per VM, referenced by `resourceClaimName`, avoids that: source and target pod share it. Creating such claims per VM is left to the user for now. KubeVirt allows that up to v1.10.0-beta.0; from 1.10.0 it refuses to migrate a VM whose DRA network uses a named claim ([kubevirt/kubevirt#19468](https://github.com/kubevirt/kubevirt/pull/19468)), so such VMs then need a claim template and a free address.

### Tenants

The chart creates one DeviceClass, `nic.kubeovn.io`, which offers every published Subnet. For tenants, an admin creates a DeviceClass per tenant whose selector only matches that tenant's Subnets, by VPC or by a list of Subnet names ([example](../demo/nic-example/examples/tenant-deviceclasses.yaml)). A claim through a tenant class only gets that tenant's Subnets; its own selectors can narrow that, never widen it.

Selectors must not fail on any device: in DRA a CEL error fails the whole allocation instead of skipping the device. The driver leaves `vpc` and `provider` out of a device whose Subnet has none, so read them as `device.attributes['nic.kubeovn.io'].?vpc.orValue('')`; `subnetName` and `subnetType` are set on every device.

A DeviceClass does not control who may use it. Any namespace that may create ResourceClaims can still name another tenant's class or the generic one. Until the driver brings an admission check for that, restrict it with a ResourceQuota per tenant namespace that sets `<class>.deviceclass.resource.k8s.io/devices: 0` for the generic class and the other tenants' classes.

### MVP limitations

- Only Subnets with a dedicated OVN provider (`<name>.<namespace>.ovn`) and a CIDR are published; the default provider, non-OVN providers and MAC-only underlay Subnets are not.
- The driver's VM DHCP server is IPv4 only; on a Subnet without `enableDHCP` a VM gets the IPv4 address of a dual-stack NIC and an IPv6-only NIC gets no address. Subnets with `enableDHCP` and `enableIPv6RA` give VMs both families from OVN.

## Local dev deployment on kind

A full kube-ovn + NIC DRA stack can be brought up in a local [kind](https://kind.sigs.k8s.io/) cluster. All targets live in the `Makefile` under the `kind-*` prefix and use `demo/kind/kind-no-cni.yaml` (default CNI disabled, DRA feature-gates enabled, `/lib/modules` mounted for OVS).

### Prerequisites

`kind`, `kubectl`, `helm`, `docker`, `curl`, `tar` (verified by `make kind-check-deps`). The VLAN connectivity test additionally needs `containerlab` and `sudo`. kube-ovn is deployed from the chart of `KUBE_OVN_VERSION` in the sibling checkout `../kube-ovn` (`KUBE_OVN_REPO`) with `func.ENABLE_DRA_NIC=true`, using an image built from that branch (`KUBE_OVN_IMAGE`).

**Kubernetes version:** the driver uses the stable DRA API (`resource.k8s.io/v1`), which requires **Kubernetes 1.34+**; the driver is built with the `k8s.io/*` v0.35 libraries, and the demo and e2e run on **Kubernetes 1.36**. `kind-create` pins the node image via `KIND_NODE_IMAGE` (default `kindest/node:v1.36.4`, the 1.36 image of **kind v0.33.0**, pinned by digest). Use kind v0.33.0 with it; kind node images are built for a specific kind release. Override to test another release:

```bash
make kind-create KIND_NODE_IMAGE=kindest/node:v1.36.4
```

### One-shot demo

```bash
# Create cluster, wire VLAN uplink via containerlab, deploy kube-ovn (+ Multus
# for the benchmark), build & load the driver image, install the chart, apply
# the NIC example.
make kind-demo

# Tear down
make kind-delete
```

### Step by step

```bash
make kind-create            # kind cluster, no CNI, DRA gates on (nodes NotReady until CNI)
make clab-deploy            # OPTIONAL: containerlab VLAN uplink + FRR gateway (needs sudo)
make kind-deploy-kube-ovn   # install kube-ovn CNI via its Helm chart -> nodes become Ready
make kind-deploy-multus     # OPTIONAL: Multus (thick mode), only for make nic-bench
make kind-deploy-nic-prereqs # provider network, VLANs, subnets
make kind-build-driver      # docker build -> kind load docker-image docker.io/soer3n/kube-ovn-dra-driver:dev
make kind-deploy-driver     # helm install kube-ovn-nic-dra (deviceProfile=nic)
make kind-deploy-nic-example # ResourceClaim + demo pod
```

### Disabling kind's default CNI and installing kube-ovn

kind ships with kindnet as its default CNI. kube-ovn must own the pod network, so kindnet is disabled at cluster-creation time in `demo/kind/kind-no-cni.yaml`:

```yaml
networking:
  disableDefaultCNI: true          # no kindnet — kube-ovn takes over
  podSubnet: "10.244.0.0/16"       # kube-ovn POD_CIDR must match
  serviceSubnet: "10.96.0.0/12"
```

Each node also mounts `/lib/modules` (read-only) so OVS can load its kernel modules, and enables the `DynamicResourceAllocation` and `DRAConsumableCapacity` feature gates on the apiserver, controller-manager, scheduler and kubelet (DRA is GA from 1.34; the second gate is needed for shared Subnets and capacity on 1.34 and 1.35).

With no CNI installed, **nodes stay `NotReady` after `make kind-create` — this is expected.** `make kind-deploy-kube-ovn` then installs the kube-ovn Helm chart (version `KUBE_OVN_VERSION`), which:

1. Labels the control-plane node `kube-ovn/role=master`.
2. Fetches the chart for the pinned version and `helm upgrade --install`s it into `kube-system` with `POD_CIDR`/`SVC_CIDR`/`JOIN_CIDR` matching the kind config and `func.ENABLE_DRA_NIC=true`.
3. Waits for all nodes to report `Ready` — at which point the CNI is live.

Verify kube-ovn on its own before adding the NIC driver:

```bash
make kind-kube-ovn-status   # component pods, node status, kube-ovn subnets
make kind-test-kube-ovn     # two pods on the default overlay + pod-to-pod ping
make kind-test-kube-ovn-clean  # remove the smoke-test pods
```

`kind-test-kube-ovn` applies `demo/kind/kube-ovn-smoke-test.yaml` (two `netshoot` pods), waits for them to be Ready, and pings one from the other — a quick confirmation that the overlay datapath works.

### Underlay / VLAN-backed networks via containerlab

The VLAN demo uses [containerlab](https://containerlab.dev/) to give the kind nodes a second NIC (`eth1`) wired to a shared Linux bridge, plus an FRR container `frr-gw` as the gateway of the VLAN subnets. Topology (`demo/containerlab/vlan-topology.yaml`):

```
  ┌───────────────────┐         ┌───────────────────┐
  │ control-plane eth1│──port1──┤                   │
  ├───────────────────┤         │   vlan-switch     │
  │ worker eth1       │──port2──┤  (Linux bridge,   │
  └───────────────────┘         │  VLAN filtering)  │
  ┌───────────────────┐         │                   │
  │ frr-gw eth1 (trunk)│─port3──┤                   │
  │  eth1.100 … eth1.900│        └───────────────────┘
  │  .253 gateways     │
  └───────────────────┘
```

`make clab-deploy` (needs `sudo` + `containerlab`):

1. (Re)creates the host bridge `vlan-switch` with `vlan_filtering 1`.
2. Deploys the topology: wires each node's `eth1` and the `frr-gw` trunk into the bridge, and gives `frr-gw` one sub-interface per VLAN with the gateway address (`eth1.100` … `eth1.800` with `.253` for the demo VLANs, `eth1.900` for the capacity demo, `eth1.1101` … `eth1.1108` with `.1` for the benchmark VLANs).
3. Allows those VLANs on the bridge ports `port1`–`port4`.
4. SNATs the demo VLAN subnets out of the host's default interface (`kind-vlan-nat`), so a VM on a VLAN can reach the internet.

`frr-gw` runs in the host network namespace, so its sub-interfaces are host interfaces: pods on a VLAN reach their gateway over L2, and the host has a connected route to every VLAN subnet, without `ip route add` and without a routing protocol.

Run **`make clab-deploy` after `make kind-create` but before `make kind-deploy-kube-ovn`**, so `eth1` exists when kube-ovn programs the provider network.

> **This containerlab gateway is a self-contained kind stand-in, not the production topology.** In a kubermatic-virtualization deployment the `virtualization.k8c.io/EdgeRouter` serves as the VLAN subnet's routed gateway and north-south edge: it attaches directly to the subnet (`peers.internal`) as the gateway and advertises it to the upstream fabric (`peers.external`, with BFD/VRF/EVPN). Pods on a VLAN subnet are reachable over L2 there as well, so neither setup needs `kube-ovn-speaker`, which announces OVN overlay pod IPs over BGP.

#### How the NIC driver consumes the VLAN underlay

`demo/nic-example/00-kube-ovn-subnets.yaml` defines the kube-ovn objects the DRA driver enumerates into ResourceSlices:

```yaml
ProviderNetwork external   # defaultInterface: eth1   (the containerlab uplink)
Vlan vlan100  (id 100, provider external)  ─┐
Subnet vlan100-subnet 172.23.0.0/24  vlan: vlan100   provider: external.vlan100-subnet.ovn
Vlan vlan200  (id 200, provider external)  ─┐
Subnet vlan200-subnet 172.24.0.0/24  vlan: vlan200   provider: external.vlan200-subnet.ovn
```

kube-ovn creates no sub-interfaces in the nodes; it bridges `eth1` into `br-external`, and the NIC profile publishes one `subnet-<name>` device (`subnetType=vlan`, `vlanId=...`) per subnet. A pod then selects the VLAN it wants via a CEL selector on its `ResourceClaim` — e.g. `subnetName == "vlan100-subnet"`. kube-ovn-controller creates the NIC's port on the VLAN subnet's logical switch, and the driver plugs the NIC into `br-int`; OVN forwards its traffic through the localnet port to `br-external` and the `eth1` trunk with tag 100.

End-to-end VLAN path:

```bash
make kind-create
make clab-deploy            # eth1 uplink + FRR gateway  (sudo)
make kind-deploy-kube-ovn
make kind-deploy-nic-prereqs  # provider network, VLANs, subnets
make kind-build-driver
make kind-deploy-driver
make kind-deploy-nic-example  # claims, demo pod
make kind-test-vlan           # dual-VLAN traffic + host routing + isolation checks
```

`make kind-vlan-status` shows the gateway sub-interfaces and the host routes to the VLAN subnets; `make clab-destroy` tears down the topology and bridge.

### Iterating on the driver

After a code change, rebuild and reload the image, then restart the plugin:

```bash
make kind-build-driver
make kind-deploy-driver     # helm upgrade --install, pullPolicy=Never uses the loaded image
kubectl -n kube-system rollout restart ds/kube-ovn-nic-dra-kube-ovn-dra-driver-kubeletplugin
```

### Verifying

```bash
# DRA driver published its devices
kubectl get resourceslices

# Demo pod NICs
kubectl exec multi-nic-demo -- ip addr

# Dual-VLAN tagged-traffic + host-routing test (requires clab-deploy first)
make kind-test-vlan
```

Tunable variables (override on the `make` command line):

| Variable | Default | Purpose |
|----------|---------|---------|
| `KIND_CLUSTER_NAME` | `nic-dra-demo` | kind cluster name |
| `KIND_NODE_IMAGE` | `kindest/node:v1.36.4` | Kubernetes node image (1.34 or later; the demo and e2e run on 1.36) |
| `PUBLISH_MODE` | `central` | Who publishes the devices: `central` (publisher, one pool per Subnet, capacity) or `node` (every kubelet plugin) |
| `KUBE_OVN_VERSION` | `dra-nic-upstream` | kube-ovn git ref to take the chart from — the branch with `--enable-dra-nic`, not a release tag |
| `KUBE_OVN_REPO` | `../kube-ovn` | local kube-ovn checkout holding `KUBE_OVN_VERSION` |
| `KUBE_OVN_IMAGE` | `docker.io/soer3n/kube-ovn:dra-driver-<version>` | kube-ovn image built from `KUBE_OVN_VERSION` |
| `MULTUS_VERSION` | `v4.2.3` | Multus daemonset version |
| `NIC_DRIVER_NAME` | `nic.kubeovn.io` | DRA driver name (DeviceClass, ResourceSlices) |

## Testing

| Command | Scope |
|---------|-------|
| `make test` | unit tests of all packages, plus `logcheck` |
| `make test-privileged` | `pkg/plumbing` datapath tests: veth, netns, addresses, routes, KubeVirt bridge/tap/DHCP in throwaway network namespaces (needs root; OVS is not involved) |
| `make setup-e2e test-e2e teardown-e2e` | kind e2e: ResourceSlices and the central pools with their capacity, allocation by kube-ovn-controller and the address on the pod NIC, release on pod deletion, two NICs on one subnet, `firstAvailable`, overlay ping between pods, per-tenant DeviceClasses, MTU and routes, driver restart, sandbox recreation and a containerd restart (NRI reconnect) |
| `E2E_CONTAINERLAB=1` | adds the VLAN underlay specs: plugging, the ping to the FRR gateway and the OVS wiring through the provider network, VLAN pools only on nodes where the ProviderNetwork is ready |
| `E2E_KUBEVIRT=1` | two workers and KubeVirt with the Plugin from `deployments/kubevirt`: live migration with a named claim on a full pool and with a claim template, a foreign pod refused the VM's claim, stop and start, a VM on a Subnet with OVN's DHCP; with `E2E_CONTAINERLAB=1` also a VM with an overlay and a VLAN NIC, from named claims and from claim templates |
| `KUBEVIRT_VERSION` | with `E2E_KUBEVIRT=1`: the KubeVirt release, default `v1.9.0` (the latest release); `v1.10.0-beta.0` works too, `make kind-deploy-kubevirt` then converts the Plugin to v1.10's `launcherHooks` |
| `E2E_RWX=1` | with `E2E_KUBEVIRT=1`: CDI and NFS, live migration of a VM with a DataVolume, with a named claim and with a claim template |
| `E2E_PUBLISH_MODE=node` | the driver in node publish mode: one pool per node without capacity, VLAN subnets only in the pools of the nodes where the ProviderNetwork is ready, and a pod past a subnet's free addresses scheduled, starting once an address is released; the central pool specs, the KubeVirt specs that fill a pool and the migration of the two-NIC VM with named claims (a claim is bound to one node) skip |
| `E2E_IP_FAMILY=dual` | a dual-stack cluster: NICs on dual-stack and IPv6-only subnets; with `E2E_KUBEVIRT=1` a VM on a dual-stack subnet with the driver's DHCP (IPv4) and one with OVN's DHCP and router advertisements (both families) |

The variables are combined freely and passed the same to `setup-e2e`, `test-e2e` and `teardown-e2e`.

kube-ovn's own `make kube-ovn-dra-e2e` covers the controller side without a driver, using a static ResourceSlice.

## Shared subnets (multiple pods, one Subnet)

A kube-ovn Subnet is a **shared IP pool**, not an exclusive device. DRA, however, allocates a plain `Device` **exclusively** — so until this was addressed, a second pod selecting the same subnet stayed `Pending`. Verified live:

```
$ kubectl get pod share-a share-b
NAME       STATUS
share-a    Running       # got subnet-ovn-subnet
share-b    Pending       # FailedScheduling: 1 cannot allocate all claims
```

**Fix (driver-only).** Every subnet device is published with `AllowMultipleAllocations: true`, by the publisher and by `internal/profiles/nic` in node mode, so many claims can allocate the same subnet device; each pod still gets its own address, MAC and logical switch port from kube-ovn-controller. The subnet genuinely has hundreds of IPs.

- **Feature gate:** `AllowMultipleAllocations` (and the richer consumable-capacity API) are gated by **`DRAConsumableCapacity`** — *alpha, default-off* in k8s 1.34 and 1.35, *beta and on by default* from 1.36. On 1.34 and 1.35 it must be enabled on **apiserver + scheduler + kubelet**; the kind config (`demo/kind/kind-no-cni.yaml`) sets it explicitly, so it works on all of them.
- **Demo/test:** `demo/nic-example/examples/shared-subnet.yaml` (two pods on `ovn-subnet`, overlay → no containerlab) and the e2e spec *"connects pods that share a subnet over the overlay"*.

**`AllowMultipleAllocations` vs consumable capacity, by publish mode:**

| | node mode: `AllowMultipleAllocations: true` | central mode (default): plus consumable capacity (`Device.Capacity` + `RequestPolicy`) |
|---|---|---|
| Effect | unlimited sharing of the device | each request consumes one address of a finite pool |
| Scheduler enforces IP exhaustion | ❌ no | ✅ yes (refuses to schedule when the pool is full) |
| Driver work | one field | the publisher publishes the free addresses and keeps them current |
| Same gate | `DRAConsumableCapacity` | `DRAConsumableCapacity` |

`AllowMultipleAllocations` is the minimal fix that makes shared subnets work. Consumable capacity, published by the central publisher (see [Publish modes](#publish-modes)), also makes **IP-pool exhaustion a scheduling decision**, a day-0 advantage over Multus. See [`kndm-dranet-comparison.md`](kndm-dranet-comparison.md).

## Publish modes

The driver can publish its devices in two ways, chosen with the Helm value `publisher.enabled` (the kubelet plugins follow with `--publish-mode`).

| | node mode (`publisher.enabled=false`) | central mode (default) |
|---|---|---|
| Who publishes | every kubelet plugin | one leader-elected `publisher` Deployment, same image |
| Pools | one per node, named after the node, with the Subnets the node can serve | one per Subnet, named after the Subnet |
| Overlay Subnet | on every node | available on all nodes |
| VLAN underlay Subnet | in the pools of the nodes where its ProviderNetwork is ready | only on the nodes where its ProviderNetwork is ready |
| IP-pool capacity | none | free addresses per address family |

**Why central.** Capacity is counted per device. With one pool per node every node would get a full pool of its own, so the cluster could hand out many times the real number of addresses. A Subnet is one pool for the whole cluster, so it is one device with a cluster-wide scope. That scope also fixes the placement of VLAN Subnets: only the nodes that carry the provider bridge and its uplink can serve a VLAN NIC. kube-ovn labels them `<provider>.provider-network.kubernetes.io/ready=true`, and the pool of a VLAN Subnet selects on that label. kube-ovn-controller needs no change, it already looks a device up by driver and pool name.

**Capacity.** Per address family the device publishes

    capacity = free + held

where `free` is the free-address count of the Subnet status and `held` is the number of DRA claims allocated to the device whose address kube-ovn already allocated. The scheduler subtracts every allocated claim from the capacity. A claim whose address is allocated is in the free count already, so it is added back, and a claim that waits for its address is counted by the scheduler alone. The scheduler therefore sees the addresses that are still free, also while a burst of pods waits for kube-ovn. A request policy gives one address per NIC by default, so claims need no capacity request. A stopped keep-vm-ip VM keeps its address, which the free count already reflects.

The numbers follow the Subnet status and the ip objects, so they are eventually consistent: in a small window a pod can still be scheduled when the last address is taken by a pod outside DRA, and then fails when kube-ovn allocates, as it would without capacity.

**Known scheduler issue:** while claims of a pool are being bound, kube-scheduler (1.36, also current master) counts their consumed capacity twice, from its assume cache and as in-flight allocations. When a burst of pods fills a pool, it can refuse a pod the pool still has room for. Usually a later binding requeues it within a second; when nothing follows, as for the last pod of a burst, it waits until the scheduler flushes its unschedulable pods after 5 minutes. Either way it is a delay, not a lost pod. A fix is proposed upstream; the e2e capacity spec fills its pool one pod at a time meanwhile.

Turn capacity off with `publisher.capacity=false`. The chart and the Makefile deploy central mode by default (`PUBLISH_MODE=central`); use `PUBLISH_MODE=node` for the other. The e2e suite runs in central mode unless it is set up with `E2E_PUBLISH_MODE=node`; the specs of each mode skip in the other.
