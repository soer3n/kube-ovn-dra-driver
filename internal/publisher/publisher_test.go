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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
)

func subnetObject(name string, spec, status map[string]any) *unstructured.Unstructured {
	obj := map[string]any{
		"apiVersion": "kubeovn.io/v1",
		"kind":       "Subnet",
		"metadata":   map[string]any{"name": name},
		fieldSpec:    spec,
	}
	if status != nil {
		obj["status"] = status
	}
	return &unstructured.Unstructured{Object: obj}
}

func noVlans(name string) (VlanInfo, error) { return VlanInfo{}, fmt.Errorf("vlan %s not found", name) }

func TestParseSubnetOverlay(t *testing.T) {
	obj := subnetObject(testOverlaySubnet, map[string]any{
		fieldCIDRBlock: "10.200.0.0/24", fieldProvider: testOverlayProvider, fieldVPC: testVPC,
	}, map[string]any{fieldV4Available: int64(250)})

	s, err := ParseSubnet(obj, noVlans)
	require.NoError(t, err)
	assert.Equal(t, typeOVN, s.Type())
	assert.Equal(t, testVPC, s.VPC)
	assert.Equal(t, testOverlayProvider, s.Provider)
	assert.Empty(t, s.ProviderNetwork)
	assert.True(t, s.V4.Enabled)
	assert.False(t, s.V6.Enabled)
	assert.Equal(t, int64(250), s.V4.Free)
}

func TestParseSubnetVlanResolvesProviderNetwork(t *testing.T) {
	obj := subnetObject(testVlanSubnet, map[string]any{
		fieldCIDRBlock: testVlanCIDR, fieldProvider: "vlan100-subnet.default.external", fieldVlan: testVlanName,
	}, nil)

	s, err := ParseSubnet(obj, func(name string) (VlanInfo, error) {
		require.Equal(t, testVlanName, name)
		return VlanInfo{ID: 100, Provider: testProviderNetwork}, nil
	})
	require.NoError(t, err)
	assert.Equal(t, typeVlan, s.Type())
	assert.Equal(t, int64(100), s.VlanID)
	assert.Equal(t, testProviderNetwork, s.ProviderNetwork)
}

func TestParseSubnetMissingVlanFails(t *testing.T) {
	obj := subnetObject(testVlanSubnet, map[string]any{fieldCIDRBlock: testVlanCIDR, fieldVlan: testGonePod}, nil)
	_, err := ParseSubnet(obj, noVlans)
	assert.ErrorContains(t, err, "vlan \"gone\"")
}

func TestParseSubnetVlanWithoutIDIsOverlayLike(t *testing.T) {
	obj := subnetObject(testSubnetS, map[string]any{fieldCIDRBlock: testIPv4CIDR, fieldVlan: testUntaggedVlan}, nil)
	s, err := ParseSubnet(obj, func(string) (VlanInfo, error) { return VlanInfo{ID: 0, Provider: testPN}, nil })
	require.NoError(t, err)
	assert.Equal(t, typeOVN, s.Type())
	assert.Empty(t, s.ProviderNetwork, "a Vlan without id does not tie the Subnet to a ProviderNetwork")
}

func TestParseVlan(t *testing.T) {
	vlan := func(id any) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{fieldSpec: map[string]any{fieldID: id, fieldProvider: testProviderNetwork}}}
	}
	assert.Equal(t, VlanInfo{ID: 100, Provider: testProviderNetwork}, ParseVlan(vlan(int64(100))))
	assert.Equal(t, VlanInfo{ID: 200, Provider: testProviderNetwork}, ParseVlan(vlan(float64(200))))
	assert.Equal(t, VlanInfo{}, ParseVlan(&unstructured.Unstructured{Object: map[string]any{}}))
}

func TestParseSubnetDualStack(t *testing.T) {
	obj := subnetObject(testDualSubnet, map[string]any{fieldCIDRBlock: "10.0.0.0/24,fd00::/112"},
		map[string]any{fieldV4Available: int64(10), fieldV6Available: float64(20)})
	s, err := ParseSubnet(obj, noVlans)
	require.NoError(t, err)
	assert.True(t, s.V4.Enabled)
	assert.True(t, s.V6.Enabled)
	assert.Equal(t, int64(10), s.V4.Free)
	assert.Equal(t, int64(20), s.V6.Free)
}

func TestParseSubnetSaturatesHugeCounts(t *testing.T) {
	// An IPv6 /64 has 2^64 addresses; JSON decodes it to a float64.
	obj := subnetObject("v6", map[string]any{fieldCIDRBlock: "fd00::/64"},
		map[string]any{fieldV6Available: math.Pow(2, 64)})
	s, err := ParseSubnet(obj, noVlans)
	require.NoError(t, err)
	assert.Equal(t, maxCount, s.V6.Free)
	assert.Equal(t, maxCount, s.V6.Capacity())
}

func TestStatusCountForms(t *testing.T) {
	status := func(v any) *unstructured.Unstructured {
		return subnetObject(testSubnetS, map[string]any{}, map[string]any{testSubnetX: v})
	}
	assert.Equal(t, int64(7), statusCount(status("7"), testSubnetX))
	assert.Equal(t, int64(0), statusCount(status("junk"), testSubnetX))
	assert.Equal(t, int64(0), statusCount(status(int64(-3)), testSubnetX))
	assert.Equal(t, int64(0), statusCount(status(math.NaN()), testSubnetX))
	assert.Equal(t, int64(0), statusCount(subnetObject(testSubnetS, map[string]any{}, nil), testSubnetX))
}

func TestDeviceAttributesAndNoCapacityByDefault(t *testing.T) {
	d := Device(Subnet{
		Name: testVlanSubnet, Provider: testProviderP, VPC: testVPCV, VlanID: 100, ProviderNetwork: testProviderNetwork,
		V4: Family{Enabled: true, Free: 5},
	}, false)

	assert.Equal(t, "subnet-vlan100-subnet", d.Name)
	assert.Nil(t, d.Capacity, "capacity is only published when asked for")
	require.NotNil(t, d.AllowMultipleAllocations)
	assert.True(t, *d.AllowMultipleAllocations)
	assert.Equal(t, testVlanSubnet, *d.Attributes[AttrSubnetName].StringValue)
	assert.Equal(t, typeVlan, *d.Attributes[AttrSubnetType].StringValue)
	assert.Equal(t, int64(100), *d.Attributes[AttrVlanID].IntValue)
	assert.Equal(t, testProviderNetwork, *d.Attributes[AttrProviderNetwork].StringValue)
	assert.Equal(t, testProviderP, *d.Attributes[AttrProvider].StringValue)
	assert.Equal(t, testVPCV, *d.Attributes[AttrVPC].StringValue)
}

func TestDeviceOverlayHasNoVlanAttributes(t *testing.T) {
	d := Device(Subnet{Name: testOverlaySubnet}, false)
	assert.Equal(t, typeOVN, *d.Attributes[AttrSubnetType].StringValue)
	assert.NotContains(t, d.Attributes, AttrVlanID)
	assert.NotContains(t, d.Attributes, AttrProviderNetwork)
}

func TestDeviceCapacityPerFamily(t *testing.T) {
	d := Device(Subnet{
		Name: testDualSubnet,
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
	overlay := Pool(Subnet{Name: testOverlaySubnet}, false)
	assert.Nil(t, overlay.NodeSelector, "an overlay pool is available on all nodes")
	require.Len(t, overlay.Slices, 1)
	require.Len(t, overlay.Slices[0].Devices, 1)

	vlan := Pool(Subnet{Name: testVlanSubnet, VlanID: 100, ProviderNetwork: testProviderNetwork}, false)
	require.NotNil(t, vlan.NodeSelector)
	require.Len(t, vlan.NodeSelector.NodeSelectorTerms, 1, "a node selector must use exactly one term")
	exprs := vlan.NodeSelector.NodeSelectorTerms[0].MatchExpressions
	require.Len(t, exprs, 1)
	assert.Equal(t, "external.provider-network.kubernetes.io/ready", exprs[0].Key)
	assert.Equal(t, corev1.NodeSelectorOpIn, exprs[0].Operator)
	assert.Equal(t, []string{"true"}, exprs[0].Values)
}

func TestPoolsAreNamedAfterTheSubnet(t *testing.T) {
	pools := Pools([]Subnet{{Name: testSubnetA}, {Name: testSubnetB, VlanID: 7, ProviderNetwork: testPN}}, true)
	require.Len(t, pools, 2)
	assert.Contains(t, pools, testSubnetA)
	assert.Contains(t, pools, testSubnetB)
	assert.Equal(t, DeviceNamePrefix+testSubnetA, pools[testSubnetA].Slices[0].Devices[0].Name)
	assert.Empty(t, pools[testSubnetA].Slices[0].Devices[0].Capacity, "no families, no capacity entries")
}

func TestDeviceName(t *testing.T) {
	assert.Equal(t, DeviceNamePrefix+testSubnetX, DeviceName(testSubnetX), "a valid name is kept")
	assert.Equal(t, "subnet-ovn-subnet", DeviceName(testOverlaySubnet))

	long := strings.Repeat("a", 60)
	for _, subnet := range []string{testDottedSubnet, long, long + testSuffixX, strings.Repeat("a.", 100) + testSubnetB} {
		name := DeviceName(subnet)
		assert.Empty(t, validation.IsDNS1123Label(name), "%q -> %q must be a DNS label", subnet, name)
		assert.Equal(t, name, DeviceName(subnet), "stable")
		assert.True(t, strings.HasPrefix(name, DeviceNamePrefix))
	}
	assert.Equal(t, testDottedDevicePrefix+DeviceName(testDottedSubnet)[len(testDottedDevicePrefix):], DeviceName(testDottedSubnet))
	assert.NotEqual(t, DeviceName(testDottedSubnet), DeviceName("net-v1"), "a dot and a dash must not collide")
	assert.NotEqual(t, DeviceName(long+testSuffixX), DeviceName(long+".y"))

	// Nothing of the name is usable: only the hash is left.
	hashOnly := DeviceName(testNoLabelChars)
	assert.Empty(t, validation.IsDNS1123Label(hashOnly))
	assert.Len(t, hashOnly, len(DeviceNamePrefix)+deviceNameHashLength)
}

func TestPublishable(t *testing.T) {
	v4 := Family{Enabled: true}
	tests := []struct {
		name    string
		subnet  Subnet
		wantErr string
	}{
		{"dedicated ovn provider", Subnet{Name: testSubnetA, Provider: testProviderA, V4: v4}, ""},
		{"ipv6 only", Subnet{Name: testSubnetA, Provider: testProviderA, V6: v4}, ""},
		{errDefaultProvider, Subnet{Name: testSubnetA, V4: v4}, errDefaultProvider},
		{"explicit default provider", Subnet{Name: testSubnetA, Provider: defaultProvider, V4: v4}, errDefaultProvider},
		{"non-ovn provider", Subnet{Name: testSubnetA, Provider: "macvlan.ns", V4: v4}, "non-OVN provider"},
		{"no address family", Subnet{Name: testSubnetA, Provider: testProviderA}, "no IPv4 or IPv6 CIDR"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.subnet.Publishable()
			if tt.wantErr == "" {
				assert.NoError(t, err)
			} else {
				assert.ErrorContains(t, err, tt.wantErr)
			}
		})
	}
}

func TestStringAttribute(t *testing.T) {
	d := resourceapi.Device{Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
		"subnetName":           {StringValue: ptr.To(testPlainValue)},
		"custom.example/vpc":   {StringValue: ptr.To(testQualifiedValue)},
		"other.example/vlanId": {StringValue: ptr.To("foreign")},
	}}
	assert.Equal(t, testPlainValue, StringAttribute(d, testCustomDriver, AttrSubnetName))
	assert.Equal(t, testQualifiedValue, StringAttribute(d, testCustomDriver, AttrVPC))
	assert.Empty(t, StringAttribute(d, testCustomDriver, AttrVlanID), "another driver's attribute")
}

// ---- held addresses ----

// Values shared by the tests of this package.
const (
	testDriver             = "nic.kubeovn.io"
	testCustomDriver       = "custom.example"
	testNS                 = "ns"
	testPod                = "pod"
	testPodA               = "pod-a"
	testPodB               = "pod-b"
	testGonePod            = "gone"
	testVM                 = "vm-1"
	testDevice             = "subnet-s"
	testOverlaySubnet      = "ovn-subnet"
	testOverlayProvider    = "ovn-subnet.default.ovn"
	testVPC                = "ovn-cluster"
	testVlanSubnet         = "vlan100-subnet"
	testVlanName           = "vlan100"
	testVlanCIDR           = "172.23.0.0/24"
	testProviderNetwork    = "external"
	testPN                 = "pn"
	testProviderA          = "a.ns.ovn"
	testDottedSubnet       = "net.v1"
	testDottedDevicePrefix = "subnet-net-v1-"
	testPlainValue         = "plain"
	testQualifiedValue     = "qualified"
	defaultProvider        = "ovn"
	typeOVN                = "ovn"
	typeVlan               = "vlan"
	testDualSubnet         = "dual"
	testSuffixX            = ".x"
	testNoLabelChars       = "..."
	testSubnetS            = "s"
	testSubnetX            = "x"
	testSubnetA            = "a"
	testSubnetB            = "b"
	testProviderP          = "p"
	testVPCV               = "v"
	testUntaggedVlan       = "untagged"
	testIPv4CIDR           = "10.0.0.0/24"
	testClaimName          = "c"
	testUnallocatedClaim   = "u"
	testForeignClaim       = "f"
	testOtherDeviceClaim   = "o"
	testUnknownSubnetClaim = "k"
	testOtherDriver        = "other.io"
	testGPUDevice          = "gpu-0"
	testGoneDevice         = "subnet-gone"
	testVMLauncher         = "virt-launcher-vm-1-abcde"
	testOtherNS            = "ns-a"
	errDefaultProvider     = "default provider"

	// Fields of the kube-ovn Subnet, Vlan and IP fixtures.
	fieldSpec        = "spec"
	fieldSubnet      = "subnet"
	fieldCIDRBlock   = "cidrBlock"
	fieldProvider    = "provider"
	fieldVPC         = "vpc"
	fieldVlan        = "vlan"
	fieldID          = "id"
	fieldV4Available = "v4availableIPs"
	fieldV6Available = "v6availableIPs"
)

func claim(namespace, name string, pods []string, results ...resourceapi.DeviceRequestAllocationResult) *resourceapi.ResourceClaim {
	c := &resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
	c.Status.Allocation = &resourceapi.AllocationResult{Devices: resourceapi.DeviceAllocationResult{Results: results}}
	for _, p := range pods {
		c.Status.ReservedFor = append(c.Status.ReservedFor, resourceapi.ResourceClaimConsumerReference{Resource: "pods", Name: p})
	}
	return c
}

func result(device string, consumed map[resourceapi.QualifiedName]string) resourceapi.DeviceRequestAllocationResult {
	r := resourceapi.DeviceRequestAllocationResult{Driver: testDriver, Pool: strings.TrimPrefix(device, DeviceNamePrefix), Device: device, Request: "nic"}
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
	subnets := []Subnet{v4Subnet(testSubnetS)}
	claims := []*resourceapi.ResourceClaim{
		claim(testNS, "has-ip", []string{testPodA}, result(testDevice, map[resourceapi.QualifiedName]string{CapacityIPv4: "1"})),
		claim(testNS, "pending", []string{testPodB}, result(testDevice, map[resourceapi.QualifiedName]string{CapacityIPv4: "1"})),
	}
	ips := []IPRef{{Namespace: testNS, Name: testPodA, Subnet: testSubnetS}}

	ApplyHeld(subnets, testDriver, claims, ips, ownersAsPods)

	assert.Equal(t, int64(1), subnets[0].V4.Held, "the claim without an address yet is left to the scheduler")
	assert.Equal(t, int64(11), subnets[0].V4.Capacity())
}

func TestHeldIgnoresUnallocatedForeignAndUnknownClaims(t *testing.T) {
	subnets := []Subnet{v4Subnet(testSubnetS)}
	unallocated := &resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: testUnallocatedClaim}}
	foreign := claim(testNS, testForeignClaim, []string{testPod}, resourceapi.DeviceRequestAllocationResult{Driver: testOtherDriver, Device: testDevice})
	otherDevice := claim(testNS, testOtherDeviceClaim, []string{testPod}, result(testGPUDevice, nil))
	unknownSubnet := claim(testNS, testUnknownSubnetClaim, []string{testPod}, result(testGoneDevice, nil))
	ips := []IPRef{{Namespace: testNS, Name: testPod, Subnet: testSubnetS}, {Namespace: testNS, Name: testPod, Subnet: testGonePod}}

	ApplyHeld(subnets, testDriver, []*resourceapi.ResourceClaim{unallocated, foreign, otherDevice, unknownSubnet}, ips, ownersAsPods)

	assert.Equal(t, int64(0), subnets[0].V4.Held)
}

func TestHeldFollowsTheVMThatOwnsTheAddress(t *testing.T) {
	// kube-ovn names the address of a VM after the VM, the claim is reserved for
	// the virt-launcher pod.
	subnets := []Subnet{v4Subnet(testSubnetS)}
	claims := []*resourceapi.ResourceClaim{claim(testNS, testClaimName, []string{testVMLauncher}, result(testDevice, nil))}
	ips := []IPRef{{Namespace: testNS, Name: testVM, Subnet: testSubnetS}}
	owners := func(_, pod string) []string { return []string{pod, testVM} }

	ApplyHeld(subnets, testDriver, claims, ips, owners)

	assert.Equal(t, int64(1), subnets[0].V4.Held)
}

func TestHeldDoesNotCountAddressesOfOtherNamespaces(t *testing.T) {
	subnets := []Subnet{v4Subnet(testSubnetS)}
	claims := []*resourceapi.ResourceClaim{claim(testOtherNS, testClaimName, []string{testPod}, result(testDevice, nil))}
	ips := []IPRef{{Namespace: "ns-b", Name: testPod, Subnet: testSubnetS}}

	ApplyHeld(subnets, testDriver, claims, ips, ownersAsPods)

	assert.Equal(t, int64(0), subnets[0].V4.Held)
}

func TestHeldTwoNICsOnOneSubnet(t *testing.T) {
	subnets := []Subnet{v4Subnet(testSubnetS)}
	two := claim(testNS, testClaimName, []string{testPod}, result(testDevice, nil), result(testDevice, nil))
	ips := []IPRef{{Namespace: testNS, Name: testPod, Subnet: testSubnetS}} // only the first NIC has its address

	ApplyHeld(subnets, testDriver, []*resourceapi.ResourceClaim{two}, ips, ownersAsPods)
	assert.Equal(t, int64(1), subnets[0].V4.Held, "one of two NICs has an address")

	ips = append(ips, IPRef{Namespace: testNS, Name: testPod, Subnet: testSubnetS})
	ApplyHeld(subnets, testDriver, []*resourceapi.ResourceClaim{two}, ips, ownersAsPods)
	assert.Equal(t, int64(2), subnets[0].V4.Held, "both NICs have their address, and a rerun starts from zero")
}

func TestHeldSharedClaimCountsOnce(t *testing.T) {
	subnets := []Subnet{v4Subnet(testSubnetS)}
	shared := claim(testNS, testClaimName, []string{testPodA, testPodB}, result(testDevice, nil))
	ips := []IPRef{{Namespace: testNS, Name: testPodA, Subnet: testSubnetS}, {Namespace: testNS, Name: testPodB, Subnet: testSubnetS}}

	ApplyHeld(subnets, testDriver, []*resourceapi.ResourceClaim{shared}, ips, ownersAsPods)

	assert.Equal(t, int64(1), subnets[0].V4.Held, "the scheduler consumed one address for the claim")
}

func TestHeldDualStack(t *testing.T) {
	subnets := []Subnet{{Name: "d", V4: Family{Enabled: true, Free: 5}, V6: Family{Enabled: true, Free: 5}}}
	claims := []*resourceapi.ResourceClaim{claim(testNS, testClaimName, []string{testPod}, result("subnet-d", map[resourceapi.QualifiedName]string{
		CapacityIPv4: "1", CapacityIPv6: "1",
	}))}
	ips := []IPRef{{Namespace: testNS, Name: testPod, Subnet: "d"}}

	ApplyHeld(subnets, testDriver, claims, ips, ownersAsPods)

	assert.Equal(t, int64(1), subnets[0].V4.Held)
	assert.Equal(t, int64(1), subnets[0].V6.Held)
}

func TestHeldDefaultsToOneAddressPerEnabledFamilyWithoutConsumedCapacity(t *testing.T) {
	subnets := []Subnet{v4Subnet(testSubnetS)}
	claims := []*resourceapi.ResourceClaim{claim(testNS, testClaimName, []string{testPod}, result(testDevice, nil))}
	ips := []IPRef{{Namespace: testNS, Name: testPod, Subnet: testSubnetS}}

	ApplyHeld(subnets, testDriver, claims, ips, ownersAsPods)

	assert.Equal(t, int64(1), subnets[0].V4.Held)
	assert.Equal(t, int64(0), subnets[0].V6.Held, "the Subnet has no IPv6")
}

func TestParseIP(t *testing.T) {
	ref, ok := ParseIP(&unstructured.Unstructured{Object: map[string]any{
		fieldSpec: map[string]any{"podName": testVM, "namespace": testNS, fieldSubnet: testSubnetS},
	}})
	require.True(t, ok)
	assert.Equal(t, IPRef{Namespace: testNS, Name: testVM, Subnet: testSubnetS}, ref)

	_, ok = ParseIP(&unstructured.Unstructured{Object: map[string]any{fieldSpec: map[string]any{fieldSubnet: testSubnetS}}})
	assert.False(t, ok, "an address without an owner cannot be tied to a claim")
}
