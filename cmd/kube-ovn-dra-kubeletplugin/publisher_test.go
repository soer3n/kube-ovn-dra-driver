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
		"apiVersion": "kubeovn.io/v1",
		"kind":       kind,
		"metadata":   map[string]any{"name": name},
		"spec":       spec,
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
		driverName: "nic.kubeovn.io",
		capacity:   capacity,
		subnets: storeOf(t,
			object("Subnet", "ovn-subnet", map[string]any{"cidrBlock": "10.200.0.0/24", "provider": "ovn-subnet.default.ovn"},
				map[string]any{"v4availableIPs": int64(8)}),
			object("Subnet", "vlan100-subnet", map[string]any{"cidrBlock": "172.23.0.0/24", "vlan": "vlan100"},
				map[string]any{"v4availableIPs": int64(100)}),
			object("Subnet", "orphan", map[string]any{"cidrBlock": "10.9.0.0/24", "vlan": "not-there-yet"}, nil),
			object("Subnet", "no-spec", nil, nil),
		),
		vlans: storeOf(t, object("Vlan", "vlan100", map[string]any{"id": int64(100), "provider": "external"}, nil)),
		ips: storeOf(t,
			object("IP", "pod-a.default.ovn-subnet.default.ovn", map[string]any{"podName": "pod-a", "namespace": "default", "subnet": "ovn-subnet"}, nil)),
		claims: storeOf(t, claims...),
		owners: ownersAsPods,
	}
}

func allocated(name, pod, device string) *resourceapi.ResourceClaim {
	c := &resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name}}
	c.Status.ReservedFor = []resourceapi.ResourceClaimConsumerReference{{Resource: "pods", Name: pod}}
	c.Status.Allocation = &resourceapi.AllocationResult{Devices: resourceapi.DeviceAllocationResult{
		Results: []resourceapi.DeviceRequestAllocationResult{{
			Driver: "nic.kubeovn.io", Pool: "ovn-subnet", Device: device, Request: "nic",
			ConsumedCapacity: map[resourceapi.QualifiedName]resource.Quantity{publisher.CapacityIPv4: resource.MustParse("1")},
		}},
	}}
	return c
}

func TestSnapshotPoolsPerSubnetWithScope(t *testing.T) {
	resources, err := testSnapshotter(t, false).resources()
	require.NoError(t, err)

	require.Len(t, resources.Pools, 2, "the Subnet whose Vlan is not there yet and the one without spec are left out")
	overlay, ok := resources.Pools["ovn-subnet"]
	require.True(t, ok, "a pool is named after its Subnet")
	assert.Nil(t, overlay.NodeSelector)

	vlan, ok := resources.Pools["vlan100-subnet"]
	require.True(t, ok)
	require.NotNil(t, vlan.NodeSelector)
	assert.Equal(t, "external.provider-network.kubernetes.io/ready", vlan.NodeSelector.NodeSelectorTerms[0].MatchExpressions[0].Key)
	assert.Empty(t, vlan.Slices[0].Devices[0].Capacity, "capacity is off")
}

func TestSnapshotCapacityIsFreeAddressesPlusHeldClaims(t *testing.T) {
	// pod-a has its address (the ip object exists), pod-b is allocated and waits for its address.
	s := testSnapshotter(t, true,
		allocated("claim-a", "pod-a", "subnet-ovn-subnet"),
		allocated("claim-b", "pod-b", "subnet-ovn-subnet"),
	)

	resources, err := s.resources()
	require.NoError(t, err)

	device := resources.Pools["ovn-subnet"].Slices[0].Devices[0]
	v4 := device.Capacity[publisher.CapacityIPv4]
	assert.Equal(t, int64(9), v4.Value.Value(), "8 free + 1 claim that already has its address")
	assert.NotContains(t, device.Capacity, publisher.CapacityIPv6)

	vlan := resources.Pools["vlan100-subnet"].Slices[0].Devices[0].Capacity[publisher.CapacityIPv4]
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
		Namespace: "default", Name: "virt-launcher-vm-1-abcde", Labels: map[string]string{"vm.kubevirt.io/name": "vm-1"},
	}}
	client := fake.NewSimpleClientset(launcher, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "plain"}})
	owners := newPodOwners(client)

	assert.Equal(t, []string{"virt-launcher-vm-1-abcde", "vm-1"}, owners("default", "virt-launcher-vm-1-abcde"))
	assert.Equal(t, []string{"plain"}, owners("default", "plain"))
	assert.Equal(t, []string{"gone"}, owners("default", "gone"), "a pod that is gone is only known by its own name")
}

func TestPodOwnersCachesAnswersButNotFailures(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "pod"}})
	gets := 0
	failing := true
	client.PrependReactor("get", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		gets++
		if failing {
			return true, nil, errors.New("apiserver unavailable")
		}
		return false, nil, nil
	})
	owners := newPodOwners(client)

	assert.Equal(t, []string{"pod"}, owners("default", "pod"), "a failure still returns the pod's own name")
	assert.Equal(t, []string{"pod"}, owners("default", "pod"))
	assert.Equal(t, 2, gets, "a failure is not cached")

	failing = false
	owners("default", "pod")
	owners("default", "pod")
	assert.Equal(t, 3, gets, "a successful answer is cached")
}

func TestRunPublisherIfRequestedIgnoresOtherArguments(t *testing.T) {
	started, err := runPublisherIfRequested([]string{"kube-ovn-dra-kubeletplugin"})
	assert.False(t, started)
	assert.NoError(t, err)

	started, err = runPublisherIfRequested([]string{"kube-ovn-dra-kubeletplugin", "--node-name=n"})
	assert.False(t, started)
	assert.NoError(t, err)
}

func TestPluginRejectsAnInvalidPublishMode(t *testing.T) {
	err := newApp().RunContext(context.Background(), []string{"kube-ovn-dra-kubeletplugin", "--node-name=n", "--publish-mode=everywhere"})
	assert.ErrorContains(t, err, `invalid publish mode "everywhere"`)
}
