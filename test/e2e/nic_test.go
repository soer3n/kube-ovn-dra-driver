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
	"os/exec"
	"path/filepath"
	"runtime"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// nicExampleFixture is the 2-NIC scaling example (1 VLAN underlay + 1 OVN
// overlay). It shares subnets with the base demo, so it can be applied/removed
// independently for a lifecycle test.
const nicExampleRelPath = "demo/nic-example/examples/2nic.yaml"

// sharedSubnetRelPath is the two-pods-one-subnet fixture (overlay only, no clab).
const sharedSubnetRelPath = "demo/nic-example/examples/shared-subnet.yaml"

// examplePodName / underlayGateway come from the demo fixtures: the 2nic pod
// gets net1 on vlan100-subnet, whose gateway is the FRR gw on eth1.100 at
// 172.23.0.253 (the subnet's spec.gateway; .1 has no device).
const (
	examplePodName    = "nic-demo-2"
	underlayGatewayIP = "172.23.0.253"
)

func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func kubectl(args ...string) (string, error) {
	cmd := exec.Command("kubectl", args...)
	cmd.Dir = repoRoot()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// draIPCount returns the number of ips.kubeovn.io objects the driver created
// (those carrying the DRA managed-by label).
func draIPCount(ctx context.Context) int {
	list, err := dynClient.Resource(ipGVR).List(ctx, metav1.ListOptions{LabelSelector: draLabel})
	Expect(err).NotTo(HaveOccurred(), "list ips.kubeovn.io")
	return len(list.Items)
}

var _ = Describe("kube-ovn NIC DRA driver", func() {
	ctx := context.Background()

	It("publishes NIC devices in ResourceSlices", func() {
		slices, err := clientset.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())

		var devices int
		var sawSubnetAttr bool
		for _, slice := range slices.Items {
			if slice.Spec.Driver != driverName {
				continue
			}
			for _, dev := range slice.Spec.Devices {
				devices++
				if _, ok := dev.Attributes[resourcev1.QualifiedName("nic.kubeovn.io/subnetName")]; ok {
					sawSubnetAttr = true
				}
			}
		}
		Expect(devices).To(BeNumerically(">", 0), "expected at least one %s device in a ResourceSlice", driverName)
		Expect(sawSubnetAttr).To(BeTrue(), "expected a device with the nic.kubeovn.io/subnetName attribute")
	})

	Context("claim lifecycle", func() {
		fixture := filepath.Join(repoRoot(), nicExampleRelPath)

		BeforeEach(func() {
			// The fixture shares pod/claim names with the demo example, and this
			// spec asserts a delta in IP reservations — so ensure it isn't already
			// applied (from a prior run or a manual deploy), or baseline would be
			// off and the +2 would never appear.
			_, _ = kubectl("delete", "-f", fixture, "--ignore-not-found", "--wait=true")
		})

		AfterEach(func() {
			_, _ = kubectl("delete", "-f", fixture, "--ignore-not-found", "--wait=true")
		})

		It("reserves an IP per NIC and releases it on teardown", func() {
			baseline := draIPCount(ctx)

			out, err := kubectl("apply", "-f", fixture)
			Expect(err).NotTo(HaveOccurred(), "kubectl apply: %s", out)

			By("the pod becoming Ready")
			Eventually(func() (string, error) {
				return kubectl("get", "pod", examplePodName, "-o", "jsonpath={.status.phase}")
			}, defaultTimeout, pollInterval).Should(Equal("Running"))

			By("two DRA IP reservations appearing (1 underlay + 1 overlay)")
			Eventually(func() int {
				return draIPCount(ctx)
			}, defaultTimeout, pollInterval).Should(Equal(baseline+2),
				"driver should create one ips.kubeovn.io per claimed NIC")

			By("the reservations being released after the claim is deleted")
			out, err = kubectl("delete", "-f", fixture, "--wait=true")
			Expect(err).NotTo(HaveOccurred(), "kubectl delete: %s", out)

			Eventually(func() int {
				return draIPCount(ctx)
			}, defaultTimeout, pollInterval).Should(Equal(baseline),
				"UnprepareResourceClaims should delete the ips.kubeovn.io objects")
		})
	})

	Context("shared subnet (multiple pods, one subnet)", func() {
		fixture := filepath.Join(repoRoot(), sharedSubnetRelPath)

		BeforeEach(func() {
			_, _ = kubectl("delete", "-f", fixture, "--ignore-not-found", "--wait=true")
		})

		AfterEach(func() {
			_, _ = kubectl("delete", "-f", fixture, "--ignore-not-found", "--wait=true")
		})

		It("lets two pods each take a NIC from the same subnet", func() {
			baseline := draIPCount(ctx)

			out, err := kubectl("apply", "-f", fixture)
			Expect(err).NotTo(HaveOccurred(), "kubectl apply: %s", out)

			By("both pods becoming Ready (the subnet device allows multiple allocations)")
			for _, pod := range []string{"shared-a", "shared-b"} {
				Eventually(func() (string, error) {
					return kubectl("get", "pod", pod, "-o", "jsonpath={.status.phase}")
				}, defaultTimeout, pollInterval).Should(Equal("Running"),
					"pod %s should schedule and run; without AllowMultipleAllocations the "+
						"second pod stays Pending (cannot allocate all claims)", pod)
			}

			By("two distinct IP reservations existing for the shared subnet")
			Eventually(func() int {
				return draIPCount(ctx)
			}, defaultTimeout, pollInterval).Should(Equal(baseline+2))
		})
	})

	Context("VLAN underlay datapath", Label("containerlab"), func() {
		fixture := filepath.Join(repoRoot(), nicExampleRelPath)

		BeforeEach(func() {
			if !containerlabEnabled() {
				Skip("set E2E_CONTAINERLAB=1 (and run `make clab-deploy`) to exercise the VLAN underlay")
			}
			_, _ = kubectl("delete", "-f", fixture, "--ignore-not-found", "--wait=true")
		})

		AfterEach(func() {
			_, _ = kubectl("delete", "-f", fixture, "--ignore-not-found", "--wait=true")
		})

		It("reaches the external VLAN gateway over the underlay NIC", func() {
			out, err := kubectl("apply", "-f", fixture)
			Expect(err).NotTo(HaveOccurred(), "kubectl apply: %s", out)

			Eventually(func() (string, error) {
				return kubectl("get", "pod", examplePodName, "-o", "jsonpath={.status.phase}")
			}, defaultTimeout, pollInterval).Should(Equal("Running"))

			By("pinging the FRR gateway " + underlayGatewayIP + " from the underlay NIC")
			Eventually(func() error {
				_, err := kubectl("exec", examplePodName, "--",
					"ping", "-c", "3", "-W", "2", underlayGatewayIP)
				return err
			}, defaultTimeout, pollInterval).Should(Succeed())
		})
	})
})
