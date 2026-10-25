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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"net/netip"
	"regexp"
	"strings"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
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
	// one address of the family per NIC. Like the attributes they carry no
	// domain, which DRA qualifies with the name of the publishing driver.
	CapacityIPv4 resourceapi.QualifiedName = "ipv4"
	CapacityIPv6 resourceapi.QualifiedName = "ipv6"

	// The device attributes. Unqualified names belong to the domain of the
	// driver that publishes them, so a CEL selector reads them as
	// device.attributes['<driver name>'].subnetName whatever the driver name.
	AttrSubnetName      resourceapi.QualifiedName = "subnetName"
	AttrSubnetType      resourceapi.QualifiedName = "subnetType"
	AttrVlanID          resourceapi.QualifiedName = "vlanId"
	AttrProvider        resourceapi.QualifiedName = "provider"
	AttrProviderNetwork resourceapi.QualifiedName = "providerNetwork"
	AttrVPC             resourceapi.QualifiedName = "vpc"

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

// DeviceName returns the name of the device of a Subnet: "subnet-<name>" if
// that is a valid DNS label, as DRA requires for device names. A Subnet name is
// a DNS subdomain, which can be longer and contain dots; such a name is
// shortened and suffixed with a hash of the full name, which keeps the device
// name stable and unique.
func DeviceName(subnet string) string {
	name := DeviceNamePrefix + subnet
	if len(validation.IsDNS1123Label(name)) == 0 {
		return name
	}
	sum := sha256.Sum256([]byte(subnet))
	hash := hex.EncodeToString(sum[:])[:deviceNameHashLength]
	base := strings.Trim(invalidLabelChars.ReplaceAllString(strings.ToLower(subnet), "-"), "-")
	base = strings.TrimRight(base[:min(len(base), validation.DNS1123LabelMaxLength-len(DeviceNamePrefix)-len(hash)-1)], "-")
	if base == "" {
		return DeviceNamePrefix + hash
	}
	return DeviceNamePrefix + base + "-" + hash
}

// deviceNameHashLength is the length of the hash suffix of a shortened device
// name; 12 hex digits make a collision between two Subnets practically
// impossible.
const deviceNameHashLength = 12

var invalidLabelChars = regexp.MustCompile(`[^a-z0-9-]+`)

// Publishable reports why a Subnet cannot back a DRA NIC, or nil if it can.
// Only such Subnets are published, so a claim cannot select a device whose
// claims would never be prepared.
func (s Subnet) Publishable() error {
	switch {
	case s.Provider == "" || s.Provider == "ovn":
		// The default provider names the pod's primary NIC.
		return fmt.Errorf("subnet %q uses the default provider; a DRA NIC needs a dedicated provider such as %q", s.Name, s.Name+".<namespace>.ovn")
	case !isOVNProvider(s.Provider):
		// A non-OVN provider (e.g. macvlan IPAM only) gets no logical switch
		// port, but the driver plugs every NIC into br-int and binds it to one.
		return fmt.Errorf("subnet %q uses the non-OVN provider %q, which the DRA NIC driver does not support", s.Name, s.Provider)
	case !s.V4.Enabled && !s.V6.Enabled:
		return fmt.Errorf("subnet %q has no IPv4 or IPv6 CIDR; a DRA NIC needs an address", s.Name)
	}
	return nil
}

// isOVNProvider mirrors kube-ovn's util.IsOvnProvider for a dedicated
// provider: "<name>.<namespace>.ovn".
func isOVNProvider(provider string) bool {
	fields := strings.Split(provider, ".")
	return len(fields) == 3 && fields[2] == "ovn"
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
		AttrSubnetName: {StringValue: ptr.To(s.Name)},
		AttrSubnetType: {StringValue: ptr.To(s.Type())},
	}
	if s.VlanID != 0 {
		attrs[AttrVlanID] = resourceapi.DeviceAttribute{IntValue: ptr.To(s.VlanID)}
	}
	if s.Provider != "" {
		attrs[AttrProvider] = resourceapi.DeviceAttribute{StringValue: ptr.To(s.Provider)}
	}
	if s.ProviderNetwork != "" {
		attrs[AttrProviderNetwork] = resourceapi.DeviceAttribute{StringValue: ptr.To(s.ProviderNetwork)}
	}
	if s.VPC != "" {
		attrs[AttrVPC] = resourceapi.DeviceAttribute{StringValue: ptr.To(s.VPC)}
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

// StringAttribute reads a string attribute of a device published by driver,
// unqualified or qualified with the driver name, which DRA treats as the same.
func StringAttribute(device resourceapi.Device, driver string, name resourceapi.QualifiedName) string {
	for _, key := range []resourceapi.QualifiedName{name, resourceapi.QualifiedName(driver + "/" + string(name))} {
		if v, ok := device.Attributes[key]; ok && v.StringValue != nil {
			return *v.StringValue
		}
	}
	return ""
}
