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
	"os"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Live migration of KubeVirt VMs with a DRA NIC. A claim may be shared by the
// virt-launcher pods of one VM, so a VM with a named claim migrates without a
// second address, also while its pool is full. A VM with a claim template gets
// a second claim for the migration's target pod, which needs a free address.
//
// The specs need KubeVirt with the kube-ovn-nic binding as a KubeVirt Plugin
// (deployments/kubevirt), the driver in central publish mode with
// kubeletPlugin.annotateVMIMAC, and two schedulable nodes
// (`E2E_KUBEVIRT=1 make setup-e2e`). The DataVolume spec also needs CDI and a
// ReadWriteMany storage class (`E2E_KUBEVIRT=1 E2E_RWX=1 make setup-e2e`).

const (
	labelKubevirt   = "kubevirt"
	labelRWX        = "rwx"
	msgSkipKubevirt = "set E2E_KUBEVIRT=1 (and run `E2E_KUBEVIRT=1 make setup-e2e`) to exercise live migration"
	msgSkipRWX      = "set E2E_RWX=1 (and run `E2E_KUBEVIRT=1 E2E_RWX=1 make setup-e2e`) to migrate a VM with a DataVolume on RWX storage"

	// envRWXStorageClass names the ReadWriteMany storage class of the
	// DataVolume spec; the default is the NFS 4.1 class of setup-e2e.sh.
	envRWXStorageClass     = "E2E_RWX_STORAGECLASS"
	defaultRWXStorageClass = "nfs4"

	migrationGroup = "migration"
	protocolIPv4   = "IPv4"
	protocolIPv6   = "IPv6"
	protocolDual   = "Dual"
	migrationLabel = "e2e=" + migrationGroup
	migrationKinds = "vmim,vm"

	// migrationSubnet leaves 4 addresses free (.201 to .204): the peer pod, the
	// two VMs and a filler pod that keeps the pool full.
	migrationSubnet     = "migration-e2e"
	migrationCIDR       = "10.250.9.0/24"
	migrationGateway    = "10.250.9.1"
	migrationExcludeIPs = `["10.250.9.1..10.250.9.200", "10.250.9.205..10.250.9.254"]`
	// rwxSubnet leaves 2 addresses free: the peer pod and the VM.
	rwxSubnet     = "migration-rwx-e2e"
	rwxCIDR       = "10.250.10.0/24"
	rwxGateway    = "10.250.10.1"
	rwxExcludeIPs = `["10.250.10.1..10.250.10.200", "10.250.10.203..10.250.10.254"]`

	namedVM    = "mig-named"
	multiNICVM = "mig-multi-nic"
	// multiNICTemplateVM is the VM with an overlay and a VLAN NIC from claim
	// templates, whose migration target pod gets new claims.
	multiNICTemplateVM = "mig-multi-nic-tmpl"
	overlayPeer        = "mig-overlay-peer"
	underlayPeer       = "mig-underlay-peer"
	// vlanNetwork is the VMI network of the VLAN NIC of the two-NIC VM.
	vlanNetwork = "vlan100"
	templateVM  = "mig-template"
	rwxVM       = "mig-rwx"
	// rwxTemplateVM has its DRA NIC from a claim template, on rwxTemplateSubnet,
	// which has free addresses for the migration target's new claim.
	rwxTemplateVM      = "mig-rwx-tmpl"
	rwxTemplatePeer    = "mig-rwx-tmpl-peer"
	rwxTemplateSubnet  = "migration-rwx-tmpl-e2e"
	rwxTemplateCIDR    = "10.250.18.0/24"
	rwxTemplateGateway = "10.250.18.1"
	nicTemplate        = "mig-nic"
	peerPod            = "mig-peer"
	rwxPeerPod         = "mig-rwx-peer"
	fillerPod          = "mig-filler"
	foreignPod         = "mig-foreign"
	blockedSuffix      = "-blocked"
	claimSuffix        = "-nic"

	// vmNetwork is the VMI network and interface of the DRA NIC. The Plugin's
	// hook uses the interface name as the tap name, so the claim config names
	// the pod interface the same.
	vmNetwork = "kubeovn0"
	// sidecarPrefix starts the names of KubeVirt's hook sidecar containers.
	sidecarPrefix = "hook-sidecar"
	vmImage       = "quay.io/containerdisks/ubuntu:22.04"
	vmDiskSize    = "5Gi"

	launcherVMLabel     = "vm.kubevirt.io/name"
	msgSharedClaim      = "can only be shared by"
	phaseSucceeded      = "Succeeded"
	phaseFailed         = "Failed"
	phasePending        = "Pending"
	reasonUnschedulable = "Unschedulable"
	fieldVMIName        = "vmiName"
	fieldNodeName       = "nodeName"
	fieldPhase          = "phase"
	fieldIPv4           = "v4IpAddress"
	fieldMAC            = "macAddress"
	flagTimeout         = "--timeout=5m"

	runStrategyHalted = "Halted"
	runStrategyAlways = "Always"

	vmTimeout        = 10 * time.Minute
	importTimeout    = 20 * time.Minute
	migrationTimeout = 6 * time.Minute
)

// migrationProvider is the provider of migrationSubnet.
var migrationProvider = ovnProvider(migrationSubnet)

var (
	vmiGVR  = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachineinstances"}
	vmimGVR = schema.GroupVersionResource{Group: vmiGVR.Group, Version: vmiGVR.Version, Resource: "virtualmachineinstancemigrations"}
)

// ovnProvider is the provider of an overlay Subnet the specs create.
func ovnProvider(subnet string) string {
	return subnet + "." + namespace + ".ovn"
}

// claimSpec is the spec of a claim for one NIC on the subnet, named iface in
// the pod.
func claimSpec(subnet, iface string) string {
	return fmt.Sprintf(`devices:
  requests:
    - name: nic0
      exactly:
        deviceClassName: %[1]s
        selectors:
          - cel:
              expression: "device.attributes['%[1]s'].subnetName == '%[2]s'"
  config:
    - requests: ["nic0"]
      opaque:
        driver: %[1]s
        parameters:
          apiVersion: nic.resource.kube-ovn.io/v1alpha1
          kind: NicConfig
          interfaceName: %[3]s
`, driverName, subnet, iface)
}

func indent(s string, spaces int) string {
	pad := strings.Repeat(" ", spaces)
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return pad + strings.Join(lines, "\n"+pad) + "\n"
}

// subnetProtocol is the kube-ovn protocol of a CIDR block: IPv4, IPv6 or, for
// one CIDR of each family, Dual.
func subnetProtocol(cidr string) string {
	switch {
	case strings.Contains(cidr, ","):
		return protocolDual
	case strings.Contains(cidr, ":"):
		return protocolIPv6
	}
	return protocolIPv4
}

// subnetManifest is an overlay Subnet labelled e2e=<group>, with the extra
// spec fields, e.g. "enableDHCP: true".
func subnetManifest(group, name, cidr, gateway, excludeIPs string, extra ...string) string {
	var fields strings.Builder
	for _, field := range extra {
		fields.WriteString("  " + field + "\n")
	}
	return fmt.Sprintf(`apiVersion: kubeovn.io/v1
kind: Subnet
metadata:
  name: %s
  labels: {e2e: %s}
spec:
  protocol: %s
  cidrBlock: %s
  gateway: %s
  excludeIps: %s
  provider: %s
%s`, name, group, subnetProtocol(cidr), cidr, gateway, excludeIPs, ovnProvider(name), fields.String())
}

// claimManifest is a claim for one NIC on the subnet, labelled e2e=<group>.
func claimManifest(group, name, subnet, iface string) string {
	return fmt.Sprintf(`---
apiVersion: resource.k8s.io/v1
kind: ResourceClaim
metadata:
  name: %s
  namespace: %s
  labels: {e2e: %s}
spec:
%s`, name, namespace, group, indent(claimSpec(subnet, iface), 2))
}

func claimTemplateManifest(name, subnet, iface string) string {
	return fmt.Sprintf(`---
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: %s
  namespace: %s
  labels: {e2e: migration}
spec:
  spec:
%s`, name, namespace, indent(claimSpec(subnet, iface), 4))
}

// claimPodManifest is a pod with the NIC of the named claim as net1, labelled
// e2e=<group>.
func claimPodManifest(group, name, claim string) string {
	return fmt.Sprintf(`---
apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
  labels: {e2e: %s}
spec:
  resourceClaims:
    - name: nic
      resourceClaimName: %s
  containers:
    - name: demo
      image: %s
      command: ["sleep", "infinity"]
      resources:
        claims:
          - name: nic
      securityContext:
        capabilities:
          add: ["NET_ADMIN"]
`, name, namespace, group, claim, netshootImage)
}

// vmNICSpec is a DRA NIC of a VM: the VMI network and interface, the provider
// of its subnet and the named claim or, with template set, the claim template
// it comes from.
type vmNICSpec struct {
	network  string
	provider string
	claim    string
	template bool
	// dhcp6 lets the guest also take an IPv6 address by DHCPv6 and router
	// advertisements, which OVN answers on a Subnet with enableDHCP and
	// enableIPv6RA.
	dhcp6 bool
}

// vmManifest is a VM with the DRA NICs. With storageClass set the root disk is
// a DataVolume on that ReadWriteMany class, else a containerDisk. The guest
// takes every NIC by DHCP, with the default route only on the pod network.
func vmManifest(name string, nics []vmNICSpec, storageClass string) string {
	dataVolumes := ""
	rootDisk := "containerDisk: {image: " + vmImage + "}"
	if storageClass != "" {
		dataVolumes = fmt.Sprintf(`  dataVolumeTemplates:
    - metadata: {name: %[1]s-rootdisk}
      spec:
        source: {registry: {url: "docker://%[2]s"}}
        storage:
          storageClassName: %[3]s
          accessModes: [ReadWriteMany]
          resources: {requests: {storage: %[4]s}}
`, name, vmImage, storageClass, vmDiskSize)
		rootDisk = "dataVolume: {name: " + name + "-rootdisk}"
	}
	var annotations, claims, networks, interfaces, ethernets strings.Builder
	for i, nic := range nics {
		claimSource := "resourceClaimName: " + nic.claim
		if nic.template {
			claimSource = "resourceClaimTemplateName: " + nic.claim
		}
		fmt.Fprintf(&annotations, "        %s.kubernetes.io/default_route: \"false\"\n", nic.provider)
		fmt.Fprintf(&claims, "        - name: nic%d\n          %s\n", i, claimSource)
		fmt.Fprintf(&networks, "        - name: %s\n          resourceClaim: {claimName: nic%d, requestName: nic0}\n", nic.network, i)
		fmt.Fprintf(&interfaces, "            - name: %s\n              binding: {name: kube-ovn-nic}\n", nic.network)
		// the pod network is enp1s0, the DRA NICs follow in the order of the interfaces
		fmt.Fprintf(&ethernets, "                enp%ds0:\n                  dhcp4: true\n                  dhcp4-overrides: {use-routes: false}\n", i+2)
		if nic.dhcp6 {
			// networkd needs the same use-routes override for both families
			ethernets.WriteString("                  dhcp6: true\n                  dhcp6-overrides: {use-routes: false}\n                  accept-ra: true\n")
		}
	}
	return fmt.Sprintf(`---
apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata:
  name: %[1]s
  namespace: %[2]s
  labels: {e2e: migration}
spec:
  runStrategy: Always
%[3]s  template:
    metadata:
      annotations:
        kubevirt.io/allow-pod-bridge-network-live-migration: "true"
%[4]s    spec:
      resourceClaims:
%[5]s      networks:
        - name: default
          pod: {}
%[6]s      domain:
        devices:
          interfaces:
            - name: default
              bridge: {}
%[7]s          disks:
            - name: rootdisk
              disk: {bus: virtio}
            - name: cloudinitdisk
              disk: {bus: virtio}
        resources:
          requests: {memory: 1Gi}
      terminationGracePeriodSeconds: 30
      volumes:
        - name: rootdisk
          %[8]s
        - name: cloudinitdisk
          cloudInitNoCloud:
            networkData: |
              version: 2
              ethernets:
                enp1s0: {dhcp4: true}
%[9]s`, name, namespace, dataVolumes, annotations.String(), claims.String(), networks.String(), interfaces.String(), rootDisk, ethernets.String())
}

func applyManifest(manifest string) {
	out, err := kubectlStdin(manifest, kubectlApply, flagFile, "-")
	Expect(err).NotTo(HaveOccurred(), msgKubectlApply, out)
}

// deleteMigrationFixtures removes what the migration specs created: the
// migrations and VMs first, then, once their launcher pods are gone and have
// released the claims, the pods, claims and subnets.
func deleteMigrationFixtures(ctx context.Context) {
	deleteLabelled := func(kinds string) {
		out, err := kubectl(kubectlDelete, kinds, flagSelector, migrationLabel, flagIgnoreNotFound, flagWait, flagTimeout)
		Expect(err).NotTo(HaveOccurred(), msgKubectlDelete, out)
	}
	deleteLabelled(migrationKinds)
	for _, vm := range []string{namedVM, templateVM, rwxVM, rwxTemplateVM, multiNICVM, multiNICTemplateVM, dualVM, ovnDHCPVM, ovnDHCPDualVM} {
		Eventually(func() int {
			return launcherPods(ctx, vm)
		}, defaultTimeout, pollInterval).Should(BeZero(), "launcher pods of %s", vm)
	}
	deleteLabelled("pod,resourceclaim,resourceclaimtemplate")
	deleteLabelled("subnet")
}

func vmiField(ctx context.Context, vm, field string) string {
	obj, err := dynClient.Resource(vmiGVR).Namespace(namespace).Get(ctx, vm, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	value, _, _ := unstructured.NestedString(obj.Object, fieldStatus, field)
	return value
}

func waitVMIRunning(ctx context.Context, vm string, timeout time.Duration) {
	Eventually(func() string {
		return vmiField(ctx, vm, fieldPhase)
	}, timeout, pollInterval).Should(Equal(phaseRunning), "vmi %s", vm)
}

// vmNIC returns the address and MAC kube-ovn allocated for the DRA NIC of the
// VM on the provider. The ip CR is named after the VM, so it outlives the
// launcher pods.
func vmNIC(ctx context.Context, vm, provider string) string {
	obj, err := dynClient.Resource(ipGVR).Get(ctx, fmt.Sprintf("%s.%s.%s", vm, namespace, provider), metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred(), "ip CR of vm %s", vm)
	ip, _, _ := unstructured.NestedString(obj.Object, fieldSpec, fieldIPv4)
	mac, _, _ := unstructured.NestedString(obj.Object, fieldSpec, fieldMAC)
	Expect(ip).NotTo(BeEmpty())
	Expect(mac).NotTo(BeEmpty())
	return ip + " " + mac
}

func expectReachable(peer, nic string) {
	ip, _, _ := strings.Cut(nic, " ")
	Eventually(func() error {
		_, err := pingFrom(peer, ifaceNet1, ip)
		return err
	}, vmTimeout, pollInterval).Should(Succeed(), "%s should reach %s", peer, ip)
}

func expectPoolFull(ctx context.Context, subnet string) {
	Eventually(func() int64 {
		free, _ := subnetFree(ctx, subnet)
		return free
	}, defaultTimeout, pollInterval).Should(BeZero(), "subnet %s should have no free address", subnet)
}

// startMigration creates a migration of the VMI and returns its name.
func startMigration(ctx context.Context, vm, name string) string {
	migration := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": vmimGVR.GroupVersion().String(),
		"kind":       "VirtualMachineInstanceMigration",
		"metadata":   map[string]any{"labels": map[string]any{"e2e": "migration"}},
		"spec":       map[string]any{fieldVMIName: vm},
	}}
	if name != "" {
		migration.SetName(name)
	} else {
		migration.SetGenerateName(vm + "-")
	}
	created, err := dynClient.Resource(vmimGVR).Namespace(namespace).Create(ctx, migration, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred(), "migrate vmi %s", vm)
	return created.GetName()
}

// migrate live-migrates the VMI and returns the phase the migration ended in.
func migrate(ctx context.Context, vm string) string {
	name := startMigration(ctx, vm, "")
	var phase string
	Eventually(func() string {
		obj, err := dynClient.Resource(vmimGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return ""
		}
		phase, _, _ = unstructured.NestedString(obj.Object, fieldStatus, fieldPhase)
		return phase
	}, migrationTimeout, pollInterval).Should(BeElementOf(phaseSucceeded, phaseFailed), "migration %s", name)
	return phase
}

// nicPeer is a DRA NIC of a VM, by the provider of its subnet, and the pod
// that reaches it on that subnet.
type nicPeer struct {
	provider string
	peer     string
}

// expectMigrations migrates the VM twice, there and back, and checks that each
// of its NICs keeps address and MAC and stays reachable from its peer pod.
func expectMigrations(ctx context.Context, vm string, nics ...nicPeer) {
	before := make([]string, len(nics))
	for i, n := range nics {
		before[i] = vmNIC(ctx, vm, n.provider)
		expectReachable(n.peer, before[i])
	}
	for range 2 {
		node := vmiField(ctx, vm, fieldNodeName)
		Expect(migrate(ctx, vm)).To(Equal(phaseSucceeded), "migration of %s from %s", vm, node)
		Eventually(func() string {
			return vmiField(ctx, vm, fieldNodeName)
		}, defaultTimeout, pollInterval).ShouldNot(Equal(node), "vmi %s should run on another node", vm)
		for i, n := range nics {
			Expect(vmNIC(ctx, vm, n.provider)).To(Equal(before[i]), "vm %s should keep IP and MAC on %s", vm, n.provider)
			expectReachable(n.peer, before[i])
		}
	}
}

// expectStopStart stops and starts the VM and checks that each of its NICs
// keeps address and MAC while it is stopped and after the start, and is
// reachable from its peer pod again.
func expectStopStart(ctx context.Context, vm string, nics ...nicPeer) {
	before := make([]string, len(nics))
	for i, n := range nics {
		before[i] = vmNIC(ctx, vm, n.provider)
		expectReachable(n.peer, before[i])
	}
	setRunStrategy := func(strategy string) {
		out, err := kubectl("patch", "vm", vm, "--type", "merge", "-p", `{"spec":{"runStrategy":"`+strategy+`"}}`)
		Expect(err).NotTo(HaveOccurred(), "patch vm %s: %s", vm, out)
	}
	setRunStrategy(runStrategyHalted)
	Eventually(func() int {
		return launcherPods(ctx, vm)
	}, defaultTimeout, pollInterval).Should(BeZero(), "launcher pods of the stopped vm %s", vm)
	for i, n := range nics {
		Expect(vmNIC(ctx, vm, n.provider)).To(Equal(before[i]), "the stopped vm %s should keep IP and MAC on %s", vm, n.provider)
	}
	setRunStrategy(runStrategyAlways)
	waitVMIRunning(ctx, vm, vmTimeout)
	for i, n := range nics {
		Expect(vmNIC(ctx, vm, n.provider)).To(Equal(before[i]), "vm %s should keep IP and MAC on %s after the start", vm, n.provider)
		expectReachable(n.peer, before[i])
	}
}

// unschedulableTarget reports whether a launcher pod of the VM waits with
// PodScheduled=False, reason Unschedulable: the migration's target pod.
func unschedulableTarget(ctx context.Context, vm string) bool {
	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: launcherVMLabel + "=" + vm})
	Expect(err).NotTo(HaveOccurred())
	for _, pod := range pods.Items {
		if pod.Status.Phase != phasePending {
			continue
		}
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse && cond.Reason == reasonUnschedulable {
				return true
			}
		}
	}
	return false
}

// expectNoSidecar checks that the launcher pods of the VM run without a hook
// sidecar: the Plugin, not a binding sidecar, adds the NIC to the domain.
func expectNoSidecar(ctx context.Context, vm string) {
	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: launcherVMLabel + "=" + vm})
	Expect(err).NotTo(HaveOccurred())
	Expect(pods.Items).NotTo(BeEmpty(), "launcher pods of %s", vm)
	for _, pod := range pods.Items {
		for _, container := range pod.Spec.Containers {
			Expect(container.Name).NotTo(HavePrefix(sidecarPrefix), "pod %s", pod.Name)
		}
	}
}

func launcherPods(ctx context.Context, vm string) int {
	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: launcherVMLabel + "=" + vm})
	Expect(err).NotTo(HaveOccurred())
	return len(pods.Items)
}

func podEvents(ctx context.Context, pod string) string {
	events, err := clientset.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{FieldSelector: "involvedObject.name=" + pod})
	Expect(err).NotTo(HaveOccurred())
	var messages []string
	for _, event := range events.Items {
		messages = append(messages, event.Message)
	}
	return strings.Join(messages, "\n")
}

var _ = Describe("Live migration of KubeVirt VMs with a DRA NIC", Ordered, Label(labelKubevirt), func() {
	ctx := context.Background()

	BeforeAll(func() {
		if !kubevirtEnabled() {
			Skip(msgSkipKubevirt)
		}
		if !publisherRunning(ctx) {
			Skip(msgSkipNodeMode)
		}
		deleteMigrationFixtures(ctx)
		DeferCleanup(deleteMigrationFixtures, ctx)

		By("creating a subnet with 4 free addresses, a peer pod and two VMs, one with a named claim, one with a claim template")
		applyManifest(subnetManifest(migrationGroup, migrationSubnet, migrationCIDR, migrationGateway, migrationExcludeIPs) +
			claimManifest(migrationGroup, peerPod, migrationSubnet, ifaceNet1) + claimPodManifest(migrationGroup, peerPod, peerPod) +
			claimManifest(migrationGroup, namedVM+claimSuffix, migrationSubnet, vmNetwork) +
			claimTemplateManifest(nicTemplate, migrationSubnet, vmNetwork) +
			vmManifest(namedVM, []vmNICSpec{{network: vmNetwork, provider: migrationProvider, claim: namedVM + claimSuffix}}, "") +
			vmManifest(templateVM, []vmNICSpec{{network: vmNetwork, provider: migrationProvider, claim: nicTemplate, template: true}}, ""))
		waitRunning(peerPod)
		waitVMIRunning(ctx, namedVM, vmTimeout)
		waitVMIRunning(ctx, templateVM, vmTimeout)
		expectNoSidecar(ctx, namedVM)

		By("filling the pool with a fourth pod")
		applyManifest(claimManifest(migrationGroup, fillerPod, migrationSubnet, ifaceNet1) + claimPodManifest(migrationGroup, fillerPod, fillerPod))
		waitRunning(fillerPod)
		expectPoolFull(ctx, migrationSubnet)
	})

	It("keeps IP and MAC of a VM with a named claim across live migrations while the pool is full", func() {
		expectPoolFull(ctx, migrationSubnet)
		expectMigrations(ctx, namedVM, nicPeer{migrationProvider, peerPod})
	})

	It("keeps the migration target of a VM with a claim template pending while the pool is full", func() {
		expectPoolFull(ctx, migrationSubnet)
		node, nic := vmiField(ctx, templateVM, fieldNodeName), vmNIC(ctx, templateVM, migrationProvider)
		name := startMigration(ctx, templateVM, templateVM+blockedSuffix)
		Eventually(func() bool {
			return unschedulableTarget(ctx, templateVM)
		}, defaultTimeout, pollInterval).Should(BeTrue(), "the target pod of %s should wait for a free address", templateVM)

		out, err := kubectl(kubectlDelete, "vmim", name, flagWait)
		Expect(err).NotTo(HaveOccurred(), msgKubectlDelete, out)
		Eventually(func() int {
			return launcherPods(ctx, templateVM)
		}, defaultTimeout, pollInterval).Should(Equal(1), "the target pod of %s should be gone", templateVM)
		Expect(vmiField(ctx, templateVM, fieldNodeName)).To(Equal(node))
		Expect(vmNIC(ctx, templateVM, migrationProvider)).To(Equal(nic))
	})

	It("migrates a VM with a claim template once the pool has a free address", func() {
		out, err := kubectl(kubectlDelete, resourcePod, fillerPod, flagWait)
		Expect(err).NotTo(HaveOccurred(), msgKubectlDelete, out)
		expectMigrations(ctx, templateVM, nicPeer{migrationProvider, peerPod})
	})

	It("refuses the named claim of a VM to a pod outside the VM", func() {
		nic := vmNIC(ctx, namedVM, migrationProvider)
		applyManifest(claimPodManifest(migrationGroup, foreignPod, namedVM+claimSuffix))
		Eventually(func() string {
			return podEvents(ctx, foreignPod)
		}, defaultTimeout, pollInterval).Should(ContainSubstring(msgSharedClaim))
		pod, err := clientset.CoreV1().Pods(namespace).Get(ctx, foreignPod, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(pod.Status.Phase).NotTo(Equal(corev1.PodPhase(phaseRunning)))

		out, err := kubectl(kubectlDelete, resourcePod, foreignPod, flagWait)
		Expect(err).NotTo(HaveOccurred(), msgKubectlDelete, out)
		Expect(vmNIC(ctx, namedVM, migrationProvider)).To(Equal(nic))
		expectReachable(peerPod, nic)
	})
	// A VM with a claim template gets a new claim for every start; kube-ovn
	// gives it the VM's address again (keep-vm-ip).
	It("keeps IP and MAC of a VM with a claim template across stop and start", func() {
		expectStopStart(ctx, templateVM, nicPeer{migrationProvider, peerPod})
	})
})

var _ = Describe("Live migration of a KubeVirt VM with an overlay and a VLAN DRA NIC", Ordered, Label(labelKubevirt, labelContainerlab), func() {
	ctx := context.Background()
	var overlayProvider, underlayProvider string

	BeforeAll(func() {
		if !kubevirtEnabled() {
			Skip(msgSkipKubevirt)
		}
		if !containerlabEnabled() {
			Skip(msgSkipContainerlab)
		}
		deleteMigrationFixtures(ctx)
		DeferCleanup(deleteMigrationFixtures, ctx)
		overlayProvider, underlayProvider = subnetProvider(ctx, overlaySubnet), subnetProvider(ctx, underlaySubnet)

		By("creating a peer pod on " + overlaySubnet + " and on " + underlaySubnet + " and two VMs with a NIC on each, one from named claims, one from claim templates")
		applyManifest(claimManifest(migrationGroup, overlayPeer, overlaySubnet, ifaceNet1) + claimPodManifest(migrationGroup, overlayPeer, overlayPeer) +
			claimManifest(migrationGroup, underlayPeer, underlaySubnet, ifaceNet1) + claimPodManifest(migrationGroup, underlayPeer, underlayPeer) +
			claimManifest(migrationGroup, multiNICVM+"-"+vmNetwork, overlaySubnet, vmNetwork) +
			claimManifest(migrationGroup, multiNICVM+"-"+vlanNetwork, underlaySubnet, vlanNetwork) +
			vmManifest(multiNICVM, []vmNICSpec{
				{network: vmNetwork, provider: overlayProvider, claim: multiNICVM + "-" + vmNetwork},
				{network: vlanNetwork, provider: underlayProvider, claim: multiNICVM + "-" + vlanNetwork},
			}, "") +
			claimTemplateManifest(multiNICTemplateVM+"-"+vmNetwork, overlaySubnet, vmNetwork) +
			claimTemplateManifest(multiNICTemplateVM+"-"+vlanNetwork, underlaySubnet, vlanNetwork) +
			vmManifest(multiNICTemplateVM, []vmNICSpec{
				{network: vmNetwork, provider: overlayProvider, claim: multiNICTemplateVM + "-" + vmNetwork, template: true},
				{network: vlanNetwork, provider: underlayProvider, claim: multiNICTemplateVM + "-" + vlanNetwork, template: true},
			}, ""))
		waitRunning(overlayPeer, underlayPeer)
		waitVMIRunning(ctx, multiNICVM, vmTimeout)
		waitVMIRunning(ctx, multiNICTemplateVM, vmTimeout)
		expectNoSidecar(ctx, multiNICVM)
	})

	// In node mode a claim is allocated from the pool of one node, so the
	// target pod on the other node cannot share it.
	It("keeps IP and MAC of both NICs across live migrations", func() {
		if !publisherRunning(ctx) {
			Skip(msgSkipNodeBoundClaim)
		}
		expectMigrations(ctx, multiNICVM, nicPeer{overlayProvider, overlayPeer}, nicPeer{underlayProvider, underlayPeer})
	})

	// Both claims are allocated anew at the start, close to each other: a NIC
	// whose claim was allocated while kube-ovn built the pod's networks used to
	// lose its address.
	It("keeps IP and MAC of both NICs across stop and start", func() {
		expectStopStart(ctx, multiNICVM, nicPeer{overlayProvider, overlayPeer}, nicPeer{underlayProvider, underlayPeer})
	})

	// The way KubeVirt supports from 1.10.0 on: the target pod gets new claims
	// from the templates, which the pools have room for, and kube-ovn gives them
	// the VM's addresses again.
	It("keeps IP and MAC of both NICs of a VM with claim templates across live migrations", func() {
		expectMigrations(ctx, multiNICTemplateVM, nicPeer{overlayProvider, overlayPeer}, nicPeer{underlayProvider, underlayPeer})
	})
})

var _ = Describe("Live migration of a KubeVirt VM with a DRA NIC and a DataVolume on RWX storage", Ordered, Label(labelKubevirt, labelRWX), func() {
	ctx := context.Background()

	BeforeAll(func() {
		if !kubevirtEnabled() {
			Skip(msgSkipKubevirt)
		}
		if !rwxEnabled() {
			Skip(msgSkipRWX)
		}
		if !publisherRunning(ctx) {
			Skip(msgSkipNodeMode)
		}
		storageClass := os.Getenv(envRWXStorageClass)
		if storageClass == "" {
			storageClass = defaultRWXStorageClass
		}
		deleteMigrationFixtures(ctx)
		DeferCleanup(deleteMigrationFixtures, ctx)

		By("creating a subnet with 2 free addresses, a peer pod and a VM with a named claim and a DataVolume on " + storageClass)
		applyManifest(subnetManifest(migrationGroup, rwxSubnet, rwxCIDR, rwxGateway, rwxExcludeIPs) +
			claimManifest(migrationGroup, rwxPeerPod, rwxSubnet, ifaceNet1) + claimPodManifest(migrationGroup, rwxPeerPod, rwxPeerPod) +
			claimManifest(migrationGroup, rwxVM+claimSuffix, rwxSubnet, vmNetwork) +
			vmManifest(rwxVM, []vmNICSpec{{network: vmNetwork, provider: ovnProvider(rwxSubnet), claim: rwxVM + claimSuffix}}, storageClass))
		waitRunning(rwxPeerPod)
		waitVMIRunning(ctx, rwxVM, importTimeout)
	})

	It("keeps IP and MAC of the VM across live migrations while the pool is full", func() {
		expectPoolFull(ctx, rwxSubnet)
		expectMigrations(ctx, rwxVM, nicPeer{ovnProvider(rwxSubnet), rwxPeerPod})
	})
})

var _ = Describe("Live migration of a KubeVirt VM with a DRA NIC from a claim template and a DataVolume on RWX storage", Ordered, Label(labelKubevirt, labelRWX), func() {
	ctx := context.Background()
	provider := ovnProvider(rwxTemplateSubnet)

	BeforeAll(func() {
		if !kubevirtEnabled() {
			Skip(msgSkipKubevirt)
		}
		if !rwxEnabled() {
			Skip(msgSkipRWX)
		}
		storageClass := os.Getenv(envRWXStorageClass)
		if storageClass == "" {
			storageClass = defaultRWXStorageClass
		}
		deleteMigrationFixtures(ctx)
		DeferCleanup(deleteMigrationFixtures, ctx)

		By("creating a subnet with free addresses, a peer pod and a VM with a claim template and a DataVolume on " + storageClass)
		applyManifest(subnetManifest(migrationGroup, rwxTemplateSubnet, rwxTemplateCIDR, rwxTemplateGateway, noExclude) +
			claimManifest(migrationGroup, rwxTemplatePeer, rwxTemplateSubnet, ifaceNet1) + claimPodManifest(migrationGroup, rwxTemplatePeer, rwxTemplatePeer) +
			claimTemplateManifest(rwxTemplateVM+claimSuffix, rwxTemplateSubnet, vmNetwork) +
			vmManifest(rwxTemplateVM, []vmNICSpec{{network: vmNetwork, provider: provider, claim: rwxTemplateVM + claimSuffix, template: true}}, storageClass))
		waitRunning(rwxTemplatePeer)
		waitVMIRunning(ctx, rwxTemplateVM, importTimeout)
	})

	It("keeps IP and MAC of the VM across live migrations", func() {
		expectMigrations(ctx, rwxTemplateVM, nicPeer{provider, rwxTemplatePeer})
	})
})
