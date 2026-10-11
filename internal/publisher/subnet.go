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

// Package publisher builds the ResourceSlice pools of the kube-ovn NIC driver.
//
// It is independent of any binary: it turns a snapshot of kube-ovn Subnets,
// Vlans, ip objects and ResourceClaims into pools, so the same logic can run in
// the driver's publisher or, later, inside kube-ovn-controller.
//
// Every Subnet becomes one device in its own pool, named after the Subnet. An
// overlay Subnet is available on all nodes. A VLAN underlay Subnet is available
// on the nodes where its ProviderNetwork is ready, because only those nodes have
// the provider bridge and the uplink. Optionally the device carries the free
// addresses of the Subnet as consumable capacity, so the scheduler refuses a pod
// when the pool is exhausted.
package publisher

import (
	"fmt"
	"math"
	"net/netip"
	"strings"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
)

// The kube-ovn resources the publisher reads.
var (
	SubnetGVR = schema.GroupVersionResource{Group: "kubeovn.io", Version: "v1", Resource: "subnets"}
	// VlanGVR is the Vlan a Subnet references by name via spec.vlan. It holds the
	// 802.1q id (spec.id) and the ProviderNetwork name (spec.provider).
	VlanGVR = schema.GroupVersionResource{Group: "kubeovn.io", Version: "v1", Resource: "vlans"}
	// IPGVR is the ip object kube-ovn creates for the address of every NIC.
	IPGVR = schema.GroupVersionResource{Group: "kubeovn.io", Version: "v1", Resource: "ips"}
)

const (
	// DeviceNamePrefix is put in front of the Subnet name to get the device name.
	DeviceNamePrefix = "subnet-"

	// CapacityIPv4 and CapacityIPv6 are the consumable capacities of a device,
	// one address of the family per NIC.
	CapacityIPv4 resourceapi.QualifiedName = "nic.kubeovn.io/ipv4"
	CapacityIPv6 resourceapi.QualifiedName = "nic.kubeovn.io/ipv6"

	// maxCount saturates address counts. An IPv6 Subnet has far more addresses
	// than a capacity can express, and more than a scheduler could ever hand out.
	maxCount = int64(1) << 53
)

// Family is the state of one address family of a Subnet.
type Family struct {
	// Enabled reports whether the Subnet has addresses of the family.
	Enabled bool
	// Free is the number of unallocated addresses, as the Subnet status reports.
	Free int64
	// Held is the number of addresses taken by DRA claims whose address kube-ovn
	// already allocated. The Subnet status counts them as used, but the
	// scheduler subtracts every allocated claim again, so they are added back.
	Held int64
}

// Capacity returns what the device publishes for the family.
func (f Family) Capacity() int64 {
	return min(max(f.Free+f.Held, 0), maxCount)
}

// Subnet is the part of a kube-ovn Subnet the driver cares about.
type Subnet struct {
	Name     string
	Provider string
	VPC      string
	// VlanID and ProviderNetwork are set for a VLAN underlay Subnet only.
	VlanID          int64
	ProviderNetwork string

	V4 Family
	V6 Family
}

// Type returns "vlan" for a VLAN underlay Subnet and "ovn" for an overlay one.
func (s Subnet) Type() string {
	if s.VlanID != 0 {
		return "vlan"
	}
	return "ovn"
}

// DeviceName returns the name of the device of a Subnet.
func DeviceName(subnet string) string {
	return DeviceNamePrefix + subnet
}

// SubnetName returns the Subnet a device name belongs to.
func SubnetName(device string) string {
	if len(device) > len(DeviceNamePrefix) && strings.HasPrefix(device, DeviceNamePrefix) {
		return device[len(DeviceNamePrefix):]
	}
	return device
}

// VlanInfo is what a Vlan object tells about a Subnet.
type VlanInfo struct {
	ID       int64
	Provider string
}

// ParseVlan reads a kube-ovn Vlan object.
func ParseVlan(obj *unstructured.Unstructured) VlanInfo {
	info := VlanInfo{}
	spec, _ := obj.Object["spec"].(map[string]any)
	switch id := spec["id"].(type) {
	case int64:
		info.ID = id
	case float64:
		info.ID = int64(id)
	}
	info.Provider, _ = spec["provider"].(string)
	return info
}

// ParseSubnet reads a kube-ovn Subnet. vlans resolves the Vlan a Subnet
// references by name; it returns an error if the Vlan cannot be read.
func ParseSubnet(obj *unstructured.Unstructured, vlans func(name string) (VlanInfo, error)) (Subnet, error) {
	s := Subnet{Name: obj.GetName()}
	spec, _, err := unstructured.NestedMap(obj.Object, "spec")
	if err != nil || spec == nil {
		return Subnet{}, fmt.Errorf("subnet %q has no spec", s.Name)
	}
	s.Provider, _ = spec["provider"].(string)
	s.VPC, _ = spec["vpc"].(string)

	// A VLAN underlay Subnet does not carry the id itself; it references a Vlan
	// by name, which holds the 802.1q id and the ProviderNetwork.
	if vlanName, _ := spec["vlan"].(string); vlanName != "" {
		vlan, err := vlans(vlanName)
		if err != nil {
			return Subnet{}, fmt.Errorf("get vlan %q referenced by subnet %q: %w", vlanName, s.Name, err)
		}
		if vlan.ID != 0 {
			s.VlanID = vlan.ID
			s.ProviderNetwork = vlan.Provider
		}
	}

	cidr, _ := spec["cidrBlock"].(string)
	for part := range strings.SplitSeq(cidr, ",") {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(part))
		if err != nil {
			continue
		}
		if prefix.Addr().Is4() {
			s.V4.Enabled = true
		} else {
			s.V6.Enabled = true
		}
	}
	s.V4.Free = statusCount(obj, "v4availableIPs")
	s.V6.Free = statusCount(obj, "v6availableIPs")
	return s, nil
}

// statusCount reads an address counter of the Subnet status. kube-ovn writes
// them as plain JSON numbers of any size, which decode to int64 or float64.
func statusCount(obj *unstructured.Unstructured, field string) int64 {
	value, found, _ := unstructured.NestedFieldNoCopy(obj.Object, "status", field)
	if !found {
		return 0
	}
	switch v := value.(type) {
	case int64:
		return min(max(v, 0), maxCount)
	case int:
		return min(max(int64(v), 0), maxCount)
	case float64:
		if math.IsNaN(v) || v < 0 {
			return 0
		}
		if v >= float64(maxCount) {
			return maxCount
		}
		return int64(v)
	case string:
		var n float64
		if _, err := fmt.Sscan(v, &n); err != nil || n < 0 {
			return 0
		}
		return min(int64(min(n, float64(maxCount))), maxCount)
	}
	return 0
}

// Device builds the device of a Subnet. With capacity, the device also carries
// the free addresses per family as consumable capacity.
func Device(s Subnet, withCapacity bool) resourceapi.Device {
	attrs := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
		"nic.kubeovn.io/subnetName": {StringValue: ptr.To(s.Name)},
		"nic.kubeovn.io/subnetType": {StringValue: ptr.To(s.Type())},
	}
	if s.VlanID != 0 {
		attrs["nic.kubeovn.io/vlanId"] = resourceapi.DeviceAttribute{IntValue: ptr.To(s.VlanID)}
	}
	if s.Provider != "" {
		attrs["nic.kubeovn.io/provider"] = resourceapi.DeviceAttribute{StringValue: ptr.To(s.Provider)}
	}
	if s.ProviderNetwork != "" {
		attrs["nic.kubeovn.io/providerNetwork"] = resourceapi.DeviceAttribute{StringValue: ptr.To(s.ProviderNetwork)}
	}
	if s.VPC != "" {
		attrs["nic.kubeovn.io/vpc"] = resourceapi.DeviceAttribute{StringValue: ptr.To(s.VPC)}
	}

	device := resourceapi.Device{
		Name:       DeviceName(s.Name),
		Attributes: attrs,
		// A kube-ovn Subnet is a shared IP pool, not an exclusive device: many
		// pods can each take an address from it. Allow the device to be
		// allocated to multiple claims (each pod still gets its own address and
		// logical switch port). Requires the DRAConsumableCapacity feature gate
		// on apiserver, scheduler and kubelet; it is beta and on by default from
		// Kubernetes 1.36.
		AllowMultipleAllocations: ptr.To(true),
	}
	if withCapacity {
		device.Capacity = capacities(s)
	}
	return device
}

// capacities publishes one capacity per address family of the Subnet. A NIC
// takes exactly one address of each family, which the request policy states, so
// claims need no capacity request.
func capacities(s Subnet) map[resourceapi.QualifiedName]resourceapi.DeviceCapacity {
	out := make(map[resourceapi.QualifiedName]resourceapi.DeviceCapacity)
	for name, family := range map[resourceapi.QualifiedName]Family{CapacityIPv4: s.V4, CapacityIPv6: s.V6} {
		if !family.Enabled {
			continue
		}
		one := resource.MustParse("1")
		out[name] = resourceapi.DeviceCapacity{
			Value: *resource.NewQuantity(family.Capacity(), resource.DecimalSI),
			RequestPolicy: &resourceapi.CapacityRequestPolicy{
				Default:     &one,
				ValidValues: []resource.Quantity{one},
			},
		}
	}
	return out
}
