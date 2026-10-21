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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NICs survive the restarts the driver has to handle: its own kubelet plugin
// restarting while pods use NICs, the pod sandbox being recreated, after which
// the NRI hook attaches the NIC again with the address of the claim, and the
// container runtime restarting, which drops the NRI connection.

const (
	restartGroup = "restart"
	restartLabel = "e2e=" + restartGroup
	// driverInstance selects the pods of the driver's Helm release.
	driverInstance  = "app.kubernetes.io/instance=kube-ovn-nic-dra"
	driverDaemonSet = "ds"
	restartPodA     = "restart-a"
	restartPodB     = "restart-b"
	sandboxPod      = "restart-sandbox"
	nriPodA         = "restart-nri-a"
	nriPodB         = "restart-nri-b"
	nriPodRepair    = "restart-nri-repair"
	nriOffPod       = "restart-nri-off"
	msgNRIRefused   = "NRI plugin is not connected"
	// containerdConfig is the config of containerd in a kind node; a copy is
	// kept while NRI is disabled in it.
	containerdConfig       = "/etc/containerd/config.toml"
	containerdConfigBackup = containerdConfig + ".e2e"
	// nriRecoveryTimeout leaves room for kubelet's backoff before it retries the
	// refused prepare.
	nriRecoveryTimeout = 2 * defaultTimeout
	kubeletPlugin      = "app.kubernetes.io/component=kubeletplugin"
	msgNRIDown         = "NRI plugin disconnected"
	hostnameLabel      = "kubernetes.io/hostname"
	// containerTool runs crictl in the kind node of a pod.
	containerTool = "docker"
	crictl        = "crictl"
	// crictlTimeout replaces crictl's default of 2 s: stopping a sandbox tears
	// down its networks and can take longer on a busy node.
	crictlTimeout = "--timeout=60s"
	rolloutStatus = "--timeout=5m"
)

func deleteRestartPods() {
	out, err := kubectl(kubectlDelete, podsAndClaims, flagSelector, restartLabel, flagIgnoreNotFound, flagWait, flagTimeout)
	Expect(err).NotTo(HaveOccurred(), msgKubectlDelete, out)
}

// startNICPod starts a pod with one NIC on the overlay subnet, on the node if
// one is given, and returns its address.
func startNICPod(ctx context.Context, pod, node string) string {
	podManifest := claimPodManifest(restartGroup, pod, pod)
	if node != "" {
		// a node selector, not nodeName: the scheduler has to allocate the claim
		podManifest = strings.Replace(podManifest, "\nspec:\n", "\nspec:\n  nodeSelector: {"+hostnameLabel+": "+node+"}\n", 1)
	}
	applyManifest(claimManifest(restartGroup, pod, overlaySubnet, ifaceNet1) + podManifest)
	waitRunning(pod)
	return expectNIC(ctx, pod, ifaceNet1, subnetProvider(ctx, overlaySubnet))
}

func podNode(ctx context.Context, pod string) string {
	p, err := clientset.CoreV1().Pods(namespace).Get(ctx, pod, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	return p.Spec.NodeName
}

// driverPod returns the kubelet plugin pod of the driver on the node.
func driverPod(ctx context.Context, node string) corev1.Pod {
	pods, err := clientset.CoreV1().Pods(kubeSystemNS).List(ctx, metav1.ListOptions{
		LabelSelector: driverInstance + "," + kubeletPlugin, FieldSelector: "spec.nodeName=" + node,
	})
	Expect(err).NotTo(HaveOccurred())
	Expect(pods.Items).To(HaveLen(1), "kubelet plugin pods on %s", node)
	return pods.Items[0]
}

// nodeShell runs the script in the kind node.
func nodeShell(node, script string) {
	out, err := exec.Command(containerTool, kubectlExec, node, "sh", "-c", script).CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "%s on %s: %s", script, node, out)
}

// disableNRI turns NRI off in containerd on the node and restarts containerd.
func disableNRI(node string) {
	nodeShell(node, "cp "+containerdConfig+" "+containerdConfigBackup+
		` && printf '\n%s\n%s\n' "[plugins.'io.containerd.nri.v1.nri']" '  disable = true' >> `+containerdConfig+
		" && systemctl restart containerd")
}

// enableNRI restores containerd's config on the node if NRI was disabled.
func enableNRI(node string) {
	nodeShell(node, "if [ -f "+containerdConfigBackup+" ]; then mv "+containerdConfigBackup+" "+containerdConfig+
		" && systemctl restart containerd; fi")
}

// workerNode returns a node without the control-plane role.
func workerNode(ctx context.Context) string {
	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: "!node-role.kubernetes.io/control-plane"})
	Expect(err).NotTo(HaveOccurred())
	Expect(nodes.Items).NotTo(BeEmpty(), "worker nodes")
	return nodes.Items[0].Name
}

func driverReady(ctx context.Context, node string) func() bool {
	return func() bool {
		return podReady(driverPod(ctx, node))
	}
}

func podReady(pod corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

// expectAddress checks that net1 of the pod carries the address.
func expectAddress(pod, ip string) {
	Eventually(func() ([]string, error) {
		l, err := linkShow(pod, ifaceNet1)
		return l.ips(), err
	}, defaultTimeout, pollInterval).Should(ContainElement(ip), "pod %s net1", pod)
}

// readySandbox returns the ready sandbox of the pod on its kind node.
func readySandbox(node, pod string) (string, error) {
	out, err := exec.Command(containerTool, kubectlExec, node, crictl, crictlTimeout, "pods", "--name", pod, "--namespace", namespace, "--state", "ready", "-q").Output()
	return strings.TrimSpace(string(out)), err
}

var _ = Describe("NICs across restarts", Ordered, func() {
	ctx := context.Background()

	BeforeAll(func() {
		deleteRestartPods()
		DeferCleanup(deleteRestartPods)
	})

	It("keeps the NIC of a running pod while the driver restarts and plugs NICs of new pods afterwards", func() {
		ip := startNICPod(ctx, restartPodA, "")

		By("restarting the kubelet plugins of the driver")
		out, err := kubectl(flagNamespace, kubeSystemNS, "rollout", "restart", driverDaemonSet, flagSelector, driverInstance)
		Expect(err).NotTo(HaveOccurred(), "rollout restart: %s", out)
		out, err = kubectl(flagNamespace, kubeSystemNS, "rollout", "status", driverDaemonSet, flagSelector, driverInstance, rolloutStatus)
		Expect(err).NotTo(HaveOccurred(), "rollout status: %s", out)

		By("the running pod keeping its NIC and a new pod getting one")
		expectAddress(restartPodA, ip)
		startNICPod(ctx, restartPodB, "")
	})

	It("attaches the NIC again with the same address when the pod sandbox is recreated", func() {
		ip := startNICPod(ctx, sandboxPod, "")
		node := podNode(ctx, sandboxPod)
		sandbox, err := readySandbox(node, sandboxPod)
		Expect(err).NotTo(HaveOccurred())
		Expect(sandbox).NotTo(BeEmpty(), "ready sandbox of %s on %s", sandboxPod, node)

		By("stopping the sandbox " + sandbox + " on " + node)
		out, err := exec.Command(containerTool, kubectlExec, node, crictl, crictlTimeout, "stopp", sandbox).CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "crictl stopp: %s", out)
		Eventually(func() (string, error) {
			return readySandbox(node, sandboxPod)
		}, defaultTimeout, pollInterval).ShouldNot(Or(BeEmpty(), Equal(sandbox)), "kubelet should recreate the sandbox")

		By("the new sandbox carrying the NIC with the address of the claim")
		waitRunning(sandboxPod)
		expectAddress(sandboxPod, ip)
	})

	It("reconnects to NRI when the container runtime restarts, keeping running NICs, restoring a removed one and plugging new ones", func() {
		ip := startNICPod(ctx, nriPodA, "")
		node := podNode(ctx, nriPodA)
		plugin := driverPod(ctx, node).Name

		By("removing net1 of a second pod on " + node + ", which Synchronize attaches again on the reconnect")
		repairIP := startNICPod(ctx, nriPodRepair, node)
		out, err := kubectl(kubectlExec, nriPodRepair, argsEnd, "ip", "link", "del", ifaceNet1)
		Expect(err).NotTo(HaveOccurred(), "ip link del: %s", out)
		_, err = linkShow(nriPodRepair, ifaceNet1)
		Expect(err).To(HaveOccurred(), "net1 of %s should be gone", nriPodRepair)

		By("restarting containerd on " + node)
		restarted := time.Now().UTC().Add(-time.Second)
		restartOut, err := exec.Command(containerTool, kubectlExec, node, "systemctl", "restart", "containerd").CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "restart containerd: %s", restartOut)
		Eventually(func() (string, error) {
			return kubectl(flagNamespace, kubeSystemNS, "logs", plugin, "--all-containers", "--since-time="+restarted.Format(time.RFC3339))
		}, defaultTimeout, pollInterval).Should(ContainSubstring(msgNRIDown), "the driver on %s should notice the NRI connection drop", node)

		By("the driver reconnecting: ready again, the running pod keeping its NIC, the removed NIC back and a new pod on the node getting one")
		Eventually(driverReady(ctx, node), defaultTimeout, pollInterval).Should(BeTrue(), "kubelet plugin on %s", node)
		expectAddress(nriPodA, ip)
		expectAddress(nriPodRepair, repairIP)
		startNICPod(ctx, nriPodB, node)
	})

	It("refuses to prepare a NIC while NRI is disabled and plugs it once NRI is back", func() {
		node := workerNode(ctx)
		DeferCleanup(enableNRI, node)

		By("disabling NRI in containerd on " + node)
		disableNRI(node)
		Eventually(driverReady(ctx, node), defaultTimeout, pollInterval).Should(BeFalse(), "the kubelet plugin on %s should not be ready without NRI", node)

		By("a pod with a NIC on " + node + " waiting: the driver refuses the prepare instead of starting it without the NIC")
		podManifest := strings.Replace(claimPodManifest(restartGroup, nriOffPod, nriOffPod), "\nspec:\n", "\nspec:\n  nodeSelector: {"+hostnameLabel+": "+node+"}\n", 1)
		applyManifest(claimManifest(restartGroup, nriOffPod, overlaySubnet, ifaceNet1) + podManifest)
		Eventually(func() string {
			return podEvents(ctx, nriOffPod)
		}, defaultTimeout, pollInterval).Should(ContainSubstring(msgNRIRefused))
		pod, err := clientset.CoreV1().Pods(namespace).Get(ctx, nriOffPod, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		Expect(pod.Status.Phase).To(Equal(corev1.PodPending))

		By("enabling NRI again: the driver gets ready and kubelet's retry prepares the claim")
		enableNRI(node)
		Eventually(driverReady(ctx, node), defaultTimeout, pollInterval).Should(BeTrue(), "kubelet plugin on %s", node)
		Eventually(func() (string, error) {
			return kubectl(kubectlGet, resourcePod, nriOffPod, flagOutput, "jsonpath={.status.phase}")
		}, nriRecoveryTimeout, pollInterval).Should(Equal(phaseRunning), "pod %s", nriOffPod)
		expectNIC(ctx, nriOffPod, ifaceNet1, subnetProvider(ctx, overlaySubnet))
	})
})
