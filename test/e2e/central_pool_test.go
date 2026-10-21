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
	"os/exec"
	"sort"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/soer3n/kube-ovn-dra-driver/internal/publisher"
)

// The central publish mode (`make kind-deploy-driver PUBLISH_MODE=central`, the
// default of the chart and the Makefile) publishes one pool per Subnet instead of one pool per
// node. These specs skip when the driver runs in node mode.

const (
	capacityIPv4 = resourcev1.QualifiedName("ipv4")

	// capacitySubnet is a small overlay subnet the capacity spec creates, so the
	// pool can be exhausted with a handful of pods.
	capacitySubnet      = "capacity-e2e"
	capacityCIDR        = "10.250.7.0/29"
	capacityLabel       = "e2e=capacity"
	capacityPodNamePref = "cap-"
	netshootImage       = "nicolaka/netshoot@sha256:47b907d662d139d1e2f22bfe14f4efca1e3f1feed283572f47c970c780c03b61"
)

var (
	providerNetworkGVR = schema.GroupVersionResource{Group: publisher.SubnetGVR.Group, Version: publisher.SubnetGVR.Version, Resource: "provider-networks"}
	vlanGVR            = publisher.VlanGVR
)

func kubectlStdin(stdin string, args ...string) (string, error) {
	cmd := exec.Command(kubectlBinary, args...)
	cmd.Dir = repoRoot()
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// publisherRunning reports whether the driver runs in central publish mode.
func publisherRunning(ctx context.Context) bool {
	deployments, err := clientset.AppsV1().Deployments(kubeSystemNS).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/component=publisher",
	})
	Expect(err).NotTo(HaveOccurred())
	return len(deployments.Items) > 0
}

// driverSlices returns the ResourceSlices of the driver by pool name.
func driverSlices(ctx context.Context) map[string][]resourcev1.ResourceSlice {
	list, err := clientset.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred())
	byPool := map[string][]resourcev1.ResourceSlice{}
	for _, slice := range list.Items {
		if slice.Spec.Driver == driverName {
			byPool[slice.Spec.Pool.Name] = append(byPool[slice.Spec.Pool.Name], slice)
		}
	}
	return byPool
}

// subnetNames returns the Subnets the driver publishes and the ones it leaves
// out (the default provider, a non-OVN provider or no CIDR, e.g. join).
func subnetNames(ctx context.Context) (published, unpublished []string) {
	list, err := dynClient.Resource(subnetGVR).List(ctx, metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred())
	noVlan := func(string) (publisher.VlanInfo, error) { return publisher.VlanInfo{}, nil }
	for i := range list.Items {
		item := &list.Items[i]
		if _, found, _ := unstructured.NestedMap(item.Object, fieldSpec); !found {
			continue
		}
		subnet, err := publisher.ParseSubnet(item, noVlan)
		Expect(err).NotTo(HaveOccurred())
		if subnet.Publishable() == nil {
			published = append(published, item.GetName())
		} else {
			unpublished = append(unpublished, item.GetName())
		}
	}
	sort.Strings(published)
	sort.Strings(unpublished)
	return published, unpublished
}

// subnetFree returns the free IPv4 addresses a Subnet reports in its status.
func subnetFree(ctx context.Context, subnet string) (int64, bool) {
	obj, err := dynClient.Resource(subnetGVR).Get(ctx, subnet, metav1.GetOptions{})
	if err != nil {
		return 0, false
	}
	value, found, _ := unstructured.NestedFieldNoCopy(obj.Object, fieldStatus, "v4availableIPs")
	if !found {
		return 0, false
	}
	switch v := value.(type) {
	case int64:
		return v, true
	case float64:
		return int64(v), true
	}
	return 0, false
}

// poolCapacity returns the IPv4 capacity the device of a Subnet publishes.
// allocatedNICs counts the NICs of the driver that allocated claims hold in the
// pool of a Subnet, e.g. those of the demo pods and VMs. Once kube-ovn gave
// them their address, the publisher adds them back to the free addresses.
func allocatedNICs(ctx context.Context, subnet string) int64 {
	claims, err := clientset.ResourceV1().ResourceClaims("").List(ctx, metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred())
	var n int64
	for _, claim := range claims.Items {
		if claim.Status.Allocation == nil {
			continue
		}
		for _, result := range claim.Status.Allocation.Devices.Results {
			if result.Driver == driverName && result.Pool == subnet {
				n++
			}
		}
	}
	return n
}

func poolCapacity(ctx context.Context, subnet string) (int64, bool) {
	for _, slice := range driverSlices(ctx)[subnet] {
		for _, device := range slice.Spec.Devices {
			if device.Name == publisher.DeviceName(subnet) {
				capacity, ok := device.Capacity[capacityIPv4]
				return capacity.Value.Value(), ok
			}
		}
	}
	return 0, false
}

// selectedNodes returns the nodes a node selector with one term of In
// expressions matches.
func selectedNodes(ctx context.Context, selector *corev1.NodeSelector) []string {
	Expect(selector.NodeSelectorTerms).To(HaveLen(1), "a pool node selector uses exactly one term")
	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	Expect(err).NotTo(HaveOccurred())
	var matched []string
	for _, node := range nodes.Items {
		ok := true
		for _, expr := range selector.NodeSelectorTerms[0].MatchExpressions {
			Expect(expr.Operator).To(Equal(corev1.NodeSelectorOpIn))
			value, has := node.Labels[expr.Key]
			if !has || !contains(expr.Values, value) {
				ok = false
			}
		}
		if ok {
			matched = append(matched, node.Name)
		}
	}
	sort.Strings(matched)
	return matched
}

func contains(values []string, v string) bool {
	for _, x := range values {
		if x == v {
			return true
		}
	}
	return false
}

// capacityManifest returns the subnet, and n pods each with their own claim for
// one NIC on it.
func capacityManifest(pods ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `apiVersion: kubeovn.io/v1
kind: Subnet
metadata:
  name: %[1]s
  labels: {e2e: capacity}
spec:
  protocol: IPv4
  cidrBlock: %[2]s
  gateway: 10.250.7.1
  provider: %[1]s.default.ovn
`, capacitySubnet, capacityCIDR)
	for _, pod := range pods {
		fmt.Fprintf(&b, `---
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: %[1]s
  namespace: default
  labels: {e2e: capacity}
spec:
  devices:
    requests:
      - name: nic
        exactly:
          deviceClassName: nic.kubeovn.io
          selectors:
            - cel:
                expression: "device.attributes['nic.kubeovn.io'].subnetName == '%[2]s'"
          count: 1
    config:
      - requests: ["nic"]
        opaque:
          driver: nic.kubeovn.io
          parameters:
            apiVersion: nic.resource.kube-ovn.io/v1alpha1
            kind: NicConfig
            interfaceName: net1
---
apiVersion: v1
kind: Pod
metadata:
  name: %[1]s
  namespace: default
  labels: {e2e: capacity}
spec:
  resourceClaims:
    - name: nics
      resourceClaimName: %[1]s
  containers:
    - name: demo
      image: %[3]s
      command: ["sleep", "infinity"]
      resources:
        claims:
          - name: nics
      securityContext:
        capabilities:
          add: ["NET_ADMIN"]
`, pod, capacitySubnet, netshootImage)
	}
	return b.String()
}

var _ = Describe("kube-ovn NIC DRA driver in central publish mode", Ordered, func() {
	ctx := context.Background()

	BeforeAll(func() {
		if !publisherRunning(ctx) {
			Skip(msgSkipNodeMode)
		}
	})

	It("publishes one pool per subnet, named after the subnet, and no pool per node", func() {
		Eventually(func(g Gomega) {
			pools := driverSlices(ctx)
			published, unpublished := subnetNames(ctx)
			g.Expect(unpublished).To(ContainElement("join"), "join uses the default provider")
			for _, subnet := range unpublished {
				g.Expect(pools).NotTo(HaveKey(subnet), "subnet %s cannot back a DRA NIC", subnet)
			}
			for _, subnet := range published {
				if strings.HasPrefix(subnet, capacitySubnet) {
					continue
				}
				slices, ok := pools[subnet]
				g.Expect(ok).To(BeTrue(), "no pool for subnet %s", subnet)
				var devices []string
				for _, slice := range slices {
					g.Expect(slice.Spec.NodeName).To(BeNil(), "pool %s is published for a node", subnet)
					for _, device := range slice.Spec.Devices {
						devices = append(devices, device.Name)
					}
				}
				g.Expect(devices).To(Equal([]string{publisher.DeviceName(subnet)}), "pool %s", subnet)
			}
		}, defaultTimeout, pollInterval).Should(Succeed())
	})

	It("makes an overlay subnet available on all nodes", func() {
		slices := driverSlices(ctx)[overlaySubnet]
		Expect(slices).NotTo(BeEmpty())
		for _, slice := range slices {
			Expect(slice.Spec.AllNodes).NotTo(BeNil())
			Expect(*slice.Spec.AllNodes).To(BeTrue())
			Expect(slice.Spec.NodeSelector).To(BeNil())
		}
	})

	It("publishes the free addresses of a subnet as capacity", func() {
		Eventually(func(g Gomega) {
			free, hasFree := subnetFree(ctx, overlaySubnet)
			capacity, hasCapacity := poolCapacity(ctx, overlaySubnet)
			g.Expect(hasFree && hasCapacity).To(BeTrue(), "subnet %s has no free count or capacity yet", overlaySubnet)
			// The subnet counts the addresses of allocated claims as used, the
			// scheduler subtracts the claims again, so the publisher adds them back.
			g.Expect(capacity).To(Equal(free + allocatedNICs(ctx, overlaySubnet)))
		}, defaultTimeout, pollInterval).Should(Succeed())
	})

	Context("VLAN underlay subnets", Label(labelContainerlab), func() {
		BeforeEach(func() {
			if !containerlabEnabled() {
				Skip(msgSkipContainerlab)
			}
		})

		It("makes a VLAN subnet available on the nodes where its provider network is ready", func() {
			subnet, err := dynClient.Resource(subnetGVR).Get(ctx, underlaySubnet, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			vlanName, _, _ := unstructured.NestedString(subnet.Object, fieldSpec, fieldVlan)
			vlan, err := dynClient.Resource(vlanGVR).Get(ctx, vlanName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			providerNetwork, _, _ := unstructured.NestedString(vlan.Object, fieldSpec, fieldProvider)
			Expect(providerNetwork).NotTo(BeEmpty())

			slices := driverSlices(ctx)[underlaySubnet]
			Expect(slices).NotTo(BeEmpty())
			for _, slice := range slices {
				Expect(slice.Spec.AllNodes).To(BeNil(), "an underlay subnet is not available on all nodes")
				Expect(slice.Spec.NodeSelector).NotTo(BeNil())
				expr := slice.Spec.NodeSelector.NodeSelectorTerms[0].MatchExpressions[0]
				Expect(expr.Key).To(Equal(providerNetwork + ".provider-network.kubernetes.io/ready"))

				pn, err := dynClient.Resource(providerNetworkGVR).Get(ctx, providerNetwork, metav1.GetOptions{})
				Expect(err).NotTo(HaveOccurred())
				ready, _, _ := unstructured.NestedStringSlice(pn.Object, fieldStatus, "readyNodes")
				sort.Strings(ready)
				Expect(selectedNodes(ctx, slice.Spec.NodeSelector)).To(Equal(ready),
					"the pool selects exactly the ready nodes of provider network %s", providerNetwork)
			}
		})
	})

	It("refuses a pod at scheduling time when the pool is exhausted, and admits it again after a release", func() {
		cleanup := func() {
			_, _ = kubectl(kubectlDelete, podsAndClaims, flagSelector, capacityLabel, flagIgnoreNotFound, flagWait)
			_, _ = kubectl(kubectlDelete, "subnet", flagSelector, capacityLabel, flagIgnoreNotFound, flagWait)
		}
		cleanup()
		DeferCleanup(cleanup)
		// Registered after the cleanup, so it reports before the pods are gone.
		DeferCleanup(startCapacityRecorder(ctx).stopAndReport)

		By("creating a small subnet and waiting for its pool to publish the free addresses")
		out, err := kubectlStdin(capacityManifest(), kubectlApply, flagFile, "-")
		Expect(err).NotTo(HaveOccurred(), msgKubectlApply, out)
		var free int64
		Eventually(func(g Gomega) {
			var ok bool
			free, ok = subnetFree(ctx, capacitySubnet)
			g.Expect(ok).To(BeTrue(), "subnet %s has no status yet", capacitySubnet)
			g.Expect(free).To(BeNumerically(">", 0))
			capacity, ok := poolCapacity(ctx, capacitySubnet)
			g.Expect(ok).To(BeTrue(), "the pool of %s publishes no capacity yet", capacitySubnet)
			g.Expect(capacity).To(Equal(free))
		}, defaultTimeout, pollInterval).Should(Succeed())

		// One pod at a time: while claims of a pool are still being bound,
		// kube-scheduler (1.36) counts their consumed capacity twice, once from
		// its assume cache and once as in-flight allocations, and can refuse a
		// pod the pool has room for. Nothing requeues that pod until the
		// unschedulable pods are flushed after 5 minutes.
		By(fmt.Sprintf("filling the pool with %d pods that each take an address, one at a time", free))
		var pods []string
		for i := int64(0); i < free; i++ {
			pods = append(pods, fmt.Sprintf("%s%d", capacityPodNamePref, i))
			out, err = kubectlStdin(capacityManifest(pods...), kubectlApply, flagFile, "-")
			Expect(err).NotTo(HaveOccurred(), msgKubectlApply, out)
			waitRunning(pods[i])
		}

		By("a pod that does not fit staying unscheduled, not stuck in container creation")
		over := capacityPodNamePref + "over"
		out, err = kubectlStdin(capacityManifest(append(pods, over)...), kubectlApply, flagFile, "-")
		Expect(err).NotTo(HaveOccurred(), msgKubectlApply, out)
		Eventually(func(g Gomega) {
			pod, err := clientset.CoreV1().Pods(namespace).Get(ctx, over, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(pod.Spec.NodeName).To(BeEmpty(), "the pod was scheduled although the pool is full")
			var unschedulable bool
			for _, c := range pod.Status.Conditions {
				if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
					unschedulable = true
				}
			}
			g.Expect(unschedulable).To(BeTrue(), "the pod is not reported unschedulable yet")
		}, defaultTimeout, pollInterval).Should(Succeed())
		Consistently(func() (string, error) {
			return kubectl(kubectlGet, resourcePod, over, flagOutput, "jsonpath={.spec.nodeName}")
		}, 20*time.Second, pollInterval).Should(BeEmpty())

		By("the pod being admitted once a pod released its address")
		out, err = kubectl(kubectlDelete, podsAndClaims, pods[0], flagWait)
		Expect(err).NotTo(HaveOccurred(), msgKubectlDelete, out)
		waitRunning(over)
	})
})
