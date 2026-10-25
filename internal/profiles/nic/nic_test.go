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

package nic

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/utils/ptr"

	configapi "github.com/soer3n/kube-ovn-dra-driver/api/kube-ovn.io/resource/nic/v1alpha1"
	"github.com/soer3n/kube-ovn-dra-driver/internal/publisher"
)

// Values shared by the tests of this package.
const (
	testNode                 = "node1"
	testSubnet               = "blue"
	testDevice               = "subnet-blue"
	testProvider             = "blue.default.ovn"
	testOtherSubnet          = "red"
	testOtherProvider        = "red.default.ovn"
	testDefaultSubnet        = "default"
	testVlan                 = "vlan100"
	testVlanProvider         = "vlan100.default.ovn"
	testProviderNetwork      = "external"
	testVPC                  = "ovn-cluster"
	testOther                = "other"
	testFixedMTUSubnet       = "fixed"
	testOverlaySubnet        = "overlay"
	testUnderlaySubnet       = "underlay"
	testLogicalGatewaySubnet = "lgw"
	testFloatMTUSubnet       = "float"
	testMissingVlanSubnet    = "missing-vlan"
	testNoPNSubnet           = "no-pn"
	testNoMTULabelSubnet     = "no-mtu-label"
	testMissingSubnet        = "missing"
	testMissingVlan          = "vlan-missing"
	testVlanNoPN             = "vlan101"
	testVlanOtherPN          = "vlan102"
	testUnderlayMTU          = "9000"
	testInvalidMTU           = "jumbo"
	testKubeOVNAPIVersion    = "kubeovn.io/v1"
	defaultProvider          = "ovn"
	typeOVN                  = "ovn"
	typeVlan                 = "vlan"
	labelTrue                = "true"
	testRequestA             = "a"
	testRequestB             = "b"

	kindSubnet = "Subnet"
	kindVlan   = "Vlan"
	kindNode   = "Node"

	// Fields of the kube-ovn and node fixtures.
	fieldAPIVersion     = "apiVersion"
	fieldKind           = "kind"
	fieldMetadata       = "metadata"
	fieldName           = "name"
	fieldLabels         = "labels"
	fieldSpec           = "spec"
	fieldStatus         = "status"
	fieldPhase          = "phase"
	fieldProvider       = "provider"
	fieldCIDRBlock      = "cidrBlock"
	fieldVlan           = "vlan"
	fieldVPC            = "vpc"
	fieldID             = "id"
	fieldMTU            = "mtu"
	fieldLogicalGateway = "logicalGateway"
	fieldEnableDHCP     = "enableDHCP"
	testOVNDHCPSubnet   = "ovn-dhcp"
	fieldV4Using        = "v4usingIPs"
)

func kubeOVNObject(kind, name string, spec map[string]interface{}) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		fieldAPIVersion: testKubeOVNAPIVersion,
		fieldKind:       kind,
		fieldMetadata:   map[string]interface{}{fieldName: name},
		fieldSpec:       spec,
	}}
}

// testNodeObject returns the profile's node, ready on the given ProviderNetworks.
func testNodeObject(readyOn ...string) *unstructured.Unstructured {
	labels := map[string]interface{}{}
	for _, pn := range readyOn {
		labels[publisher.ReadyLabel(pn)] = labelTrue
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		fieldAPIVersion: "v1",
		fieldKind:       kindNode,
		fieldMetadata:   map[string]interface{}{fieldName: testNode, fieldLabels: labels},
	}}
}

// newTestProfile returns a profile whose node is ready on the ProviderNetwork
// "external", unless objects contain another node object.
func newTestProfile(objects ...runtime.Object) Profile {
	hasNode := false
	for _, obj := range objects {
		if u, ok := obj.(*unstructured.Unstructured); ok && u.GetKind() == kindNode {
			hasNode = true
		}
	}
	if !hasNode {
		objects = append(objects, testNodeObject(testProviderNetwork))
	}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{subnetGVR: "SubnetList", vlanGVR: "VlanList", nodeGVR: "NodeList"}, objects...)
	return NewProfile(testNode, client)
}

// subnetSpec returns the spec of a publishable Subnet with the given provider.
func subnetSpec(provider string, extra ...string) map[string]interface{} {
	spec := map[string]interface{}{fieldProvider: provider, fieldCIDRBlock: "10.10.0.0/24"}
	for i := 0; i+1 < len(extra); i += 2 {
		spec[extra[i]] = extra[i+1]
	}
	return spec
}

func TestEnumerateDevices(t *testing.T) {
	p := newTestProfile(
		kubeOVNObject(kindSubnet, testSubnet, subnetSpec(testProvider, fieldVPC, testVPC)),
		kubeOVNObject(kindSubnet, testVlan, subnetSpec(testVlanProvider, fieldVlan, testVlan)),
		// Not publishable: the default provider, a non-OVN provider, no CIDR.
		kubeOVNObject(kindSubnet, testDefaultSubnet, subnetSpec(defaultProvider)),
		kubeOVNObject(kindSubnet, "macvlan", subnetSpec("macvlan.default")),
		kubeOVNObject(kindSubnet, "nocidr", map[string]interface{}{fieldProvider: "nocidr.default.ovn"}),
		kubeOVNObject(kindVlan, testVlan, map[string]interface{}{fieldID: int64(100), fieldProvider: testProviderNetwork}),
	)
	resources, err := p.EnumerateDevices()
	require.NoError(t, err)
	require.Contains(t, resources.Pools, testNode)
	devices := map[string]resourceapi.Device{}
	for _, d := range resources.Pools[testNode].Slices[0].Devices {
		devices[d.Name] = d
	}
	require.Len(t, devices, 2)

	blue := devices[testDevice]
	assert.Equal(t, ptr.To(testSubnet), blue.Attributes[publisher.AttrSubnetName].StringValue)
	assert.Equal(t, ptr.To(typeOVN), blue.Attributes[publisher.AttrSubnetType].StringValue)
	assert.Equal(t, ptr.To(testProvider), blue.Attributes[publisher.AttrProvider].StringValue)
	assert.Equal(t, ptr.To(testVPC), blue.Attributes[publisher.AttrVPC].StringValue)
	assert.NotContains(t, blue.Attributes, publisher.AttrVlanID)
	assert.Equal(t, ptr.To(true), blue.AllowMultipleAllocations)

	vlan := devices["subnet-vlan100"]
	assert.Equal(t, ptr.To(typeVlan), vlan.Attributes[publisher.AttrSubnetType].StringValue)
	assert.Equal(t, ptr.To(int64(100)), vlan.Attributes[publisher.AttrVlanID].IntValue)
	assert.Equal(t, ptr.To(testProviderNetwork), vlan.Attributes[publisher.AttrProviderNetwork].StringValue)
}

// A VLAN Subnet is only published on a node where its ProviderNetwork is ready.
func TestEnumerateDevicesSkipsVlanWithoutProviderNetwork(t *testing.T) {
	p := newTestProfile(
		testNodeObject(testOther),
		kubeOVNObject(kindSubnet, testSubnet, subnetSpec(testProvider)),
		kubeOVNObject(kindSubnet, testVlan, subnetSpec(testVlanProvider, fieldVlan, testVlan)),
		kubeOVNObject(kindVlan, testVlan, map[string]interface{}{fieldID: int64(100), fieldProvider: testProviderNetwork}),
	)
	resources, err := p.EnumerateDevices()
	require.NoError(t, err)
	devices := resources.Pools[testNode].Slices[0].Devices
	require.Len(t, devices, 1)
	assert.Equal(t, testDevice, devices[0].Name)
}

// A Subnet whose name is no valid device name gets a hashed device name; the
// CDI environment still names the Subnet.
func TestEnumerateDevicesLongSubnetName(t *testing.T) {
	name := "net.v1"
	p := newTestProfile(kubeOVNObject(kindSubnet, name, subnetSpec("net.default.ovn")))
	resources, err := p.EnumerateDevices()
	require.NoError(t, err)
	devices := resources.Pools[testNode].Slices[0].Devices
	require.Len(t, devices, 1)
	assert.Equal(t, publisher.DeviceName(name), devices[0].Name)
	edits, err := p.ApplyConfig(nil, []*resourceapi.DeviceRequestAllocationResult{{Request: testRequestA, Device: devices[0].Name}})
	require.NoError(t, err)
	assert.Contains(t, edits["a/"+devices[0].Name].Env, "KUBE_OVN_NIC_NET1_SUBNET="+name)
}

func TestEnumerateDevicesMissingVlan(t *testing.T) {
	p := newTestProfile(kubeOVNObject(kindSubnet, testVlan, map[string]interface{}{fieldVlan: testVlan}))
	_, err := p.EnumerateDevices()
	assert.Error(t, err)
}

func TestApplyConfig(t *testing.T) {
	p := newTestProfile()
	results := []*resourceapi.DeviceRequestAllocationResult{{Request: testRequestA, Device: testDevice}, {Request: testRequestB, Device: testDevice}}

	edits, err := p.ApplyConfig(nil, results)
	require.NoError(t, err)
	require.Len(t, edits, 2, "one entry per result, even on a shared device")
	assert.ElementsMatch(t, []string{"KUBE_OVN_NIC_NET1_SUBNET=blue", "KUBE_OVN_NIC_NET1_DEVICE=subnet-blue"},
		edits["a/subnet-blue"].Env)

	edits, err = p.ApplyConfig(&configapi.NicConfig{InterfaceName: "eth-9"}, results[1:])
	require.NoError(t, err)
	assert.Contains(t, edits["b/subnet-blue"].Env, "KUBE_OVN_NIC_ETH_9_SUBNET=blue")

	_, err = p.ApplyConfig(&runtime.Unknown{}, results)
	assert.Error(t, err)
}

func TestValidate(t *testing.T) {
	p := newTestProfile()
	assert.NoError(t, p.Validate(&configapi.NicConfig{InterfaceName: "net2"}))
	assert.Error(t, p.Validate(&configapi.NicConfig{InterfaceName: "much-too-long-name"}))
	assert.Error(t, p.Validate(&runtime.Unknown{}))
}

func TestSchemeBuilder(t *testing.T) {
	scheme := runtime.NewScheme()
	sb := newTestProfile().SchemeBuilder()
	require.NoError(t, sb.AddToScheme(scheme))
	assert.True(t, scheme.Recognizes(configapi.SchemeGroupVersion.WithKind(configapi.NicConfigKind)))
}

func TestSubnetNameFromDevice(t *testing.T) {
	p := newTestProfile()
	assert.Equal(t, testSubnet, p.subnetNameFromDevice(testDevice))
	assert.Equal(t, testOther, p.subnetNameFromDevice(testOther))
}

func TestWatchDevices(t *testing.T) {
	p := newTestProfile(kubeOVNObject(kindSubnet, testSubnet, map[string]interface{}{fieldProvider: testProvider}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changes := make(chan struct{}, 10)
	require.NoError(t, p.WatchDevices(ctx, func() { changes <- struct{}{} }))
	// The initial list reports the existing subnet.
	expectChange := func(what string) {
		t.Helper()
		select {
		case <-changes:
		case <-time.After(5 * time.Second):
			t.Fatalf("no change reported for %s", what)
		}
	}
	expectNoChange := func(what string) {
		t.Helper()
		select {
		case <-changes:
			t.Fatalf("change reported for %s", what)
		case <-time.After(200 * time.Millisecond):
		}
	}
	expectChange("the initial list")

	subnets := p.dynamicClient.Resource(subnetGVR)
	red := kubeOVNObject(kindSubnet, testOtherSubnet, map[string]interface{}{fieldProvider: testOtherProvider})
	_, err := subnets.Create(ctx, red, metav1.CreateOptions{})
	require.NoError(t, err)
	expectChange("a new subnet")

	blue, err := subnets.Get(ctx, testSubnet, metav1.GetOptions{})
	require.NoError(t, err)
	blue.Object[fieldStatus] = map[string]interface{}{fieldV4Using: int64(3)}
	blue, err = subnets.Update(ctx, blue, metav1.UpdateOptions{})
	require.NoError(t, err)
	expectNoChange("a status update")

	blue.Object["spec"] = map[string]interface{}{fieldProvider: "blue2.default.ovn"}
	_, err = subnets.Update(ctx, blue, metav1.UpdateOptions{})
	require.NoError(t, err)
	expectChange("a spec update")

	require.NoError(t, subnets.Delete(ctx, testOtherSubnet, metav1.DeleteOptions{}))
	expectChange("a deleted subnet")

	_, err = p.dynamicClient.Resource(vlanGVR).Create(ctx, kubeOVNObject(kindVlan, testVlan, map[string]interface{}{fieldID: int64(100)}), metav1.CreateOptions{})
	require.NoError(t, err)
	expectChange("a new vlan")

	nodes := p.dynamicClient.Resource(nodeGVR)
	node, err := nodes.Get(ctx, testNode, metav1.GetOptions{})
	require.NoError(t, err)
	node.SetLabels(map[string]string{publisher.ReadyLabel(testOther): labelTrue})
	node, err = nodes.Update(ctx, node, metav1.UpdateOptions{})
	require.NoError(t, err)
	expectChange("a provider network becoming ready on the node")
	node.Object[fieldStatus] = map[string]interface{}{fieldPhase: "Running"}
	_, err = nodes.Update(ctx, node, metav1.UpdateOptions{})
	require.NoError(t, err)
	expectNoChange("a node status update")
}

func TestSubnetMTU(t *testing.T) {
	node := testNodeObject(testProviderNetwork)
	node.SetLabels(map[string]string{fmt.Sprintf(providerNetworkMTULabel, testProviderNetwork): testUnderlayMTU})
	p := newTestProfile(node,
		kubeOVNObject(kindVlan, testVlan, map[string]interface{}{fieldID: int64(100), fieldProvider: testProviderNetwork}),
		kubeOVNObject(kindVlan, testVlanNoPN, map[string]interface{}{fieldID: int64(101)}),
		kubeOVNObject(kindVlan, testVlanOtherPN, map[string]interface{}{fieldID: int64(102), fieldProvider: testOther}),
		kubeOVNObject(kindSubnet, testFixedMTUSubnet, map[string]interface{}{fieldProvider: testProvider, fieldMTU: int64(1300)}),
		kubeOVNObject(kindSubnet, testFloatMTUSubnet, map[string]interface{}{fieldProvider: testProvider, fieldMTU: float64(1200)}),
		kubeOVNObject(kindSubnet, testOverlaySubnet, subnetSpec(testProvider)),
		kubeOVNObject(kindSubnet, testUnderlaySubnet, subnetSpec(testVlanProvider, fieldVlan, testVlan)),
		kubeOVNObject(kindSubnet, testLogicalGatewaySubnet, map[string]interface{}{fieldProvider: testVlanProvider, fieldVlan: testVlan, fieldLogicalGateway: true}),
		kubeOVNObject(kindSubnet, testMissingVlanSubnet, subnetSpec(testVlanProvider, fieldVlan, testMissingVlan)),
		kubeOVNObject(kindSubnet, testNoPNSubnet, subnetSpec(testVlanProvider, fieldVlan, testVlanNoPN)),
		kubeOVNObject(kindSubnet, testNoMTULabelSubnet, subnetSpec(testVlanProvider, fieldVlan, testVlanOtherPN)),
	)
	tests := []struct {
		name    string
		subnet  string
		want    int
		wantErr bool
	}{
		{"mtu of the subnet", testFixedMTUSubnet, 1300, false},
		{"mtu of the subnet decoded as float", testFloatMTUSubnet, 1200, false},
		{"overlay takes the pod's eth0", testOverlaySubnet, 0, false},
		{"underlay takes the provider network's mtu", testUnderlaySubnet, 9000, false},
		{"logical gateway is routed like overlay", testLogicalGatewaySubnet, 0, false},
		{"vlan without provider network", testNoPNSubnet, 0, false},
		{"provider network without mtu label", testNoMTULabelSubnet, 0, false},
		{"missing vlan", testMissingVlanSubnet, 0, true},
		{"missing subnet", testMissingSubnet, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := p.SubnetMTU(context.Background(), tt.subnet)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}

	t.Run("invalid mtu label", func(t *testing.T) {
		node := testNodeObject(testProviderNetwork)
		node.SetLabels(map[string]string{fmt.Sprintf(providerNetworkMTULabel, testProviderNetwork): testInvalidMTU})
		p := newTestProfile(node,
			kubeOVNObject(kindVlan, testVlan, map[string]interface{}{fieldID: int64(100), fieldProvider: testProviderNetwork}),
			kubeOVNObject(kindSubnet, testUnderlaySubnet, subnetSpec(testVlanProvider, fieldVlan, testVlan)),
		)
		_, err := p.SubnetMTU(context.Background(), testUnderlaySubnet)
		assert.ErrorContains(t, err, testInvalidMTU)
	})
}

func TestSubnetOVNDHCP(t *testing.T) {
	p := newTestProfile(testNodeObject(testProviderNetwork),
		kubeOVNObject(kindSubnet, testOVNDHCPSubnet, map[string]interface{}{fieldProvider: testProvider, fieldEnableDHCP: true}),
		kubeOVNObject(kindSubnet, testOverlaySubnet, subnetSpec(testProvider)),
	)
	enabled, err := p.SubnetOVNDHCP(context.Background(), testOVNDHCPSubnet)
	require.NoError(t, err)
	assert.True(t, enabled, "spec.enableDHCP")
	enabled, err = p.SubnetOVNDHCP(context.Background(), testOverlaySubnet)
	require.NoError(t, err)
	assert.False(t, enabled, "no spec.enableDHCP")
	_, err = p.SubnetOVNDHCP(context.Background(), testMissingSubnet)
	assert.Error(t, err)
}
