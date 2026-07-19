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

// Package annotation implements helpers to request and wait for kube-ovn IPAM
// allocations via pod annotations.
//
// kube-ovn keys every per-network annotation by the network's PROVIDER, using
// the template "<provider>.kubernetes.io/<field>" (see kube-ovn
// pkg/util/const.go *AnnotationTemplate and pkg/controller/pod.go, which writes
// them with fmt.Sprintf(template, subnet.Spec.Provider)). The provider is the
// kube-ovn Subnet's spec.provider, e.g. "external.vlan100-subnet.ovn"; the
// default OVN network uses the sentinel provider "ovn".
//
// There is NO "_<ifaceName>" suffix — kube-ovn distinguishes NICs by provider,
// not by interface name. A pod may therefore hold one allocation per provider.
//
// Workflow:
//  1. Write  <provider>.kubernetes.io/logical_switch = <subnetName>   (request)
//  2. kube-ovn-controller reconciles and writes back, keyed by <provider>:
//     ip_address, mac_address, cidr, gateway, and allocated="true".
//  3. The driver waits for allocated="true", then reads the result.
package annotation

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	coreclientset "k8s.io/client-go/kubernetes"
)

const (
	// kube-ovn per-provider annotation templates ("%s" = provider). Mirrors
	// kube-ovn pkg/util/const.go *AnnotationTemplate constants.
	tmplLogicalSwitch = "%s.kubernetes.io/logical_switch"
	tmplIPAddress     = "%s.kubernetes.io/ip_address"
	tmplMACAddress    = "%s.kubernetes.io/mac_address"
	tmplCIDR          = "%s.kubernetes.io/cidr"
	tmplGateway       = "%s.kubernetes.io/gateway"
	// allocated="true" is the readiness signal kube-ovn-controller sets once
	// IPAM is done for the provider.
	tmplAllocated = "%s.kubernetes.io/allocated"

	// waitTimeout is the maximum time we wait for kube-ovn to respond.
	waitTimeout  = 30 * time.Second
	waitInterval = 500 * time.Millisecond
)

// AllocatedNIC holds the IPAM result written back by kube-ovn-controller.
type AllocatedNIC struct {
	// IP is the allocated IP address (CIDR notation, e.g. "10.0.1.5/24").
	IP string
	// MAC is the allocated MAC address (e.g. "00:11:22:33:44:55").
	MAC string
	// CIDR is the subnet CIDR block (e.g. "10.0.1.0/24").
	CIDR string
	// Gateway is the gateway for the subnet (e.g. "10.0.1.1").
	Gateway string
	// SubnetName is the kube-ovn Subnet used.
	SubnetName string
}

// Writer writes and removes kube-ovn subnet request annotations on pods.
type Writer struct {
	client coreclientset.Interface
}

// NewWriter creates a new annotation Writer.
func NewWriter(client coreclientset.Interface) *Writer {
	return &Writer{client: client}
}

// RequestSubnet writes the logical_switch annotation that tells kube-ovn to
// allocate an address from subnetName for the given provider on the pod.
func (w *Writer) RequestSubnet(ctx context.Context, namespace, podName, provider, subnetName string) error {
	key := fmt.Sprintf(tmplLogicalSwitch, provider)
	patch := buildAnnotationPatch(key, subnetName)
	raw, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshal annotation patch: %w", err)
	}
	_, err = w.client.CoreV1().Pods(namespace).Patch(ctx, podName, types.MergePatchType, raw, metav1.PatchOptions{})
	return err
}

// ReleaseSubnet removes all kube-ovn annotations for the provider from the pod.
func (w *Writer) ReleaseSubnet(ctx context.Context, namespace, podName, provider string) error {
	keys := []string{
		fmt.Sprintf(tmplLogicalSwitch, provider),
		fmt.Sprintf(tmplIPAddress, provider),
		fmt.Sprintf(tmplMACAddress, provider),
		fmt.Sprintf(tmplCIDR, provider),
		fmt.Sprintf(tmplGateway, provider),
		fmt.Sprintf(tmplAllocated, provider),
	}
	annotations := make(map[string]interface{})
	for _, k := range keys {
		annotations[k] = nil // JSON merge-patch: null removes the key
	}
	patch := map[string]interface{}{"metadata": map[string]interface{}{"annotations": annotations}}
	raw, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshal annotation patch: %w", err)
	}
	_, err = w.client.CoreV1().Pods(namespace).Patch(ctx, podName, types.MergePatchType, raw, metav1.PatchOptions{})
	return err
}

// WaitForAllocation polls the pod until kube-ovn has written back the IPAM
// result annotations for the provider, or until the context deadline is reached.
func WaitForAllocation(ctx context.Context, client coreclientset.Interface, namespace, podName, provider string) (*AllocatedNIC, error) {
	ctx, cancel := context.WithTimeout(ctx, waitTimeout)
	defer cancel()

	var result *AllocatedNIC
	err := wait.PollUntilContextTimeout(ctx, waitInterval, waitTimeout, true, func(ctx context.Context) (bool, error) {
		pod, err := client.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		nic, ok := extractAllocation(pod, provider)
		if !ok {
			return false, nil
		}
		result = nic
		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("waiting for kube-ovn IPAM allocation for provider %q on pod %s/%s: %w", provider, namespace, podName, err)
	}
	return result, nil
}

// extractAllocation reads the kube-ovn response annotations from a pod for one
// provider. It gates on allocated="true" — the signal kube-ovn-controller sets
// when IPAM is complete — then returns the result. Returns (nil, false) until
// then.
func extractAllocation(pod *corev1.Pod, provider string) (*AllocatedNIC, bool) {
	ann := pod.Annotations
	if ann == nil {
		return nil, false
	}
	if ann[fmt.Sprintf(tmplAllocated, provider)] != "true" {
		return nil, false
	}
	ip := ann[fmt.Sprintf(tmplIPAddress, provider)]
	mac := ann[fmt.Sprintf(tmplMACAddress, provider)]
	if ip == "" || mac == "" {
		return nil, false
	}
	return &AllocatedNIC{
		IP:         ip,
		MAC:        mac,
		CIDR:       ann[fmt.Sprintf(tmplCIDR, provider)],
		Gateway:    ann[fmt.Sprintf(tmplGateway, provider)],
		SubnetName: ann[fmt.Sprintf(tmplLogicalSwitch, provider)],
	}, true
}

func buildAnnotationPatch(key, value string) map[string]interface{} {
	return map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]interface{}{
				key: value,
			},
		},
	}
}
