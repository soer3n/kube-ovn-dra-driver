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

// Package kubeovnip manages kube-ovn IP CRD objects (ips.kubeovn.io/v1) for
// DRA-driven IP pre-reservation — the Multus-free IPAM path.
//
// Unlike the pod-annotation path (pkg/annotation), this does NOT require a
// NetworkAttachmentDefinition or the k8s.v1.cni.cncf.io/networks annotation:
// the driver creates an IP object directly and kube-ovn-controller's reserved-
// IP reconciler (pkg/controller/ip.go handleAddReservedIP) allocates an address
// from the subnet pool and writes it back into the IP object's spec. That makes
// it the IPAM building block for replacing Multus.
//
// HARD CONSTRAINTS verified against kube-ovn (pkg/controller/ip.go,
// pkg/ovs/util.go), required for the controller to act on the object:
//
//   - metadata.name MUST equal ovs.PodNameToPortName(podName, namespace,
//     provider) — i.e. "<pod>.<ns>" for the default provider "ovn", else
//     "<pod>.<ns>.<provider>". If the name does not match, the controller
//     silently ignores the object (returns nil) and nothing is ever allocated.
//   - spec.subnet, spec.podName and spec.namespace must be set.
//
// The controller writes back spec.ipAddress / spec.v4IpAddress / spec.macAddress
// (NOT a gateway — read that from the Subnet CR). It reserves the address only;
// it does NOT create the OVN Logical Switch Port. That is fine for VLAN underlay
// (pure L2 on br-<provider>, no LSP needed); OVN overlay would additionally need
// the LSP created before the port binds.
//
// Because the name is pod+provider-derived, the reservation is pod-scoped, not
// claim-scoped — the pod identity is known at PrepareResourceClaims time (via
// claim.Status.ReservedFor), so this is fine.
//
// Lifecycle:
//
//	Reserve()  — create IP object, wait for the controller to populate the address
//	Release()  — delete IP object, returning the address to the subnet pool
package kubeovnip

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
)

var ipGVR = schema.GroupVersionResource{
	Group:    "kubeovn.io",
	Version:  "v1",
	Resource: "ips",
}

var subnetGVR = schema.GroupVersionResource{
	Group:    "kubeovn.io",
	Version:  "v1",
	Resource: "subnets",
}

// SubnetGatewayCIDR returns the gateway and CIDR block of a kube-ovn Subnet.
// The IP CRD reserves a bare address with no gateway/mask, so the caller needs
// the Subnet to build the full IP/CIDR and per-NIC routes.
func SubnetGatewayCIDR(ctx context.Context, dc dynamic.Interface, subnetName string) (gateway, cidr string, err error) {
	obj, err := dc.Resource(subnetGVR).Get(ctx, subnetName, metav1.GetOptions{})
	if err != nil {
		return "", "", fmt.Errorf("get subnet %q: %w", subnetName, err)
	}
	gateway, _, _ = unstructured.NestedString(obj.Object, "spec", "gateway")
	cidr, _, _ = unstructured.NestedString(obj.Object, "spec", "cidrBlock")
	return gateway, cidr, nil
}

// ovnProvider is kube-ovn's sentinel provider for the default OVN network.
const ovnProvider = "ovn"

// Allocation holds the result of a successful IP reservation.
type Allocation struct {
	// IP is the allocated address in CIDR notation (e.g. "10.200.0.5/24").
	IP string
	// MAC is the allocated MAC address (e.g. "00:00:00:aa:bb:cc").
	MAC string
}

// ReserveParams identifies the pod+subnet the address is reserved for. PodName,
// Namespace and Provider together determine the mandatory IP object name.
type ReserveParams struct {
	// SubnetName is the kube-ovn Subnet to allocate from (spec.subnet).
	SubnetName string
	// Provider is the Subnet's spec.provider; "" is treated as the default
	// "ovn" provider.
	Provider string
	// PodName and Namespace identify the pod the reservation is bound to.
	PodName   string
	Namespace string
}

// IPName returns the IP CRD object name kube-ovn requires, matching
// ovs.PodNameToPortName(podName, namespace, provider).
func IPName(podName, namespace, provider string) string {
	if provider == "" || provider == ovnProvider {
		return podName + "." + namespace
	}
	return podName + "." + namespace + "." + provider
}

// Reserve creates an IP CRD object for the given pod+subnet and waits for
// kube-ovn-controller to populate it with an allocated address.
//
// If an IP object with the same name already exists (driver restart / retry),
// it is reused — no duplicate is created.
//
// ctx should carry a reasonable deadline (30–60s recommended).
func Reserve(ctx context.Context, dc dynamic.Interface, p ReserveParams) (*Allocation, error) {
	if p.SubnetName == "" || p.PodName == "" || p.Namespace == "" {
		return nil, fmt.Errorf("kubeovnip: SubnetName, PodName and Namespace are required")
	}
	ipName := IPName(p.PodName, p.Namespace, p.Provider)
	client := dc.Resource(ipGVR)

	// Try to create; tolerate AlreadyExists (idempotent). The name MUST match
	// PodNameToPortName or the controller silently ignores the object.
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "kubeovn.io/v1",
			"kind":       "IP",
			"metadata": map[string]interface{}{
				"name": ipName,
				"labels": map[string]interface{}{
					"dra.kubeovn.io/managed-by": "kube-ovn-dra-driver",
				},
			},
			"spec": map[string]interface{}{
				"subnet":    p.SubnetName,
				"podName":   p.PodName,
				"namespace": p.Namespace,
			},
		},
	}

	_, err := client.Create(ctx, obj, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		return nil, fmt.Errorf("create IP object %q: %w", ipName, err)
	}

	// Poll until the controller writes back the allocated address.
	var alloc *Allocation
	pollErr := wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 60*time.Second, true,
		func(ctx context.Context) (bool, error) {
			current, getErr := client.Get(ctx, ipName, metav1.GetOptions{})
			if getErr != nil {
				return false, getErr
			}
			ip, _, _ := unstructured.NestedString(current.Object, "spec", "ipAddress")
			if ip == "" {
				ip, _, _ = unstructured.NestedString(current.Object, "spec", "v4IpAddress")
			}
			mac, _, _ := unstructured.NestedString(current.Object, "spec", "macAddress")
			if ip != "" && mac != "" {
				alloc = &Allocation{IP: ip, MAC: mac}
				return true, nil
			}
			return false, nil
		})
	if pollErr != nil {
		// Best-effort cleanup on timeout.
		_ = Release(context.Background(), dc, ipName)
		return nil, fmt.Errorf("waiting for kube-ovn to allocate IP for %q: %w", ipName, pollErr)
	}

	return alloc, nil
}

// Release deletes the IP CRD object, returning the address to the subnet pool.
// Tolerates NotFound (idempotent).
func Release(ctx context.Context, dc dynamic.Interface, ipName string) error {
	err := dc.Resource(ipGVR).Delete(ctx, ipName, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete IP object %q: %w", ipName, err)
	}
	return nil
}
