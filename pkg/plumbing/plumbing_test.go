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

import (
	"testing"
)

func TestSpecPortName(t *testing.T) {
	tests := []struct {
		name     string
		spec     Spec
		expected string
	}{
		{"default ovn provider", Spec{PodName: "p", PodNamespace: "ns", Provider: "ovn"}, "p.ns"},
		{"empty provider treated as ovn", Spec{PodName: "p", PodNamespace: "ns", Provider: ""}, "p.ns"},
		{"explicit provider", Spec{PodName: "p", PodNamespace: "ns", Provider: "external.vlan100-subnet.ovn"}, "p.ns.external.vlan100-subnet.ovn"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.spec.PortName(); got != tt.expected {
				t.Errorf("PortName() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestSpecValidate(t *testing.T) {
	base := func() Spec {
		return Spec{
			PodName:      "p",
			PodNamespace: "ns",
			NetnsPath:    "/proc/1/ns/net",
			IfaceName:    "net1",
			IP:           "172.23.0.5/24",
			ContainerID:  "abcdef0123456789",
		}
	}
	ovn := func() Spec { s := base(); s.Type = SubnetTypeOVN; s.IfaceID = "p.ns"; return s }
	vlan := func() Spec { s := base(); s.Type = SubnetTypeVLAN; s.Provider = "external"; s.VlanID = 100; return s }

	tests := []struct {
		name    string
		spec    Spec
		wantErr bool
	}{
		{"valid ovn", ovn(), false},
		{"valid vlan", vlan(), false},
		{"missing netns", func() Spec { s := ovn(); s.NetnsPath = ""; return s }(), true},
		{"missing iface", func() Spec { s := ovn(); s.IfaceName = ""; return s }(), true},
		{"missing ip", func() Spec { s := ovn(); s.IP = ""; return s }(), true},
		{"missing containerID", func() Spec { s := ovn(); s.ContainerID = ""; return s }(), true},
		{"ovn missing iface-id", func() Spec { s := ovn(); s.IfaceID = ""; return s }(), true},
		{"vlan missing provider", func() Spec { s := vlan(); s.Provider = ""; return s }(), true},
		{"vlan missing vlanID", func() Spec { s := vlan(); s.VlanID = 0; return s }(), true},
		{"unknown type", func() Spec { s := base(); s.Type = "bogus"; return s }(), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.spec.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestVethNames(t *testing.T) {
	a := &ovsAttacher{hostVethPrefix: "dra"}
	host, pod, err := a.vethNames(Spec{ContainerID: "abcdef0123456789", IfaceName: "net1"})
	if err != nil {
		t.Fatalf("vethNames() unexpected error: %v", err)
	}
	if host != "abcdef01_net1_h" {
		t.Errorf("host = %q, want abcdef01_net1_h", host)
	}
	if pod != "abcdef01_net1_c" {
		t.Errorf("pod = %q, want abcdef01_net1_c", pod)
	}
	// Kernel interface names must fit IFNAMSIZ (15 chars).
	if len(host) > 15 || len(pod) > 15 {
		t.Errorf("veth name exceeds 15 chars: host=%q (%d) pod=%q (%d)", host, len(host), pod, len(pod))
	}
}

// TestVethNamesTooLong guards the fix for a real crash: an IfaceName long
// enough to underflow containerID[:12-len(iface)] used to panic with
// "slice bounds out of range [:-2]" instead of returning an error (hit live
// against a kube-ovn-network-binding-plugin demo using a 14-char IfaceName).
func TestVethNamesTooLong(t *testing.T) {
	a := &ovsAttacher{hostVethPrefix: "dra"}
	_, _, err := a.vethNames(Spec{ContainerID: "abcdef0123456789", IfaceName: "poddda1950dc07"})
	if err == nil {
		t.Fatal("vethNames() with a 14-char IfaceName: want error, got nil")
	}
}

func TestPendingStore(t *testing.T) {
	s := NewPendingStore()
	s.Add("uid-1", Spec{IfaceName: "net1"})
	s.Add("uid-1", Spec{IfaceName: "net2"})
	s.Add("uid-2", Spec{IfaceName: "net1"})

	if got := s.Peek("uid-1"); len(got) != 2 {
		t.Fatalf("Peek(uid-1) len = %d, want 2", len(got))
	}
	taken := s.Take("uid-1")
	if len(taken) != 2 {
		t.Fatalf("Take(uid-1) len = %d, want 2", len(taken))
	}
	if got := s.Take("uid-1"); len(got) != 0 {
		t.Errorf("Take(uid-1) after drain len = %d, want 0", len(got))
	}
	if got := s.Peek("uid-2"); len(got) != 1 {
		t.Errorf("Peek(uid-2) len = %d, want 1", len(got))
	}
}

func TestProviderBridge(t *testing.T) {
	if got := providerBridge("external"); got != "br-external" {
		t.Errorf("providerBridge(external) = %q, want br-external", got)
	}
}
