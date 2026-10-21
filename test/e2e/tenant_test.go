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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/soer3n/kube-ovn-dra-driver/internal/publisher"
)

// Per-tenant DeviceClasses (demo/nic-example/examples/tenant-deviceclasses.yaml)
// select a tenant's subnets by the vpc or subnetName attribute the driver
// publishes. Tenant A's subnet is in a VPC of its own.

const (
	tenantRelPath      = "demo/nic-example/examples/tenant-deviceclasses.yaml"
	tenantLabel        = "e2e=tenants"
	tenantAClass       = "tenant-a.nic.kubeovn.io"
	tenantBClass       = "tenant-b.nic.kubeovn.io"
	tenantASubnet      = "tenant-a-1"
	tenantBSubnet      = "tenant-b-1"
	tenantAPod         = "tenant-a-nic"
	tenantBPod         = "tenant-b-nic"
	msgNotAllocatedYet = "claim %s is not allocated"
)

// tenantClaimPodManifest is a claim for one NIC through the device class and a
// pod that uses it as net1.
func tenantClaimPodManifest(name, class string) string {
	return fmt.Sprintf(`---
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: %[1]s
  namespace: %[2]s
  labels: {e2e: tenants}
spec:
  devices:
    requests:
      - name: nic
        exactly:
          deviceClassName: %[3]s
    config:
      - requests: ["nic"]
        opaque:
          driver: %[4]s
          parameters:
            apiVersion: nic.resource.kube-ovn.io/v1alpha1
            kind: NicConfig
            interfaceName: %[5]s
---
apiVersion: v1
kind: Pod
metadata:
  name: %[1]s
  namespace: %[2]s
  labels: {e2e: tenants}
spec:
  resourceClaims:
    - name: nic
      resourceClaimName: %[1]s
  containers:
    - name: demo
      image: %[6]s
      command: ["sleep", "infinity"]
      resources:
        claims:
          - name: nic
      securityContext:
        capabilities:
          add: ["NET_ADMIN"]
`, name, namespace, class, driverName, ifaceNet1, netshootImage)
}

func deleteTenantPods() {
	out, err := kubectl(kubectlDelete, podsAndClaims, flagSelector, tenantLabel, flagIgnoreNotFound, flagWait, flagTimeout)
	Expect(err).NotTo(HaveOccurred(), msgKubectlDelete, out)
}

// devicePublished reports whether the driver publishes the device of a subnet,
// in a pool of its own (central mode) or of a node.
func devicePublished(ctx context.Context, subnet string) bool {
	for _, slices := range driverSlices(ctx) {
		for _, slice := range slices {
			for _, device := range slice.Spec.Devices {
				if device.Name == publisher.DeviceName(subnet) {
					return true
				}
			}
		}
	}
	return false
}

// claimDevice returns the device allocated to the claim's only request.
func claimDevice(ctx context.Context, claim string) (string, error) {
	c, err := clientset.ResourceV1().ResourceClaims(namespace).Get(ctx, claim, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	if c.Status.Allocation == nil || len(c.Status.Allocation.Devices.Results) == 0 {
		return "", fmt.Errorf(msgNotAllocatedYet, claim)
	}
	return c.Status.Allocation.Devices.Results[0].Device, nil
}

// expectTenantNIC starts a pod with a NIC through the class and checks that it
// is plugged into the subnet.
func expectTenantNIC(ctx context.Context, pod, class, wantSubnet string) {
	applyManifest(tenantClaimPodManifest(pod, class))
	waitRunning(pod)
	device, err := claimDevice(ctx, pod)
	Expect(err).NotTo(HaveOccurred())
	Expect(device).To(Equal(publisher.DeviceName(wantSubnet)), "claim %s through %s", pod, class)
	expectNIC(ctx, pod, ifaceNet1, subnetProvider(ctx, wantSubnet))
}

var _ = Describe("Per-tenant DeviceClasses", Ordered, func() {
	ctx := context.Background()

	BeforeAll(func() {
		deleteTenantPods()
		useFixture(tenantRelPath)
		DeferCleanup(deleteTenantPods)
		for _, subnet := range []string{tenantASubnet, tenantBSubnet} {
			Eventually(func() bool {
				return devicePublished(ctx, subnet)
			}, defaultTimeout, pollInterval).Should(BeTrue(), "device of subnet %s", subnet)
		}
	})

	It("allocates a claim through a tenant class from that tenant's subnet, by VPC and by subnet list", func() {
		By("tenant A's class selects its VPC")
		expectTenantNIC(ctx, tenantAPod, tenantAClass, tenantASubnet)
		By("tenant B's class lists its subnet")
		expectTenantNIC(ctx, tenantBPod, tenantBClass, tenantBSubnet)
	})
})
