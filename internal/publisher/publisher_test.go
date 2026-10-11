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

package publisher

import (
	"fmt"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func subnetObject(name string, spec, status map[string]any) *unstructured.Unstructured {
	obj := map[string]any{
		"apiVersion": "kubeovn.io/v1",
		"kind":       "Subnet",
		"metadata":   map[string]any{"name": name},
		"spec":       spec,
	}
	if status != nil {
		obj["status"] = status
	}
	return &unstructured.Unstructured{Object: obj}
}

func noVlans(name string) (VlanInfo, error) { return VlanInfo{}, fmt.Errorf("vlan %s not found", name) }

func TestParseSubnetOverlay(t *testing.T) {
	obj := subnetObject("ovn-subnet", map[string]any{
		"cidrBlock": "10.200.0.0/24", "provider": "ovn-subnet.default.ovn", "vpc": "ovn-cluster",
	}, map[string]any{"v4availableIPs": int64(250)})

	s, err := ParseSubnet(obj, noVlans)
	require.NoError(t, err)
	assert.Equal(t, "ovn", s.Type())
	assert.Equal(t, "ovn-cluster", s.VPC)
	assert.Equal(t, "ovn-subnet.default.ovn", s.Provider)
	assert.Empty(t, s.ProviderNetwork)
	assert.True(t, s.V4.Enabled)
	assert.False(t, s.V6.Enabled)
	assert.Equal(t, int64(250), s.V4.Free)
}

func TestParseSubnetVlanResolvesProviderNetwork(t *testing.T) {
	obj := subnetObject("vlan100-subnet", map[string]any{
		"cidrBlock": "172.23.0.0/24", "provider": "vlan100-subnet.default.external", "vlan": "vlan100",
	}, nil)

	s, err := ParseSubnet(obj, func(name string) (VlanInfo, error) {
		require.Equal(t, "vlan100", name)
		return VlanInfo{ID: 100, Provider: "external"}, nil
	})
	require.NoError(t, err)
	assert.Equal(t, "vlan", s.Type())
	assert.Equal(t, int64(100), s.VlanID)
	assert.Equal(t, "external", s.ProviderNetwork)
}

func TestParseSubnetMissingVlanFails(t *testing.T) {
	obj := subnetObject("vlan100-subnet", map[string]any{"cidrBlock": "172.23.0.0/24", "vlan": "gone"}, nil)
	_, err := ParseSubnet(obj, noVlans)
	assert.ErrorContains(t, err, "vlan \"gone\"")
}

func TestParseSubnetVlanWithoutIDIsOverlayLike(t *testing.T) {
	obj := subnetObject("s", map[string]any{"cidrBlock": "10.0.0.0/24", "vlan": "untagged"}, nil)
	s, err := ParseSubnet(obj, func(string) (VlanInfo, error) { return VlanInfo{ID: 0, Provider: "pn"}, nil })
	require.NoError(t, err)
	assert.Equal(t, "ovn", s.Type())
	assert.Empty(t, s.ProviderNetwork, "a Vlan without id does not tie the Subnet to a ProviderNetwork")
}

func TestParseVlan(t *testing.T) {
	vlan := func(id any) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"id": id, "provider": "external"}}}
	}
	assert.Equal(t, VlanInfo{ID: 100, Provider: "external"}, ParseVlan(vlan(int64(100))))
	assert.Equal(t, VlanInfo{ID: 200, Provider: "external"}, ParseVlan(vlan(float64(200))))
	assert.Equal(t, VlanInfo{}, ParseVlan(&unstructured.Unstructured{Object: map[string]any{}}))
}

func TestParseSubnetDualStack(t *testing.T) {
	obj := subnetObject("dual", map[string]any{"cidrBlock": "10.0.0.0/24,fd00::/112"},
		map[string]any{"v4availableIPs": int64(10), "v6availableIPs": float64(20)})
	s, err := ParseSubnet(obj, noVlans)
	require.NoError(t, err)
	assert.True(t, s.V4.Enabled)
	assert.True(t, s.V6.Enabled)
	assert.Equal(t, int64(10), s.V4.Free)
	assert.Equal(t, int64(20), s.V6.Free)
}

func TestParseSubnetSaturatesHugeCounts(t *testing.T) {
	// An IPv6 /64 has 2^64 addresses; JSON decodes it to a float64.
	obj := subnetObject("v6", map[string]any{"cidrBlock": "fd00::/64"},
		map[string]any{"v6availableIPs": math.Pow(2, 64)})
	s, err := ParseSubnet(obj, noVlans)
	require.NoError(t, err)
	assert.Equal(t, maxCount, s.V6.Free)
	assert.Equal(t, maxCount, s.V6.Capacity())
}

func TestStatusCountForms(t *testing.T) {
	status := func(v any) *unstructured.Unstructured {
		return subnetObject("s", map[string]any{}, map[string]any{"x": v})
	}
	assert.Equal(t, int64(7), statusCount(status("7"), "x"))
	assert.Equal(t, int64(0), statusCount(status("junk"), "x"))
	assert.Equal(t, int64(0), statusCount(status(int64(-3)), "x"))
	assert.Equal(t, int64(0), statusCount(status(math.NaN()), "x"))
	assert.Equal(t, int64(0), statusCount(subnetObject("s", map[string]any{}, nil), "x"))
}

func TestDeviceAttributesAndNoCapacityByDefault(t *testing.T) {
	d := Device(Subnet{
		Name: "vlan100-subnet", Provider: "p", VPC: "v", VlanID: 100, ProviderNetwork: "external",
		V4: Family{Enabled: true, Free: 5},
	}, false)

	assert.Equal(t, "subnet-vlan100-subnet", d.Name)
	assert.Nil(t, d.Capacity, "capacity is only published when asked for")
	require.NotNil(t, d.AllowMultipleAllocations)
	assert.True(t, *d.AllowMultipleAllocations)
	assert.Equal(t, "vlan100-subnet", *d.Attributes["nic.kubeovn.io/subnetName"].StringValue)
	assert.Equal(t, "vlan", *d.Attributes["nic.kubeovn.io/subnetType"].StringValue)
	assert.Equal(t, int64(100), *d.Attributes["nic.kubeovn.io/vlanId"].IntValue)
	assert.Equal(t, "external", *d.Attributes["nic.kubeovn.io/providerNetwork"].StringValue)
	assert.Equal(t, "p", *d.Attributes["nic.kubeovn.io/provider"].StringValue)
	assert.Equal(t, "v", *d.Attributes["nic.kubeovn.io/vpc"].StringValue)
}

func TestDeviceOverlayHasNoVlanAttributes(t *testing.T) {
	d := Device(Subnet{Name: "ovn-subnet"}, false)
	assert.Equal(t, "ovn", *d.Attributes["nic.kubeovn.io/subnetType"].StringValue)
	assert.NotContains(t, d.Attributes, resourceapi.QualifiedName("nic.kubeovn.io/vlanId"))
	assert.NotContains(t, d.Attributes, resourceapi.QualifiedName("nic.kubeovn.io/providerNetwork"))
}

func TestDeviceCapacityPerFamily(t *testing.T) {
	d := Device(Subnet{
		Name: "dual",
		V4:   Family{Enabled: true, Free: 10, Held: 2},
		V6:   Family{Enabled: true, Free: 100},
	}, true)

	require.Len(t, d.Capacity, 2)
	v4 := d.Capacity[CapacityIPv4]
	assert.Equal(t, int64(12), v4.Value.Value(), "free plus held")
	v6 := d.Capacity[CapacityIPv6]
	assert.Equal(t, int64(100), v6.Value.Value())

	require.NotNil(t, v4.RequestPolicy)
	assert.Equal(t, int64(1), v4.RequestPolicy.Default.Value(), "a NIC takes one address by default")
	require.Len(t, v4.RequestPolicy.ValidValues, 1)
	assert.Equal(t, int64(1), v4.RequestPolicy.ValidValues[0].Value(), "and cannot take more")
}

func TestDeviceCapacityOnlyForEnabledFamilies(t *testing.T) {
	d := Device(Subnet{Name: "v4only", V4: Family{Enabled: true, Free: 3}}, true)
	assert.Contains(t, d.Capacity, CapacityIPv4)
	assert.NotContains(t, d.Capacity, CapacityIPv6)
}

func TestFamilyCapacityNeverNegative(t *testing.T) {
	assert.Equal(t, int64(0), Family{Enabled: true, Free: -4}.Capacity())
	assert.Equal(t, int64(0), Family{Enabled: true}.Capacity(), "a full pool publishes zero")
}

func TestPoolScope(t *testing.T) {
	overlay := Pool(Subnet{Name: "ovn-subnet"}, false)
	assert.Nil(t, overlay.NodeSelector, "an overlay pool is available on all nodes")
	require.Len(t, overlay.Slices, 1)
	require.Len(t, overlay.Slices[0].Devices, 1)

	vlan := Pool(Subnet{Name: "vlan100-subnet", VlanID: 100, ProviderNetwork: "external"}, false)
	require.NotNil(t, vlan.NodeSelector)
	require.Len(t, vlan.NodeSelector.NodeSelectorTerms, 1, "a node selector must use exactly one term")
	exprs := vlan.NodeSelector.NodeSelectorTerms[0].MatchExpressions
	require.Len(t, exprs, 1)
	assert.Equal(t, "external.provider-network.kubernetes.io/ready", exprs[0].Key)
	assert.Equal(t, corev1.NodeSelectorOpIn, exprs[0].Operator)
	assert.Equal(t, []string{"true"}, exprs[0].Values)
}

func TestPoolsAreNamedAfterTheSubnet(t *testing.T) {
	pools := Pools([]Subnet{{Name: "a"}, {Name: "b", VlanID: 7, ProviderNetwork: "pn"}}, true)
	require.Len(t, pools, 2)
	assert.Contains(t, pools, "a")
	assert.Contains(t, pools, "b")
	assert.Equal(t, "subnet-a", pools["a"].Slices[0].Devices[0].Name)
	assert.Empty(t, pools["a"].Slices[0].Devices[0].Capacity, "no families, no capacity entries")
}

func TestSubnetNameFromDevice(t *testing.T) {
	assert.Equal(t, "ovn-subnet", SubnetName("subnet-ovn-subnet"))
	assert.Equal(t, "other", SubnetName("other"))
	assert.Equal(t, "subnet-", SubnetName("subnet-"))
	assert.Equal(t, "subnet-x", DeviceName("x"))
}

// ---- held addresses ----

const testDriver = "nic.kubeovn.io"

func claim(namespace, name string, pods []string, results ...resourceapi.DeviceRequestAllocationResult) *resourceapi.ResourceClaim {
	c := &resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
	c.Status.Allocation = &resourceapi.AllocationResult{Devices: resourceapi.DeviceAllocationResult{Results: results}}
	for _, p := range pods {
		c.Status.ReservedFor = append(c.Status.ReservedFor, resourceapi.ResourceClaimConsumerReference{Resource: "pods", Name: p})
	}
	return c
}

func result(device string, consumed map[resourceapi.QualifiedName]string) resourceapi.DeviceRequestAllocationResult {
	r := resourceapi.DeviceRequestAllocationResult{Driver: testDriver, Pool: "p", Device: device, Request: "nic"}
	if consumed != nil {
		r.ConsumedCapacity = map[resourceapi.QualifiedName]resource.Quantity{}
		for k, v := range consumed {
			r.ConsumedCapacity[k] = resource.MustParse(v)
		}
	}
	return r
}

func ownersAsPods(_, pod string) []string { return []string{pod} }

func v4Subnet(name string) Subnet { return Subnet{Name: name, V4: Family{Enabled: true, Free: 10}} }

func TestHeldCountsOnlyClaimsWithAnAddress(t *testing.T) {
	subnets := []Subnet{v4Subnet("s")}
	claims := []*resourceapi.ResourceClaim{
		claim("ns", "has-ip", []string{"pod-a"}, result("subnet-s", map[resourceapi.QualifiedName]string{CapacityIPv4: "1"})),
		claim("ns", "pending", []string{"pod-b"}, result("subnet-s", map[resourceapi.QualifiedName]string{CapacityIPv4: "1"})),
	}
	ips := []IPRef{{Namespace: "ns", Name: "pod-a", Subnet: "s"}}

	ApplyHeld(subnets, testDriver, claims, ips, ownersAsPods)

	assert.Equal(t, int64(1), subnets[0].V4.Held, "the claim without an address yet is left to the scheduler")
	assert.Equal(t, int64(11), subnets[0].V4.Capacity())
}

func TestHeldIgnoresUnallocatedForeignAndUnknownClaims(t *testing.T) {
	subnets := []Subnet{v4Subnet("s")}
	unallocated := &resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "u"}}
	foreign := claim("ns", "f", []string{"pod"}, resourceapi.DeviceRequestAllocationResult{Driver: "other.io", Device: "subnet-s"})
	otherDevice := claim("ns", "o", []string{"pod"}, result("gpu-0", nil))
	unknownSubnet := claim("ns", "k", []string{"pod"}, result("subnet-gone", nil))
	ips := []IPRef{{Namespace: "ns", Name: "pod", Subnet: "s"}, {Namespace: "ns", Name: "pod", Subnet: "gone"}}

	ApplyHeld(subnets, testDriver, []*resourceapi.ResourceClaim{unallocated, foreign, otherDevice, unknownSubnet}, ips, ownersAsPods)

	assert.Equal(t, int64(0), subnets[0].V4.Held)
}

func TestHeldFollowsTheVMThatOwnsTheAddress(t *testing.T) {
	// kube-ovn names the address of a VM after the VM, the claim is reserved for
	// the virt-launcher pod.
	subnets := []Subnet{v4Subnet("s")}
	claims := []*resourceapi.ResourceClaim{claim("ns", "c", []string{"virt-launcher-vm-1-abcde"}, result("subnet-s", nil))}
	ips := []IPRef{{Namespace: "ns", Name: "vm-1", Subnet: "s"}}
	owners := func(_, pod string) []string { return []string{pod, "vm-1"} }

	ApplyHeld(subnets, testDriver, claims, ips, owners)

	assert.Equal(t, int64(1), subnets[0].V4.Held)
}

func TestHeldDoesNotCountAddressesOfOtherNamespaces(t *testing.T) {
	subnets := []Subnet{v4Subnet("s")}
	claims := []*resourceapi.ResourceClaim{claim("ns-a", "c", []string{"pod"}, result("subnet-s", nil))}
	ips := []IPRef{{Namespace: "ns-b", Name: "pod", Subnet: "s"}}

	ApplyHeld(subnets, testDriver, claims, ips, ownersAsPods)

	assert.Equal(t, int64(0), subnets[0].V4.Held)
}

func TestHeldTwoNICsOnOneSubnet(t *testing.T) {
	subnets := []Subnet{v4Subnet("s")}
	two := claim("ns", "c", []string{"pod"}, result("subnet-s", nil), result("subnet-s", nil))
	ips := []IPRef{{Namespace: "ns", Name: "pod", Subnet: "s"}} // only the first NIC has its address

	ApplyHeld(subnets, testDriver, []*resourceapi.ResourceClaim{two}, ips, ownersAsPods)
	assert.Equal(t, int64(1), subnets[0].V4.Held, "one of two NICs has an address")

	ips = append(ips, IPRef{Namespace: "ns", Name: "pod", Subnet: "s"})
	ApplyHeld(subnets, testDriver, []*resourceapi.ResourceClaim{two}, ips, ownersAsPods)
	assert.Equal(t, int64(2), subnets[0].V4.Held, "both NICs have their address, and a rerun starts from zero")
}

func TestHeldSharedClaimCountsOnce(t *testing.T) {
	subnets := []Subnet{v4Subnet("s")}
	shared := claim("ns", "c", []string{"pod-a", "pod-b"}, result("subnet-s", nil))
	ips := []IPRef{{Namespace: "ns", Name: "pod-a", Subnet: "s"}, {Namespace: "ns", Name: "pod-b", Subnet: "s"}}

	ApplyHeld(subnets, testDriver, []*resourceapi.ResourceClaim{shared}, ips, ownersAsPods)

	assert.Equal(t, int64(1), subnets[0].V4.Held, "the scheduler consumed one address for the claim")
}

func TestHeldDualStack(t *testing.T) {
	subnets := []Subnet{{Name: "d", V4: Family{Enabled: true, Free: 5}, V6: Family{Enabled: true, Free: 5}}}
	claims := []*resourceapi.ResourceClaim{claim("ns", "c", []string{"pod"}, result("subnet-d", map[resourceapi.QualifiedName]string{
		CapacityIPv4: "1", CapacityIPv6: "1",
	}))}
	ips := []IPRef{{Namespace: "ns", Name: "pod", Subnet: "d"}}

	ApplyHeld(subnets, testDriver, claims, ips, ownersAsPods)

	assert.Equal(t, int64(1), subnets[0].V4.Held)
	assert.Equal(t, int64(1), subnets[0].V6.Held)
}

func TestHeldDefaultsToOneAddressPerEnabledFamilyWithoutConsumedCapacity(t *testing.T) {
	subnets := []Subnet{v4Subnet("s")}
	claims := []*resourceapi.ResourceClaim{claim("ns", "c", []string{"pod"}, result("subnet-s", nil))}
	ips := []IPRef{{Namespace: "ns", Name: "pod", Subnet: "s"}}

	ApplyHeld(subnets, testDriver, claims, ips, ownersAsPods)

	assert.Equal(t, int64(1), subnets[0].V4.Held)
	assert.Equal(t, int64(0), subnets[0].V6.Held, "the Subnet has no IPv6")
}

func TestParseIP(t *testing.T) {
	ref, ok := ParseIP(&unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"podName": "vm-1", "namespace": "ns", "subnet": "s"},
	}})
	require.True(t, ok)
	assert.Equal(t, IPRef{Namespace: "ns", Name: "vm-1", Subnet: "s"}, ref)

	_, ok = ParseIP(&unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"subnet": "s"}}})
	assert.False(t, ok, "an address without an owner cannot be tied to a claim")
}
