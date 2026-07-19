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

package plumbing

import "context"

// SandboxHandler is the bridge between NRI pod-sandbox events and the Attacher.
// It is deliberately decoupled from the NRI API types so this package does not
// (yet) depend on github.com/containerd/nri — the real NRI plugin in
// cmd/kube-ovn-dra-kubeletplugin will adapt NRI's *api.PodSandbox into these
// plain calls.
//
// Wiring (to be added in cmd/kube-ovn-dra-kubeletplugin):
//
//	import "github.com/containerd/nri/pkg/stub"
//
//	type nriPlugin struct{ h *plumbing.SandboxHandler }
//
//	func (p *nriPlugin) RunPodSandbox(ctx, sb *api.PodSandbox) error {
//	    return p.h.OnRunPodSandbox(ctx, sb.Uid, sb.Id, netnsPathOf(sb), sb.GetLabels())
//	}
//	func (p *nriPlugin) StopPodSandbox(ctx, sb *api.PodSandbox) error {
//	    return p.h.OnStopPodSandbox(ctx, string(sb.Uid))
//	}
//
// netnsPathOf extracts the network-namespace path from sb.Linux.Namespaces
// (Type == "network").
//
// NRI is enabled by default in the containerd 2.x shipped with the kind v1.35
// node image, so no runtime patching is required (see demo/kind/kind-no-cni.yaml).
type SandboxHandler struct {
	store    *PendingStore
	attacher Attacher
}

// NewSandboxHandler wires a PendingStore to an Attacher.
func NewSandboxHandler(store *PendingStore, attacher Attacher) *SandboxHandler {
	return &SandboxHandler{store: store, attacher: attacher}
}

// kubevirtVirtLauncherLabel is the well-known label KubeVirt sets on every
// virt-launcher pod (verified against a live v1.9.0-rc.0 cluster:
// metadata.labels["kubevirt.io"] == "virt-launcher"). Its presence is how
// OnRunPodSandbox decides whether Attach must wrap the veth in a bridge+tap
// (see Spec.KubeVirtVMI) instead of handing the veth to the pod directly.
const kubevirtVirtLauncherLabel = "virt-launcher"

// OnRunPodSandbox drains the pending NIC Specs for podUID, fills in the
// now-known netns path, and attaches each. Called from the NRI RunPodSandbox
// hook. labels are the pod's labels from the NRI event, used only to detect a
// KubeVirt virt-launcher pod. Returns the first attach error; the caller
// decides whether to fail the sandbox (recommended — a half-networked pod is
// worse than a failed one).
//
// TODO(plumbing): on partial failure, Detach the NICs already attached so the
// pod doesn't come up with some interfaces missing.
func (h *SandboxHandler) OnRunPodSandbox(ctx context.Context, podUID, containerID, netnsPath string, labels map[string]string) error {
	specs := h.store.Take(podUID)
	isVMI := labels[kubevirtLabelKey] == kubevirtVirtLauncherLabel
	for i := range specs {
		// Fill in the fields only known once the sandbox exists.
		specs[i].ContainerID = containerID
		specs[i].NetnsPath = netnsPath
		specs[i].KubeVirtVMI = isVMI
		if err := h.attacher.Attach(ctx, specs[i]); err != nil {
			return err
		}
	}
	return nil
}

// kubevirtLabelKey is the label key checked against kubevirtVirtLauncherLabel.
const kubevirtLabelKey = "kubevirt.io"

// OnStopPodSandbox detaches every NIC for the pod. Called from the NRI
// StopPodSandbox hook. IPAM release stays in UnprepareResourceClaims; this only
// tears down the datapath.
//
// TODO(plumbing): source the Specs to detach from the checkpoint (see the note
// on PendingStore) since Take() already emptied the store at attach time.
func (h *SandboxHandler) OnStopPodSandbox(ctx context.Context, podUID string) error {
	var firstErr error
	for _, spec := range h.store.Peek(podUID) {
		if err := h.attacher.Detach(ctx, spec); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
