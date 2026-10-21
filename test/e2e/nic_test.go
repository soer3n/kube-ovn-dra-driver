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
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Fixtures, relative to the repository root. They share subnets with the base
// demo, so each can be applied and removed independently.
const (
	// twoNICRelPath is the 2-NIC scaling example: net1 on vlan100-subnet
	// (VLAN underlay) and net2 on ovn-subnet (OVN overlay).
	twoNICRelPath = "demo/nic-example/examples/2nic.yaml"
	// sharedSubnetRelPath is two pods with one NIC each on ovn-subnet.
	sharedSubnetRelPath = "demo/nic-example/examples/shared-subnet.yaml"
	// sameSubnetRelPath is one pod with two NICs on ovn-subnet.
	sameSubnetRelPath = "demo/nic-example/examples/same-subnet-2nic.yaml"
	// firstAvailableRelPath is one claim with two firstAvailable requests that
	// fall back to ovn-subnet, with the interface names on the parent requests.
	firstAvailableRelPath = "demo/nic-example/examples/first-available.yaml"
)

// Names from the demo fixtures. The underlay gateway is the FRR gateway on
// eth1.100 (vlan100-subnet's spec.gateway).
const (
	twoNICPod         = "nic-demo-2"
	sameSubnetPod     = "same-subnet-2nic"
	firstAvailablePod = "first-available"
	// firstAvailableClaim and fallbackSubrequest are from first-available.yaml.
	firstAvailableClaim = "first-available"
	fallbackSubrequest  = "fallback"
	// providersAnnotation is where kube-ovn-controller records the subnet and
	// provider of each NIC on its claim.
	providersAnnotation = "ovn.kubernetes.io/dra_providers"
	underlaySubnet      = "vlan100-subnet"
	overlaySubnet       = "ovn-subnet"
	underlayGatewayIP   = "172.23.0.253"
	sharedPodA          = "shared-a"
	sharedPodB          = "shared-b"
	parentDir           = ".."
)

func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), parentDir, parentDir))
}

func kubectl(args ...string) (string, error) {
	cmd := exec.Command(kubectlBinary, args...)
	cmd.Dir = repoRoot()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// pingFrom pings ip three times through the pod's iface.
func pingFrom(pod, iface, ip string) (string, error) {
	return kubectl(kubectlExec, pod, argsEnd, "ping", flagPingCount, "3", "-W", "2", "-I", iface, ip)
}

// providerKey is the key kube-ovn gives a NIC that shares its provider with
// another NIC of the pod: "<provider>.<interface>".
func providerKey(provider, iface string) string {
	return provider + "." + iface
}

// useFixture applies a fixture for the current spec and removes it afterwards.
// It also removes leftovers of an earlier run first.
func useFixture(relPath string) {
	fixture := filepath.Join(repoRoot(), relPath)
	_, _ = kubectl(kubectlDelete, flagFile, fixture, flagIgnoreNotFound, flagWait)
	DeferCleanup(func() {
		_, _ = kubectl(kubectlDelete, flagFile, fixture, flagIgnoreNotFound, flagWait)
	})
	out, err := kubectl(kubectlApply, flagFile, fixture)
	Expect(err).NotTo(HaveOccurred(), msgKubectlApply, out)
}

func waitRunning(pods ...string) {
	for _, pod := range pods {
		Eventually(func() (string, error) {
			return kubectl(kubectlGet, resourcePod, pod, flagOutput, "jsonpath={.status.phase}")
		}, defaultTimeout, pollInterval).Should(Equal(phaseRunning), "pod %s", pod)
	}
}

func subnetProvider(ctx context.Context, subnet string) string {
	obj, err := dynClient.Resource(subnetGVR).Get(ctx, subnet, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred(), "get subnet %s", subnet)
	provider, _, _ := unstructured.NestedString(obj.Object, fieldSpec, fieldProvider)
	Expect(provider).NotTo(BeEmpty(), "subnet %s has no provider", subnet)
	return provider
}

// expectNIC checks that kube-ovn-controller allocated the NIC keyed by key and
// that the driver configured its addresses on iface in the pod. It returns the
// address annotation, which holds one address per family, comma separated.
func expectNIC(ctx context.Context, pod, iface, key string) string {
	p, err := clientset.CoreV1().Pods(namespace).Get(ctx, pod, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	Expect(p.Annotations).To(HaveKeyWithValue(key+".kubernetes.io/allocated", "true"), "pod %s NIC %s", pod, key)
	ip := p.Annotations[key+".kubernetes.io/ip_address"]
	Expect(ip).NotTo(BeEmpty(), "pod %s NIC %s has no address", pod, key)

	for addr := range strings.SplitSeq(ip, ",") {
		Eventually(func() ([]string, error) {
			l, err := linkShow(pod, iface)
			return l.ips(), err
		}, defaultTimeout, pollInterval).Should(ContainElement(addr), "pod %s %s should carry %s", pod, iface, addr)
	}
	return ip
}

// ipCRExists reports whether kube-ovn's ip CR for the pod's NIC keyed by key
// exists.
func ipCRExists(ctx context.Context, pod, key string) bool {
	_, err := dynClient.Resource(ipGVR).Get(ctx, fmt.Sprintf("%s.%s.%s", pod, namespace, key), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false
	}
	Expect(err).NotTo(HaveOccurred())
	return true
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
				if _, ok := dev.Attributes[resourcev1.QualifiedName("subnetName")]; ok {
					sawSubnetAttr = true
				}
			}
		}
		Expect(devices).To(BeNumerically(">", 0), "expected at least one %s device in a ResourceSlice", driverName)
		Expect(sawSubnetAttr).To(BeTrue(), "expected a device with the subnetName attribute")
	})

	// The fixture puts net1 on a VLAN subnet, which is only published where its
	// ProviderNetwork is ready, i.e. with the containerlab fabric.
	It("plugs the NICs kube-ovn-controller allocates, wires the VLAN NIC through its provider network and releases them with the pod", Label(labelContainerlab), func() {
		if !containerlabEnabled() {
			Skip(msgSkipContainerlab)
		}
		useFixture(twoNICRelPath)
		waitRunning(twoNICPod)
		underlay, overlay := subnetProvider(ctx, underlaySubnet), subnetProvider(ctx, overlaySubnet)

		By("each NIC carrying the address kube-ovn-controller allocated")
		expectNIC(ctx, twoNICPod, ifaceNet1, underlay)
		expectNIC(ctx, twoNICPod, ifaceNet2, overlay)
		Expect(ipCRExists(ctx, twoNICPod, underlay)).To(BeTrue())
		Expect(ipCRExists(ctx, twoNICPod, overlay)).To(BeTrue())

		By("pinging the FRR gateway " + underlayGatewayIP + " from the underlay NIC")
		Eventually(func() (string, error) {
			out, err := pingFrom(twoNICPod, ifaceNet1, underlayGatewayIP)
			return strings.TrimSpace(out), err
		}, defaultTimeout, pollInterval).Should(ContainSubstring(noPacketLoss))

		expectVLANWiring(ctx)

		By("kube-ovn-controller releasing the addresses when the pod is deleted")
		out, err := kubectl(kubectlDelete, flagFile, filepath.Join(repoRoot(), twoNICRelPath), flagWait)
		Expect(err).NotTo(HaveOccurred(), msgKubectlDelete, out)
		Eventually(func() bool {
			return ipCRExists(ctx, twoNICPod, underlay) || ipCRExists(ctx, twoNICPod, overlay)
		}, defaultTimeout, pollInterval).Should(BeFalse())
	})

	It("keys NICs of one pod on one subnet by interface name", func() {
		useFixture(sameSubnetRelPath)
		waitRunning(sameSubnetPod)
		provider := subnetProvider(ctx, overlaySubnet)

		ip1 := expectNIC(ctx, sameSubnetPod, ifaceNet1, providerKey(provider, ifaceNet1))
		ip2 := expectNIC(ctx, sameSubnetPod, ifaceNet2, providerKey(provider, ifaceNet2))
		Expect(ip1).NotTo(Equal(ip2))
		Expect(ipCRExists(ctx, sameSubnetPod, providerKey(provider, ifaceNet1))).To(BeTrue())
		Expect(ipCRExists(ctx, sameSubnetPod, providerKey(provider, ifaceNet2))).To(BeTrue())

		By("kube-ovn-controller releasing the addresses when the pod is deleted")
		out, err := kubectl(kubectlDelete, flagFile, filepath.Join(repoRoot(), sameSubnetRelPath), flagWait)
		Expect(err).NotTo(HaveOccurred(), msgKubectlDelete, out)
		Eventually(func() bool {
			return ipCRExists(ctx, sameSubnetPod, providerKey(provider, ifaceNet1)) || ipCRExists(ctx, sameSubnetPod, providerKey(provider, ifaceNet2))
		}, defaultTimeout, pollInterval).Should(BeFalse())
	})

	It("resolves firstAvailable requests and names their NICs after the parent request's config", func() {
		useFixture(firstAvailableRelPath)
		waitRunning(firstAvailablePod)
		provider := subnetProvider(ctx, overlaySubnet)

		ip1 := expectNIC(ctx, firstAvailablePod, ifaceNet1, providerKey(provider, ifaceNet1))
		ip2 := expectNIC(ctx, firstAvailablePod, ifaceNet2, providerKey(provider, ifaceNet2))
		Expect(ip1).NotTo(Equal(ip2))

		claim, err := clientset.ResourceV1().ResourceClaims(namespace).Get(ctx, firstAvailableClaim, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		for _, result := range claim.Status.Allocation.Devices.Results {
			Expect(result.Request).To(HaveSuffix("/"+fallbackSubrequest), "request %s should be allocated by its fallback", result.Request)
		}
		By("checking the subnet and provider kube-ovn-controller recorded on the claim")
		records := map[string]struct {
			Subnet   string `json:"subnet"`
			Provider string `json:"provider"`
		}{}
		Expect(json.Unmarshal([]byte(claim.Annotations[providersAnnotation]), &records)).To(Succeed())
		Expect(records).NotTo(BeEmpty())
		for device, record := range records {
			Expect(record.Subnet).To(Equal(overlaySubnet), "device %s", device)
			Expect(record.Provider).To(Equal(provider), "device %s", device)
		}
	})

	It("connects pods that share a subnet over the overlay", func() {
		useFixture(sharedSubnetRelPath)
		waitRunning(sharedPodA, sharedPodB)
		provider := subnetProvider(ctx, overlaySubnet)

		ipA := expectNIC(ctx, sharedPodA, ifaceNet1, provider)
		ipB := expectNIC(ctx, sharedPodB, ifaceNet1, provider)
		Expect(ipA).NotTo(Equal(ipB))

		By("pinging shared-b's NIC from shared-a")
		Eventually(func() (string, error) {
			return pingFrom(sharedPodA, ifaceNet1, ipB)
		}, defaultTimeout, pollInterval).Should(ContainSubstring(noPacketLoss))
	})
})
