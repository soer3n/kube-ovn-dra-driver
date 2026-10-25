//go:build e2e

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

package e2e

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// The MTU and the routes of a NIC in the pod: the Subnet's spec.mtu, else the
// MTU of the pod's eth0, and the routes of the NIC's routes annotation.

const (
	podNetGroup   = "podnet"
	podNetLabel   = "e2e=" + podNetGroup
	podNetPod     = "podnet"
	mtuSubnet     = "mtu-e2e"
	mtuCIDR       = "10.250.14.0/24"
	mtuGateway    = "10.250.14.1"
	subnetMTU     = 1300
	extraRouteDst = "198.51.100.0/24"
	ifaceEth0     = "eth0"
	podNetKinds   = "subnet"
)

// podNetManifest is the subnet with an MTU, a claim on it and one on the
// overlay subnet, and a pod with both NICs and an extra route on the first.
func podNetManifest() string {
	return fmt.Sprintf(`apiVersion: kubeovn.io/v1
kind: Subnet
metadata:
  name: %[1]s
  labels: {e2e: %[2]s}
spec:
  protocol: IPv4
  cidrBlock: %[3]s
  gateway: %[4]s
  mtu: %[5]d
  provider: %[6]s
`, mtuSubnet, podNetGroup, mtuCIDR, mtuGateway, subnetMTU, ovnProvider(mtuSubnet)) +
		claimManifest(podNetGroup, podNetPod+"-"+ifaceNet1, mtuSubnet, ifaceNet1) +
		claimManifest(podNetGroup, podNetPod+"-"+ifaceNet2, overlaySubnet, ifaceNet2) +
		fmt.Sprintf(`---
apiVersion: v1
kind: Pod
metadata:
  name: %[1]s
  namespace: %[2]s
  labels: {e2e: %[3]s}
  annotations:
    %[4]s.kubernetes.io/routes: '[{"dst": "%[5]s", "gw": "%[6]s"}]'
spec:
  resourceClaims:
    - name: nic1
      resourceClaimName: %[1]s-%[7]s
    - name: nic2
      resourceClaimName: %[1]s-%[8]s
  containers:
    - name: demo
      image: %[9]s
      command: ["sleep", "infinity"]
      resources:
        claims:
          - name: nic1
          - name: nic2
      securityContext:
        capabilities:
          add: ["NET_ADMIN"]
`, podNetPod, namespace, podNetGroup, ovnProvider(mtuSubnet), extraRouteDst, mtuGateway, ifaceNet1, ifaceNet2, netshootImage)
}

func deletePodNet() {
	for _, kinds := range []string{podsAndClaims, podNetKinds} {
		out, err := kubectl(kubectlDelete, kinds, flagSelector, podNetLabel, flagIgnoreNotFound, flagWait, flagTimeout)
		Expect(err).NotTo(HaveOccurred(), msgKubectlDelete, out)
	}
}

var _ = Describe("MTU and routes of a NIC", Ordered, func() {
	ctx := context.Background()

	BeforeAll(func() {
		deletePodNet()
		DeferCleanup(deletePodNet)
		applyManifest(podNetManifest())
		waitRunning(podNetPod)
		expectNIC(ctx, podNetPod, ifaceNet1, ovnProvider(mtuSubnet))
		expectNIC(ctx, podNetPod, ifaceNet2, subnetProvider(ctx, overlaySubnet))
	})

	It("takes the MTU of the Subnet, else the MTU of the pod's eth0", func() {
		net1, err := linkShow(podNetPod, ifaceNet1)
		Expect(err).NotTo(HaveOccurred())
		Expect(net1.Mtu).To(Equal(subnetMTU), "net1 is on %s with spec.mtu %d", mtuSubnet, subnetMTU)

		eth0, err := linkShow(podNetPod, ifaceEth0)
		Expect(err).NotTo(HaveOccurred())
		net2, err := linkShow(podNetPod, ifaceNet2)
		Expect(err).NotTo(HaveOccurred())
		Expect(net2.Mtu).To(Equal(eth0.Mtu), "net2 is on %s, which has no spec.mtu", overlaySubnet)
	})

	It("adds the routes of the NIC's routes annotation", func() {
		routes, err := routeShow(podNetPod, ifaceNet1)
		Expect(err).NotTo(HaveOccurred())
		// ip leaves dev out of the routes it lists for one dev
		Expect(routes).To(ContainElement(And(HaveField("Dst", extraRouteDst), HaveField("Gateway", mtuGateway))))
	})
})
