# KubeVirt Summit 2026 — CFP submission draft

Status: draft for Sessionize submission (CFP closes 2026-07-22).
A findings talk comparing the Multus and DRA models for kube-ovn secondary
VM networking. Neutral side-by-side comparison — not an argument for one over
the other. Grounded in `docs/nic-driver.md`, `docs/architecture.md`,
`docs/kndm-dranet-comparison.md`, `docs/multi-network-api-integration.md`.

---

## Title

**Two ways to give a VM a second NIC: Multus and DRA on kube-ovn, side by side**

_(alt: "Comparing Multus and Dynamic Resource Allocation for kube-ovn VM networking")_

## Elevator pitch (≤ 300 chars)

KubeVirt VEP-183 lets VMs consume NICs from external DRA drivers — a second way
to attach a secondary network alongside the established Multus path. I built a
kube-ovn DRA driver and compared the two models directly. This talk lays both
out side by side: how each works, and how they differ.

## Session description

Last year I talked about taming VM sprawl across clusters with kcp. This year:
how those VMs get their networks.

A KubeVirt VM that needs a second network interface has, today, an established
answer: Multus plus a `NetworkAttachmentDefinition`. KubeVirt's VEP-183
(`NetworkDevicesWithDRA`) adds a second one: `ResourceClaim` as a first-class VM
`NetworkSource`, so VMs consume interfaces from externally supplied DRA drivers —
the same way DraNet hands over hardware NICs. But DraNet is built for
*passthrough of real host devices*. What does the DRA model look like for an SDN
like kube-ovn, where there's no device to discover and the NIC has to be
*synthesized* on demand?

To find out, I built a DRA driver that gives pods (and, via VEP-183, VMs)
secondary kube-ovn NICs — overlay and VLAN underlay — and set it next to the
Multus path for kube-ovn. This is a findings talk, and deliberately even-handed:
both models work end-to-end, each is a better fit for different situations, and
the useful contribution is a clear map of *how they differ* — not a verdict.

I'll walk through, for each model:

- **How the attach actually happens.** Multus multiplexes delegate CNIs in the
  pod sandbox-setup path, invoked serially. The DRA driver splits the work across
  two phases — IPAM in `PrepareResourceClaims` before the pod netns exists, then
  the veth/OVS attach inside a ~2s NRI `RunPodSandbox` window once it does. Why a
  naive single-phase DRA driver can't work, and how each model's shape follows
  from where in the lifecycle it runs.
- **What the scheduler sees.** Multus annotations are invisible to the scheduler;
  a DRA `ResourceClaim` is a scheduling input. What that changes in practice —
  IP-pool exhaustion surfacing as a scheduling decision vs. a late CNI failure,
  underlay/provider placement — and where it makes no difference (node-agnostic
  overlay, where DRA is effectively just a provisioning trigger).
- **How a shared subnet is modeled.** Multus doesn't have to think about this. DRA
  allocates devices *exclusively* by default, so the first design broke the moment
  a second pod selected the same subnet; a kube-ovn Subnet is really a shared IP
  pool (`AllowMultipleAllocations` / `DRAConsumableCapacity`), not a scarce device.
- **Lifecycle and the bugs the demo doesn't show.** A NAD is config; a DRA claim
  has a first-class allocate/release. Building the DRA path surfaced real
  control-plane work: VLAN subnet misclassification, and kube-ovn's GC reaping the
  reserved-IP logical switch ports ~10 minutes in (DRA-created ports carry no pod
  annotation) — connectivity silently dropping until fixed. Most of the hard work
  turned out to be controller-side, not in the node driver.
- **Hot-plug — the same boundary for both, via DRA.** DRA claim specs are
  immutable in Kubernetes 1.35/1.36, and VEP-183 lists DRA-NIC hot-plug as an
  explicit Non-Goal. KubeVirt still hot-plugs Multus interfaces through its own
  mechanism, independent of DRA. So this is a difference of *mechanism*, not a
  capability one model has and the other lacks.
- **Where each sits in the ecosystem.** DraNet/SR-IOV for hardware passthrough,
  ovs-cni for trunks, Multus for the established secondary-NIC path, a DRA driver
  for scheduler-visible virtual SDN endpoints — complementary layers, and a
  cluster can run more than one.

You'll leave with a clear, even-handed comparison of the two models for KubeVirt
VM networking on an SDN: how each works, which situations each fits, what's still
missing at the API seam (Network→DeviceClass binding, interface-name ownership,
capacity signaling), and where the upstream conversation needs to go.

## Key takeaways

1. There are now two models for attaching a secondary NIC to a KubeVirt VM —
   Multus + NAD, and DRA via VEP-183 — and this talk maps how they differ rather
   than ranking them.
2. They differ most in scheduler visibility, IP-pool capacity handling, attach
   sequencing, and lifecycle; they are equivalent for plain node-agnostic overlay
   and for hot-plug (neither does DRA hot-plug yet).
3. On an SDN like kube-ovn, most of the DRA-model work is control-plane-side
   (GC / LSP lifecycle), not node-side plumbing.
4. The models are complementary layers of VM networking, not a winner-take-all
   choice.

## Suggested metadata

- **Level:** Intermediate–Advanced
- **Track fit:** Storage & network improvements / Integration with other CNCF
  projects / Building custom clouds
- **Format:** ~25–30 min + Q&A; live-or-recorded kind demo available
  (`make kind-demo`)

## Speaker bio

Sören Henning is a software engineer at Kubermatic working on Kubernetes
virtualization and multi-cluster infrastructure. At KubeVirt Summit 2025 he
presented "Taming the VM Sprawl: KCP Meets KubeVirt," on managing KubeVirt VMs
across clusters with the CNCF project kcp. His recent work explores how Dynamic
Resource Allocation intersects with SDN networking for VMs — building a kube-ovn
DRA driver as a reference to inform the SIG-Network and KubeVirt community
conversation.

## Open items before submitting (2026-07-02)

- **Latency numbers.** The serial-attach cost of Multus (~5s/NIC cited in notes)
  is not yet independently measured — run `make nic-bench` before July 22 to
  include a real figure in the comparison, or keep the framing qualitative.
- **Bio.** Confirm title/affiliation wording.
- **Demo recording.** Decide live vs. pre-recorded kind demo for the online format.
