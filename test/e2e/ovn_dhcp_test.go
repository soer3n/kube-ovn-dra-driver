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

// A VM's NIC on a Subnet with OVN's DHCP (spec.enableDHCP) takes its addresses
// from OVN: the driver starts no DHCP server for it, and with
// spec.enableIPv6RA the guest gets its IPv6 address too. On other Subnets the
// driver serves IPv4 by DHCP, as the other VM specs cover.

const (
	ovnDHCPSubnet    = "ovn-dhcp-e2e"
	ovnDHCPCIDR      = "10.250.22.0/24"
	ovnDHCPGateway   = "10.250.22.1"
	ovnDHCPVM        = "mig-ovn-dhcp"
	ovnDHCPPeer      = "mig-ovn-dhcp-peer"
	ovnDHCPDual      = "ovn-dhcp-dual-e2e"
	ovnDHCPDualCIDR  = "10.250.23.0/24,fd00:250:23::/64"
	ovnDHCPDualGW    = "10.250.23.1,fd00:250:23::1"
	ovnDHCPDualVM    = "mig-ovn-dhcp-dual"
	ovnDHCPDualPeer  = "mig-ovn-dhcp-dual-peer"
	fieldEnableDHCP  = "enableDHCP: true"
	fieldEnableIPv6R = "enableIPv6RA: true"
	// dhcpServerPort is port 67 as /proc/net/udp lists it: the local address
	// ends in :0043.
	dhcpServerPort = ":0043"
	computeCtr     = "compute"
)

// dhcpServers counts the UDP sockets on the DHCPv4 server port in the netns of
// the VM's launcher pod. KubeVirt serves the pod network with one; the driver
// adds one per NIC it serves.
func dhcpServers(ctx context.Context, vm string) int {
	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: launcherVMLabel + "=" + vm})
	Expect(err).NotTo(HaveOccurred())
	Expect(pods.Items).To(HaveLen(1), "launcher pods of %s", vm)
	out, err := kubectl(kubectlExec, pods.Items[0].Name, flagContainer, computeCtr, argsEnd, "cat", "/proc/net/udp")
	Expect(err).NotTo(HaveOccurred(), "read /proc/net/udp: %s", out)
	servers := 0
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 1 && strings.HasSuffix(strings.ToUpper(fields[1]), dhcpServerPort) {
			servers++
		}
	}
	return servers
}

// startOVNDHCPVM creates the subnet with the extra fields, a peer pod and a VM
// with a NIC on it, and waits until both run.
func startOVNDHCPVM(ctx context.Context, subnet, cidr, gateway, vm, peer string, dhcp6 bool, extra ...string) {
	deleteMigrationFixtures(ctx)
	DeferCleanup(deleteMigrationFixtures, ctx)
	applyManifest(subnetManifest(migrationGroup, subnet, cidr, gateway, noExclude, extra...) +
		claimManifest(migrationGroup, peer, subnet, ifaceNet1) + claimPodManifest(migrationGroup, peer, peer) +
		claimManifest(migrationGroup, vm+claimSuffix, subnet, vmNetwork) +
		vmManifest(vm, []vmNICSpec{{network: vmNetwork, provider: ovnProvider(subnet), claim: vm + claimSuffix, dhcp6: dhcp6}}, ""))
	waitRunning(peer)
	waitVMIRunning(ctx, vm, vmTimeout)
}

var _ = Describe("A KubeVirt VM on a Subnet with OVN's DHCP", Ordered, Label(labelKubevirt), func() {
	ctx := context.Background()

	BeforeAll(func() {
		if !kubevirtEnabled() {
			Skip(msgSkipKubevirt)
		}
		startOVNDHCPVM(ctx, ovnDHCPSubnet, ovnDHCPCIDR, ovnDHCPGateway, ovnDHCPVM, ovnDHCPPeer, false, fieldEnableDHCP)
	})

	It("takes its address from OVN, without a DHCP server of the driver", func() {
		expectReachable(ovnDHCPPeer, vmNIC(ctx, ovnDHCPVM, ovnProvider(ovnDHCPSubnet)))
		Expect(dhcpServers(ctx, ovnDHCPVM)).To(Equal(1), "only KubeVirt's DHCP server for the pod network")
	})
})

var _ = Describe("A KubeVirt VM on a dual-stack Subnet with OVN's DHCP and IPv6 router advertisements", Ordered, Label(labelKubevirt, labelDual), func() {
	ctx := context.Background()
	provider := ovnProvider(ovnDHCPDual)

	BeforeAll(func() {
		if !kubevirtEnabled() {
			Skip(msgSkipKubevirt)
		}
		if !dualEnabled() {
			Skip(msgSkipDual)
		}
		startOVNDHCPVM(ctx, ovnDHCPDual, ovnDHCPDualCIDR, ovnDHCPDualGW, ovnDHCPDualVM, ovnDHCPDualPeer, true, fieldEnableDHCP, fieldEnableIPv6R)
	})

	It("takes its IPv4 and IPv6 address from OVN and is reachable over both", func() {
		ip, err := dynClient.Resource(ipGVR).Get(ctx, ovnDHCPDualVM+"."+namespace+"."+provider, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		v4, _, _ := unstructured.NestedString(ip.Object, fieldSpec, fieldIPv4)
		v6, _, _ := unstructured.NestedString(ip.Object, fieldSpec, fieldIPv6)
		Expect(v6).NotTo(BeEmpty())
		for _, addr := range []string{v4, v6} {
			expectReachable(ovnDHCPDualPeer, addr)
		}
		Expect(dhcpServers(ctx, ovnDHCPDualVM)).To(Equal(1), "only KubeVirt's DHCP server for the pod network")
	})
})
