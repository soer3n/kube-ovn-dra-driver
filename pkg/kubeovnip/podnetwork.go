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

// This file sketches a second reservation mode that would integrate with the
// proposed multi-network-api PodNetwork concept (kubernetes-sigs/multi-network-api).
//
// STATUS: IDEA / EXPLORATORY — NOT part of the committed Multus-replacement
// path (that is pkg/kubeovnip IP-CRD reservation + pkg/plumbing). The upstream
// API is still being designed and there are (at least) two competing concrete
// proposals, so the direction is unsettled:
//
//   - PodNetwork (jingjli-goog "api-design-condensed"): a cluster-scoped,
//     immutable CRD with spec.provider + spec.networkRef (GK/name/namespace)
//     pointing at the implementation's own network CR; the driver auto-creates
//     one PodNetwork per network. Group multinetwork.networking.x-k8s.io.
//   - NetworkKind (LionelJouin "network-class-design"): a cluster-scoped CRD
//     whose spec.implementationType is a GroupKind that CLASSIFIES existing
//     impl CRs as pod networks (no per-network object).
//
// Both converge on the DRA contract: the driver advertises attach devices in
// ResourceSlices with standard "multinetwork.networking.k8s.io/" device
// attributes (podNetwork; + podNetworkNamespace, networkKind in the NetworkKind
// variant), and reports attachment in the ResourceClaim device status.
//
// The group/version/schema below are OUR OWN placeholder predating both
// proposals — they match NEITHER and exist only to sketch the flow:
//   - Group:    multinetwork.networking.k8s.io
//   - Version:  v1alpha1
//   - Resource: podnetworks (cluster-scoped)
//
// Do not build on this; revisit once upstream converges on one design.
//
// The placeholder PodNetwork object carries a parameters field with a
// kube-ovn-specific subnet name:
//
//	apiVersion: multinetwork.networking.k8s.io/v1alpha1
//	kind: PodNetwork
//	metadata:
//	  name: blue-network
//	spec:
//	  provider: nic.kubeovn.io
//	  parameters:
//	    subnetName: vlan-subnet
//
// In this mode the DRA driver:
//  1. Receives a device whose ResourceSlice attribute
//     device.attributes["networking.dra.x-k8s.io"].podNetwork names a PodNetwork.
//  2. Looks up that PodNetwork object and extracts spec.parameters.subnetName.
//  3. Delegates to Reserve() with the resolved subnet name.
//
// If upstream ever ships a real API, replace the dynamic client calls (and the
// placeholder schema above) with the actual types.
package kubeovnip

import (
	"context"
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// podNetworkGVR is the GroupVersionResource for the proposed PodNetwork CRD.
// Tracking: kubernetes-sigs/multi-network-api PR#2 / PR#4.
var podNetworkGVR = schema.GroupVersionResource{
	Group:    "multinetwork.networking.k8s.io",
	Version:  "v1alpha1",
	Resource: "podnetworks",
}

// PodNetworkParams is the expected shape of spec.parameters inside a PodNetwork
// object when the kube-ovn DRA driver is the designated provider.
//
// This is driver-defined; the PodNetwork spec intentionally keeps parameters
// opaque (runtime.RawExtension in the upstream draft).
type PodNetworkParams struct {
	// SubnetName is the kube-ovn Subnet CRD name to use for IP allocation.
	SubnetName string `json:"subnetName"`
}

// ReserveForPodNetwork is the multi-network-api-aware variant of Reserve.
//
// It looks up the named PodNetwork CRD object, validates that its provider
// matches nic.kubeovn.io, extracts the kube-ovn subnet name from
// spec.parameters.subnetName, and then delegates to Reserve.
//
// Arguments:
//   - podNetworkName: the value of the networking.dra.x-k8s.io/podNetwork
//     device attribute from the ResourceSlice.
//   - pod: pod identity + provider, forwarded to Reserve (the IP object name is
//     derived from these, per kube-ovn's PodNameToPortName requirement).
//
// Returns the same *Allocation as Reserve.
func ReserveForPodNetwork(
	ctx context.Context,
	dc dynamic.Interface,
	podNetworkName string,
	pod ReserveParams,
) (*Allocation, error) {
	subnetName, err := subnetFromPodNetwork(ctx, dc, podNetworkName)
	if err != nil {
		return nil, err
	}
	pod.SubnetName = subnetName
	return Reserve(ctx, dc, pod)
}

// ReleaseForPodNetwork is a convenience wrapper around Release that matches
// the signature symmetry with ReserveForPodNetwork.
func ReleaseForPodNetwork(
	ctx context.Context,
	dc dynamic.Interface,
	podName, namespace, provider string,
) error {
	return Release(ctx, dc, IPName(podName, namespace, provider))
}

// subnetFromPodNetwork fetches the PodNetwork object and returns the
// spec.parameters.subnetName value.
func subnetFromPodNetwork(ctx context.Context, dc dynamic.Interface, podNetworkName string) (string, error) {
	obj, err := dc.Resource(podNetworkGVR).Get(ctx, podNetworkName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get PodNetwork %q: %w", podNetworkName, err)
	}

	// Validate provider so we don't accidentally process a PodNetwork meant
	// for a different CNI driver.
	provider, _, _ := getNestedString(obj.Object, "spec", "provider")
	if provider != "" && provider != "nic.kubeovn.io" {
		return "", fmt.Errorf("PodNetwork %q has provider %q, expected nic.kubeovn.io", podNetworkName, provider)
	}

	// The upstream draft stores driver-specific config as an opaque blob in
	// spec.parameters (runtime.RawExtension → arbitrary JSON when serialised).
	rawParams, ok, err := getNestedMap(obj.Object, "spec", "parameters")
	if err != nil || !ok {
		return "", fmt.Errorf("PodNetwork %q: spec.parameters not found or malformed", podNetworkName)
	}

	// Re-marshal to a concrete struct for type-safe access.
	b, err := json.Marshal(rawParams)
	if err != nil {
		return "", fmt.Errorf("PodNetwork %q: marshal parameters: %w", podNetworkName, err)
	}
	var params PodNetworkParams
	if err := json.Unmarshal(b, &params); err != nil {
		return "", fmt.Errorf("PodNetwork %q: unmarshal parameters: %w", podNetworkName, err)
	}
	if params.SubnetName == "" {
		return "", fmt.Errorf("PodNetwork %q: spec.parameters.subnetName is empty", podNetworkName)
	}
	return params.SubnetName, nil
}

// getNestedString is a thin wrapper that returns ("", false, nil) instead of
// panicking on wrong types, matching the unstructured helper conventions.
func getNestedString(obj map[string]interface{}, fields ...string) (string, bool, error) {
	val := nestedGet(obj, fields...)
	if val == nil {
		return "", false, nil
	}
	s, ok := val.(string)
	return s, ok, nil
}

func getNestedMap(obj map[string]interface{}, fields ...string) (map[string]interface{}, bool, error) {
	val := nestedGet(obj, fields...)
	if val == nil {
		return nil, false, nil
	}
	m, ok := val.(map[string]interface{})
	return m, ok, nil
}

func nestedGet(obj map[string]interface{}, fields ...string) interface{} {
	cur := interface{}(obj)
	for _, f := range fields {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return nil
		}
		cur = m[f]
	}
	return cur
}
