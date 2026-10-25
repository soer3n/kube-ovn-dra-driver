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

package nicprepare

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"github.com/soer3n/kube-ovn-dra-driver/internal/publisher"
	"github.com/soer3n/kube-ovn-dra-driver/pkg/plumbing"
)

// TestWithMask covers combining the bare IP kube-ovn annotates with the subnet
// CIDR mask into the CIDR form the plumbing layer needs.

// Values shared by the tests of this package.
const (
	testBlueSubnet       = "blue"
	testDeviceName       = "subnet-blue"
	testPool             = "pool1"
	testChangedProvider  = "blue2.default.ovn"
	testRecordedProvider = "blue.default.ovn"
	testInvalidRecord    = "{"
	testUnderlayIP       = "172.23.0.5/24"
	testUnderlayCIDR     = "172.23.0.0/24"
	testOtherIP          = "1.2.3.4"
	testNS               = "default"
	testNode             = "node1"
	testNodeA            = "node-a"
	testNodeB            = "node-b"
	testPod              = "pod1"
	testPodUID           = "uid-1"
	testLauncherPod      = "virt-launcher-vm1-abcde"
	testSourcePod        = "launcher-src"
	testSourceUID        = "uid-src"
	testTargetPod        = "launcher-dst"
	testTargetUID        = "uid-dst"
	testVM               = "vm1"
	testOtherVM          = "vm2"
	testOwner            = "mypod"
	testIface            = "net1"
	testClaim            = "claim"
	testOtherGroup       = "example.com"
	testOtherResource    = "things"
	testOtherUID         = "x"
	podsResource         = "pods"
	verbGet              = "get"
	testAPIError         = "apiserver unavailable"
	testSubnet           = "ovn-subnet"

	testMAC          = "00:00:00:aa:bb:cc"
	testIP           = "10.10.0.5"
	testIPWithMask   = "10.10.0.5/24"
	testCIDR         = "10.10.0.0/24"
	testGateway      = "10.10.0.1"
	testRouteGateway = "10.10.0.254"
	testRouteDst     = "192.168.0.0/16"
	testOwnGwDst     = "10.20.0.0/16"
	testIPv6WithMask = "fd00::5/120"
	testCIDRv6       = "fd00::/120"
	testGatewayV6    = "fd00::1"
	testDualIPs      = testIP + ",fd00::5"
	testDualCIDRs    = testCIDR + "," + testCIDRv6
	testDualGateways = testGateway + "," + testGatewayV6
	testOwnerPort    = testOwner + "." + testNS

	// kube-ovn's per-provider annotations, after the provider key.
	annAllocated  = ".kubernetes.io/allocated"
	annIPAddress  = ".kubernetes.io/ip_address"
	annMACAddress = ".kubernetes.io/mac_address"
	annCIDR       = ".kubernetes.io/cidr"
	annGateway    = ".kubernetes.io/gateway"
	annRoutes     = ".kubernetes.io/routes"
	annVM         = ".kubernetes.io/virtualmachine"
	annTrue       = "true"
)

func TestWithMask(t *testing.T) {
	tests := []struct {
		name     string
		ip       string
		cidr     string
		expected string
	}{
		{"bare ip + /24", "172.23.0.5", testUnderlayCIDR, testUnderlayIP},
		{"bare ip + /16", "10.0.1.5", "10.0.0.0/16", "10.0.1.5/16"},
		{"ip already has mask", testUnderlayIP, testUnderlayCIDR, testUnderlayIP},
		{"empty ip unchanged", "", testUnderlayCIDR, ""},
		{"unparseable cidr falls back to bare ip", testOtherIP, "not-a-cidr", testOtherIP},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := withMask(tt.ip, tt.cidr); got != tt.expected {
				t.Errorf("withMask(%q,%q) = %q, want %q", tt.ip, tt.cidr, got, tt.expected)
			}
		})
	}
}

func TestValidateSecondaryProvider(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		subnet   string
		wantErr  bool
	}{
		// Default/empty provider names the pod's PRIMARY port — never valid for a secondary NIC.
		{"empty provider rejected", "", testSubnet, true},
		{"default ovn provider rejected", defaultProvider, testSubnet, true},
		// A dedicated provider (multus convention) is accepted.
		{"dedicated overlay provider ok", "ovn-subnet.default.ovn", testSubnet, false},
		{"vlan provider ok", "external.vlan100-subnet.ovn", "vlan100-subnet", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSecondaryProvider(tt.provider, tt.subnet)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateSecondaryProvider(%q,%q) err=%v, wantErr=%v", tt.provider, tt.subnet, err, tt.wantErr)
			}
		})
	}
}

func TestRequestIPAM(t *testing.T) {
	const provider = testRecordedProvider
	device := resourceapi.Device{
		Name: testDeviceName,
		Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			publisher.AttrSubnetName: {StringValue: ptr.To(testBlueSubnet)},
			publisher.AttrProvider:   {StringValue: ptr.To(provider)},
		},
	}
	claim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: testClaim, Namespace: testNS},
		Status: resourceapi.ResourceClaimStatus{
			ReservedFor: []resourceapi.ResourceClaimConsumerReference{{Resource: podsResource, Name: testLauncherPod, UID: testPodUID}},
		},
	}
	result := &resourceapi.DeviceRequestAllocationResult{Device: device.Name}

	tests := []struct {
		name        string
		annotations map[string]string
		wantPort    string
		wantIP      string
		wantGateway string
		wantRoutes  []plumbing.Route
	}{
		{
			name: "port named after the pod",
			annotations: map[string]string{
				provider + annAllocated:  annTrue,
				provider + annIPAddress:  testIP,
				provider + annMACAddress: testMAC,
				provider + annCIDR:       testCIDR,
				provider + annGateway:    testGateway,
				provider + annRoutes:     fmt.Sprintf(`[{"dst":%q},{"dst":%q,"gw":%q}]`, testRouteDst, testOwnGwDst, testRouteGateway),
			},
			wantRoutes:  []plumbing.Route{{Destination: testRouteDst}, {Destination: testOwnGwDst, Gateway: testRouteGateway}},
			wantPort:    "virt-launcher-vm1-abcde.default." + provider,
			wantIP:      testIPWithMask,
			wantGateway: testGateway,
		},
		{
			name: "keep-vm-ip port named after the vm, first family of dual-stack",
			annotations: map[string]string{
				provider + annAllocated:  annTrue,
				provider + annIPAddress:  testDualIPs,
				provider + annMACAddress: testMAC,
				provider + annCIDR:       testDualCIDRs,
				provider + annGateway:    testDualGateways,
				provider + annVM:         testVM,
			},
			wantPort:    "vm1.default." + provider,
			wantIP:      testIPWithMask,
			wantGateway: testGateway,
		},
		{
			// A second NIC on the same provider makes kube-ovn key both by interface name.
			name: "nic sharing its provider is keyed by interface name",
			annotations: map[string]string{
				provider + ".net1.kubernetes.io/allocated":   annTrue,
				provider + ".net1.kubernetes.io/ip_address":  "10.10.0.9",
				provider + ".net1.kubernetes.io/mac_address": "00:00:00:aa:bb:dd",
				provider + ".net1.kubernetes.io/cidr":        testCIDR,
				provider + ".net1.kubernetes.io/gateway":     testGateway,
				provider + ".net2.kubernetes.io/allocated":   annTrue,
				provider + ".net2.kubernetes.io/ip_address":  "10.10.0.10",
				provider + ".net2.kubernetes.io/mac_address": "00:00:00:aa:bb:ee",
			},
			wantPort:    "virt-launcher-vm1-abcde.default." + provider + ".net1",
			wantIP:      "10.10.0.9/24",
			wantGateway: testGateway,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewSimpleClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: testLauncherPod, Namespace: testNS, UID: testPodUID, Annotations: tt.annotations,
			}, Spec: corev1.PodSpec{NodeName: testNode}})
			cfg, err := RequestIPAM(context.Background(), client, claim, result, device, testIface, testNode)
			if err != nil {
				t.Fatalf("RequestIPAM() error = %v", err)
			}
			if cfg.LogicalSwitchPort != tt.wantPort {
				t.Errorf("LogicalSwitchPort = %q, want %q", cfg.LogicalSwitchPort, tt.wantPort)
			}
			if cfg.IP != tt.wantIP {
				t.Errorf("IP = %q, want %q", cfg.IP, tt.wantIP)
			}
			if cfg.Gateway != tt.wantGateway {
				t.Errorf("Gateway = %q, want %q", cfg.Gateway, tt.wantGateway)
			}
			if !reflect.DeepEqual(cfg.Routes, tt.wantRoutes) {
				t.Errorf("Routes = %v, want %v", cfg.Routes, tt.wantRoutes)
			}
			if cfg.PodName != testLauncherPod || cfg.PodUID != testPodUID {
				t.Errorf("pod = %s/%s, want virt-launcher-vm1-abcde/uid-1", cfg.PodName, cfg.PodUID)
			}
		})
	}

	t.Run("fails on an invalid routes annotation", func(t *testing.T) {
		client := fake.NewSimpleClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: testLauncherPod, Namespace: testNS, UID: testPodUID, Annotations: map[string]string{
				provider + annAllocated:  annTrue,
				provider + annIPAddress:  testIP,
				provider + annMACAddress: testMAC,
				provider + annRoutes:     "not json",
			},
		}, Spec: corev1.PodSpec{NodeName: testNode}})
		if _, err := RequestIPAM(context.Background(), client, claim, result, device, testIface, testNode); err == nil {
			t.Fatal("expected an error for an invalid routes annotation")
		}
	})

	t.Run("fails without a subnetName attribute", func(t *testing.T) {
		dev := device.DeepCopy()
		delete(dev.Attributes, publisher.AttrSubnetName)
		client := fake.NewSimpleClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: testLauncherPod, Namespace: testNS, UID: testPodUID}, Spec: corev1.PodSpec{NodeName: testNode}})
		if _, err := RequestIPAM(context.Background(), client, claim, result, *dev, testIface, testNode); err == nil {
			t.Fatal("expected an error for a device without subnetName")
		}
	})

	t.Run("names the likely causes when kube-ovn-controller does not allocate", func(t *testing.T) {
		client := fake.NewSimpleClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: testLauncherPod, Namespace: testNS, UID: testPodUID}, Spec: corev1.PodSpec{NodeName: testNode}})
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err := RequestIPAM(ctx, client, claim, &resourceapi.DeviceRequestAllocationResult{Device: device.Name, Driver: "nic.kubeovn.io"}, device, testIface, testNode)
		if err == nil {
			t.Fatal("expected a timeout error")
		}
		for _, want := range []string{"pod default/virt-launcher-vm1-abcde", provider + annAllocated, provider + ".net1.kubernetes.io/allocated", "--enable-dra-nic", "--dra-nic-driver-name=nic.kubeovn.io", "pod's events"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q", err, want)
			}
		}
	})

	t.Run("rejects the default provider without waiting", func(t *testing.T) {
		dev := device.DeepCopy()
		dev.Attributes[publisher.AttrProvider] = resourceapi.DeviceAttribute{StringValue: ptr.To(defaultProvider)}
		client := fake.NewSimpleClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: testLauncherPod, Namespace: testNS, UID: testPodUID}, Spec: corev1.PodSpec{NodeName: testNode}})
		if _, err := RequestIPAM(context.Background(), client, claim, result, *dev, testIface, testNode); err == nil {
			t.Fatal("expected an error for the default provider")
		}
	})
}

func TestDualStackAddresses(t *testing.T) {
	tests := []struct {
		name             string
		ips, cidrs, gws  string
		wantIPs          []string
		wantCIDR, wantGw string
	}{
		{"ipv4", testIP, testCIDR, testGateway, []string{testIPWithMask}, testCIDR, testGateway},
		{"ipv6", "fd00::5", testCIDRv6, testGatewayV6, []string{testIPv6WithMask}, testCIDRv6, testGatewayV6},
		{"dual-stack", testDualIPs, testDualCIDRs, testDualGateways, []string{testIPWithMask, testIPv6WithMask}, testCIDR, testGateway},
		{"dual-stack, ipv6 listed first", "fd00::5,10.10.0.5", "fd00::/120,10.10.0.0/24", "fd00::1,10.10.0.1", []string{testIPWithMask, testIPv6WithMask}, testCIDR, testGateway},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ips, cidr, gw := dualStackAddresses(tt.ips, tt.cidrs, tt.gws)
			if strings.Join(ips, ",") != strings.Join(tt.wantIPs, ",") || cidr != tt.wantCIDR || gw != tt.wantGw {
				t.Errorf("dualStackAddresses() = %v, %q, %q; want %v, %q, %q", ips, cidr, gw, tt.wantIPs, tt.wantCIDR, tt.wantGw)
			}
		})
	}
}

func TestPortName(t *testing.T) {
	tests := []struct {
		name, owner, namespace, provider, expected string
	}{
		{"default provider", testOwner, testNS, defaultProvider, testOwnerPort},
		{"empty provider", testOwner, testNS, "", testOwnerPort},
		{"dedicated provider", testVM, testNS, testRecordedProvider, "vm1.default.blue.default.ovn"},
		{"provider keyed by interface name", testVM, testNS, "blue.default.ovn.net2", "vm1.default.blue.default.ovn.net2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := portName(tt.owner, tt.namespace, tt.provider); got != tt.expected {
				t.Errorf("portName(%q,%q,%q) = %q, want %q", tt.owner, tt.namespace, tt.provider, got, tt.expected)
			}
		})
	}
}

func TestLookupPodForClaim(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: testPod, Namespace: testNS, UID: testPodUID}, Spec: corev1.PodSpec{NodeName: testNode}}
	claimFor := func(refs ...resourceapi.ResourceClaimConsumerReference) *resourceapi.ResourceClaim {
		return &resourceapi.ResourceClaim{
			ObjectMeta: metav1.ObjectMeta{Name: testClaim, Namespace: testNS},
			Status:     resourceapi.ResourceClaimStatus{ReservedFor: refs},
		}
	}
	podRef := resourceapi.ResourceClaimConsumerReference{Resource: podsResource, Name: testPod, UID: testPodUID}

	tests := []struct {
		name    string
		claim   *resourceapi.ResourceClaim
		wantErr bool
	}{
		{"reserved for the pod", claimFor(podRef), false},
		{"skips other consumers", claimFor(resourceapi.ResourceClaimConsumerReference{APIGroup: testOtherGroup, Resource: testOtherResource, UID: testOtherUID}, podRef), false},
		{"not reserved", claimFor(), true},
		{"pod gone", claimFor(resourceapi.ResourceClaimConsumerReference{Resource: podsResource, Name: "pod2", UID: "uid-2"}), true},
		{"recreated pod with the same name", claimFor(resourceapi.ResourceClaimConsumerReference{Resource: podsResource, Name: testPod, UID: "uid-old"}), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name, ns, uid, err := lookupPodForClaim(context.Background(), fake.NewSimpleClientset(pod), tt.claim, testNode)
			if (err != nil) != tt.wantErr {
				t.Fatalf("lookupPodForClaim() err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && (name != testPod || ns != testNS || uid != testPodUID) {
				t.Errorf("lookupPodForClaim() = %s/%s %s, want default/pod1 uid-1", ns, name, uid)
			}
		})
	}
}

// A claim shared by the source and the target pod of a live migration is
// prepared on both nodes; each node must pick the pod scheduled on it. Any
// other sharing is rejected.
func TestLookupPodForClaimSharedClaim(t *testing.T) {
	launcher := func(name, uid, node, vm string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS, UID: types.UID(uid), Labels: map[string]string{kubevirtVMLabel: vm}},
			Spec:       corev1.PodSpec{NodeName: node},
		}
	}
	source := launcher(testSourcePod, testSourceUID, testNodeA, testVM)
	target := launcher(testTargetPod, testTargetUID, testNodeB, testVM)
	otherVM := launcher(testTargetPod, testTargetUID, testNodeB, testOtherVM)
	sameNode := launcher(testTargetPod, testTargetUID, testNodeA, testVM)
	plain := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: testTargetPod, Namespace: testNS, UID: testTargetUID}, Spec: corev1.PodSpec{NodeName: testNodeB}}
	claim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "vm-nic", Namespace: testNS},
		Status: resourceapi.ResourceClaimStatus{ReservedFor: []resourceapi.ResourceClaimConsumerReference{
			{Resource: podsResource, Name: testSourcePod, UID: testSourceUID},
			{Resource: podsResource, Name: testTargetPod, UID: testTargetUID},
		}},
	}

	tests := []struct {
		name     string
		pods     []runtime.Object
		nodeName string
		wantPod  string
		wantErr  bool
	}{
		{"source node", []runtime.Object{source, target}, testNodeA, testSourcePod, false},
		{"target node", []runtime.Object{source, target}, testNodeB, testTargetPod, false},
		{"no pod on the node", []runtime.Object{source, target}, "node-c", "", true},
		{"source already gone", []runtime.Object{target}, testNodeB, testTargetPod, false},
		{"pods of different VMs", []runtime.Object{source, otherVM}, testNodeB, "", true},
		{"pod that is no VM", []runtime.Object{source, plain}, testNodeB, "", true},
		{"two consumers on one node", []runtime.Object{source, sameNode}, testNodeA, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name, _, _, err := lookupPodForClaim(context.Background(), fake.NewSimpleClientset(tt.pods...), claim, tt.nodeName)
			if (err != nil) != tt.wantErr {
				t.Fatalf("lookupPodForClaim() err = %v, wantErr %v", err, tt.wantErr)
			}
			if name != tt.wantPod {
				t.Errorf("lookupPodForClaim() = %q, want %q", name, tt.wantPod)
			}
		})
	}
}

// An API error other than NotFound is returned, not taken as a gone consumer.
func TestLookupPodForClaimAPIError(t *testing.T) {
	client := fake.NewSimpleClientset()
	client.PrependReactor(verbGet, podsResource, func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New(testAPIError)
	})
	claim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: testClaim, Namespace: testNS},
		Status: resourceapi.ResourceClaimStatus{ReservedFor: []resourceapi.ResourceClaimConsumerReference{
			{Resource: podsResource, Name: testPod, UID: testPodUID},
		}},
	}
	_, _, _, err := lookupPodForClaim(context.Background(), client, claim, testNode)
	if err == nil || !strings.Contains(err.Error(), testAPIError) {
		t.Fatalf("lookupPodForClaim() err = %v, want the API error", err)
	}
}

// A NIC recorded on the claim keeps the subnet and provider kube-ovn-controller
// allocated it with, even if the published device names another provider now.
func TestRequestIPAMUsesRecordedProvider(t *testing.T) {
	device := resourceapi.Device{
		Name: testDeviceName,
		Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			publisher.AttrSubnetName: {StringValue: ptr.To(testSubnet)},
			publisher.AttrProvider:   {StringValue: ptr.To(testChangedProvider)},
		},
	}
	result := &resourceapi.DeviceRequestAllocationResult{Pool: testPool, Device: device.Name}
	record, err := json.Marshal(map[string]nicRecord{testPool + "/" + testDeviceName: {Subnet: testSubnet, Provider: testRecordedProvider}})
	if err != nil {
		t.Fatal(err)
	}
	claim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: testClaim, Namespace: testNS, Annotations: map[string]string{providersAnnotation: string(record)}},
		Status: resourceapi.ResourceClaimStatus{
			ReservedFor: []resourceapi.ResourceClaimConsumerReference{{Resource: podsResource, Name: testLauncherPod, UID: testPodUID}},
		},
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: testLauncherPod, Namespace: testNS, UID: testPodUID, Annotations: map[string]string{
		testRecordedProvider + annAllocated:  annTrue,
		testRecordedProvider + annIPAddress:  testIP,
		testRecordedProvider + annMACAddress: testMAC,
		testRecordedProvider + annCIDR:       testCIDR,
		testRecordedProvider + annGateway:    testGateway,
	}}, Spec: corev1.PodSpec{NodeName: testNode}}

	cfg, err := RequestIPAM(context.Background(), fake.NewSimpleClientset(pod), claim, result, device, testIface, testNode)
	if err != nil {
		t.Fatalf("RequestIPAM() error = %v", err)
	}
	if cfg.Provider != testRecordedProvider || cfg.SubnetName != testSubnet {
		t.Errorf("RequestIPAM() provider %q subnet %q, want the recorded %q and %q", cfg.Provider, cfg.SubnetName, testRecordedProvider, testSubnet)
	}

	claim.Annotations[providersAnnotation] = testInvalidRecord
	if _, err := RequestIPAM(context.Background(), fake.NewSimpleClientset(pod), claim, result, device, testIface, testNode); err == nil {
		t.Error("RequestIPAM() with an invalid record: want an error")
	}
}
