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
	"bytes"
	"fmt"
	"io"
	"net"
	"time"

	dhcp "github.com/krolaw/dhcp4"
)

// dhcpServeCloser is what ensureVMIDHCPServer needs from the listener it
// hands to dhcp.Serve: krolaw/dhcp4/conn.NewUDP4FilterListener returns an
// unexported concrete type, so callers outside that package can only name it
// through an interface — this is dhcp.ServeConn (ReadFrom/WriteTo) plus
// io.Closer (to stop the server from Detach/unwireVMIBridge).
type dhcpServeCloser interface {
	dhcp.ServeConn
	io.Closer
}

// A KubeVirt guest's secondary NIC has no way to learn its IP/gateway/routes
// except by asking a DHCP server on the wire: unlike the plain-pod case
// (configurePodIface), we cannot reach into the guest's kernel and configure
// its routing table directly — the pod netns is just a dumb L2 bridge from
// the guest's point of view.
//
// It might seem natural to let the request reach kube-ovn's own OVN-side DHCP
// responder (subnet.Spec.EnableDHCP) across that bridge, since it already
// knows this exact IP/gateway from IPAM. That path was tried first and does
// not work reliably: kube-ovn's reply legitimately arrives at this pod's veth
// port (confirmed via tcpdump + ovn-trace), but forwarding a single unicast
// reply across a freshly-built Linux bridge to a specific tap port turned out
// to depend on kernel/bridge-ID details that are hard to pin down.
//
// KubeVirt itself never actually relies on that path either — for EVERY
// bridge-bound interface, primary or Multus secondary, it starts its own
// local, single-client DHCP server bound to the bridge (pkg/network/dhcp:
// SingleClientDHCPServer), seeded from the IP it already has from IPAM, and
// answers the guest locally. This mirrors that exact design: the DRA
// ResourceClaim allocation already gave us spec.IP/Gateway/Routes, so we
// serve them locally instead of depending on a real round trip to OVN.
const dhcpInfiniteLease = 999 * 24 * time.Hour

// fakeDHCPServerIP is the "server identifier" (DHCP option 54) our answers
// carry. It does not need to be a real, configured address: the dhcp4
// library only places it in the packet, and each VMI's server lives in its
// own, otherwise-isolated pod netns, so reusing this constant across every
// VMI is safe. Mirrors KubeVirt's own "fake bridge IP" convention
// (pkg/network/setup/netpod's bridgeFakeIPBase).
var fakeDHCPServerIP = net.IPv4(169, 254, 75, 1).To4()

// vmiDHCPOptions builds the DHCP options for the single-client server,
// mirroring KubeVirt's own pkg/network/dhcp/server prepareDHCPOptions (minus
// DNS/search-domain/NTP knobs Spec doesn't carry).
func vmiDHCPOptions(spec Spec, mask net.IPMask) (dhcp.Options, error) {
	opts := dhcp.Options{}

	if len(mask) != 0 {
		opts[dhcp.OptionSubnetMask] = []byte(mask)
	}

	if spec.Gateway != "" {
		gw := net.ParseIP(spec.Gateway)
		if gw == nil {
			return nil, fmt.Errorf("plumbing: parse Spec.Gateway %q for DHCP options", spec.Gateway)
		}
		opts[dhcp.OptionRouter] = gw.To4()
	}

	if spec.MTU > 0 {
		opts[dhcp.OptionInterfaceMTU] = []byte{byte(spec.MTU >> 8), byte(spec.MTU)} //nolint:mnd
	}

	routes, err := classlessStaticRoutes(spec.Routes, spec.Gateway)
	if err != nil {
		return nil, err
	}
	if len(routes) != 0 {
		opts[dhcp.OptionClasslessRouteFormat] = routes
	}

	return opts, nil
}

// classlessStaticRoutes encodes spec.Routes as DHCP option 121 (RFC 3442):
// each entry is a destination network reachable via gateway, matching
// configurePodIface's own per-NIC route semantics for the plain-pod case.
// Refuses a default route for the same reason configurePodIface does: the
// default route belongs to eth0, never to this secondary NIC.
func classlessStaticRoutes(routes []string, gateway string) ([]byte, error) {
	if len(routes) == 0 {
		return nil, nil
	}
	var gw net.IP
	if gateway != "" {
		gw = net.ParseIP(gateway).To4()
	}

	var out []byte
	for _, r := range routes {
		_, dst, err := net.ParseCIDR(r)
		if err != nil {
			return nil, fmt.Errorf("plumbing: parse route %q for DHCP options: %w", r, err)
		}
		width, _ := dst.Mask.Size()
		if width == 0 {
			return nil, fmt.Errorf("plumbing: refusing to serve a default route %q via DHCP on a secondary NIC", r)
		}
		octets := (width-1)/8 + 1 //nolint:mnd
		ip4 := dst.IP.To4()
		if ip4 == nil {
			continue // IPv6 routes are out of scope for this IPv4-only DHCP server
		}
		out = append(out, byte(width))
		out = append(out, ip4[:octets]...)
		if gw != nil {
			out = append(out, gw...)
		} else {
			out = append(out, 0, 0, 0, 0) //nolint:mnd
		}
	}
	return out, nil
}

// singleClientDHCPHandler answers DISCOVER/REQUEST from exactly one MAC
// (the guest's virtio-net address) with one fixed lease — the IP kube-ovn's
// IPAM already allocated. Mirrors krolaw/dhcp4's Handler interface exactly
// as KubeVirt's own DHCPHandler (pkg/network/dhcp/server) does.
type singleClientDHCPHandler struct {
	serverIP  net.IP
	clientIP  net.IP
	clientMAC net.HardwareAddr
	options   dhcp.Options
}

func (h *singleClientDHCPHandler) ServeDHCP(p dhcp.Packet, msgType dhcp.MessageType, _ dhcp.Options) dhcp.Packet {
	if len(h.clientMAC) != 0 && !bytes.Equal(p.CHAddr(), h.clientMAC) {
		return nil // not our client
	}
	switch msgType {
	case dhcp.Discover:
		return dhcp.ReplyPacket(p, dhcp.Offer, h.serverIP, h.clientIP, dhcpInfiniteLease, h.options.SelectOrderOrAll(nil))
	case dhcp.Request:
		return dhcp.ReplyPacket(p, dhcp.ACK, h.serverIP, h.clientIP, dhcpInfiniteLease, h.options.SelectOrderOrAll(nil))
	default:
		return nil
	}
}
