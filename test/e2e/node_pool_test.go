//go:build e2e

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

package e2e

import (
	"context"
	"fmt"
	"sort"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/soer3n/kube-ovn-dra-driver/internal/publisher"
)

// The node publish mode (`make kind-deploy-driver PUBLISH_MODE=node`) lets every
// kubelet plugin publish a pool for its node, named after the node, with a device
// per Subnet and no capacity. These specs skip when the publisher runs.

// pluginNodes returns the nodes that run a kubelet plugin of the driver.
func pluginNodes(ctx context.Context) []string {
	pods, err := clientset.CoreV1().Pods(kubeSystemNS).List(ctx, metav1.ListOptions{
		LabelSelector: driverInstance + "," + kubeletPlugin,
	})
	Expect(err).NotTo(HaveOccurred())
	var nodes []string
	for _, pod := range pods.Items {
		nodes = append(nodes, pod.Spec.NodeName)
	}
	sort.Strings(nodes)
	return nodes
}

// nodePoolDevices returns the device names of the pool of a node.
func nodePoolDevices(ctx context.Context, node string) []string {
	var devices []string
	for _, slice := range driverSlices(ctx)[node] {
		Expect(slice.Spec.NodeName).NotTo(BeNil(), "pool %s is not published for a node", node)
		Expect(*slice.Spec.NodeName).To(Equal(node))
		for _, device := range slice.Spec.Devices {
			Expect(device.Capacity).To(BeEmpty(), "device %s of pool %s has capacity", device.Name, node)
			devices = append(devices, device.Name)
		}
	}
	return devices
}

var _ = Describe("kube-ovn NIC DRA driver in node publish mode", Ordered, func() {
	ctx := context.Background()

	BeforeAll(func() {
		if publisherRunning(ctx) {
			Skip(msgSkipCentralMode)
		}
	})

	It("publishes one pool per node, named after the node, with a device per overlay subnet and no capacity", func() {
		nodes := pluginNodes(ctx)
		Expect(nodes).NotTo(BeEmpty())
		Eventually(func(g Gomega) {
			pools := driverSlices(ctx)
			published, unpublished := subnetNames(ctx)
			for _, subnet := range append(published, unpublished...) {
				g.Expect(pools).NotTo(HaveKey(subnet), "a pool is named after subnet %s", subnet)
			}
			g.Expect(pools).To(HaveLen(len(nodes)), "one pool per kubelet plugin")
			for _, node := range nodes {
				devices := nodePoolDevices(ctx, node)
				g.Expect(devices).To(ContainElement(publisher.DeviceName(overlaySubnet)), "pool %s", node)
				for _, subnet := range unpublished {
					g.Expect(devices).NotTo(ContainElement(publisher.DeviceName(subnet)), "subnet %s cannot back a DRA NIC", subnet)
				}
			}
		}, defaultTimeout, pollInterval).Should(Succeed())
	})

	Context("VLAN underlay subnets", Label(labelContainerlab), func() {
		BeforeEach(func() {
			if !containerlabEnabled() {
				Skip(msgSkipContainerlab)
			}
		})

		It("publishes a VLAN subnet only in the pools of the nodes where its provider network is ready", func() {
			subnet, err := dynClient.Resource(subnetGVR).Get(ctx, underlaySubnet, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			vlanName, _, _ := unstructured.NestedString(subnet.Object, fieldSpec, fieldVlan)
			vlan, err := dynClient.Resource(vlanGVR).Get(ctx, vlanName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			providerNetwork, _, _ := unstructured.NestedString(vlan.Object, fieldSpec, fieldProvider)
			pn, err := dynClient.Resource(providerNetworkGVR).Get(ctx, providerNetwork, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			ready, _, _ := unstructured.NestedStringSlice(pn.Object, fieldStatus, "readyNodes")
			Expect(ready).NotTo(BeEmpty(), "provider network %s is ready nowhere", providerNetwork)

			var publishing []string
			for _, node := range pluginNodes(ctx) {
				if contains(nodePoolDevices(ctx, node), publisher.DeviceName(underlaySubnet)) {
					publishing = append(publishing, node)
				}
			}
			sort.Strings(ready)
			Expect(publishing).To(Equal(ready), "the nodes that publish %s are the ready nodes of %s", underlaySubnet, providerNetwork)
		})
	})

	// Without capacity the scheduler does not know how many addresses are left:
	// a pod past the last one is scheduled and waits for kube-ovn instead.
	It("schedules a pod past the free addresses of a subnet, which starts once an address is released", func() {
		cleanup := func() {
			_, _ = kubectl(kubectlDelete, podsAndClaims, flagSelector, capacityLabel, flagIgnoreNotFound, flagWait)
			_, _ = kubectl(kubectlDelete, "subnet", flagSelector, capacityLabel, flagIgnoreNotFound, flagWait)
		}
		cleanup()
		DeferCleanup(cleanup)

		By("creating a small subnet and waiting for the nodes to publish it")
		out, err := kubectlStdin(capacityManifest(), kubectlApply, flagFile, "-")
		Expect(err).NotTo(HaveOccurred(), msgKubectlApply, out)
		var free int64
		Eventually(func(g Gomega) {
			var ok bool
			free, ok = subnetFree(ctx, capacitySubnet)
			g.Expect(ok).To(BeTrue(), "subnet %s has no status yet", capacitySubnet)
			g.Expect(free).To(BeNumerically(">", 0))
			for _, node := range pluginNodes(ctx) {
				g.Expect(nodePoolDevices(ctx, node)).To(ContainElement(publisher.DeviceName(capacitySubnet)), "pool %s", node)
			}
		}, defaultTimeout, pollInterval).Should(Succeed())

		By(fmt.Sprintf("filling the subnet with %d pods that each take an address", free))
		var pods []string
		for i := int64(0); i < free; i++ {
			pods = append(pods, fmt.Sprintf("%s%d", capacityPodNamePref, i))
		}
		out, err = kubectlStdin(capacityManifest(pods...), kubectlApply, flagFile, "-")
		Expect(err).NotTo(HaveOccurred(), msgKubectlApply, out)
		waitRunning(pods...)

		By("a pod that does not fit being scheduled, but not starting")
		over := capacityPodNamePref + "over"
		out, err = kubectlStdin(capacityManifest(append(pods, over)...), kubectlApply, flagFile, "-")
		Expect(err).NotTo(HaveOccurred(), msgKubectlApply, out)
		Eventually(func() (string, error) {
			return kubectl(kubectlGet, resourcePod, over, flagOutput, "jsonpath={.spec.nodeName}")
		}, defaultTimeout, pollInterval).ShouldNot(BeEmpty(), "the pod was not scheduled")
		Consistently(func() (string, error) {
			return kubectl(kubectlGet, resourcePod, over, flagOutput, "jsonpath={.status.phase}")
		}, 20*time.Second, pollInterval).ShouldNot(Equal(phaseRunning), "the pod started without an address")

		By("the pod starting once a pod released its address")
		out, err = kubectl(kubectlDelete, podsAndClaims, pods[0], flagWait)
		Expect(err).NotTo(HaveOccurred(), msgKubectlDelete, out)
		waitRunning(over)
	})
})
