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

package publisher

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/resourceslice"
)

// ReadyLabel is the node label kube-ovn sets to "true" on the nodes where a
// ProviderNetwork is ready, that is where the provider bridge and its uplink
// exist. Nodes excluded from the ProviderNetwork never get it.
func ReadyLabel(providerNetwork string) string {
	return fmt.Sprintf("%s.provider-network.kubernetes.io/ready", providerNetwork)
}

// ReadyNodeSelector selects the nodes where a ProviderNetwork is ready.
func ReadyNodeSelector(providerNetwork string) *corev1.NodeSelector {
	return &corev1.NodeSelector{
		NodeSelectorTerms: []corev1.NodeSelectorTerm{{
			MatchExpressions: []corev1.NodeSelectorRequirement{{
				Key:      ReadyLabel(providerNetwork),
				Operator: corev1.NodeSelectorOpIn,
				Values:   []string{"true"},
			}},
		}},
	}
}

// Pool builds the pool of a Subnet: one slice with the Subnet's device. The
// pool is available on all nodes for an overlay Subnet, and on the nodes of the
// ProviderNetwork for a VLAN underlay Subnet.
func Pool(s Subnet, withCapacity bool) resourceslice.Pool {
	pool := resourceslice.Pool{
		Slices: []resourceslice.Slice{{Devices: []resourceapi.Device{Device(s, withCapacity)}}},
	}
	if s.ProviderNetwork != "" {
		pool.NodeSelector = ReadyNodeSelector(s.ProviderNetwork)
	}
	return pool
}

// Pools builds the pools of all Subnets, keyed by pool name, which is the name
// of the Subnet.
func Pools(subnets []Subnet, withCapacity bool) map[string]resourceslice.Pool {
	pools := make(map[string]resourceslice.Pool, len(subnets))
	for _, s := range subnets {
		pools[s.Name] = Pool(s, withCapacity)
	}
	return pools
}
