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

package nicprepare

import "testing"

// TestWithMask covers combining the bare IP the IP CRD returns with the subnet
// CIDR mask into the CIDR form the plumbing layer needs.
func TestWithMask(t *testing.T) {
	tests := []struct {
		name     string
		ip       string
		cidr     string
		expected string
	}{
		{"bare ip + /24", "172.23.0.5", "172.23.0.0/24", "172.23.0.5/24"},
		{"bare ip + /16", "10.0.1.5", "10.0.0.0/16", "10.0.1.5/16"},
		{"ip already has mask", "172.23.0.5/24", "172.23.0.0/24", "172.23.0.5/24"},
		{"empty ip unchanged", "", "172.23.0.0/24", ""},
		{"unparseable cidr falls back to bare ip", "1.2.3.4", "not-a-cidr", "1.2.3.4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := withMask(tt.ip, tt.cidr); got != tt.expected {
				t.Errorf("withMask(%q,%q) = %q, want %q", tt.ip, tt.cidr, got, tt.expected)
			}
		})
	}
}

func TestValidateSecondaryProvider(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		subnet   string
		wantErr  bool
	}{
		// Default/empty provider names the pod's PRIMARY port — never valid for a secondary NIC.
		{"empty provider rejected", "", "ovn-subnet", true},
		{"default ovn provider rejected", "ovn", "ovn-subnet", true},
		// A dedicated provider (multus convention) is accepted.
		{"dedicated overlay provider ok", "ovn-subnet.default.ovn", "ovn-subnet", false},
		{"vlan provider ok", "external.vlan100-subnet.ovn", "vlan100-subnet", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSecondaryProvider(tt.provider, tt.subnet)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateSecondaryProvider(%q,%q) err=%v, wantErr=%v", tt.provider, tt.subnet, err, tt.wantErr)
			}
		})
	}
}
