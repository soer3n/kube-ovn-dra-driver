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

package main

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	"github.com/soer3n/kube-ovn-dra-driver/internal/publisher"
)

func object(kind, name string, spec, status map[string]any) *unstructured.Unstructured {
	obj := map[string]any{
		fieldAPIVersion: kubeOVNAPIVersion,
		fieldKind:       kind,
		fieldMetadata:   map[string]any{fieldName: name},
		fieldSpec:       spec,
	}
	if status != nil {
		obj["status"] = status
	}
	return &unstructured.Unstructured{Object: obj}
}

func storeOf(t *testing.T, objs ...any) cache.Store {
	t.Helper()
	store := cache.NewStore(cache.MetaNamespaceKeyFunc)
	for _, o := range objs {
		require.NoError(t, store.Add(o))
	}
	return store
}

func ownersAsPods(_, pod string) []string { return []string{pod} }

func testSnapshotter(t *testing.T, capacity bool, claims ...any) *snapshotter {
	t.Helper()
	return &snapshotter{
		driverName: testDriver,
		capacity:   capacity,
		subnets: storeOf(t,
			object(kindSubnet, testOverlaySubnet, map[string]any{fieldCIDRBlock: "10.200.0.0/24", fieldProvider: testOverlayProvider},
				map[string]any{fieldV4Available: int64(8)}),
			object(kindSubnet, testVlanSubnet, map[string]any{fieldCIDRBlock: "172.23.0.0/24", fieldVlan: testVlan, fieldProvider: testVlanSubnetProvider},
				map[string]any{fieldV4Available: int64(100)}),
			object(kindSubnet, "orphan", map[string]any{fieldCIDRBlock: "10.9.0.0/24", fieldVlan: "not-there-yet"}, nil),
			object(kindSubnet, "no-spec", nil, nil),
			object(kindSubnet, "default-provider", map[string]any{fieldCIDRBlock: "10.8.0.0/24"}, nil),
		),
		vlans: storeOf(t, object(kindVlan, testVlan, map[string]any{fieldID: int64(100), fieldProvider: "external"}, nil)),
		ips: storeOf(t,
			object(kindIP, "pod-a.default.ovn-subnet.default.ovn", map[string]any{fieldPodName: testPodA, fieldNamespace: testNS, fieldSubnet: testOverlaySubnet}, nil)),
		claims: storeOf(t, claims...),
		owners: ownersAsPods,
	}
}

func allocated(name, pod, device string) *resourceapi.ResourceClaim {
	c := &resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: name}}
	c.Status.ReservedFor = []resourceapi.ResourceClaimConsumerReference{{Resource: podsResource, Name: pod}}
	c.Status.Allocation = &resourceapi.AllocationResult{Devices: resourceapi.DeviceAllocationResult{
		Results: []resourceapi.DeviceRequestAllocationResult{{
			Driver: testDriver, Pool: testOverlaySubnet, Device: device, Request: "nic",
			ConsumedCapacity: map[resourceapi.QualifiedName]resource.Quantity{publisher.CapacityIPv4: resource.MustParse("1")},
		}},
	}}
	return c
}

func TestSnapshotPoolsPerSubnetWithScope(t *testing.T) {
	resources, err := testSnapshotter(t, false).resources()
	require.NoError(t, err)

	require.Len(t, resources.Pools, 2, "the Subnets whose Vlan is not there yet, without spec or with the default provider are left out")
	overlay, ok := resources.Pools[testOverlaySubnet]
	require.True(t, ok, "a pool is named after its Subnet")
	assert.Nil(t, overlay.NodeSelector)

	vlan, ok := resources.Pools[testVlanSubnet]
	require.True(t, ok)
	require.NotNil(t, vlan.NodeSelector)
	assert.Equal(t, "external.provider-network.kubernetes.io/ready", vlan.NodeSelector.NodeSelectorTerms[0].MatchExpressions[0].Key)
	assert.Empty(t, vlan.Slices[0].Devices[0].Capacity, "capacity is off")
}

func TestSnapshotCapacityIsFreeAddressesPlusHeldClaims(t *testing.T) {
	// pod-a has its address (the ip object exists), pod-b is allocated and waits for its address.
	s := testSnapshotter(t, true,
		allocated("claim-a", testPodA, testOverlayDevice),
		allocated("claim-b", "pod-b", testOverlayDevice),
	)

	resources, err := s.resources()
	require.NoError(t, err)

	device := resources.Pools[testOverlaySubnet].Slices[0].Devices[0]
	v4 := device.Capacity[publisher.CapacityIPv4]
	assert.Equal(t, int64(9), v4.Value.Value(), "8 free + 1 claim that already has its address")
	assert.NotContains(t, device.Capacity, publisher.CapacityIPv6)

	vlan := resources.Pools[testVlanSubnet].Slices[0].Devices[0].Capacity[publisher.CapacityIPv4]
	assert.Equal(t, int64(100), vlan.Value.Value())
}

func TestSnapshotIsStableBetweenCalls(t *testing.T) {
	s := testSnapshotter(t, true)
	first, err := s.resources()
	require.NoError(t, err)
	second, err := s.resources()
	require.NoError(t, err)
	assert.Equal(t, first, second, "an unchanged cluster must not look like a change")
}

func TestPodOwnersAddsTheVMOfALauncherPod(t *testing.T) {
	launcher := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: testNS, Name: testVMLauncher, Labels: map[string]string{"vm.kubevirt.io/name": testVM},
	}}
	client := fake.NewSimpleClientset(launcher, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: testPlainPod}})
	owners := newPodOwners(client)

	assert.Equal(t, []string{testVMLauncher, testVM}, owners(testNS, testVMLauncher))
	assert.Equal(t, []string{testPlainPod}, owners(testNS, testPlainPod))
	assert.Equal(t, []string{testGonePod}, owners(testNS, testGonePod), "a pod that is gone is only known by its own name")
}

func TestPodOwnersCachesAnswersButNotFailures(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: testPlainPod}})
	gets := 0
	failing := true
	client.PrependReactor(verbGet, podsResource, func(clienttesting.Action) (bool, runtime.Object, error) {
		gets++
		if failing {
			return true, nil, errors.New("apiserver unavailable")
		}
		return false, nil, nil
	})
	owners := newPodOwners(client)

	assert.Equal(t, []string{testPlainPod}, owners(testNS, testPlainPod), "a failure still returns the pod's own name")
	assert.Equal(t, []string{testPlainPod}, owners(testNS, testPlainPod))
	assert.Equal(t, 2, gets, "a failure is not cached")

	failing = false
	owners(testNS, testPlainPod)
	owners(testNS, testPlainPod)
	assert.Equal(t, 3, gets, "a successful answer is cached")
}

func TestRunPublisherIfRequestedIgnoresOtherArguments(t *testing.T) {
	started, err := runPublisherIfRequested([]string{pluginBinary})
	assert.False(t, started)
	assert.NoError(t, err)

	started, err = runPublisherIfRequested([]string{pluginBinary, testNodeNameFlag})
	assert.False(t, started)
	assert.NoError(t, err)
}

func TestPluginRejectsANonPositiveAllocationTimeout(t *testing.T) {
	for _, timeout := range []string{"0s", "-1s"} {
		err := newApp().RunContext(context.Background(), []string{pluginBinary, testNodeNameFlag, "--allocation-timeout=" + timeout})
		assert.ErrorContains(t, err, "allocation timeout must be positive")
	}
}

func TestPluginRejectsAnInvalidPublishMode(t *testing.T) {
	err := newApp().RunContext(context.Background(), []string{pluginBinary, testNodeNameFlag, "--publish-mode=everywhere"})
	assert.ErrorContains(t, err, `invalid publish mode "everywhere"`)
}
