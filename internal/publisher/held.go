/*
 * Copyright 2026 soer3n.
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

package publisher

import (
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// The capacity a device publishes is the free addresses of the Subnet plus the
// addresses held by DRA claims:
//
//	capacity = free + held
//
// The scheduler subtracts the consumption of every allocated claim from the
// capacity. A claim whose address kube-ovn already allocated is in the free
// count already, so adding it back as "held" makes the scheduler's subtraction
// right. A claim that is allocated but has no address yet is not added back, so
// the scheduler's subtraction is the only one that counts it. The result is that
// the scheduler sees exactly the addresses still free, also while a burst of
// pods waits for kube-ovn to allocate their addresses.

// IPRef identifies an ip object of kube-ovn: the address of a pod or VM NIC.
type IPRef struct {
	Namespace string
	// Name is the pod or VM the address belongs to (spec.podName).
	Name   string
	Subnet string
}

// ParseIP reads a kube-ovn ip object. It reports false if the object lacks the
// fields that tie the address to a Subnet and its owner.
func ParseIP(obj *unstructured.Unstructured) (IPRef, bool) {
	spec, _, _ := unstructured.NestedMap(obj.Object, "spec")
	ref := IPRef{}
	ref.Name, _ = spec["podName"].(string)
	ref.Namespace, _ = spec["namespace"].(string)
	ref.Subnet, _ = spec["subnet"].(string)
	return ref, ref.Name != "" && ref.Subnet != ""
}

// OwnersOf returns the names an ip object of a pod's NIC can carry: the pod
// itself, and the VM of a KubeVirt virt-launcher pod, because kube-ovn names the
// address of a VM after the VM so that it survives restarts.
type OwnersOf func(namespace, pod string) []string

// ApplyHeld sets Held on the families of subnets from the allocated claims of
// the driver. claims are all ResourceClaims, ips are all ip objects.
func ApplyHeld(subnets []Subnet, driver string, claims []*resourceapi.ResourceClaim, ips []IPRef, owners OwnersOf) {
	byName := make(map[string]*Subnet, len(subnets))
	for i := range subnets {
		subnets[i].V4.Held, subnets[i].V6.Held = 0, 0
		byName[subnets[i].Name] = &subnets[i]
	}

	// Addresses by namespace, owner and Subnet.
	type key struct{ namespace, name, subnet string }
	addresses := make(map[key]int64, len(ips))
	for _, ip := range ips {
		addresses[key{ip.Namespace, ip.Name, ip.Subnet}]++
	}

	type use struct{ v4, v6 int64 }
	for _, claim := range claims {
		if claim.Status.Allocation == nil {
			continue
		}
		perSubnet := make(map[string]*use)
		for _, result := range claim.Status.Allocation.Devices.Results {
			if result.Driver != driver {
				continue
			}
			// Every Subnet is a pool of its own, named after the Subnet.
			subnet := byName[result.Pool]
			if subnet == nil || result.Device != DeviceName(subnet.Name) {
				continue
			}
			u := perSubnet[subnet.Name]
			if u == nil {
				u = &use{}
				perSubnet[subnet.Name] = u
			}
			v4, v6 := consumed(result, subnet)
			u.v4 += v4
			u.v6 += v6
		}
		if len(perSubnet) == 0 {
			continue
		}

		// The pods the claim is reserved for, and the owners of their addresses.
		var names []string
		for _, ref := range claim.Status.ReservedFor {
			if ref.Resource == "pods" {
				names = append(names, owners(claim.Namespace, ref.Name)...)
			}
		}
		for subnetName, u := range perSubnet {
			var have int64
			seen := make(map[string]bool, len(names))
			for _, name := range names {
				if seen[name] {
					continue
				}
				seen[name] = true
				have += addresses[key{claim.Namespace, name, subnetName}]
			}
			subnet := byName[subnetName]
			subnet.V4.Held += min(u.v4, have)
			subnet.V6.Held += min(u.v6, have)
		}
	}
}

// consumed returns the addresses of each family an allocation result takes. The
// scheduler records them in ConsumedCapacity. If it did not, for example because
// the device was allocated before it published capacity, a NIC takes one
// address of each family the Subnet has.
func consumed(result resourceapi.DeviceRequestAllocationResult, s *Subnet) (v4, v6 int64) {
	if len(result.ConsumedCapacity) > 0 {
		if q, ok := result.ConsumedCapacity[CapacityIPv4]; ok {
			v4 = q.Value()
		}
		if q, ok := result.ConsumedCapacity[CapacityIPv6]; ok {
			v6 = q.Value()
		}
		return v4, v6
	}
	if s.V4.Enabled {
		v4 = 1
	}
	if s.V6.Enabled {
		v6 = 1
	}
	return v4, v6
}
