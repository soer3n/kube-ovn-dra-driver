/*
 * Copyright 2025 The Kubernetes Authors.
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

package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/utils/ptr"

	nicconfig "github.com/soer3n/kube-ovn-dra-driver/api/kube-ovn.io/resource/nic/v1alpha1"
	nicprofile "github.com/soer3n/kube-ovn-dra-driver/internal/profiles/nic"
	"github.com/soer3n/kube-ovn-dra-driver/pkg/nicprepare"
	"github.com/soer3n/kube-ovn-dra-driver/pkg/plumbing"
)

const (
	testDriver   = "nic.kubeovn.io"
	testNode     = "node1"
	testNS       = "default"
	testPod      = "pod1"
	testPodUID   = "pod-uid-1"
	testProvider = "blue.default.ovn"
	testDevice   = "subnet-blue"

	testIface1        = "net1"
	testIface2        = "net2"
	testUnderlayIface = "underlay"
	testClassIface    = "class0"
	testClaimIface    = "claim0"
	testKeyNet1       = testProvider + "." + testIface1
	testKeyNet2       = testProvider + "." + testIface2
	testPort          = testPod + "." + testNS + "." + testProvider
	testPortNet1      = testPort + "." + testIface1
	testPortNet2      = testPort + "." + testIface2
	testClaimUID2     = "claim-uid-2"
	testOtherDevice   = "subnet-red"
	testOtherDriver   = "gpu.example.com"
	testOtherRequest  = "other"
	testParentRequest = "network"
	testSubRequestRef = "network/vlan"
	testSubRef        = testRequestA + "/sub"
	testCIDR          = "10.10.0.0/24"
	testGateway       = "10.10.0.1"
	testIPWithMask    = "10.10.0.5/24"
	testMAC2          = "00:00:00:aa:bb:02"
	testLauncherPod   = "virt-launcher-vm1-abcde"
	testVlanProvider  = "vlan100.default.ovn"
	testVMIP          = "172.17.0.10/24"
	testVMIPv6        = "fd00::10/120"
	testVMGateway     = "172.17.0.1"
	testNetnsPath     = "/var/run/netns/x"
	testSandboxID     = "sandbox-1"
	kubeovnGroup      = "kubeovn.io"
	apiVersionV1      = "v1"
	kubeOVNAPIVersion = kubeovnGroup + "/" + apiVersionV1
	testMAC           = "00:00:00:aa:bb:cc"
	testVMPort        = "vm1.default." + testVlanProvider
	testNRIClaimUID   = "claim"
	testIPCPath       = "/proc/1/ns/ipc"
	nsTypeIPC         = "ipc"
	testUnknownKind   = "Unknown"
	testRequestA      = "a"
	testRequestB      = "b"
	testOtherSubRef   = testRequestA + "/other"
	testGPURequest    = "gpu"
	testGPUDevice     = "gpu0"
	testIP1           = "10.10.0.5"
	testIP2           = "10.10.0.6"
	testMAC1          = "00:00:00:aa:bb:01"
	testVMName        = "vm1"
	testVMIface       = "kubeovn0"
	testIfaceX        = "x"
	testTooLongIface  = "much-too-long-name"

	// kube-ovn's per-provider annotations, after the provider key.
	annAllocated      = ".kubernetes.io/allocated"
	annIPAddress      = ".kubernetes.io/ip_address"
	annMACAddress     = ".kubernetes.io/mac_address"
	annCIDR           = ".kubernetes.io/cidr"
	annGateway        = ".kubernetes.io/gateway"
	annVirtualMachine = ".kubernetes.io/virtualmachine"
	annTrue           = "true"

	testOverlaySubnet      = "ovn-subnet"
	testOverlayProvider    = "ovn-subnet.default.ovn"
	testOverlayDevice      = "subnet-ovn-subnet"
	testVlanSubnet         = "vlan100-subnet"
	testVlanSubnetProvider = "vlan100-subnet.default.ovn"
	testVlan               = "vlan100"
	testPlainPod           = "plain"
	testGonePod            = "gone"
	testPodA               = "pod-a"
	testVM                 = "vm-1"
	testVMLauncher         = "virt-launcher-vm-1-abcde"
	podsResource           = "pods"
	verbGet                = "get"
	pluginBinary           = "kube-ovn-dra-kubeletplugin"
	testNodeNameFlag       = "--node-name=n"

	kindSubnet = "Subnet"
	kindVlan   = "Vlan"
	kindIP     = "IP"

	// Fields of the kube-ovn and node fixtures.
	fieldAPIVersion  = "apiVersion"
	fieldKind        = "kind"
	fieldMetadata    = "metadata"
	fieldName        = "name"
	fieldSpec        = "spec"
	fieldStatus      = "status"
	fieldProvider    = "provider"
	fieldCIDRBlock   = "cidrBlock"
	fieldVlan        = "vlan"
	fieldID          = "id"
	fieldV4Available = "v4availableIPs"
	fieldPodName     = "podName"
	fieldNamespace   = "namespace"
	fieldSubnet      = "subnet"
)

// newTestDeviceState builds a DeviceState for the nic profile on one node with
// the kube-ovn subnet "blue", using temporary CDI and checkpoint directories.
func newTestDeviceState(t *testing.T, pods ...runtime.Object) *DeviceState {
	t.Helper()
	state, err := NewDeviceState(newTestConfig(t, pods...))
	require.NoError(t, err)
	return state
}

var testSubnetGVR = schema.GroupVersionResource{Group: kubeovnGroup, Version: apiVersionV1, Resource: "subnets"}

func testSubnet(name, provider string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		fieldAPIVersion: kubeOVNAPIVersion,
		fieldKind:       "Subnet",
		fieldMetadata:   map[string]interface{}{fieldName: name},
		fieldSpec:       map[string]interface{}{fieldProvider: provider, fieldCIDRBlock: testCIDR},
	}}
}

func newTestConfig(t *testing.T, pods ...runtime.Object) *Config {
	t.Helper()
	config, _ := newTestConfigWithClient(t, pods...)
	return config
}

// newTestConfigWithClient is newTestConfig that also returns the fake kube-ovn
// client holding the subnet "blue".
func newTestConfigWithClient(t *testing.T, pods ...runtime.Object) (*Config, dynamic.Interface) {
	t.Helper()
	dynamicClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		testSubnetGVR: "SubnetList",
		{Group: kubeovnGroup, Version: apiVersionV1, Resource: "vlans"}: "VlanList",
		{Version: apiVersionV1, Resource: "nodes"}:                      "NodeList",
	}, testSubnet("blue", testProvider), &unstructured.Unstructured{Object: map[string]interface{}{
		fieldAPIVersion: apiVersionV1, fieldKind: "Node", fieldMetadata: map[string]interface{}{fieldName: testNode},
	}})
	config := &Config{
		flags: &Flags{
			nodeName:                    testNode,
			cdiRoot:                     t.TempDir(),
			kubeletPluginsDirectoryPath: t.TempDir(),
			profile:                     nicprofile.ProfileName,
			driverName:                  testDriver,
		},
		coreclient: fake.NewSimpleClientset(pods...),
		profile:    nicprofile.NewProfile(testNode, dynamicClient),
		nicStore:   plumbing.NewNICStore(),
	}
	return config, dynamicClient
}

// allocatedPod returns the pod with kube-ovn's allocation annotations for the
// given provider keys.
func allocatedPod(keys ...string) *corev1.Pod {
	annotations := map[string]string{}
	for i, key := range keys {
		annotations[key+annAllocated] = annTrue
		annotations[key+annIPAddress] = []string{testIP1, testIP2}[i]
		annotations[key+annMACAddress] = []string{testMAC1, testMAC2}[i]
		annotations[key+annCIDR] = testCIDR
		annotations[key+annGateway] = testGateway
	}
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: testPod, Namespace: testNS, UID: testPodUID, Annotations: annotations}, Spec: corev1.PodSpec{NodeName: testNode}}
}

func nicConfig(iface string, requests ...string) resourceapi.DeviceAllocationConfiguration {
	cfg := nicconfig.DefaultNicConfig()
	cfg.InterfaceName = iface
	raw, err := json.Marshal(cfg)
	if err != nil {
		panic(err)
	}
	return resourceapi.DeviceAllocationConfiguration{
		Source:   resourceapi.AllocationConfigSourceClaim,
		Requests: requests,
		DeviceConfiguration: resourceapi.DeviceConfiguration{Opaque: &resourceapi.OpaqueDeviceConfiguration{
			Driver:     testDriver,
			Parameters: runtime.RawExtension{Raw: raw},
		}},
	}
}

// testClaim returns a claim reserved for the test pod with one result per
// request, all on the blue subnet device.
func testClaim(configs []resourceapi.DeviceAllocationConfiguration, requests ...string) *resourceapi.ResourceClaim {
	var results []resourceapi.DeviceRequestAllocationResult
	for _, request := range requests {
		results = append(results, resourceapi.DeviceRequestAllocationResult{Request: request, Driver: testDriver, Pool: testNode, Device: testDevice})
	}
	results = append(results, resourceapi.DeviceRequestAllocationResult{Request: testGPURequest, Driver: testOtherDriver, Pool: testNode, Device: testGPUDevice})
	return &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim", Namespace: testNS, UID: "claim-uid-1"},
		Status: resourceapi.ResourceClaimStatus{
			Allocation:  &resourceapi.AllocationResult{Devices: resourceapi.DeviceAllocationResult{Results: results, Config: configs}},
			ReservedFor: []resourceapi.ResourceClaimConsumerReference{{Resource: "pods", Name: testPod, UID: testPodUID}},
		},
	}
}

func TestPrepareUnprepare(t *testing.T) {
	// Two NICs of the pod on one provider are keyed by interface name.
	state := newTestDeviceState(t, allocatedPod(testKeyNet1, testKeyNet2))
	claimA := testClaim(nil, testRequestA)
	claimB := testClaim([]resourceapi.DeviceAllocationConfiguration{nicConfig(testIface2, testRequestB)}, testRequestB)
	claimB.UID = testClaimUID2

	for _, claim := range []*resourceapi.ResourceClaim{claimA, claimB} {
		devices, err := state.Prepare(context.Background(), claim)
		require.NoError(t, err)
		require.Len(t, devices, 1, "the result of the other driver must be skipped")
		assert.Equal(t, testDevice, devices[0].DeviceName)
		assert.Len(t, devices[0].CdiDeviceIds, 2, "claim device and common device")
	}

	specs := state.nicStore.Specs(testPodUID)
	require.Len(t, specs, 2)
	byIface := map[string]plumbing.Spec{}
	for _, spec := range specs {
		byIface[spec.IfaceName] = spec
	}
	assert.Equal(t, testPortNet1, byIface[testIface1].IfaceID)
	assert.Equal(t, testIPWithMask, byIface[testIface1].IP)
	assert.Equal(t, testPortNet2, byIface[testIface2].IfaceID)
	assert.Equal(t, testMAC2, byIface[testIface2].MAC)

	// A repeated prepare is served from the checkpoint.
	again, err := state.Prepare(context.Background(), claimA)
	require.NoError(t, err)
	assert.Len(t, again, 1)
	assert.Len(t, state.nicStore.Specs(testPodUID), 2, "a checkpointed claim must not be added twice")

	require.NoError(t, state.Unprepare(string(claimA.UID)))
	specs = state.nicStore.Specs(testPodUID)
	require.Len(t, specs, 1, "unprepare drops the claim's NICs")
	assert.Equal(t, testIface2, specs[0].IfaceName)
	for _, claim := range []*resourceapi.ResourceClaim{claimA, claimB} {
		require.NoError(t, state.Unprepare(string(claim.UID)))
		require.NoError(t, state.Unprepare(string(claim.UID)), "unprepare is idempotent")
	}
	assert.Empty(t, state.nicStore.Specs(testPodUID))
	checkpoint := newCheckpoint()
	require.NoError(t, state.checkpointManager.GetCheckpoint(DriverPluginCheckpointFile, checkpoint))
	assert.Empty(t, checkpoint.V1.PreparedClaims)
}

func TestPrepareTwoRequestsOnOneDevice(t *testing.T) {
	state := newTestDeviceState(t, allocatedPod(testKeyNet1, testKeyNet2))
	claim := testClaim([]resourceapi.DeviceAllocationConfiguration{nicConfig(testIface2, testRequestB)}, testRequestA, testRequestB)

	devices, err := state.Prepare(context.Background(), claim)
	require.NoError(t, err, "two requests on one shared subnet device must get distinct CDI devices")
	require.Len(t, devices, 2)
	assert.NotEqual(t, devices[0].CdiDeviceIds[1], devices[1].CdiDeviceIds[1])

	specs := state.nicStore.Specs(testPodUID)
	require.Len(t, specs, 2)
	ifaces := []string{specs[0].IfaceName, specs[1].IfaceName}
	assert.ElementsMatch(t, []string{testIface1, testIface2}, ifaces)
	require.NoError(t, state.Unprepare(string(claim.UID)))
}

func TestPrepareRejectsDuplicateInterfaceNames(t *testing.T) {
	state := newTestDeviceState(t, allocatedPod(testKeyNet1))

	// Both requests fall back to the default name net1.
	_, err := state.Prepare(context.Background(), testClaim(nil, testRequestA, testRequestB))
	assert.ErrorContains(t, err, `more than one NIC named "net1"`)

	claimA := testClaim(nil, testRequestA)
	_, err = state.Prepare(context.Background(), claimA)
	require.NoError(t, err)
	claimB := testClaim(nil, testRequestB)
	claimB.UID = testClaimUID2
	_, err = state.Prepare(context.Background(), claimB)
	assert.ErrorContains(t, err, "already used by another DRA NIC of pod default/pod1")
	assert.Len(t, state.nicStore.Specs(testPodUID), 1, "the rejected claim must not add NICs")

	// A repeated prepare of the same claim is no conflict.
	_, err = state.Prepare(context.Background(), claimA)
	require.NoError(t, err)
}

func TestRestartRestoresNICs(t *testing.T) {
	config := newTestConfig(t, allocatedPod(testProvider))
	state, err := NewDeviceState(config)
	require.NoError(t, err)
	claim := testClaim(nil, testRequestA)
	_, err = state.Prepare(context.Background(), claim)
	require.NoError(t, err)

	// A restarted plugin starts with an empty NIC store and the same checkpoint.
	restarted := *config
	restarted.nicStore = plumbing.NewNICStore()
	state, err = NewDeviceState(&restarted)
	require.NoError(t, err)
	specs := restarted.nicStore.Specs(testPodUID)
	require.Len(t, specs, 1)
	assert.Equal(t, string(claim.UID), specs[0].ClaimUID)
	assert.Equal(t, testPort, specs[0].IfaceID)
	assert.Equal(t, testIPWithMask, specs[0].IP)

	require.NoError(t, state.Unprepare(string(claim.UID)))
	assert.Empty(t, restarted.nicStore.Specs(testPodUID))
}

func TestPrepareErrors(t *testing.T) {
	state := newTestDeviceState(t, allocatedPod(testProvider))

	unallocated := testClaim(nil, testRequestA)
	unallocated.Status.Allocation = nil
	_, err := state.Prepare(context.Background(), unallocated)
	assert.ErrorContains(t, err, "not yet allocated")

	unknownDevice := testClaim(nil, testRequestA)
	unknownDevice.Status.Allocation.Devices.Results[0].Device = testOtherDevice
	_, err = state.Prepare(context.Background(), unknownDevice)
	assert.ErrorContains(t, err, "not allocatable")

	badConfig := testClaim([]resourceapi.DeviceAllocationConfiguration{nicConfig(testTooLongIface)}, testRequestA)
	unknownKind, err := json.Marshal(map[string]string{fieldKind: testUnknownKind})
	require.NoError(t, err)
	badConfig.Status.Allocation.Devices.Config[0].Opaque.Parameters.Raw = unknownKind
	_, err = state.Prepare(context.Background(), badConfig)
	assert.ErrorContains(t, err, "opaque device configs")
	assert.NotContains(t, err.Error(), "json parse error", "the config is valid JSON of an unknown kind")

	notReserved := testClaim(nil, testRequestA)
	notReserved.Status.ReservedFor = nil
	_, err = state.Prepare(context.Background(), notReserved)
	assert.ErrorContains(t, err, "NIC IPAM")
	assert.Empty(t, state.nicStore.Specs(testPodUID))
}

func TestNicSpecFromConfig(t *testing.T) {
	cfg := &nicprepare.NicDeviceConfig{
		PodUID:            testPodUID,
		PodName:           testLauncherPod,
		PodNamespace:      testNS,
		IfaceName:         testIface1,
		Provider:          testVlanProvider,
		LogicalSwitchPort: testVMPort,
		IP:                testVMIP,
		ExtraIPs:          []string{testVMIPv6},
		MAC:               testMAC,
		Gateway:           testVMGateway,
		OVNDHCP:           true,
	}
	assert.Equal(t, plumbing.Spec{
		PodUID:       testPodUID,
		PodName:      testLauncherPod,
		PodNamespace: testNS,
		IfaceName:    testIface1,
		IP:           testVMIP,
		ExtraIPs:     []string{testVMIPv6},
		MAC:          testMAC,
		Gateway:      testVMGateway,
		Provider:     testVlanProvider,
		IfaceID:      testVMPort,
		OVNDHCP:      true,
	}, nicSpecFromConfig(cfg))
}

func TestGetOpaqueDeviceConfigs(t *testing.T) {
	state := newTestDeviceState(t)
	classConfig := nicConfig(testClassIface)
	classConfig.Source = resourceapi.AllocationConfigSourceClass
	otherDriver := nicConfig(testOtherRequest)
	otherDriver.Opaque.Driver = testOtherDriver

	configs, err := GetOpaqueDeviceConfigs(state.configDecoder, testDriver,
		[]resourceapi.DeviceAllocationConfiguration{nicConfig(testClaimIface, testRequestA), otherDriver, classConfig})
	require.NoError(t, err)
	require.Len(t, configs, 2)
	assert.Equal(t, testClassIface, ifaceOf(configs[0].Config))
	assert.Equal(t, testClaimIface, ifaceOf(configs[1].Config), "claim configs take precedence over class configs")
	assert.Equal(t, []string{testRequestA}, configs[1].Requests)

	invalidSource := nicConfig(testIfaceX)
	invalidSource.Source = "bogus"
	_, err = GetOpaqueDeviceConfigs(state.configDecoder, testDriver, []resourceapi.DeviceAllocationConfiguration{invalidSource})
	assert.Error(t, err)

	notOpaque := nicConfig(testIfaceX)
	notOpaque.Opaque = nil
	_, err = GetOpaqueDeviceConfigs(state.configDecoder, testDriver, []resourceapi.DeviceAllocationConfiguration{notOpaque})
	assert.Error(t, err)
}

func ifaceOf(config runtime.Object) string {
	if c, ok := config.(*nicconfig.NicConfig); ok {
		return c.InterfaceName
	}
	return ""
}

func TestCheckAdminAccess(t *testing.T) {
	state := &DeviceState{}
	claim := testClaim(nil, testRequestA)
	assert.False(t, state.checkAdminAccess(claim))
	assert.False(t, state.checkAdminAccess(nil))
	claim.Status.Allocation.Devices.Results[0].AdminAccess = ptr.To(true)
	assert.True(t, state.checkAdminAccess(claim))
}

func TestDriverPrepareUnprepareResourceClaims(t *testing.T) {
	d := &driver{state: newTestDeviceState(t, allocatedPod(testProvider))}
	ok := testClaim(nil, testRequestA)
	failing := testClaim(nil, testRequestA)
	failing.UID = testClaimUID2
	failing.Status.Allocation = nil

	results, err := d.PrepareResourceClaims(context.Background(), []*resourceapi.ResourceClaim{ok, failing})
	require.NoError(t, err)
	require.NoError(t, results[ok.UID].Err)
	require.Len(t, results[ok.UID].Devices, 1)
	assert.Equal(t, []string{testRequestA}, results[ok.UID].Devices[0].Requests)
	assert.Equal(t, testDevice, results[ok.UID].Devices[0].DeviceName)
	assert.Error(t, results[failing.UID].Err)

	unprepared, err := d.UnprepareResourceClaims(context.Background(), []kubeletplugin.NamespacedObject{{UID: ok.UID}})
	require.NoError(t, err)
	assert.NoError(t, unprepared[ok.UID])
}

func TestRefreshDevices(t *testing.T) {
	config, client := newTestConfigWithClient(t)
	state, err := NewDeviceState(config)
	require.NoError(t, err)

	_, changed, err := state.RefreshDevices()
	require.NoError(t, err)
	assert.False(t, changed, "nothing changed since startup")

	_, err = client.Resource(testSubnetGVR).Create(context.Background(), testSubnet("red", "red.default.ovn"), metav1.CreateOptions{})
	require.NoError(t, err)

	resources, changed, err := state.RefreshDevices()
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Len(t, resources.Pools[testNode].Slices[0].Devices, 2)
	assert.Contains(t, state.allocatable, testOtherDevice, "a claim for the new subnet must be allocatable")
}

// A config naming a firstAvailable request applies to the subrequest the
// allocator picked, the way kube-ovn-controller reads the interface name.
func TestPrepareFirstAvailableSubrequest(t *testing.T) {
	tests := []struct {
		name      string
		requests  []string
		wantIface string
	}{
		{"config for the parent request", []string{testParentRequest}, testUnderlayIface},
		{"config for the subrequest", []string{testSubRequestRef}, testUnderlayIface},
		{"config for another request", []string{testOtherRequest}, testIface1},
		{"config for all requests", nil, testUnderlayIface},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := newTestDeviceState(t, allocatedPod(testProvider))
			claim := testClaim([]resourceapi.DeviceAllocationConfiguration{nicConfig(testUnderlayIface, tt.requests...)}, testSubRequestRef)
			_, err := state.Prepare(context.Background(), claim)
			require.NoError(t, err)
			specs := state.nicStore.Specs(testPodUID)
			require.Len(t, specs, 1)
			assert.Equal(t, tt.wantIface, specs[0].IfaceName)
		})
	}
}

func TestConfigMatchesRequest(t *testing.T) {
	assert.True(t, configMatchesRequest(nil, testRequestA))
	assert.True(t, configMatchesRequest([]string{testRequestA}, testRequestA))
	assert.True(t, configMatchesRequest([]string{testRequestA}, testSubRef))
	assert.True(t, configMatchesRequest([]string{testSubRef}, testSubRef))
	assert.False(t, configMatchesRequest([]string{testOtherSubRef}, testSubRef))
	assert.False(t, configMatchesRequest([]string{testRequestB}, testSubRef))
	assert.False(t, configMatchesRequest([]string{testSubRef}, testRequestA))
}

// With --annotate-vmi-mac the MAC of a VM's NIC is recorded on its VMI, keyed
// by the interface name; other pods are left alone.
func TestPrepareAnnotatesVMIMAC(t *testing.T) {
	vmPod := allocatedPod(testProvider)
	vmPod.Annotations[testProvider+annVirtualMachine] = testVMName
	state := newTestDeviceState(t, vmPod)
	type annotated struct{ namespace, name, key, value string }
	var got []annotated
	state.annotateVMI = func(_ context.Context, namespace, name string, annotations map[string]string) error {
		for k, v := range annotations {
			got = append(got, annotated{namespace, name, k, v})
		}
		return nil
	}
	_, err := state.Prepare(context.Background(), testClaim([]resourceapi.DeviceAllocationConfiguration{nicConfig(testVMIface, testRequestA)}, testRequestA))
	require.NoError(t, err)
	assert.Equal(t, []annotated{{testNS, testVMName, vmiMACAnnotationPrefix + testVMIface, testMAC1}}, got)

	got = nil
	plain := newTestDeviceState(t, allocatedPod(testProvider))
	plain.annotateVMI = state.annotateVMI
	_, err = plain.Prepare(context.Background(), testClaim(nil, testRequestA))
	require.NoError(t, err)
	assert.Empty(t, got, "a pod that is no VM")
}
