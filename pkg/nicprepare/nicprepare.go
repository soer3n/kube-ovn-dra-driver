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

// Package nicprepare resolves the kube-ovn allocation of a DRA-allocated NIC.
//
// kube-ovn-controller (--enable-dra-nic) treats every device allocated by this
// driver as a pod network attachment, the same way it treats a Multus one: it
// allocates the address and MAC, creates the logical switch port and ip CR,
// and writes the result to the pod's per-provider annotations. The driver
// allocates nothing itself. RequestIPAM waits for that result in
// PrepareResourceClaims and returns what the NRI hook needs to plug the NIC.
// Nothing must be released either: kube-ovn-controller releases the address
// when the pod is deleted, or keeps it for a keep-vm-ip KubeVirt VM.
package nicprepare

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coreclientset "k8s.io/client-go/kubernetes"

	"github.com/soer3n/kube-ovn-dra-driver/internal/publisher"
	"github.com/soer3n/kube-ovn-dra-driver/pkg/annotation"
	"github.com/soer3n/kube-ovn-dra-driver/pkg/plumbing"
)

// defaultProvider is the provider of the pod's primary kube-ovn NIC.
const defaultProvider = "ovn"

// providersAnnotation is where kube-ovn-controller records on a claim the subnet
// and provider it allocated each NIC with, as JSON keyed by "<pool>/<device>".
// A recorded NIC keeps them even if the subnet's provider changed since.
const providersAnnotation = "ovn.kubernetes.io/dra_providers"

// nicRecord is one entry of providersAnnotation.
type nicRecord struct {
	Subnet   string `json:"subnet"`
	Provider string `json:"provider"`
}

// recordedNIC returns the subnet and provider kube-ovn-controller recorded for
// the allocated device, if it did.
func recordedNIC(claim *resourceapi.ResourceClaim, result *resourceapi.DeviceRequestAllocationResult) (nicRecord, bool, error) {
	value := claim.Annotations[providersAnnotation]
	if value == "" {
		return nicRecord{}, false, nil
	}
	records := map[string]nicRecord{}
	if err := json.Unmarshal([]byte(value), &records); err != nil {
		return nicRecord{}, false, fmt.Errorf("invalid %s annotation of claim %s/%s: %w", providersAnnotation, claim.Namespace, claim.Name, err)
	}
	record, ok := records[result.Pool+"/"+result.Device]
	return record, ok, nil
}

// NicDeviceConfig holds everything the driver needs to plug one NIC. It is
// created during PrepareResourceClaims.
type NicDeviceConfig struct {
	// DeviceName is the DRA device name (e.g. "subnet-myvlan").
	DeviceName string
	// IfaceName is the desired interface name inside the pod netns.
	IfaceName string
	// SubnetName is the kube-ovn Subnet name.
	SubnetName string
	// Provider is the kube-ovn Subnet's spec.provider, which keys the NIC's
	// annotations and logical switch port.
	Provider string
	// PodName, PodNamespace and PodUID identify the target pod. PodUID is the
	// key the NRI sandbox hook uses to find the pending attach Spec.
	PodName      string
	PodNamespace string
	PodUID       string

	// LogicalSwitchPort is the OVN port the attach binds to via
	// external_ids:iface-id. kube-ovn names it after the VM instead of the pod
	// for keep-vm-ip VMs.
	LogicalSwitchPort string

	// ExtraIPs are the NIC's addresses of the other IP family (dual-stack), in
	// CIDR notation. IP is the IPv4 address in that case.
	ExtraIPs []string

	// IPAM result, captured from WaitForAllocation. Carried so the caller can
	// build a plumbing.Spec for the attach step without re-reading annotations.
	IP      string
	MAC     string
	CIDR    string
	Gateway string
	// VMName is the KubeVirt VM (and VMI) of a virt-launcher pod, empty for
	// other pods. kube-ovn names the NIC's port after it.
	VMName string

	// Routes are the NIC's extra routes from kube-ovn's routes annotation.
	Routes []plumbing.Route
	// MTU is the NIC's MTU, 0 to take the one of the pod's primary interface.
	// The caller sets it (see DeviceState.subnetMTU).
	MTU int
	// OVNDHCP reports whether the Subnet has OVN's DHCP enabled, so a VM's
	// guest takes its addresses from OVN. The caller sets it (see
	// DeviceState.subnetOVNDHCP).
	OVNDHCP bool
}

// RequestIPAM waits for kube-ovn-controller to allocate the NIC from the claim
// and reads the result from the pod's per-provider annotations. nodeName is the
// node the claim is prepared on; it picks the pod of a shared claim.
func RequestIPAM(
	ctx context.Context,
	client coreclientset.Interface,
	claim *resourceapi.ResourceClaim,
	result *resourceapi.DeviceRequestAllocationResult,
	device resourceapi.Device,
	ifaceName string,
	nodeName string,
) (*NicDeviceConfig, error) {
	podName, podNS, podUID, err := lookupPodForClaim(ctx, client, claim, nodeName)
	if err != nil {
		return nil, fmt.Errorf("find pod for claim %s/%s: %w", claim.Namespace, claim.Name, err)
	}

	// The controller keys the result annotations by the subnet's provider: the
	// one it recorded on the claim, or for a new NIC the subnet's spec.provider,
	// which the published provider attribute carries.
	subnetName := extractSubnetName(device, result.Driver)
	provider := extractProvider(device, result.Driver)
	if record, ok, err := recordedNIC(claim, result); err != nil {
		return nil, err
	} else if ok {
		subnetName, provider = record.Subnet, record.Provider
	}
	if subnetName == "" {
		return nil, fmt.Errorf("device %s has no subnetName attribute", device.Name)
	}
	if err := validateSecondaryProvider(provider, subnetName); err != nil {
		return nil, err
	}

	// Like repeated Multus attachments, kube-ovn-controller keys NICs that share a
	// provider in one pod by "<provider>.<interfaceName>", and a NIC alone on its
	// provider by the provider itself. It uses one or the other, never both.
	alloc, err := annotation.WaitForAllocation(ctx, client, podNS, podName, provider+"."+ifaceName, provider)
	if err != nil {
		return nil, fmt.Errorf("kube-ovn-controller did not allocate NIC %q on subnet %q for pod %s/%s "+
			"(no %s.kubernetes.io/allocated or %s.%s.kubernetes.io/allocated annotation); check the pod's events "+
			"and that kube-ovn-controller runs with --enable-dra-nic and --dra-nic-driver-name=%s: %w",
			ifaceName, subnetName, podNS, podName, provider, provider, ifaceName, result.Driver, err)
	}

	var routes []plumbing.Route
	if alloc.Routes != "" {
		if err := json.Unmarshal([]byte(alloc.Routes), &routes); err != nil {
			return nil, fmt.Errorf("invalid %s.kubernetes.io/routes annotation of pod %s/%s: %w", alloc.Provider, podNS, podName, err)
		}
	}

	portOwner := podName
	if alloc.VMName != "" {
		portOwner = alloc.VMName
	}
	ips, cidr, gateway := dualStackAddresses(alloc.IP, alloc.CIDR, alloc.Gateway)

	return &NicDeviceConfig{
		DeviceName:        result.Device,
		IfaceName:         ifaceName,
		SubnetName:        subnetName,
		Provider:          provider,
		PodName:           podName,
		PodNamespace:      podNS,
		PodUID:            podUID,
		LogicalSwitchPort: portName(portOwner, podNS, alloc.Provider),
		IP:                ips[0],
		ExtraIPs:          ips[1:],
		MAC:               alloc.MAC,
		CIDR:              cidr,
		Gateway:           gateway,
		Routes:            routes,
		VMName:            alloc.VMName,
	}, nil
}

// dualStackAddresses pairs kube-ovn's comma separated ip_address, cidr and
// gateway annotations by IP family and returns the addresses in CIDR notation,
// IPv4 first, with the CIDR and gateway of the first one.
func dualStackAddresses(ipList, cidrList, gatewayList string) (ips []string, cidr, gateway string) {
	cidrs := strings.Split(cidrList, ",")
	gateways := strings.Split(gatewayList, ",")
	byFamily := func(values []string, v4 bool) string {
		for _, value := range values {
			ip, _, _ := strings.Cut(value, "/")
			if parsed := net.ParseIP(ip); parsed != nil && (parsed.To4() != nil) == v4 {
				return value
			}
		}
		return ""
	}
	for _, v4 := range []bool{true, false} {
		ip := byFamily(strings.Split(ipList, ","), v4)
		if ip == "" {
			continue
		}
		familyCIDR, familyGateway := byFamily(cidrs, v4), byFamily(gateways, v4)
		if len(ips) == 0 {
			cidr, gateway = familyCIDR, familyGateway
		}
		ips = append(ips, withMask(ip, familyCIDR))
	}
	if len(ips) == 0 {
		ips = []string{withMask(ipList, cidrList)}
	}
	return ips, cidr, gateway
}

// portName returns the logical switch port name kube-ovn-controller uses for a
// pod or VM NIC, matching kube-ovn's ovs.PodNameToPortName.
func portName(owner, namespace, provider string) string {
	if provider == "" || provider == defaultProvider {
		return owner + "." + namespace
	}
	return owner + "." + namespace + "." + provider
}

// withMask combines a bare IP (e.g. "172.23.0.5") with the mask length of the
// subnet CIDR (e.g. "172.23.0.0/24") to produce "172.23.0.5/24". If ip already
// has a mask or cidr cannot be parsed, ip is returned unchanged.
func withMask(ip, cidr string) string {
	if ip == "" || strings.Contains(ip, "/") {
		return ip
	}
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return ip
	}
	ones, _ := ipnet.Mask.Size()
	return fmt.Sprintf("%s/%d", ip, ones)
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// kubevirtVMLabel is the label KubeVirt sets on every virt-launcher pod of a VMI.
const kubevirtVMLabel = "vm.kubevirt.io/name"

// lookupPodForClaim returns the pod on nodeName that the claim is reserved for.
// kubelet always populates ReservedFor before calling PrepareResourceClaims.
//
// A claim stands for one NIC with one address, so it can only be shared by the
// virt-launcher pods of one KubeVirt VM: the source and the target pod of a
// live migration, which kube-ovn gives the VM's address (keep-vm-ip). Each node
// prepares the claim for its own pod. kubelet prepares a claim only once per
// node, so a second consumer on the same node would get no NIC; it is rejected.
func lookupPodForClaim(ctx context.Context, client coreclientset.Interface, claim *resourceapi.ResourceClaim, nodeName string) (podName, podNS, podUID string, err error) {
	podNS = claim.Namespace
	var consumers, onNode []*corev1.Pod
	refs := 0
	for _, ref := range claim.Status.ReservedFor {
		if ref.Resource != "pods" || ref.APIGroup != "" {
			continue
		}
		refs++
		pod, getErr := client.CoreV1().Pods(podNS).Get(ctx, ref.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(getErr) || (getErr == nil && pod.UID != ref.UID) {
			// A consumer that is already gone, e.g. a migration's source pod.
			continue
		}
		if getErr != nil {
			return "", "", "", fmt.Errorf("get pod %s/%s: %w", podNS, ref.Name, getErr)
		}
		consumers = append(consumers, pod)
		if pod.Spec.NodeName == nodeName {
			onNode = append(onNode, pod)
		}
	}
	if refs == 0 {
		return "", "", "", fmt.Errorf("claim %s/%s has no pod in ReservedFor", claim.Namespace, claim.Name)
	}
	if err := checkSharedClaim(claim, consumers); err != nil {
		return "", "", "", err
	}
	switch len(onNode) {
	case 0:
		return "", "", "", fmt.Errorf("claim %s/%s has no pod on node %s in ReservedFor", claim.Namespace, claim.Name, nodeName)
	case 1:
		return onNode[0].Name, podNS, string(onNode[0].UID), nil
	default:
		return "", "", "", fmt.Errorf("claim %s/%s is reserved for more than one pod on node %s (%s, %s); "+
			"a DRA NIC is prepared once per node, use one claim per pod", claim.Namespace, claim.Name, nodeName, onNode[0].Name, onNode[1].Name)
	}
}

// checkSharedClaim rejects a claim shared by pods that are not the
// virt-launcher pods of one KubeVirt VM.
func checkSharedClaim(claim *resourceapi.ResourceClaim, consumers []*corev1.Pod) error {
	if len(consumers) < 2 {
		return nil
	}
	vm := consumers[0].Labels[kubevirtVMLabel]
	for _, pod := range consumers {
		if vm == "" || pod.Labels[kubevirtVMLabel] != vm {
			return fmt.Errorf("claim %s/%s is shared by pods %s and %s; a DRA NIC claim can only be shared by "+
				"the virt-launcher pods of one KubeVirt VM (live migration), use one claim per pod",
				claim.Namespace, claim.Name, consumers[0].Name, pod.Name)
		}
	}
	return nil
}

// extractSubnetName reads the subnetName device attribute.
func extractSubnetName(device resourceapi.Device, driver string) string {
	return publisher.StringAttribute(device, driver, publisher.AttrSubnetName)
}

// validateSecondaryProvider rejects subnets that cannot back a secondary NIC.
// A DRA NIC is always secondary, so the subnet needs a dedicated kube-ovn
// provider (e.g. "<subnet>.<namespace>.ovn", the same convention a multus
// NetworkAttachmentDefinition uses). The default provider "ovn" (or an empty
// provider) names the pod's PRIMARY interface and is never valid here; using it
// would make kube-ovn either hijack the primary port or silently ignore the
// reservation.
func validateSecondaryProvider(provider, subnetName string) error {
	if provider == "" || provider == defaultProvider {
		return fmt.Errorf("subnet %q uses the default provider %q and cannot be attached as a "+
			"secondary NIC; set a dedicated spec.provider on the kube-ovn Subnet (e.g. %q)",
			subnetName, defaultProvider, subnetName+".<namespace>.ovn")
	}
	return nil
}

// extractProvider reads the provider device attribute (the kube-ovn Subnet's
// spec.provider). Empty when the subnet has no explicit provider.
func extractProvider(device resourceapi.Device, driver string) string {
	return publisher.StringAttribute(device, driver, publisher.AttrProvider)
}
