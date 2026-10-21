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

// Package e2e contains end-to-end tests for the kube-ovn NIC DRA driver.
//
// The suite assumes a running cluster with the driver and kube-ovn (with
// --enable-dra-nic) already deployed — bring one up with `make setup-e2e`, which
// drives everything through the Makefile's kind-*/kube-ovn targets so all
// cluster/kube-ovn Helm values stay sourced from a single place. Run with
// `make test-e2e`; tear down with `make teardown-e2e`.
//
// Specs are labelled:
//   - default specs run on a plain kind cluster (no containerlab).
//   - specs labelled "containerlab" exercise the VLAN underlay datapath and need
//     `make clab-deploy` (sudo + containerlab); they are skipped unless
//     E2E_CONTAINERLAB=1.
//   - specs labelled "kubevirt" live-migrate KubeVirt VMs and need KubeVirt with
//     the kube-ovn-nic binding and two workers (`E2E_KUBEVIRT=1 make
//     setup-e2e`); they are skipped unless E2E_KUBEVIRT=1. The spec also
//     labelled "rwx" needs CDI and a ReadWriteMany storage class (E2E_RWX=1),
//     the one also labelled "containerlab" the VLAN underlay as well.
//   - specs labelled "dual" need a dual-stack cluster (`E2E_IP_FAMILY=dual make
//     setup-e2e`); they are skipped unless E2E_IP_FAMILY=dual.
//
// The driver runs in central publish mode unless the cluster is set up with
// E2E_PUBLISH_MODE=node. The specs of one mode find out which mode runs and skip
// in the other.
package e2e

import (
	"os"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/soer3n/kube-ovn-dra-driver/internal/publisher"
)

// Values shared by the tests of this package.
const (
	labelContainerlab     = "containerlab"
	msgSkipContainerlab   = "set E2E_CONTAINERLAB=1 (and run `make clab-deploy`) to exercise the VLAN underlay"
	msgKubectlDelete      = "kubectl delete: %s"
	msgSkipNodeMode       = "the driver runs in node mode; set it up with E2E_PUBLISH_MODE=central to run these specs"
	msgSkipCentralMode    = "the driver runs in central mode; set it up with E2E_PUBLISH_MODE=node to run these specs"
	msgSkipNodeBoundClaim = "the driver runs in node mode, where a named claim is bound to the pool of one node and cannot follow a VM to another node"
	phaseRunning          = "Running"
)

const (
	// driverName must match the DeviceClass / ResourceSlice driver the chart
	// registers (Helm driverName default; see deployments/helm/...).
	driverName = "nic.kubeovn.io"
	namespace  = "default"

	defaultTimeout = 4 * time.Minute
	pollInterval   = 5 * time.Second
)

// kubectl verbs and flags, and the names the specs share.
const (
	kubectlApply       = "apply"
	kubectlBinary      = "kubectl"
	kubectlDelete      = "delete"
	kubectlGet         = "get"
	kubectlExec        = "exec"
	flagFile           = "-f"
	flagIgnoreNotFound = "--ignore-not-found"
	flagWait           = "--wait=true"
	flagSelector       = "-l"
	flagOutput         = "-o"
	flagNamespace      = "-n"
	argsEnd            = "--"
	flagContainer      = "-c"
	flagPingCount      = "-c"
	resourcePod        = "pod"
	msgKubectlApply    = "kubectl apply: %s"
	msgOvsVsctl        = "ovs-vsctl: %s"
	podsAndClaims      = "pod,resourceclaim"

	kubeSystemNS  = "kube-system"
	ifaceNet1     = "net1"
	ifaceNet2     = "net2"
	fieldSpec     = "spec"
	fieldStatus   = "status"
	fieldVlan     = "vlan"
	fieldProvider = "provider"
	noPacketLoss  = " 0% packet loss"
)

var (
	ipGVR     = publisher.IPGVR
	subnetGVR = publisher.SubnetGVR
)

var (
	clientset kubernetes.Interface
	dynClient dynamic.Interface
)

// containerlabEnabled reports whether the VLAN-underlay (containerlab) specs
// should run. They need the host-side clab topology, so they are opt-in.
func containerlabEnabled() bool {
	return os.Getenv("E2E_CONTAINERLAB") == "1"
}

// kubevirtEnabled reports whether the live migration specs should run. They
// need KubeVirt with the kube-ovn-nic binding and two workers, so they are
// opt-in.
func kubevirtEnabled() bool {
	return os.Getenv("E2E_KUBEVIRT") == "1"
}

// dualEnabled reports whether the cluster is dual-stack, which the dual-stack
// and IPv6-only specs need.
func dualEnabled() bool {
	return os.Getenv("E2E_IP_FAMILY") == "dual"
}

// rwxEnabled reports whether the DataVolume migration spec should run. It also
// needs CDI and a ReadWriteMany storage class.
func rwxEnabled() bool {
	return os.Getenv("E2E_RWX") == "1"
}

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "kube-ovn NIC DRA e2e suite")
}

var _ = BeforeSuite(func() {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		kubeconfig = clientcmd.RecommendedHomeFile
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	Expect(err).NotTo(HaveOccurred(), "load kubeconfig %q", kubeconfig)

	clientset, err = kubernetes.NewForConfig(cfg)
	Expect(err).NotTo(HaveOccurred())
	dynClient, err = dynamic.NewForConfig(cfg)
	Expect(err).NotTo(HaveOccurred())
})
