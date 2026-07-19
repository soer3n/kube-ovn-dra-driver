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

package kubeovnip

import "testing"

// TestIPName guards the kube-ovn coupling: the IP object name MUST equal
// ovs.PodNameToPortName(pod, ns, provider) or the controller silently ignores it.
func TestIPName(t *testing.T) {
	tests := []struct {
		name      string
		podName   string
		namespace string
		provider  string
		expected  string
	}{
		{"default ovn provider", "mypod", "default", "ovn", "mypod.default"},
		{"empty provider treated as ovn", "mypod", "default", "", "mypod.default"},
		{"explicit underlay provider", "mypod", "default", "external.vlan100-subnet.ovn", "mypod.default.external.vlan100-subnet.ovn"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IPName(tt.podName, tt.namespace, tt.provider); got != tt.expected {
				t.Errorf("IPName(%q,%q,%q) = %q, want %q", tt.podName, tt.namespace, tt.provider, got, tt.expected)
			}
		})
	}
}
