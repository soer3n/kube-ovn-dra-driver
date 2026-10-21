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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// NICs on dual-stack and IPv6-only subnets in a dual-stack cluster
// (E2E_IP_FAMILY=dual). A VM gets the IPv4 address of a dual-stack NIC by DHCP
// only, an MVP limitation of the driver's DHCP server.

const (
	labelDual   = "dual"
	msgSkipDual = "set E2E_IP_FAMILY=dual (and run `E2E_IP_FAMILY=dual make setup-e2e`) to exercise dual-stack and IPv6-only NICs"
	dualGroup   = "dual"
	dualLabel   = "e2e=" + dualGroup
	noExclude   = "[]"

	dualSubnet  = "dual-e2e"
	dualCIDR    = "10.250.15.0/24,fd00:250:15::/112"
	dualGateway = "10.250.15.1,fd00:250:15::1"
	v6Subnet    = "ipv6-e2e"
	v6CIDR      = "fd00:250:16::/112"
	v6Gateway   = "fd00:250:16::1"
	dualPodA    = "dual-a"
	dualPodB    = "dual-b"
	v6PodA      = "ipv6-a"
	v6PodB      = "ipv6-b"

	dualVMSubnet  = "dual-vm-e2e"
	dualVMCIDR    = "10.250.17.0/24,fd00:250:17::/112"
	dualVMGateway = "10.250.17.1,fd00:250:17::1"
	dualVM        = "mig-dual"
	dualVMPeer    = "mig-dual-peer"
	fieldIPv6     = "v6IpAddress"
)

func deleteDualFixtures() {
	for _, kinds := range []string{podsAndClaims, podNetKinds} {
		out, err := kubectl(kubectlDelete, kinds, flagSelector, dualLabel, flagIgnoreNotFound, flagWait, flagTimeout)
		Expect(err).NotTo(HaveOccurred(), msgKubectlDelete, out)
	}
}

// expectPodsConnected checks that the pods have an address of each family of
// the subnet on net1 and reach each other on every one of them.
func expectPodsConnected(ctx context.Context, subnet string, families int, podA, podB string) {
	provider := ovnProvider(subnet)
	ipsA := strings.Split(expectNIC(ctx, podA, ifaceNet1, provider), ",")
	ipsB := strings.Split(expectNIC(ctx, podB, ifaceNet1, provider), ",")
	Expect(ipsA).To(HaveLen(families), "pod %s", podA)
	Expect(ipsB).To(HaveLen(families), "pod %s", podB)
	for _, ip := range ipsB {
		Eventually(func() error {
			_, err := pingFrom(podA, ifaceNet1, ip)
			return err
		}, defaultTimeout, pollInterval).Should(Succeed(), "%s should reach %s", podA, ip)
	}
}

var _ = Describe("Dual-stack and IPv6-only NICs", Ordered, Label(labelDual), func() {
	ctx := context.Background()

	BeforeAll(func() {
		if !dualEnabled() {
			Skip(msgSkipDual)
		}
		deleteDualFixtures()
		DeferCleanup(deleteDualFixtures)
		var manifest strings.Builder
		manifest.WriteString(subnetManifest(dualGroup, dualSubnet, dualCIDR, dualGateway, noExclude))
		manifest.WriteString("---\n" + subnetManifest(dualGroup, v6Subnet, v6CIDR, v6Gateway, noExclude))
		for _, p := range []struct{ pod, subnet string }{{dualPodA, dualSubnet}, {dualPodB, dualSubnet}, {v6PodA, v6Subnet}, {v6PodB, v6Subnet}} {
			manifest.WriteString(claimManifest(dualGroup, p.pod, p.subnet, ifaceNet1) + claimPodManifest(dualGroup, p.pod, p.pod))
		}
		applyManifest(manifest.String())
		waitRunning(dualPodA, dualPodB, v6PodA, v6PodB)
	})

	It("plugs a NIC of a dual-stack subnet with an address of each family and connects pods over both", func() {
		expectPodsConnected(ctx, dualSubnet, 2, dualPodA, dualPodB)
	})

	It("plugs a NIC of an IPv6-only subnet and connects pods over IPv6", func() {
		expectPodsConnected(ctx, v6Subnet, 1, v6PodA, v6PodB)
	})
})

var _ = Describe("A KubeVirt VM with a NIC on a dual-stack subnet", Ordered, Label(labelKubevirt, labelDual), func() {
	ctx := context.Background()
	provider := ovnProvider(dualVMSubnet)

	BeforeAll(func() {
		if !kubevirtEnabled() {
			Skip(msgSkipKubevirt)
		}
		if !dualEnabled() {
			Skip(msgSkipDual)
		}
		deleteMigrationFixtures(ctx)
		DeferCleanup(deleteMigrationFixtures, ctx)
		applyManifest(subnetManifest(migrationGroup, dualVMSubnet, dualVMCIDR, dualVMGateway, noExclude) +
			claimManifest(migrationGroup, dualVMPeer, dualVMSubnet, ifaceNet1) + claimPodManifest(migrationGroup, dualVMPeer, dualVMPeer) +
			claimManifest(migrationGroup, dualVM+claimSuffix, dualVMSubnet, vmNetwork) +
			vmManifest(dualVM, []vmNICSpec{{network: vmNetwork, provider: provider, claim: dualVM + claimSuffix}}, ""))
		waitRunning(dualVMPeer)
		waitVMIRunning(ctx, dualVM, vmTimeout)
	})

	It("gets an address of each family from kube-ovn and its IPv4 address by DHCP", func() {
		ip, err := dynClient.Resource(ipGVR).Get(ctx, dualVM+"."+namespace+"."+provider, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		v6, _, _ := unstructured.NestedString(ip.Object, fieldSpec, fieldIPv6)
		Expect(v6).NotTo(BeEmpty(), "kube-ovn allocates the IPv6 address of the VM's NIC too")
		expectReachable(dualVMPeer, vmNIC(ctx, dualVM, provider))
		Expect(dhcpServers(ctx, dualVM)).To(Equal(2), "the subnet has no OVN DHCP, so the driver serves the NIC next to KubeVirt's server for the pod network")
	})
})
