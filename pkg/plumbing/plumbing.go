/*
 * Copyright The Kubernetes Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// Package plumbing attaches a DRA-allocated kube-ovn NIC into a pod's network
// namespace WITHOUT Multus.
//
// # Why this exists
//
// pkg/annotation and pkg/kubeovnip handle IPAM only: they reserve an IP/MAC
// from a kube-ovn Subnet. The veth pair + OVS port that actually plumbs the
// interface into the pod netns is, today, created by the kube-ovn CNI invoked
// through Multus. This package is the missing attach step that lets the driver
// own the whole secondary-NIC lifecycle so Multus can be dropped — see the
// "Roadmap: replacing Multus" section of docs/nic-driver.md.
//
// # Timing
//
// IPAM (RequestIPAM / kubeovnip.Reserve) runs in PrepareResourceClaims, BEFORE
// the pod sandbox exists. The attach can only run once the sandbox netns is
// created, so it is driven from an NRI hook (RunPodSandbox). The two phases are
// bridged by a PendingStore (see store.go): Prepare registers a Spec keyed by
// pod UID; the NRI hook drains it and calls Attach.
//
//	PrepareResourceClaims ──> IPAM reserve ──> PendingStore.Add(podUID, Spec)
//	NRI RunPodSandbox      ──> PendingStore.Take(podUID) ──> Attacher.Attach
//	NRI StopPodSandbox     ──> Attacher.Detach ──> IPAM release
//
// NOTE: every method below is a SKELETON. The netlink/OVS calls are stubbed and
// return ErrNotImplemented. They are intentionally expressed behind small
// private helpers so the real implementation (vishvananda/netlink + ovs-vsctl,
// or libovsdb) can be filled in one helper at a time without reshaping the API.
package plumbing

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
)

// ErrNotImplemented is returned by the stubbed datapath helpers until the real
// netlink/OVS implementation lands.
var ErrNotImplemented = errors.New("plumbing: not implemented yet")

// SubnetType selects which OVS bridge the pod's host veth is attached to. The
// two cases use DIFFERENT bridges and different binding mechanisms:
//
//   - OVN overlay  → br-int, with external_ids:iface-id set so OVN binds the
//     Logical Switch Port to this chassis.
//   - VLAN underlay → the dedicated provider bridge "br-<Provider>", as an
//     access port tagged with the subnet's VLAN id. Traffic egresses the
//     physical trunk NIC that kube-ovn added to that bridge from the
//     ProviderNetwork CR. This path does NOT go through br-int / OVN.
//
// The provider bridge and its trunk uplink are provisioned by kube-ovn from the
// ProviderNetwork CR (pkg/daemon: configProviderNic adds the NIC with trunks=).
// The driver only adds the per-pod access port; it never creates the bridge.
type SubnetType string

const (
	// SubnetTypeOVN is a pure OVN overlay subnet (port on br-int).
	SubnetTypeOVN SubnetType = "ovn"
	// SubnetTypeVLAN is a VLAN-backed underlay subnet (tagged port on br-<provider>).
	SubnetTypeVLAN SubnetType = "vlan"
)

// Datapath constants mirrored from kube-ovn (keep in sync with KUBE_OVN_VERSION).
const (
	// integrationBridge is the OVN integration bridge used for overlay ports.
	integrationBridge = "br-int"
	// providerBridgePrefix + <provider> is the dedicated VLAN/underlay bridge,
	// matching kube-ovn's util.ExternalBridgeName ("br-" + provider).
	providerBridgePrefix = "br-"
	// ovnProvider is kube-ovn's sentinel provider for the default OVN network;
	// PodNameToPortName omits the provider suffix for it. See pkg/ovs/util.go.
	ovnProvider = "ovn"
	// cniVendor is the external_ids:vendor value kube-ovn stamps on ports.
	cniVendor = "kube-ovn"
)

// providerBridge returns the dedicated underlay bridge name for a provider,
// matching kube-ovn's util.ExternalBridgeName.
func providerBridge(provider string) string { return providerBridgePrefix + provider }

// Spec is everything the attach step needs for ONE secondary NIC. It is built
// in PrepareResourceClaims from the DRA device attributes + the kube-ovn IPAM
// result, then consumed later by the NRI sandbox hook.
type Spec struct {
	// --- identity (used as the PendingStore key and for OVS external_ids) ---

	// PodUID / PodName / PodNamespace identify the target pod.
	PodUID       string
	PodName      string
	PodNamespace string

	// --- where to plumb ---

	// NetnsPath is the pod sandbox network namespace path. EMPTY at IPAM time;
	// filled in by the NRI hook from the RunPodSandbox event before Attach.
	NetnsPath string
	// IfaceName is the interface name inside the pod netns (e.g. "net1").
	IfaceName string

	// --- IPAM result (from pkg/annotation or pkg/kubeovnip) ---

	// IP is the allocated address in CIDR notation (e.g. "172.23.0.5/24").
	IP string
	// MAC is the allocated hardware address (e.g. "00:11:22:33:44:55").
	MAC string
	// Gateway is the subnet gateway. Used for per-NIC routes ONLY — the default
	// route belongs to eth0 and must never be touched (this is a secondary NIC).
	Gateway string
	// Routes are extra routes to install via this interface (CIDR strings).
	// The subnet CIDR itself is added from IP; Gateway is not made default.
	Routes []string
	// MTU for the pod-side interface; 0 means inherit the bridge/default.
	MTU int

	// ContainerID is the sandbox container ID (from the NRI event). Used to
	// derive the host/pod veth names exactly as kube-ovn does — see vethNames.
	ContainerID string

	// --- bridge / binding ---

	// Type selects the bridge: br-int (ovn) vs br-<Provider> (vlan).
	Type SubnetType
	// Provider is the kube-ovn ProviderNetwork name. Required for VLAN underlay
	// (bridge "br-<Provider>"); for OVN it is the provider used to derive the
	// iface-id (the default OVN network uses the sentinel "ovn").
	Provider string
	// VlanID is the 802.1q tag for the access port on the provider bridge
	// (VLAN underlay only).
	VlanID int
	// IfaceID is the OVN-overlay-only external_ids:iface-id. It MUST equal the
	// Logical Switch Port name kube-ovn-controller created for this
	// pod+interface, or OVN will not bind the port. kube-ovn computes this as
	// PodNameToPortName(pod, ns, provider) = "<pod>.<ns>" for provider "ovn",
	// else "<pod>.<ns>.<provider>" (pkg/ovs/util.go). Leave empty for VLAN
	// underlay — the provider-bridge path is not OVN-bound.
	IfaceID string

	// KubeVirtVMI marks that this pod is a KubeVirt virt-launcher pod (detected
	// from the "kubevirt.io: virt-launcher" pod label in the NRI RunPodSandbox
	// event — see cmd/kube-ovn-dra-kubeletplugin/nri.go). A plain pod can
	// consume the veth directly; a VM's QEMU process cannot — QEMU's tap netdev
	// backend needs an actual tun/tap-driver device (TUNSETIFF), and a veth is
	// not one. When true, Attach additionally creates a Linux bridge + a real
	// tap device inside the pod netns, enslaves both the tap and the existing
	// veth to it, and renames the veth out of the way so the tap ends up with
	// the IfaceName the KubeVirt network-binding-plugin sidecar
	// (kube-ovn-network-binding-plugin) already expects as its domain XML
	// target — no change needed on that side.
	KubeVirtVMI bool
}

// PortName returns kube-ovn's iface-id / Logical Switch Port name for this NIC,
// matching ovs.PodNameToPortName(pod, namespace, provider).
func (s *Spec) PortName() string {
	if s.Provider == "" || s.Provider == ovnProvider {
		return s.PodName + "." + s.PodNamespace
	}
	return s.PodName + "." + s.PodNamespace + "." + s.Provider
}

// Validate checks that the fields required for an attach are present.
func (s *Spec) Validate() error {
	switch {
	case s.NetnsPath == "":
		return errors.New("plumbing: Spec.NetnsPath is empty (sandbox not resolved yet)")
	case s.IfaceName == "":
		return errors.New("plumbing: Spec.IfaceName is empty")
	case s.IP == "":
		return errors.New("plumbing: Spec.IP is empty")
	case s.ContainerID == "":
		return errors.New("plumbing: Spec.ContainerID is empty (cannot derive veth names)")
	}
	switch s.Type {
	case SubnetTypeVLAN:
		if s.Provider == "" {
			return errors.New("plumbing: VLAN underlay requires Provider (bridge br-<provider>)")
		}
		if s.VlanID <= 0 {
			return errors.New("plumbing: VLAN underlay requires a positive VlanID")
		}
	case SubnetTypeOVN:
		if s.IfaceID == "" {
			return errors.New("plumbing: OVN overlay requires IfaceID, or OVN will not bind the port")
		}
	default:
		return errors.New("plumbing: unknown SubnetType " + string(s.Type))
	}
	return nil
}

// Attacher creates and tears down the pod-side datapath for one NIC.
type Attacher interface {
	// Attach creates the veth pair, moves the pod end into Spec.NetnsPath,
	// configures it from the IPAM result, and wires the host end to OVS.
	// Must be idempotent: a retried Attach for an already-plumbed iface is a
	// no-op, not an error.
	Attach(ctx context.Context, spec Spec) error

	// Detach removes the OVS port and veth for one NIC. Idempotent: detaching
	// something already gone returns nil.
	Detach(ctx context.Context, spec Spec) error
}

// ovsAttacher is the kube-ovn / OVS implementation of Attacher.
type ovsAttacher struct {
	// hostVethPrefix is the prefix for the host-side veth name; the full name
	// is derived deterministically from the pod UID + iface so Detach can find
	// it without state. Kept short to respect the 15-char IFNAMSIZ limit.
	hostVethPrefix string

	// dhcpServers tracks the per-VMI single-client DHCP servers started by
	// ensureVMIDHCPServer, keyed by bridge name (globally unique — see
	// shortHashName). This is in-process state, unlike everything else in
	// this package: it does not survive a driver restart, only a running
	// process's lifetime, matching KubeVirt's own per-launcher-process DHCP
	// server (this driver plays that same role for VMI secondary NICs, just
	// for potentially many pods in one process instead of one per pod).
	dhcpMu      sync.Mutex
	dhcpServers map[string]dhcpServeCloser
}

// NewOVSAttacher returns the default OVS-backed Attacher.
func NewOVSAttacher() Attacher {
	return &ovsAttacher{hostVethPrefix: "dra", dhcpServers: make(map[string]dhcpServeCloser)}
}

// Attach is the high-level recipe. Each step is stubbed; see the private
// helpers below. The ordering matters: build the link, enter the netns,
// configure addressing, THEN hand the host end to OVS so OVN can bind it.
func (a *ovsAttacher) Attach(ctx context.Context, spec Spec) error {
	if err := spec.Validate(); err != nil {
		return err
	}

	hostVeth, podVeth, err := a.vethNames(spec)
	if err != nil {
		return err
	}

	// 1. Create the veth pair on the host side.
	if err := a.createVethPair(ctx, hostVeth, podVeth, spec.MTU); err != nil {
		return err
	}

	// 2. Move the pod end into the sandbox netns and rename it to IfaceName.
	if err := a.moveIntoNetns(ctx, podVeth, spec.NetnsPath, spec.IfaceName); err != nil {
		a.cleanupHostVeth(ctx, hostVeth) // best-effort
		return err
	}

	// 3. Configure addressing inside the pod netns (IP/MAC/MTU + per-NIC
	//    routes). MUST NOT replace the default route.
	if err := a.configurePodIface(ctx, spec); err != nil {
		a.cleanupHostVeth(ctx, hostVeth)
		return err
	}

	// 4. Attach the host end to the correct OVS bridge and stamp external_ids
	//    so OVN binds the Logical Switch Port to this chassis.
	if err := a.attachToOVS(ctx, hostVeth, spec); err != nil {
		a.cleanupHostVeth(ctx, hostVeth)
		return err
	}

	// 5. KubeVirt VMIs need a real tap device, not a veth, for QEMU to attach
	//    to. Wrap the veth in a bridge+tap, freeing IfaceName for the tap.
	if spec.KubeVirtVMI {
		if err := a.wireVMIBridge(ctx, spec); err != nil {
			a.detachFromOVS(ctx, hostVeth, spec)
			a.cleanupHostVeth(ctx, hostVeth)
			return err
		}
	}

	return nil
}

// Detach is the symmetric teardown: drop the bridge/tap (if any), the OVS
// port, then the veth.
func (a *ovsAttacher) Detach(ctx context.Context, spec Spec) error {
	if spec.KubeVirtVMI {
		if err := a.unwireVMIBridge(ctx, spec); err != nil {
			return err
		}
	}

	hostVeth, _, err := a.vethNames(spec)
	if err != nil {
		return err
	}
	// Deleting the OVS port first stops OVN from re-binding while we tear down.
	if err := a.detachFromOVS(ctx, hostVeth, spec); err != nil {
		return err
	}
	return a.cleanupHostVeth(ctx, hostVeth)
}

// ---------------------------------------------------------------------------
// Datapath helpers — STUBBED. Fill these in one at a time.
// ---------------------------------------------------------------------------

// vethNames derives the host/pod veth endpoint names, matching kube-ovn's
// generateNicName (pkg/daemon/ovs_linux.go) so names are deterministic and fit
// the 15-char IFNAMSIZ limit:
//
//	host = containerID[:12-len(iface)] + "_" + iface + "_h"
//	pod  = containerID[:12-len(iface)] + "_" + iface + "_c"
//
// (kube-ovn special-cases iface=="eth0" to containerID[:12]+"_h"/"_c"; that is
// the primary NIC and out of scope here.) hostVethPrefix is unused for the
// kube-ovn-compatible scheme and kept only for non-kube-ovn experiments.
func (a *ovsAttacher) vethNames(spec Spec) (host, pod string, err error) {
	cid := spec.ContainerID
	if len(spec.IfaceName) > 12 || len(cid) < 12-len(spec.IfaceName) {
		return "", "", fmt.Errorf(
			"plumbing: IfaceName %q (len %d) is too long to fit the veth naming scheme "+
				"(containerID[:12-len(iface)]+\"_\"+iface, needs len(iface) <= 12 and a "+
				"container ID at least 12-len(iface) chars long)",
			spec.IfaceName, len(spec.IfaceName))
	}
	base := cid[:12-len(spec.IfaceName)] + "_" + spec.IfaceName
	return base + "_h", base + "_c", nil
}

// shortHashName derives a fixed-length, IFNAMSIZ-safe interface name from key,
// regardless of key's own length (unlike vethNames' scheme, this never
// underflows). Used for the bridge/tap devices wireVMIBridge adds; NOT used
// for the veth pair itself, which must stay on kube-ovn's own naming scheme
// so kube-ovn's control plane keeps recognizing it.
func shortHashName(prefix, key string) string {
	sum := sha256.Sum256([]byte(key))
	return fmt.Sprintf("%s%x", prefix, sum[:4])[:len(prefix)+8]
}

// bridgeNameFor and vethRenameFor derive the bridge and the "moved aside"
// veth name wireVMIBridge uses for a given IfaceName. Deterministic from
// IfaceName alone so Detach's unwireVMIBridge needs no extra stored state.
func bridgeNameFor(ifaceName string) string { return shortHashName("kvb", ifaceName) }
func vethRenameFor(ifaceName string) string { return shortHashName("kvv", ifaceName) }

// kubevirtQemuUID is the uid/gid virt-launcher's compute and hook-sidecar
// containers run as in modern (non-root-by-default) KubeVirt — verified
// against a live v1.9.0-rc.0 cluster's pod spec
// (securityContext.runAsUser/runAsGroup on both the "compute" and
// "hook-sidecar-*" containers). The tap device wireVMIBridge creates must be
// owned by this uid/gid or QEMU (running as this user) cannot open it.
const kubevirtQemuUID = 107

// The per-step datapath helpers (createVethPair, moveIntoNetns,
// configurePodIface, attachToOVS, detachFromOVS, cleanupHostVeth) are
// platform-specific and live in plumbing_linux.go (real netlink/OVS
// implementation) and plumbing_nolinux.go (ErrNotImplemented stubs).
