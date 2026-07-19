# kube-ovn NIC DRA Driver

A Dynamic Resource Allocation (DRA) driver for kube-ovn virtual NICs,
built on top of the `kubernetes-sigs/dra-example-driver` scaffold.

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│  ResourceSlice (per node)                                       │
│  Device: subnet-<name>                                          │
│    attributes: subnetName, subnetType (vlan|ovn), vlanId,       │
│                provider, vpc                                    │
└─────────────────────────────────────────────────────────────────┘
              ↑ published by
┌─────────────────────────────────────────────────────────────────┐
│  kubelet DRA plugin (DaemonSet)                                 │
│  cmd/kube-ovn-dra-kubeletplugin  --device-profile=nic            │
│                                                                 │
│  PrepareResourceClaims                                          │
│    → annotation.RequestSubnet  (write ovn annotation to pod)   │
│    → annotation.WaitForAllocation  (poll for kube-ovn response) │
│      — OR — kubeovnip.Reserve  (create ips.kubeovn.io object)   │
│                                                                 │
│  UnprepareResourceClaims                                        │
│    → annotation.ReleaseSubnet / kubeovnip.Release               │
└─────────────────────────────────────────────────────────────────┘
```

> **Current scope:** the driver handles the **full** secondary-NIC lifecycle for
> `net1`/`net2`/… — it reserves an address/MAC from the chosen kube-ovn Subnet
> (`pkg/kubeovnip`) **and** performs the netns attach (veth + OVS port via
> `pkg/plumbing` + an NRI hook), so **no Multus is required**. Validated
> end-to-end for both OVN overlay and VLAN underlay, given the kube-ovn
> controller patch. A Multus-compatible IPAM-only mode (`pkg/annotation`) is also
> kept. See *Multus-free secondary NICs — implemented and validated* below.

## New packages

| Path | Purpose |
|------|---------|
| `api/kube-ovn.io/resource/nic/v1alpha1/` | `NicConfig` CRD type (interfaceName) |
| `internal/profiles/nic/` | NIC device profile — enumerates kube-ovn Subnets via dynamic client |
| `pkg/annotation/` | Writes/watches kube-ovn IPAM annotations on pods |
| `pkg/kubeovnip/` | Multus-free IPAM: reserves `ips.kubeovn.io` objects. (Also contains a `PodNetwork`/multi-network-api reservation sketch — see note below.) |
| `pkg/nicprepare/` | Orchestrates the IPAM reservation lifecycle for a claimed NIC |
| `pkg/plumbing/` | Veth/OVS/netns attach + NRI sandbox hook — the driver-owned NIC attach (validated; see *Multus-free secondary NICs* below) |

> **multi-network-api / `PodNetwork` is an exploratory idea, not a committed
> path.** `pkg/kubeovnip/podnetwork.go`, `docs/multi-network-api-integration.md`,
> and the demo `PodNetwork` CRD sketch integration with
> [`kubernetes-sigs/multi-network-api`](https://github.com/kubernetes-sigs/multi-network-api),
> which is still pre-API (requirements + an early concept discussion, no API
> types yet). The schema used here is our own placeholder. It's kept to revisit
> if/when an upstream API materializes — the committed Multus-free path
> is `pkg/kubeovnip` (IP CRD) + `pkg/plumbing`, independent of it.

## IPAM flow

kube-ovn uses a pod-annotation convention for multi-NIC IPAM:

1. DRA driver writes:
   ```
   ovn.kubernetes.io/logical_switch_<ifaceName> = <subnetName>
   ```
2. `kube-ovn-controller` reconciles and writes back:
   ```
   ovn.kubernetes.io/ip_address_<ifaceName>  = 10.0.1.5/24
   ovn.kubernetes.io/mac_address_<ifaceName> = 00:11:22:33:44:55
   ovn.kubernetes.io/gateway_<ifaceName>     = 10.0.1.1
   ```
3. Driver reads response, plumbs kernel/OVS side.

## Device types

### VLAN-backed (`subnetType=vlan`)

OVS provider bridge (`br-<provider>`) + access port with VLAN tag.
The kube-ovn `Subnet` CR must reference a `Vlan` + `ProviderNetwork`.

### OVN overlay (`subnetType=ovn`)

OVS port on `br-int` with `external_ids:iface-id=<podName>.<ns>_<iface>`.
OVN controller automatically binds the Logical Switch Port to the chassis.

## Local dev deployment on kind

A full kube-ovn + Multus + NIC DRA stack can be brought up in a local
[kind](https://kind.sigs.k8s.io/) cluster. All targets live in the `Makefile`
under the `kind-*` prefix and use `demo/kind/kind-no-cni.yaml` (default CNI
disabled, DRA feature-gates enabled, `/lib/modules` mounted for OVS).

### Prerequisites

`kind`, `kubectl`, `helm`, `docker`, `curl`, `tar` (verified by
`make kind-check-deps`). The VLAN connectivity test additionally needs
`containerlab` and `sudo`.

**Kubernetes version:** the driver uses the stable DRA API
(`resource.k8s.io/v1`), which requires **Kubernetes 1.34+**; the repo is built
and tested against **1.35** (`k8s.io/*` deps pinned to `v0.35.x`). `kind-create`
pins the node image via `KIND_NODE_IMAGE` (default `kindest/node:v1.35.0`).
**kind v0.31.0** already defaults to v1.35.0, so a recent kind binary needs no
extra flags; older kind releases (≤ v0.29, which default to ≤ v1.33) won't serve
the stable DRA API. Override to test another release:

```bash
make kind-create KIND_NODE_IMAGE=kindest/node:v1.34.3
```

### One-shot demo

```bash
# Create cluster, wire VLAN uplink via containerlab, deploy kube-ovn + multus,
# build & load the driver image, install the chart, apply the NIC example.
make kind-demo

# Tear down
make kind-delete
```

### Step by step

```bash
make kind-create            # kind cluster, no CNI, DRA gates on (nodes NotReady until CNI)
make clab-deploy            # OPTIONAL: containerlab VLAN uplink + FRR BGP gateway (needs sudo)
make kind-deploy-kube-ovn   # install kube-ovn CNI via its Helm chart -> nodes become Ready
make kind-deploy-multus     # install Multus (thick mode)
make kind-build-driver      # docker build -> kind load docker-image nic.kubeovn.io:dev
make kind-deploy-driver     # helm install kube-ovn-nic-dra (deviceProfile=nic)
make kind-deploy-nic-example # subnets, PodNetwork, DeviceClass, ResourceClaim, demo pod
```

### Disabling kind's default CNI and installing kube-ovn

kind ships with kindnet as its default CNI. kube-ovn must own the pod network,
so kindnet is disabled at cluster-creation time in `demo/kind/kind-no-cni.yaml`:

```yaml
networking:
  disableDefaultCNI: true          # no kindnet — kube-ovn takes over
  podSubnet: "10.244.0.0/16"       # kube-ovn POD_CIDR must match
  serviceSubnet: "10.96.0.0/12"
```

Each node also mounts `/lib/modules` (read-only) so OVS can load its kernel
modules, and enables the `DynamicResourceAllocation` feature-gate on the
apiserver, controller-manager and kubelet (required for ResourceSlices).

With no CNI installed, **nodes stay `NotReady` after `make kind-create` — this
is expected.** `make kind-deploy-kube-ovn` then installs the kube-ovn Helm chart
(version `KUBE_OVN_VERSION`), which:

1. Labels the control-plane node `kube-ovn/role=master`.
2. Fetches the chart for the pinned version and `helm upgrade --install`s it
   into `kube-system` with `POD_CIDR`/`SVC_CIDR`/`JOIN_CIDR` matching the kind
   config.
3. Waits for all nodes to report `Ready` — at which point the CNI is live.

Verify kube-ovn on its own before adding Multus or the NIC driver:

```bash
make kind-kube-ovn-status   # component pods, node status, kube-ovn subnets
make kind-test-kube-ovn     # two pods on the default overlay + pod-to-pod ping
make kind-test-kube-ovn-clean  # remove the smoke-test pods
```

`kind-test-kube-ovn` applies `demo/kind/kube-ovn-smoke-test.yaml` (two
`netshoot` pods), waits for them to be Ready, and pings one from the other —
a quick confirmation that the overlay datapath works.

### Underlay / VLAN-backed networks via containerlab

The VLAN demo uses [containerlab](https://containerlab.dev/) to give the kind
nodes a second NIC (`eth1`) wired to a shared Linux bridge, plus an FRR BGP
gateway so the host can route into the pod VLANs. Topology
(`demo/containerlab/vlan-topology.yaml`):

```
  ┌───────────────────┐         ┌───────────────────┐
  │ control-plane eth1│──port1──┤                   │
  ├───────────────────┤         │   vlan-switch     │
  │ worker eth1       │──port2──┤  (Linux bridge,   │
  └───────────────────┘         │  VLAN filtering)  │
  ┌───────────────────┐         │                   │
  │ frr-gw eth1 (trunk)│─port3──┤                   │
  │  eth1.100 / eth1.200│        └───────────────────┘
  │  BGP ASN 65001     │
  └───────────────────┘
```

`make clab-deploy` (needs `sudo` + `containerlab`):

1. (Re)creates the host bridge `vlan-switch` with `vlan_filtering 1`.
2. Renders the FRR config with the live node IPs (`kind-frr-update-peers`).
3. Deploys the topology — wires each node's `eth1` and the FRR trunk into the
   bridge, brings up `eth1.100`/`eth1.200` sub-interfaces on FRR.
4. Allows VLAN 100 and 200 on bridge ports `port1`–`port3`.

FRR (ASN 65001) peers over eBGP with `kube-ovn-speaker` (ASN 65000) on each
node and redistributes the VLAN subnet routes into the host kernel via zebra —
so the host reaches pods without any manual `ip route add`.

Run **`make clab-deploy` after `make kind-create` but before
`make kind-deploy-kube-ovn`**, so `eth1` exists when kube-ovn programs the
provider network.

> **This containerlab FRR + `kube-ovn-speaker` setup is a self-contained kind
> stand-in, not the production topology.** In a kubermatic-virtualization
> deployment the `virtualization.k8c.io/EdgeRouter` serves as the VLAN subnet's
> routed gateway and north-south edge: it attaches directly to the subnet
> (`peers.internal`) as the gateway and advertises it to the upstream fabric
> (`peers.external`, with BFD/VRF/EVPN). That replaces **both** the demo
> `frr-gw` **and** `kube-ovn-speaker` — the speaker only exists to announce OVN
> *overlay* pod IPs over BGP, which is unnecessary for VLAN underlay where pods
> are directly L2-reachable on the VLAN. The demo peers with the speaker only
> because `frr-gw` is not itself the on-VLAN gateway and learns the routes via
> BGP redistribution instead.

#### How the NIC driver consumes the VLAN underlay

`demo/nic-example/00-kube-ovn-subnets.yaml` defines the kube-ovn objects the
DRA driver enumerates into ResourceSlices:

```yaml
ProviderNetwork external   # defaultInterface: eth1   (the containerlab uplink)
Vlan vlan100  (id 100, provider external)  ─┐
Subnet vlan100-subnet 172.23.0.0/24  vlan: vlan100   provider: external.vlan100-subnet.ovn
Vlan vlan200  (id 200, provider external)  ─┐
Subnet vlan200-subnet 172.24.0.0/24  vlan: vlan200   provider: external.vlan200-subnet.ovn
```

kube-ovn creates `eth1.100` / `eth1.200` sub-interfaces inside each node, and
the NIC profile publishes one `subnet-<name>` device (`subnetType=vlan`,
`vlanId=...`) per subnet. A pod then selects the VLAN it wants via a CEL
selector on its `ResourceClaim` — e.g. `subnetName == "vlan100-subnet"` — and
the driver plumbs a tagged access port for that VLAN into the pod netns.

End-to-end VLAN path:

```bash
make kind-create
make clab-deploy            # eth1 uplink + FRR BGP gateway  (sudo)
make kind-deploy-kube-ovn
make kind-deploy-multus
make kind-build-driver
make kind-deploy-driver
make kind-deploy-nic-example  # provider network, VLANs, subnets, claims, demo pod
make kind-test-vlan           # dual-VLAN traffic + host routing + isolation checks
```

`make kind-frr-status` shows the BGP session state and the VLAN routes learned
into the host table; `make clab-destroy` tears down the topology and bridge.

### Iterating on the driver

After a code change, rebuild and reload the image, then restart the plugin:

```bash
make kind-build-driver
make kind-deploy-driver     # helm upgrade --install, pullPolicy=Never uses the loaded image
kubectl -n kube-system rollout restart ds/kube-ovn-nic-dra-kubeletplugin
```

### Verifying

```bash
# DRA driver published its devices
kubectl get resourceslices

# Demo pod NICs
kubectl exec multi-nic-demo -- ip addr

# Hotplug a second NIC into the running pod
make kind-demo-hotplug

# Dual-VLAN tagged-traffic + host-routing test (requires clab-deploy first)
make kind-test-vlan
```

Tunable variables (override on the `make` command line):

| Variable | Default | Purpose |
|----------|---------|---------|
| `KIND_CLUSTER_NAME` | `nic-dra-demo` | kind cluster name |
| `KIND_NODE_IMAGE` | `kindest/node:v1.35.0` | Kubernetes node image (must be ≥ v1.34) |
| `KUBE_OVN_VERSION` | `v1.15.9` | kube-ovn chart/version (the DRA patch is built on this tag) |
| `MULTUS_VERSION` | `v4.2.3` | Multus daemonset version |
| `NIC_DRIVER_NAME` | `nic.kubeovn.io` | driver name / image repo |

## Running

```bash
# Build
make build

# Deploy (helm)
helm install kube-ovn-nic-dra deployments/helm/kube-ovn-dra-driver \
  --set driverName=nic.resource.kube-ovn.io \
  --set deviceProfile=nic

# Example ResourceClaim
kubectl apply -f - <<EOF
apiVersion: resource.k8s.io/v1beta1
kind: ResourceClaim
metadata:
  name: my-nic
spec:
  devices:
    requests:
    - name: nic
      deviceClassName: kube-ovn-nic
      selectors:
      - cel:
          expression: 'device.attributes["subnetName"].stringValue == "myvlan"'
EOF
```

## Multus-free secondary NICs — implemented and validated

The driver owns the *entire* secondary-NIC lifecycle — IPAM **and** the netns
attach — so a pod gets `net1`/`net2`/… purely from its `ResourceClaim`s, with
**no Multus, no `NetworkAttachmentDefinition`, and no `k8s.v1.cni.cncf.io/networks`
annotation**. A workload asks for an extra network via a `ResourceClaim` (plus
structured-parameter scheduling) *instead of* a Multus annotation. (This is a
Multus-**free path**, not a claim to replace Multus the project — the two can
coexist, and the broader "DRA backs a network API" framing is in
[`kndm-dranet-comparison.md`](kndm-dranet-comparison.md).) Validated
end-to-end on the kind demo for **both OVN overlay and VLAN underlay** (see
*Current status* below), so it is **no longer a roadmap item** — it works, given
the kube-ovn controller patch (the `add-dra-support-for-secondary-nics` image).

### The two pieces (both implemented)

**1. Multus-free IPAM — `pkg/kubeovnip` (the `ips.kubeovn.io` CRD).** Creating an
IP object makes the controller's reserved-IP reconciler (`handleAddReservedIP`,
`pkg/controller/ip.go`) allocate from the subnet pool with **no NAD / networks
annotation**. Verified constraints, enforced in `pkg/kubeovnip`:

- `metadata.name` **must** equal `ovs.PodNameToPortName(pod, ns, provider)`
  (`<pod>.<ns>` for provider `ovn`, else `<pod>.<ns>.<provider>`), or the
  controller silently ignores the object.
- `spec.{subnet,podName,namespace}` are required.
- The controller writes back `spec.ipAddress` / `spec.v4IpAddress` /
  `spec.macAddress`. There is **no gateway** on the IP object — read it from the
  Subnet CR.
- The IP object reserves the **address**; the **OVN Logical Switch Port** is then
  created by the kube-ovn DRA controller patch (`ensureDRALogicalSwitchPort`,
  `pkg/controller/ip.go`) for **both** overlay *and* VLAN underlay — the live run
  showed the underlay port needs a bound LSP too (the earlier "underlay needs no
  LSP" assumption was wrong; see the GC/LSP fixes under *Remaining work / risks*).

> The alternative `pkg/annotation` mode is **Multus-coupled** and kept only for
> running *alongside* Multus: kube-ovn-controller learns a pod's networks
> *exclusively* from the `k8s.v1.cni.cncf.io/networks` annotation → the referenced
> NAD (`getPodAttachmentNet`, `pkg/controller/pod.go`), deriving
> `provider = <nad>.<ns>.ovn`; writing the `<provider>.kubernetes.io/logical_switch`
> annotation without a NAD does nothing. So it is IPAM-on-top-of-Multus, not a
> Multus-free path — use it only when Multus is present.

**2. netns attach — `pkg/plumbing` + an NRI hook.** The veth pair + OVS port +
moving the interface into the pod netns are performed by the driver itself at
sandbox time (NRI `RunPodSandbox`), not by a CNI invoked through Multus —
implemented and validated for both subnet types (see below).

**So the Multus-free stack is `pkg/kubeovnip` (IPAM) + `pkg/plumbing`/NRI (attach)
+ the kube-ovn controller patch (LSP creation + GC sparing)** — validated
end-to-end. `pkg/annotation` remains only as the Multus-compatible mode.

### What `pkg/plumbing` does (implemented, validated)

`pkg/plumbing` is driven from an **NRI hook** (`RunPodSandbox`), wired into
`cmd/kube-ovn-dra-kubeletplugin`: the prepare path stashes one `plumbing.Spec`
per NIC in a `PendingStore` keyed by pod UID, and the NRI hook drains it once the
sandbox netns exists and runs the attach. The datapath
(`plumbing_linux.go`, via `vishvananda/netlink` + `ovs-vsctl`) is implemented for
**both** subnet types; `plumbing_nolinux.go` keeps the package cross-compilable.
Per claimed NIC, `Attach` (with rollback on any step) does:

1. **Resolve the netns** from the NRI `PodSandbox` event
   (`Linux.Namespaces[type==network].Path`), plus the sandbox container ID for
   veth naming.
2. **Create the veth pair** (`createVethPair`), move the pod end into the netns
   and rename it to `InterfaceName` (`moveIntoNetns`) — names match kube-ovn's
   `generateNicName` (`<cid>_<iface>_h/_c`).
3. **Configure the pod side** (`configurePodIface`) from the IPAM result: MAC
   (while down) → MTU → IP/CIDR → up → per-NIC routes. Default routes are
   **rejected** defensively (eth0 keeps the default; this is a secondary NIC).
4. **Create the OVS port** on the correct bridge — the two subnet types use
   **different bridges and different binding mechanisms** (verified against
   kube-ovn `pkg/daemon/ovs_linux.go` + `pkg/ovs/util.go`):
   - **OVN overlay (`subnetType=ovn`)**: port on **`br-int`**, with
     `external_ids:iface-id` set to the Logical Switch Port name
     kube-ovn-controller created — `PodNameToPortName(pod, ns, provider)` =
     `<pod>.<ns>` for the default provider `ovn`, else `<pod>.<ns>.<provider>`.
     Getting this string exactly right is what lets OVN bind the LSP to the
     chassis. Also stamp `external_ids:vendor=kube-ovn`, `pod_name`,
     `pod_namespace`, `ip`, `pod_netns`.
   - **VLAN underlay (`subnetType=vlan`)**: access port on the **dedicated
     provider bridge `br-<provider>`** (kube-ovn `util.ExternalBridgeName`),
     tagged with the subnet's VLAN id. Traffic egresses the physical **trunk**
     NIC that kube-ovn added to that bridge from the `ProviderNetwork` CR — it
     does **not** traverse `br-int`/OVN, so there is **no `iface-id`** here.
     This is the path the `make kind-test-vlan` containerlab topology exercises
     (`ProviderNetwork external` → `br-external`, trunk `eth1`, VLANs 100/200).
5. **NRI `StopPodSandbox`**: `Detach` removes the OVS port then the veth
   (`detachFromOVS` + `cleanupHostVeth`); IPAM release stays in
   `UnprepareResourceClaims`.

### Current status

| Piece | State |
|-------|-------|
| Multus-free IPAM (`pkg/kubeovnip`) | ✅ implemented (IP CRD, `PodNameToPortName`-named) |
| Attach datapath (`pkg/plumbing`, both types) | ✅ implemented & validated (netlink + `ovs-vsctl`) |
| NRI plugin + `PendingStore` wiring | ✅ wired into `cmd/kube-ovn-dra-kubeletplugin` |
| Spec built from IPAM in prepare | ✅ uses `pkg/kubeovnip` (IP CRD, Multus-free); `pkg/annotation` kept as the Multus-compatible mode |
| IPAM release on teardown | ✅ `UnprepareResourceClaims` deletes the `ips.kubeovn.io` object (release info persisted per device in the checkpoint); kube-ovn then GCs the overlay LSP |
| Multiple pods sharing one Subnet | ✅ subnet devices published with `AllowMultipleAllocations=true` (needs the `DRAConsumableCapacity` gate). See "Shared subnets" below |
| DaemonSet NRI/OVS host wiring (Helm) | ✅ behind `kubeletPlugin.nicAttach.enabled` (hostNetwork/PID, privileged, OVS run-dir mount) |
| Runtime image ships `ovs-vsctl` | ✅ `Dockerfile` `nic` stage (Debian + openvswitch); `make kind-build-driver` uses `--target nic` |
| Exercised against live kube-ovn | ✅ validated end-to-end on kind (both VLAN underlay and OVN overlay; debugged to working — see below) |

### Remaining work / risks

- **Live validation: done.** Run end-to-end on the kind demo — NIC-claiming pods
  get `net1`/`net2` with the right address and reach both the VLAN underlay
  (bidirectional via the external FRR gateway) and the OVN overlay. Getting there
  required three fixes found during the live run:
  1. *Driver — VLAN subnet classification* (`internal/profiles/nic/nic.go`):
     resolve `subnet.spec.vlan` → `Vlan` CR (`spec.id`, `spec.provider`) instead
     of a non-existent `subnet.spec.vlanId`, so VLAN subnets publish
     `subnetType=vlan`/`vlanId`/`providerNetwork` and attach to `br-<provider>`.
  2. *kube-ovn — GC sparing* (`pkg/controller/gc.go`): reserved-IP LSPs have no
     pod annotation, so `markAndCleanLSP` reaped them ~10 min in and connectivity
     dropped; fix adds reserved-IP LSP names to the keep-set.
  3. *kube-ovn — LSP for underlay + heal on reuse* (`pkg/controller/ip.go`,
     `pkg/controller/pod.go`): create the LSP for **both** overlay and VLAN
     underlay, re-ensure it for reused IP objects across pod recreations, and
     stop the pod-sync path from tearing down DRA-managed ports.

  > ⚠️ Fixes 2 and 3 are **not yet in the committed
  > `add-dra-support-for-secondary-nics` branch** — they live in the kube-ovn
  > working tree. The branch must be rebuilt to include them before publishing a
  > reference to it (see the kube-ovn dependency note above).
- **IPAM release on teardown:** done. `UnprepareResourceClaims` →
  `unprepareDevices` calls `ReleaseIPAMViaIPObject` for each NIC, deleting the
  `ips.kubeovn.io` object (release info — pod name/namespace/provider — is
  persisted per device in the checkpoint so it survives a plugin restart).
  Deleting the IP object makes kube-ovn's reserved-IP delete path remove the
  overlay LSP. The veth/OVS port teardown is handled separately by the NRI
  `StopPodSandbox` `Detach`.
- **NRI:** enabled by default in the containerd 2.x shipped with the kind v1.35
  node image (see the kind config comment).
- **Risk — OVN `iface-id`:** encoded in `plumbing.Spec.PortName()` to match
  `ovs.PodNameToPortName`; re-verify against the pinned `KUBE_OVN_VERSION` on a
  live run. For VLAN, the driver assumes kube-ovn already created
  `br-<provider>` + the trunk uplink from the `ProviderNetwork` CR — it should
  fail clearly if that bridge is absent rather than create a disconnected one.
- **LSP required for both subnet types (revised):** the IP-CRD reserve only
  allocates the address; the LSP is created by the kube-ovn DRA patch
  (`ensureDRALogicalSwitchPort`). The live run showed this is needed for **both**
  overlay *and* VLAN underlay — without a bound LSP the underlay port has no
  datapath either — so the patch no longer gates LSP creation on
  `subnet.Spec.Vlan == ""`. (Earlier drafts of this doc described VLAN underlay
  as needing no LSP; that was wrong and is corrected here.)

The Multus-free stack is validated end-to-end on the kind demo: Multus-free IPAM
(`kubeovnip`) + the attach datapath for both overlay and underlay + Helm/image
wiring + IPAM release on teardown. The remaining gate is purely packaging on the
kube-ovn side: fold the GC/LSP fixes (above) into the
`add-dra-support-for-secondary-nics` branch, based on kube-ovn `main`
(`v1.17.0`), then `kind-deploy-multus` can be dropped from the demo and the
roadmap is closed.

## Shared subnets (multiple pods, one Subnet)

A kube-ovn Subnet is a **shared IP pool**, not an exclusive device. DRA, however,
allocates a plain `Device` **exclusively** — so until this was addressed, a
second pod selecting the same subnet stayed `Pending`. Verified live:

```
$ kubectl get pod share-a share-b
NAME       STATUS
share-a    Running       # got subnet-ovn-subnet
share-b    Pending       # FailedScheduling: 1 cannot allocate all claims
```

**Fix (driver-only).** `internal/profiles/nic` publishes each subnet device with
`AllowMultipleAllocations: true`, so many claims can allocate the same subnet
device; each pod still gets its own `ips.kubeovn.io` / IP / MAC / LSP (the IPAM
path is already per-pod via `PodNameToPortName`, so nothing else changes). No
kube-ovn change is needed — the subnet genuinely has hundreds of IPs.

- **Feature gate:** `AllowMultipleAllocations` (and the richer consumable-capacity
  API) are gated by **`DRAConsumableCapacity`** — *alpha, default-off* in k8s
  1.34/1.35. It must be enabled on **apiserver + scheduler + kubelet**; the kind
  config (`demo/kind/kind-no-cni.yaml`) sets it.
- **Demo/test:** `demo/nic-example/examples/shared-subnet.yaml` (two pods on
  `ovn-subnet`, overlay → no containerlab) and the e2e spec
  *"shared subnet (multiple pods, one subnet)"*.

**`AllowMultipleAllocations` vs consumable-capacity — pick per goal:**

| | `AllowMultipleAllocations: true` (used now) | Consumable capacity (`Device.Capacity` + `RequestPolicy`) |
|---|---|---|
| Effect | unlimited sharing of the device | each request consumes 1 unit of a finite pool |
| Scheduler enforces IP exhaustion | ❌ no | ✅ yes (refuses to schedule when the pool is full) |
| Driver work | one field | publish capacity from the Subnet's free-IP count + keep it fresh |
| Same gate | `DRAConsumableCapacity` | `DRAConsumableCapacity` |

`AllowMultipleAllocations` is the minimal fix that makes shared subnets work.
Consumable-capacity is the KND-idiomatic upgrade that also makes **IP-pool
exhaustion a scheduling decision** (a genuine day-0 advantage over Multus) — the
natural next step if the driver continues. See
[`kndm-dranet-comparison.md`](kndm-dranet-comparison.md).
