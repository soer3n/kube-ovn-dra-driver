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

// Values shared by the tests of this package.
const (
	testPodIPv6WithMask = "fd00::5/120"
	testHostVethPrefix  = "vh"
	testPodIPWithMask   = "10.10.0.5/24"
	testPodDualIPs      = "10.10.0.5,fd00::5"
	testPodMAC          = "00:00:00:aa:bb:cc"
	testContainerID16   = "0123456789abcdef"
	testVMPort          = "vm1.default.blue.default.ovn"
	testSandbox         = "abcdef0123456789"
	testSandboxNet1     = "abcdef0123456789/net1"
	testHashedIface     = "nicdda1950d"
	testIface1          = "net1"
	testIface2          = "net2"
	testGateway         = "10.10.0.1"
	testRouteGateway    = "10.10.0.254"
	testRouteDst        = "10.0.0.0/8"
	testPodRouteDst     = "192.168.0.0/16"
	testOwnGwDst        = "10.20.0.0/16"
	testDefaultRoute    = "0.0.0.0/0"
	testIPv6RouteDst    = "fd00::/64"
	testIPv6Gateway     = "fd01::1"
	testClaimUID        = "claim"
	testPodUID          = "uid-1"
	testNetnsPath       = "/var/run/netns/x"
	testNS              = "default"
	testContainerID     = "cid"
	testLauncherPod     = "virt-launcher-vm1"
	testClaimA          = "claim-a"
	testClaimB          = "claim-b"
	testPodUID2         = "uid-2"
	testIP              = "10.0.0.5/24"
	testReplacedValue   = "again"
)

func TestSpecValidate(t *testing.T) {
	base := func() Spec {
		return Spec{
			PodName:      "p",
			PodNamespace: "ns",
			NetnsPath:    "/proc/1/ns/net",
			IfaceName:    testIface1,
			IP:           "172.23.0.5/24",
			ContainerID:  testSandbox,
			IfaceID:      "p.ns.blue.ns.ovn",
		}
	}

	tests := []struct {
		name    string
		spec    Spec
		wantErr bool
	}{
		{"valid", base(), false},
		{"missing netns", func() Spec { s := base(); s.NetnsPath = ""; return s }(), true},
		{"missing iface", func() Spec { s := base(); s.IfaceName = ""; return s }(), true},
		{"missing ip", func() Spec { s := base(); s.IP = ""; return s }(), true},
		{"missing containerID", func() Spec { s := base(); s.ContainerID = ""; return s }(), true},
		{"missing iface-id", func() Spec { s := base(); s.IfaceID = ""; return s }(), true},
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
	a := &ovsAttacher{}
	host, pod, err := a.vethNames(Spec{ContainerID: testSandbox, IfaceName: testIface1})
	if err != nil {
		t.Fatalf("vethNames() unexpected error: %v", err)
	}
	wantHost := shortHashName(testHostVethPrefix, testSandboxNet1)
	wantPod := shortHashName("vc", testSandboxNet1)
	if host != wantHost {
		t.Errorf("host = %q, want %q", host, wantHost)
	}
	if pod != wantPod {
		t.Errorf("pod = %q, want %q", pod, wantPod)
	}
	if host == pod {
		t.Errorf("host and pod names must differ, both = %q", host)
	}
	// Kernel interface names must fit IFNAMSIZ (15 chars).
	if len(host) > 15 || len(pod) > 15 {
		t.Errorf("veth name exceeds 15 chars: host=%q (%d) pod=%q (%d)", host, len(host), pod, len(pod))
	}
	// Deterministic: same input must reproduce the same names (Attach's own
	// idempotency and Detach both depend on this).
	host2, pod2, err := a.vethNames(Spec{ContainerID: testSandbox, IfaceName: testIface1})
	if err != nil || host2 != host || pod2 != pod {
		t.Errorf("vethNames() not deterministic: got (%q, %q, %v), want (%q, %q, nil)", host2, pod2, err, host, pod)
	}
}

// TestVethNamesLongIfaceName guards two historical bugs at once. First, an
// IfaceName long enough to underflow containerID[:12-len(iface)] used to
// panic with "slice bounds out of range [:-2]" instead of returning an error
// (hit live against a kube-ovn-network-binding-plugin demo using a 14-char
// IfaceName). Second, and more subtly, even a non-underflowing but long
// IfaceName (this driver's CRD-supplied names like "nicdda1950d" run 11
// chars) left only 12-len(iface) characters of the containerID to
// distinguish names, so two unrelated sandboxes whose container IDs merely
// shared a leading hex digit collided on identical veth names — reproduced
// live as "look up pod veth ...: Link not found" during a kube-ovn-dra-vmi
// redeploy. The hash-based scheme has no such underflow or entropy collapse:
// it must succeed here, produce a fixed-length name, and differ from a
// same-length-class iface name's names computed from a different key.
func TestVethNamesLongIfaceName(t *testing.T) {
	a := &ovsAttacher{}
	host, pod, err := a.vethNames(Spec{ContainerID: testSandbox, IfaceName: "poddda1950dc07"})
	if err != nil {
		t.Fatalf("vethNames() with a 14-char IfaceName: unexpected error: %v", err)
	}
	if len(host) > 15 || len(pod) > 15 {
		t.Errorf("veth name exceeds 15 chars: host=%q (%d) pod=%q (%d)", host, len(host), pod, len(pod))
	}

	// Different container IDs sharing a leading hex digit must NOT collide
	// (the actual bug: the old scheme derived names from only 1 truncated
	// character of the containerID here, since 12-len("nicdda1950d")=1).
	hostA, podA, err := a.vethNames(Spec{ContainerID: "cabc000000000000", IfaceName: testHashedIface})
	if err != nil {
		t.Fatalf("vethNames() unexpected error: %v", err)
	}
	hostB, podB, err := a.vethNames(Spec{ContainerID: "cdef000000000000", IfaceName: testHashedIface})
	if err != nil {
		t.Fatalf("vethNames() unexpected error: %v", err)
	}
	if hostA == hostB || podA == podB {
		t.Errorf("different container IDs sharing a leading hex digit collided: A=(%q,%q) B=(%q,%q)", hostA, podA, hostB, podB)
	}
}

func TestVethNamesEmptyContainerID(t *testing.T) {
	a := &ovsAttacher{}
	if _, _, err := a.vethNames(Spec{ContainerID: "", IfaceName: testIface1}); err == nil {
		t.Fatal("vethNames() with empty ContainerID: want error, got nil")
	}
}

func TestNICStore(t *testing.T) {
	s := NewNICStore()
	s.Add(Spec{PodUID: testPodUID, ClaimUID: testClaimA, IfaceName: testIface1})
	s.Add(Spec{PodUID: testPodUID, ClaimUID: testClaimB, IfaceName: testIface2})
	s.Add(Spec{PodUID: testPodUID2, ClaimUID: "claim-c", IfaceName: testIface1})
	s.Add(Spec{PodUID: testPodUID, ClaimUID: testClaimA, IfaceName: testIface1, IP: testIP})

	specs := s.Specs(testPodUID)
	if len(specs) != 2 || specs[0].IP != testIP || specs[1].IfaceName != testIface2 {
		t.Fatalf("Specs(uid-1) = %+v, want net1 (replaced) and net2", specs)
	}
	specs[0].NetnsPath = "/changed"
	if s.Specs(testPodUID)[0].NetnsPath != "" {
		t.Error("Specs must return a copy")
	}
	if got := s.Specs(testPodUID); len(got) != 2 {
		t.Errorf("Specs must not drain the store, got %d", len(got))
	}

	s.RemoveClaim(testClaimA)
	if got := s.Specs(testPodUID); len(got) != 1 || got[0].ClaimUID != testClaimB {
		t.Errorf("after RemoveClaim(claim-a) Specs(uid-1) = %+v, want claim-b only", got)
	}
	s.RemoveClaim(testClaimB)
	if got := s.Specs(testPodUID); got != nil {
		t.Errorf("after removing all claims Specs(uid-1) = %+v, want nil", got)
	}
	if got := s.Specs(testPodUID2); len(got) != 1 {
		t.Errorf("Specs(uid-2) len = %d, want 1", len(got))
	}

	s.MarkAttached(testPodUID, Spec{IfaceName: testIface1})
	s.MarkAttached(testPodUID, Spec{IfaceName: testIface1, ContainerID: testReplacedValue})
	s.MarkAttached(testPodUID, Spec{IfaceName: testIface2})
	if got := s.TakeAttached(testPodUID); len(got) != 2 || got[0].ContainerID != testReplacedValue {
		t.Errorf("TakeAttached(uid-1) = %+v, want net1 (replaced) and net2", got)
	}
	if got := s.TakeAttached(testPodUID); len(got) != 0 {
		t.Errorf("TakeAttached(uid-1) after drain len = %d, want 0", len(got))
	}
}

func TestDeviceNames(t *testing.T) {
	for _, name := range []string{bridgeNameFor(testIface1), vethRenameFor(testIface1), shortHashName(testHostVethPrefix, "a/very/long/key/that/exceeds/ifnamsiz")} {
		if len(name) > 15 {
			t.Errorf("%q exceeds IFNAMSIZ", name)
		}
	}
	if bridgeNameFor(testIface1) == bridgeNameFor(testIface2) || bridgeNameFor(testIface1) == vethRenameFor(testIface1) {
		t.Error("bridge and veth rename names must be distinct per interface")
	}
	if name := bridgeNameFor(testIface1); name != shortHashName("kvb", testIface1) {
		t.Errorf("bridgeNameFor(net1) = %q, want the deterministic hash name", name)
	}
}

func TestRouteParse(t *testing.T) {
	tests := []struct {
		name           string
		route          Route
		defaultGateway string
		wantGateway    string
		wantErr        bool
	}{
		{"default gateway", Route{Destination: testRouteDst}, testGateway, testGateway, false},
		{"own gateway", Route{Destination: testRouteDst, Gateway: testRouteGateway}, testGateway, testRouteGateway, false},
		{"on-link without gateway", Route{Destination: testRouteDst}, "", "", false},
		{"ipv6 route on-link next to an ipv4 gateway", Route{Destination: testIPv6RouteDst}, testGateway, "", false},
		{"ipv6 route via its gateway", Route{Destination: testIPv6RouteDst, Gateway: testIPv6Gateway}, testGateway, testIPv6Gateway, false},
		{"gateway of another family", Route{Destination: testRouteDst, Gateway: testIPv6Gateway}, "", "", true},
		{"default route", Route{Destination: testDefaultRoute}, testGateway, "", true},
		{"invalid gateway", Route{Destination: testRouteDst, Gateway: "x"}, "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, gw, err := tt.route.parse(tt.defaultGateway)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parse() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			got := ""
			if gw != nil {
				got = gw.String()
			}
			if got != tt.wantGateway {
				t.Errorf("parse() gateway = %q, want %q", got, tt.wantGateway)
			}
		})
	}
}
