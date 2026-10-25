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

package main

import (
	"context"
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

// vmiMACAnnotationPrefix keys the MAC of a VM's DRA NIC on its VMI, followed
// by the NIC's interface name, which is also the VMI interface name and the tap
// name. A KubeVirt Plugin CEL hook reads it to add the NIC to the domain.
const vmiMACAnnotationPrefix = "nic.kubeovn.io/mac."

var vmiGVR = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachineinstances"}

// vmiAnnotator merges annotations into a VMI.
type vmiAnnotator func(ctx context.Context, namespace, name string, annotations map[string]string) error

// dynamicVMIAnnotator merges the annotations into the VMI with a merge patch.
func dynamicVMIAnnotator(client dynamic.Interface) vmiAnnotator {
	return func(ctx context.Context, namespace, name string, annotations map[string]string) error {
		patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": annotations}})
		if err != nil {
			return err
		}
		_, err = client.Resource(vmiGVR).Namespace(namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
		return err
	}
}
