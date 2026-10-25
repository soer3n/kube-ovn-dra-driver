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

package plumbing

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

const (
	// testOVNDHCPNetns does not exist: for a NIC whose Subnet has OVN's DHCP
	// the attacher must not open it.
	testOVNDHCPNetns = "/var/run/netns/does-not-exist"
	testOVNDHCPIface = "kubeovn0"
)

func TestIPsWithoutMask(t *testing.T) {
	got := strings.Join(ipsWithoutMask([]string{testPodIPWithMask, "", testPodIPv6WithMask}), ",")
	if got != testPodDualIPs {
		t.Errorf("ipsWithoutMask() joined = %q, want %q", got, testPodDualIPs)
	}
}

// newTestNetns locks the test goroutine to its OS thread, moves the thread into
// a fresh network namespace that plays the host, and creates a named namespace
// that plays the pod sandbox. It returns the pod netns path. The datapath tests
// need root and skip otherwise.
func newTestNetns(t *testing.T) string {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root to create network namespaces")
	}
	runtime.LockOSThread()
	orig, err := netns.Get()
	if err != nil {
		t.Fatalf("get current netns: %v", err)
	}
	name := fmt.Sprintf("dra-%s-%d", strings.ToLower(t.Name()), os.Getpid())
	podNS, err := netns.NewNamed(name)
	if err != nil {
		t.Fatalf("create pod netns: %v", err)
	}
	hostNS, err := netns.New()
	if err != nil {
		t.Fatalf("create host netns: %v", err)
	}
	t.Cleanup(func() {
		_ = netns.Set(orig)
		_ = orig.Close()
		_ = hostNS.Close()
		_ = podNS.Close()
		_ = netns.DeleteNamed(name)
		runtime.UnlockOSThread()
	})
	return "/var/run/netns/" + name
}

func podHandle(t *testing.T, path string) *netlink.Handle {
	t.Helper()
	ns, err := netns.GetFromPath(path)
	if err != nil {
		t.Fatalf("open netns %s: %v", path, err)
	}
	defer ns.Close()
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		t.Fatalf("netlink handle for %s: %v", path, err)
	}
	t.Cleanup(h.Close)
	return h
}

// plumbPodIface runs the netns part of Attach (everything except OVS) twice, to
// cover idempotency, and returns the host veth name.
func plumbPodIface(t *testing.T, a *ovsAttacher, spec Spec) string {
	t.Helper()
	ctx := context.Background()
	host, pod, err := a.vethNames(spec)
	if err != nil {
		t.Fatalf("vethNames() error = %v", err)
	}
	for range 2 {
		if err := a.createVethPair(ctx, host, pod, spec.MTU); err != nil {
			t.Fatalf("createVethPair() error = %v", err)
		}
		if err := a.moveIntoNetns(ctx, pod, spec.NetnsPath, spec.IfaceName); err != nil {
			t.Fatalf("moveIntoNetns() error = %v", err)
		}
		if err := a.configurePodIface(ctx, spec); err != nil {
			t.Fatalf("configurePodIface() error = %v", err)
		}
	}
	return host
}

func TestDatapathPodNIC(t *testing.T) {
	spec := Spec{
		NetnsPath:   newTestNetns(t),
		IfaceName:   testIface1,
		IP:          testPodIPWithMask,
		ExtraIPs:    []string{testPodIPv6WithMask},
		MAC:         testPodMAC,
		Gateway:     testGateway,
		Routes:      []Route{{Destination: testPodRouteDst}, {Destination: testOwnGwDst, Gateway: testRouteGateway}},
		MTU:         1400,
		ContainerID: testContainerID16,
		IfaceID:     "pod1.default.blue.default.ovn",
	}
	a := &ovsAttacher{dhcpServers: make(map[dhcpKey]dhcpServeCloser)}
	host := plumbPodIface(t, a, spec)

	h := podHandle(t, spec.NetnsPath)
	link, err := h.LinkByName(testIface1)
	if err != nil {
		t.Fatalf("net1 not in the pod netns: %v", err)
	}
	if got := link.Attrs().HardwareAddr.String(); got != spec.MAC {
		t.Errorf("MAC = %s, want %s", got, spec.MAC)
	}
	if got := link.Attrs().MTU; got != spec.MTU {
		t.Errorf("MTU = %d, want %d", got, spec.MTU)
	}
	if link.Attrs().Flags&net.FlagUp == 0 {
		t.Error("net1 is not up")
	}
	addrs, err := h.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		t.Fatalf("list addresses: %v", err)
	}
	var got []string
	for _, addr := range addrs {
		if addr.IP.IsGlobalUnicast() {
			got = append(got, addr.IPNet.String())
		}
	}
	if strings.Join(got, ",") != "10.10.0.5/24,fd00::5/120" {
		t.Errorf("addresses = %v, want 10.10.0.5/24 and fd00::5/120", got)
	}
	routes, err := h.RouteList(link, netlink.FAMILY_V4)
	if err != nil {
		t.Fatalf("list routes: %v", err)
	}
	var viaGateway, viaOwnGateway, defaultRoute bool
	for _, r := range routes {
		switch {
		case r.Dst == nil:
			defaultRoute = true
		case r.Dst.String() == testPodRouteDst && r.Gw.Equal(net.ParseIP(spec.Gateway)):
			viaGateway = true
		case r.Dst.String() == testOwnGwDst && r.Gw.Equal(net.ParseIP(testRouteGateway)):
			viaOwnGateway = true
		}
	}
	if !viaGateway || !viaOwnGateway || defaultRoute {
		t.Errorf("routes = %v, want 192.168.0.0/16 via %s, 10.20.0.0/16 via 10.10.0.254 and no default route", routes, spec.Gateway)
	}

	if err := a.cleanupHostVeth(context.Background(), host); err != nil {
		t.Fatalf("cleanupHostVeth() error = %v", err)
	}
	if _, err := h.LinkByName(testIface1); err == nil {
		t.Error("net1 still exists after the host veth was deleted")
	}
	if err := a.cleanupHostVeth(context.Background(), host); err != nil {
		t.Errorf("second cleanupHostVeth() error = %v", err)
	}
}

func TestDatapathVMINIC(t *testing.T) {
	spec := Spec{
		NetnsPath:   newTestNetns(t),
		IfaceName:   testIface1,
		IP:          testPodIPWithMask,
		ExtraIPs:    []string{testPodIPv6WithMask},
		MAC:         testPodMAC,
		Gateway:     testGateway,
		ContainerID: testContainerID16,
		IfaceID:     testVMPort,
		KubeVirtVMI: true,
	}
	a := &ovsAttacher{dhcpServers: make(map[dhcpKey]dhcpServeCloser)}
	host := plumbPodIface(t, a, spec)
	ctx := context.Background()
	for range 2 {
		if err := a.wireVMIBridge(ctx, spec); err != nil {
			t.Fatalf("wireVMIBridge() error = %v", err)
		}
	}
	// A repeated attach of the wired NIC, e.g. from NRI Synchronize after a
	// plugin restart, must leave the tap alone.
	h := podHandle(t, spec.NetnsPath)
	tapBefore, err := h.LinkByName(testIface1)
	if err != nil {
		t.Fatalf("tap missing: %v", err)
	}
	plumbPodIface(t, a, spec)
	if err := a.wireVMIBridge(ctx, spec); err != nil {
		t.Fatalf("wireVMIBridge() after a repeated attach error = %v", err)
	}
	tapAfter, err := h.LinkByName(testIface1)
	if err != nil {
		t.Fatalf("tap missing after a repeated attach: %v", err)
	}
	if tapAfter.Attrs().HardwareAddr.String() != tapBefore.Attrs().HardwareAddr.String() {
		t.Errorf("repeated attach changed the tap MAC from %s to %s", tapBefore.Attrs().HardwareAddr, tapAfter.Attrs().HardwareAddr)
	}
	if addrs, _ := h.AddrList(tapAfter, netlink.FAMILY_ALL); len(addrs) != 0 {
		for _, addr := range addrs {
			if addr.IP.IsGlobalUnicast() {
				t.Errorf("repeated attach configured %s on the tap", addr.IPNet)
			}
		}
	}

	tap, err := h.LinkByName(testIface1)
	if err != nil || tap.Type() != "tuntap" {
		t.Fatalf("net1 = %v (%v), want the tap device", tap, err)
	}
	bridge, err := h.LinkByName(bridgeNameFor(testIface1))
	if err != nil {
		t.Fatalf("bridge missing: %v", err)
	}
	veth, err := h.LinkByName(vethRenameFor(testIface1))
	if err != nil {
		t.Fatalf("renamed veth missing: %v", err)
	}
	for _, port := range []netlink.Link{tap, veth} {
		if port.Attrs().MasterIndex != bridge.Attrs().Index {
			t.Errorf("%s is not enslaved to the bridge", port.Attrs().Name)
		}
	}
	addrs, err := h.AddrList(veth, netlink.FAMILY_V6)
	if err != nil {
		t.Fatalf("list addresses: %v", err)
	}
	for _, addr := range addrs {
		if addr.IP.IsGlobalUnicast() {
			t.Errorf("VMI NIC got the extra address %s, the guest only gets its IPv4 address by DHCP", addr.IPNet)
		}
	}
	a.dhcpMu.Lock()
	_, serving := a.dhcpServers[dhcpKey{netnsPath: spec.NetnsPath, bridge: bridgeNameFor(spec.IfaceName)}]
	a.dhcpMu.Unlock()
	if !serving {
		t.Error("no DHCP server started for the VMI NIC")
	}

	for range 2 {
		if err := a.unwireVMIBridge(ctx, spec); err != nil {
			t.Fatalf("unwireVMIBridge() error = %v", err)
		}
	}
	if _, err := h.LinkByName(testIface1); err == nil {
		t.Error("tap still exists after unwireVMIBridge")
	}
	if _, err := h.LinkByName(bridgeNameFor(testIface1)); err == nil {
		t.Error("bridge still exists after unwireVMIBridge")
	}
	a.dhcpMu.Lock()
	_, serving = a.dhcpServers[dhcpKey{netnsPath: spec.NetnsPath, bridge: bridgeNameFor(spec.IfaceName)}]
	a.dhcpMu.Unlock()
	if serving {
		t.Error("DHCP server still registered after unwireVMIBridge")
	}
	if err := a.cleanupHostVeth(ctx, host); err != nil {
		t.Fatalf("cleanupHostVeth() error = %v", err)
	}
}

// A VMI with several DRA NICs has ONE pod netns and one bridge per NIC. Each NIC
// needs its own DHCP server: a server keyed by the netns alone made the second
// NIC find the first one's entry and never start its own, so the guest got a
// lease on the first NIC only. Detaching one NIC must also leave the others'
// servers running.
func TestDatapathVMIMultipleNICsEachGetADHCPServer(t *testing.T) {
	netnsPath := newTestNetns(t)
	nic := func(ifaceName, ip, mac, ifaceID string) Spec {
		return Spec{
			NetnsPath:   netnsPath,
			IfaceName:   ifaceName,
			IP:          ip,
			MAC:         mac,
			Gateway:     testGateway,
			ContainerID: testContainerID16,
			IfaceID:     ifaceID,
			KubeVirtVMI: true,
		}
	}
	first := nic(testIface1, testPodIPWithMask, "00:00:00:aa:bb:01", testVMPort)
	second := nic(testIface2, "10.20.0.5/24", "00:00:00:aa:bb:02", "vm1.default.green.default.ovn")

	a := &ovsAttacher{dhcpServers: make(map[dhcpKey]dhcpServeCloser)}
	hosts := []string{plumbPodIface(t, a, first), plumbPodIface(t, a, second)}
	ctx := context.Background()
	for _, spec := range []Spec{first, second} {
		if err := a.wireVMIBridge(ctx, spec); err != nil {
			t.Fatalf("wireVMIBridge(%s) error = %v", spec.IfaceName, err)
		}
	}

	serving := func(spec Spec) bool {
		a.dhcpMu.Lock()
		defer a.dhcpMu.Unlock()
		_, ok := a.dhcpServers[dhcpKey{netnsPath: netnsPath, bridge: bridgeNameFor(spec.IfaceName)}]
		return ok
	}
	a.dhcpMu.Lock()
	registered := len(a.dhcpServers)
	a.dhcpMu.Unlock()
	if registered != 2 {
		t.Fatalf("%d DHCP servers registered for a pod with two NICs, want 2: the second NIC found the first one's and started none", registered)
	}
	if !serving(first) || !serving(second) {
		t.Fatalf("DHCP servers: net1 = %v, net2 = %v, want one for each NIC of the pod", serving(first), serving(second))
	}

	if err := a.unwireVMIBridge(ctx, first); err != nil {
		t.Fatalf("unwireVMIBridge(net1) error = %v", err)
	}
	if serving(first) {
		t.Error("net1's DHCP server is still registered after its NIC was detached")
	}
	if !serving(second) {
		t.Error("detaching net1 stopped net2's DHCP server")
	}

	if err := a.unwireVMIBridge(ctx, second); err != nil {
		t.Fatalf("unwireVMIBridge(net2) error = %v", err)
	}
	if serving(second) {
		t.Error("net2's DHCP server is still registered after its NIC was detached")
	}
	for _, host := range hosts {
		if err := a.cleanupHostVeth(ctx, host); err != nil {
			t.Fatalf("cleanupHostVeth(%s) error = %v", host, err)
		}
	}
}

// stubDHCPListener stands in for the listener of a server that was started for
// an earlier pod and never stopped.
type stubDHCPListener struct{ closed bool }

func (s *stubDHCPListener) ReadFrom([]byte) (int, net.Addr, error) { return 0, nil, io.EOF }
func (s *stubDHCPListener) WriteTo([]byte, net.Addr) (int, error)  { return 0, nil }
func (s *stubDHCPListener) Close() error                           { s.closed = true; return nil }

// The bridge name is the same for every pod instance of one VMI network, so an
// entry leaked by an earlier pod (a crash, a force delete, a driver restart
// between Attach and Detach) must not stop a NEW pod's server from starting:
// the netns path is part of the key for exactly that reason.
func TestEnsureVMIDHCPServerIgnoresAStaleEntryOfAnotherNetns(t *testing.T) {
	spec := Spec{
		NetnsPath:   newTestNetns(t),
		IfaceName:   testIface1,
		IP:          testPodIPWithMask,
		MAC:         testPodMAC,
		Gateway:     testGateway,
		ContainerID: testContainerID16,
		IfaceID:     testVMPort,
		KubeVirtVMI: true,
	}
	stale := &stubDHCPListener{}
	staleKey := dhcpKey{netnsPath: "/var/run/netns/an-earlier-pod", bridge: bridgeNameFor(spec.IfaceName)}
	a := &ovsAttacher{dhcpServers: map[dhcpKey]dhcpServeCloser{staleKey: stale}}

	host := plumbPodIface(t, a, spec)
	ctx := context.Background()
	if err := a.wireVMIBridge(ctx, spec); err != nil {
		t.Fatalf("wireVMIBridge() error = %v", err)
	}

	a.dhcpMu.Lock()
	_, started := a.dhcpServers[dhcpKey{netnsPath: spec.NetnsPath, bridge: bridgeNameFor(spec.IfaceName)}]
	_, staleKept := a.dhcpServers[staleKey]
	a.dhcpMu.Unlock()
	if !started {
		t.Error("a stale entry of an earlier pod stopped the new pod's DHCP server from starting")
	}
	if !staleKept || stale.closed {
		t.Error("starting the new pod's server touched the earlier pod's entry")
	}

	if err := a.unwireVMIBridge(ctx, spec); err != nil {
		t.Fatalf("unwireVMIBridge() error = %v", err)
	}
	if err := a.cleanupHostVeth(ctx, host); err != nil {
		t.Fatalf("cleanupHostVeth() error = %v", err)
	}
}

// Without an MTU from the Subnet, a NIC takes the MTU of the pod's primary
// interface, which kube-ovn-cni set for the overlay.
func TestPrimaryMTU(t *testing.T) {
	path := newTestNetns(t)
	a := &ovsAttacher{dhcpServers: make(map[dhcpKey]dhcpServeCloser)}
	if got := a.primaryMTU(context.Background(), path); got != 0 {
		t.Errorf("primaryMTU() without eth0 = %d, want 0", got)
	}

	h := podHandle(t, path)
	eth0 := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: primaryIface, MTU: 1400}}
	if err := h.LinkAdd(eth0); err != nil {
		t.Fatalf("add eth0: %v", err)
	}
	if got := a.primaryMTU(context.Background(), path); got != 1400 {
		t.Errorf("primaryMTU() = %d, want the MTU of eth0, 1400", got)
	}
	if got := a.primaryMTU(context.Background(), "/var/run/netns/does-not-exist"); got != 0 {
		t.Errorf("primaryMTU() of a missing netns = %d, want 0", got)
	}
}

// A VMI NIC whose Subnet has OVN's DHCP gets no DHCP server from the driver,
// and the attacher returns before touching the netns.
func TestEnsureVMIDHCPServerSkipsOVNDHCP(t *testing.T) {
	a, ok := NewOVSAttacher().(*ovsAttacher)
	if !ok {
		t.Fatal("NewOVSAttacher does not return an *ovsAttacher")
	}
	spec := Spec{NetnsPath: testOVNDHCPNetns, IfaceName: testOVNDHCPIface, OVNDHCP: true}
	if err := a.ensureVMIDHCPServer(context.Background(), spec, bridgeNameFor(spec.IfaceName)); err != nil {
		t.Fatalf("ensureVMIDHCPServer: %v", err)
	}
	if len(a.dhcpServers) != 0 {
		t.Fatalf("expected no DHCP server, got %d", len(a.dhcpServers))
	}
}
