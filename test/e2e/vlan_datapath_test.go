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
	"bytes"
	"context"
	"os/exec"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ovsVsctl runs ovs-vsctl in the ovs-ovn pod of a node.
func ovsVsctl(ctx context.Context, node string, args ...string) (string, error) {
	pods, err := clientset.CoreV1().Pods("kube-system").List(ctx, metav1.ListOptions{
		LabelSelector: "app=ovs", FieldSelector: "spec.nodeName=" + node,
	})
	Expect(err).NotTo(HaveOccurred())
	Expect(pods.Items).To(HaveLen(1), "expected one ovs-ovn pod on node %s", node)
	// Read stdout only: kubectl prints notices such as "Defaulted container" on
	// stderr, which must not end up in a port or bridge name.
	cmd := exec.Command("kubectl", append([]string{"-n", "kube-system", "exec", pods.Items[0].Name, "-c", "openvswitch", "--", "ovs-vsctl"}, args...)...)
	cmd.Dir = repoRoot()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return strings.TrimSpace(stdout.String() + stderr.String()), err
	}
	return strings.TrimSpace(stdout.String()), nil
}

var _ = Describe("VLAN underlay wiring through the provider network", Label("containerlab"), func() {
	ctx := context.Background()

	BeforeEach(func() {
		if !containerlabEnabled() {
			Skip("set E2E_CONTAINERLAB=1 (and run `make clab-deploy`) to exercise the VLAN underlay")
		}
	})

	It("plugs the NIC into br-int and reaches the VLAN through the provider bridge of its provider network", func() {
		useFixture(twoNICRelPath)
		waitRunning(twoNICPod)

		subnet, err := dynClient.Resource(subnetGVR).Get(ctx, underlaySubnet, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		vlanName, _, _ := unstructured.NestedString(subnet.Object, "spec", "vlan")
		vlan, err := dynClient.Resource(vlanGVR).Get(ctx, vlanName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		providerNetwork, _, _ := unstructured.NestedString(vlan.Object, "spec", "provider")
		Expect(providerNetwork).NotTo(BeEmpty())

		pod, err := clientset.CoreV1().Pods(namespace).Get(ctx, twoNICPod, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		node := pod.Spec.NodeName

		By("the host end of the VLAN NIC being a port of br-int that carries the logical switch port as iface-id")
		iface := twoNICPod + "." + namespace + "." + subnetProvider(ctx, underlaySubnet)
		port, err := ovsVsctl(ctx, node, "--bare", "--columns=name", "find", "Interface", "external_ids:iface-id="+iface)
		Expect(err).NotTo(HaveOccurred(), "ovs-vsctl: %s", port)
		Expect(port).NotTo(BeEmpty(), "no OVS interface with iface-id %s on node %s", iface, node)
		bridge, err := ovsVsctl(ctx, node, "port-to-br", port)
		Expect(err).NotTo(HaveOccurred(), "ovs-vsctl: %s", bridge)
		Expect(bridge).To(Equal("br-int"), "the NIC is plugged into the integration bridge, not into the provider bridge")

		By("the provider network having its own bridge with the uplink and the localnet patch port of the subnet")
		providerBridge := "br-" + providerNetwork
		out, err := ovsVsctl(ctx, node, "br-exists", providerBridge)
		Expect(err).NotTo(HaveOccurred(), "provider bridge %s does not exist on node %s: %s", providerBridge, node, out)
		ports, err := ovsVsctl(ctx, node, "list-ports", providerBridge)
		Expect(err).NotTo(HaveOccurred(), "ovs-vsctl: %s", ports)
		Expect(strings.Fields(ports)).To(ContainElement(HavePrefix("eth")), "the provider bridge carries the uplink")
		Expect(strings.Fields(ports)).To(ContainElement("patch-localnet."+underlaySubnet+"-to-br-int"),
			"the localnet port of the subnet connects the provider bridge to br-int")
	})
})
