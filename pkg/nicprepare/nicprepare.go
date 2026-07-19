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

// Package nicprepare implements the kube-ovn NIC lifecycle for DRA-allocated
// virtual NICs.
//
// # Design
//
// All plumbing is delegated to the existing kube-ovn stack — there is no
// direct OVS, netlink, or veth management in this driver.
//
// The driver's only job is to write (and later remove) kube-ovn pod
// annotations. kube-ovn-controller reacts to those annotations and:
//
//  1. Allocates an IP/MAC from the requested Subnet (writes back
//     ovn.kubernetes.io/ip_address_<iface>, mac_address_<iface>, gateway_<iface>).
//  2. Creates / binds the OVN Logical Switch Port.
//
// kube-ovn-cni then wires the actual veth + OVS port when the container
// runtime calls the CNI plugin during pod sandbox creation — exactly as it
// would for a regular kube-ovn pod. No NRI hook, no Phase 2.
//
// # Lifecycle
//
//   - RequestIPAM  — called from PrepareResourceClaims; writes the
//     logical_switch annotation and waits for kube-ovn-controller to confirm.
//   - ReleaseIPAM  — called from UnprepareResourceClaims or on error; removes
//     all kube-ovn annotations for the interface so the address returns to
//     the subnet pool.
package nicprepare

import (
	"context"
	"fmt"
	"net"
	"strings"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	coreclientset "k8s.io/client-go/kubernetes"

	"github.com/soer3n/kube-ovn-dra-driver/pkg/annotation"
	"github.com/soer3n/kube-ovn-dra-driver/pkg/kubeovnip"
)

// NicDeviceConfig holds everything the driver needs to track for one NIC.
// It is created during PrepareResourceClaims and stored keyed by pod UID so
// ReleaseIPAM can clean up annotations if UnprepareResourceClaims is called.
type NicDeviceConfig struct {
	// DeviceName is the DRA device name (e.g. "subnet-myvlan").
	DeviceName string
	// IfaceName is the desired interface name inside the pod netns.
	IfaceName string
	// SubnetName is the kube-ovn Subnet name.
	SubnetName string
	// Provider is the kube-ovn Subnet's spec.provider — the key used for all
	// per-NIC annotations (and for annotation cleanup on release).
	Provider string
	// PodName, PodNamespace and PodUID identify the target pod. PodUID is the
	// key the NRI sandbox hook uses to find the pending attach Spec.
	PodName      string
	PodNamespace string
	PodUID       string

	// IPAM result, captured from WaitForAllocation. Carried so the caller can
	// build a plumbing.Spec for the attach step without re-reading annotations.
	IP      string
	MAC     string
	CIDR    string
	Gateway string
}

// RequestIPAM writes the kube-ovn subnet annotation on the pod and waits for
// kube-ovn-controller to confirm the allocation.
//
// Called from PrepareResourceClaims (slow path is fine here).
// The pod sandbox does not need to exist yet — annotations can be set before
// the container runtime starts the sandbox.
//
// kube-ovn-cni automatically wires the veth + OVS port when the CNI plugin
// runs on sandbox creation; no further action is needed from this driver.
func RequestIPAM(
	ctx context.Context,
	client coreclientset.Interface,
	claim *resourceapi.ResourceClaim,
	result *resourceapi.DeviceRequestAllocationResult,
	device resourceapi.Device,
	ifaceName string,
) (*NicDeviceConfig, error) {
	// 1. Resolve the pod this claim is reserved for.
	podName, podNS, podUID, err := lookupPodForClaim(ctx, client, claim)
	if err != nil {
		return nil, fmt.Errorf("find pod for claim %s/%s: %w", claim.Namespace, claim.Name, err)
	}

	// 2. Extract the target subnet and provider from the device attributes.
	subnetName := extractSubnetName(device)
	if subnetName == "" {
		return nil, fmt.Errorf("device %s has no subnetName attribute", device.Name)
	}
	// kube-ovn keys per-NIC annotations by the subnet's provider. The default
	// OVN network uses the sentinel provider "ovn"; underlay/secondary subnets
	// carry an explicit provider (e.g. "external.vlan100-subnet.ovn").
	provider := extractProvider(device)
	if provider == "" {
		provider = "ovn"
	}

	// 3. Write <provider>.kubernetes.io/logical_switch=<subnetName>.
	//    kube-ovn-controller reconciles this and writes back the allocated
	//    ip_address / mac_address / cidr / gateway annotations + allocated=true.
	writer := annotation.NewWriter(client)
	if err := writer.RequestSubnet(ctx, podNS, podName, provider, subnetName); err != nil {
		return nil, fmt.Errorf("write kube-ovn subnet annotation: %w", err)
	}

	// 4. Wait for confirmation — ensures the address is reserved before
	//    PrepareResourceClaims returns.
	alloc, err := annotation.WaitForAllocation(ctx, client, podNS, podName, provider)
	if err != nil {
		_ = writer.ReleaseSubnet(ctx, podNS, podName, provider)
		return nil, fmt.Errorf("wait for kube-ovn IPAM on provider %q: %w", provider, err)
	}

	return &NicDeviceConfig{
		DeviceName:   result.Device,
		IfaceName:    ifaceName,
		SubnetName:   subnetName,
		Provider:     provider,
		PodName:      podName,
		PodNamespace: podNS,
		PodUID:       podUID,
		IP:           alloc.IP,
		MAC:          alloc.MAC,
		CIDR:         alloc.CIDR,
		Gateway:      alloc.Gateway,
	}, nil
}

// ReleaseIPAM removes the kube-ovn annotations for this NIC from the pod,
// returning the address to the subnet pool.
//
// Called from UnprepareResourceClaims or during error cleanup.
func ReleaseIPAM(ctx context.Context, client coreclientset.Interface, cfg *NicDeviceConfig) error {
	return annotation.NewWriter(client).ReleaseSubnet(ctx, cfg.PodNamespace, cfg.PodName, cfg.Provider)
}

// RequestIPAMViaIPObject is the Multus-free IPAM path: instead of writing the
// per-provider pod annotation (which kube-ovn-controller only reconciles for
// providers it learns from a NetworkAttachmentDefinition), it reserves an
// address by creating an ips.kubeovn.io object. No NAD, no Multus.
//
// It resolves the pod, reserves the address, and reads the Subnet's gateway/CIDR
// (the IP object carries a bare address with no mask/gateway) so the returned
// NicDeviceConfig has a CIDR-form IP ready for the plumbing attach.
func RequestIPAMViaIPObject(
	ctx context.Context,
	client coreclientset.Interface,
	dc dynamic.Interface,
	claim *resourceapi.ResourceClaim,
	result *resourceapi.DeviceRequestAllocationResult,
	device resourceapi.Device,
	ifaceName string,
) (*NicDeviceConfig, error) {
	podName, podNS, podUID, err := lookupPodForClaim(ctx, client, claim)
	if err != nil {
		return nil, fmt.Errorf("find pod for claim %s/%s: %w", claim.Namespace, claim.Name, err)
	}

	subnetName := extractSubnetName(device)
	if subnetName == "" {
		return nil, fmt.Errorf("device %s has no subnetName attribute", device.Name)
	}
	// A DRA-attached NIC is always a SECONDARY interface, so the subnet must carry
	// a dedicated provider (kube-ovn's multus convention "<name>.<ns>.ovn"). kube-
	// ovn derives the IP object / LSP name from PodNameToPortName(pod, ns, provider)
	// and requires it to match the subnet's spec.provider. The default provider
	// "ovn" (or empty) names the pod's PRIMARY port "<pod>.<ns>", so it can never
	// back a secondary NIC — reject it with a clear message instead of creating an
	// IP object kube-ovn will silently never reconcile (which would hang prepare).
	provider := extractProvider(device)
	if err := validateSecondaryProvider(provider, subnetName); err != nil {
		return nil, err
	}

	alloc, err := kubeovnip.Reserve(ctx, dc, kubeovnip.ReserveParams{
		SubnetName: subnetName,
		Provider:   provider,
		PodName:    podName,
		Namespace:  podNS,
	})
	if err != nil {
		return nil, fmt.Errorf("reserve IP object for subnet %q: %w", subnetName, err)
	}

	gateway, cidr, err := kubeovnip.SubnetGatewayCIDR(ctx, dc, subnetName)
	if err != nil {
		_ = kubeovnip.Release(ctx, dc, kubeovnip.IPName(podName, podNS, provider))
		return nil, fmt.Errorf("read subnet %q gateway/cidr: %w", subnetName, err)
	}

	return &NicDeviceConfig{
		DeviceName:   result.Device,
		IfaceName:    ifaceName,
		SubnetName:   subnetName,
		Provider:     provider,
		PodName:      podName,
		PodNamespace: podNS,
		PodUID:       podUID,
		IP:           withMask(alloc.IP, cidr),
		MAC:          alloc.MAC,
		CIDR:         cidr,
		Gateway:      gateway,
	}, nil
}

// ReleaseIPAMViaIPObject deletes the ips.kubeovn.io object, returning the
// address to the subnet pool.
func ReleaseIPAMViaIPObject(ctx context.Context, dc dynamic.Interface, cfg *NicDeviceConfig) error {
	return kubeovnip.Release(ctx, dc, kubeovnip.IPName(cfg.PodName, cfg.PodNamespace, cfg.Provider))
}

// withMask combines a bare IP (e.g. "172.23.0.5") with the mask length of the
// subnet CIDR (e.g. "172.23.0.0/24") to produce "172.23.0.5/24". If ip already
// has a mask or cidr is unparizable, ip is returned unchanged.
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

// lookupPodForClaim resolves the pod name/namespace from claim.Status.ReservedFor.
// kubelet always populates ReservedFor before calling PrepareResourceClaims.
func lookupPodForClaim(ctx context.Context, client coreclientset.Interface, claim *resourceapi.ResourceClaim) (podName, podNS, podUID string, err error) {
	podNS = claim.Namespace
	for _, ref := range claim.Status.ReservedFor {
		if ref.Resource != "pods" || ref.APIGroup != "" {
			continue
		}
		uid := types.UID(ref.UID)
		pods, listErr := client.CoreV1().Pods(podNS).List(ctx, metav1.ListOptions{
			FieldSelector: fmt.Sprintf("metadata.uid=%s", uid),
		})
		if listErr != nil || len(pods.Items) == 0 {
			pods, listErr = client.CoreV1().Pods(podNS).List(ctx, metav1.ListOptions{})
			if listErr != nil {
				return "", "", "", fmt.Errorf("list pods in namespace %s: %w", podNS, listErr)
			}
		}
		for _, pod := range pods.Items {
			if pod.UID == uid {
				return pod.Name, podNS, string(uid), nil
			}
		}
		return "", "", "", fmt.Errorf("pod with UID %s not found in namespace %s", uid, podNS)
	}
	return "", "", "", fmt.Errorf("claim %s/%s has no pod in ReservedFor", claim.Namespace, claim.Name)
}

// extractSubnetName reads the subnetName device attribute.
func extractSubnetName(device resourceapi.Device) string {
	if device.Attributes == nil {
		return ""
	}
	if v := device.Attributes["nic.kubeovn.io/subnetName"]; v.StringValue != nil {
		return *v.StringValue
	}
	return ""
}

// validateSecondaryProvider rejects subnets that cannot back a secondary NIC.
// A DRA NIC is always secondary, so the subnet needs a dedicated kube-ovn
// provider (e.g. "<subnet>.<namespace>.ovn", the same convention a multus
// NetworkAttachmentDefinition uses). The default provider "ovn" (or an empty
// provider) names the pod's PRIMARY interface and is never valid here; using it
// would make kube-ovn either hijack the primary port or silently ignore the
// reservation.
func validateSecondaryProvider(provider, subnetName string) error {
	if provider == "" || provider == "ovn" {
		return fmt.Errorf("subnet %q uses the default provider %q and cannot be attached as a "+
			"secondary NIC; set a dedicated spec.provider on the kube-ovn Subnet (e.g. %q)",
			subnetName, "ovn", subnetName+".<namespace>.ovn")
	}
	return nil
}

// extractProvider reads the provider device attribute (the kube-ovn Subnet's
// spec.provider). Empty when the subnet has no explicit provider.
func extractProvider(device resourceapi.Device) string {
	if device.Attributes == nil {
		return ""
	}
	if v := device.Attributes["nic.kubeovn.io/provider"]; v.StringValue != nil {
		return *v.StringValue
	}
	return ""
}
