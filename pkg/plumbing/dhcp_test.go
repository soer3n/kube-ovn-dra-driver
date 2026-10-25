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
	"bytes"
	"net"
	"testing"

	dhcp "github.com/krolaw/dhcp4"
)

func TestVMIDHCPOptions(t *testing.T) {
	mask := net.CIDRMask(24, 32)
	opts, err := vmiDHCPOptions(Spec{Gateway: testGateway, MTU: 1400, Routes: []Route{{Destination: testPodRouteDst}}}, mask)
	if err != nil {
		t.Fatalf("vmiDHCPOptions() error = %v", err)
	}
	if !bytes.Equal(opts[dhcp.OptionSubnetMask], []byte(mask)) {
		t.Errorf("subnet mask = %v, want %v", opts[dhcp.OptionSubnetMask], []byte(mask))
	}
	if !bytes.Equal(opts[dhcp.OptionRouter], net.IPv4(10, 10, 0, 1).To4()) {
		t.Errorf("router = %v, want 10.10.0.1", opts[dhcp.OptionRouter])
	}
	if !bytes.Equal(opts[dhcp.OptionInterfaceMTU], []byte{0x05, 0x78}) {
		t.Errorf("mtu = %v, want 1400", opts[dhcp.OptionInterfaceMTU])
	}
	if !bytes.Equal(opts[dhcp.OptionClasslessRouteFormat], []byte{16, 192, 168, 10, 10, 0, 1}) {
		t.Errorf("classless routes = %v", opts[dhcp.OptionClasslessRouteFormat])
	}

	minimal, err := vmiDHCPOptions(Spec{}, nil)
	if err != nil || len(minimal) != 0 {
		t.Errorf("vmiDHCPOptions(empty) = %v, %v; want no options", minimal, err)
	}
	if _, err := vmiDHCPOptions(Spec{Gateway: "not-an-ip"}, mask); err == nil {
		t.Error("vmiDHCPOptions() with an invalid gateway: want error")
	}
}

func TestClasslessStaticRoutes(t *testing.T) {
	route := func(dst, gw string) []Route { return []Route{{Destination: dst, Gateway: gw}} }
	tests := []struct {
		name    string
		routes  []Route
		gateway string
		want    []byte
		wantErr bool
	}{
		{"none", nil, testGateway, nil, false},
		{"/8 via gateway", route(testRouteDst, ""), testGateway, []byte{8, 10, 10, 10, 0, 1}, false},
		{"/8 via its own gateway", route(testRouteDst, testRouteGateway), testGateway, []byte{8, 10, 10, 10, 0, 254}, false},
		{"/32 without gateway", route("1.2.3.4/32", ""), "", []byte{32, 1, 2, 3, 4, 0, 0, 0, 0}, false},
		{"ipv6 skipped", route(testIPv6RouteDst, ""), testGateway, nil, false},
		{"gateway of another family", route(testRouteDst, "fd00::1"), testGateway, nil, true},
		{"default route refused", route(testDefaultRoute, ""), testGateway, nil, true},
		{"invalid route", route("bogus", ""), testGateway, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := classlessStaticRoutes(tt.routes, tt.gateway)
			if (err != nil) != tt.wantErr {
				t.Fatalf("classlessStaticRoutes() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !bytes.Equal(got, tt.want) {
				t.Errorf("classlessStaticRoutes() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSingleClientDHCPHandler(t *testing.T) {
	clientMAC := net.HardwareAddr{0x00, 0x00, 0x00, 0xaa, 0xbb, 0xcc}
	h := &singleClientDHCPHandler{
		serverIP:  fakeDHCPServerIP,
		clientIP:  net.IPv4(10, 10, 0, 5).To4(),
		clientMAC: clientMAC,
		options:   dhcp.Options{dhcp.OptionRouter: net.IPv4(10, 10, 0, 1).To4()},
	}
	request := func(mt dhcp.MessageType, mac net.HardwareAddr) dhcp.Packet {
		return dhcp.RequestPacket(mt, mac, nil, []byte{1, 2, 3, 4}, true, nil)
	}
	replyType := func(p dhcp.Packet) dhcp.MessageType {
		return dhcp.MessageType(p.ParseOptions()[dhcp.OptionDHCPMessageType][0])
	}

	offer := h.ServeDHCP(request(dhcp.Discover, clientMAC), dhcp.Discover, nil)
	if offer == nil || replyType(offer) != dhcp.Offer || !offer.YIAddr().Equal(h.clientIP) {
		t.Fatalf("Discover: got %v, want an offer of %v", offer, h.clientIP)
	}
	ack := h.ServeDHCP(request(dhcp.Request, clientMAC), dhcp.Request, nil)
	if ack == nil || replyType(ack) != dhcp.ACK || !ack.YIAddr().Equal(h.clientIP) {
		t.Fatalf("Request: got %v, want an ACK of %v", ack, h.clientIP)
	}
	if got := h.ServeDHCP(request(dhcp.Discover, net.HardwareAddr{0, 0, 0, 0, 0, 1}), dhcp.Discover, nil); got != nil {
		t.Error("Discover from another MAC must be ignored")
	}
	if got := h.ServeDHCP(request(dhcp.Release, clientMAC), dhcp.Release, nil); got != nil {
		t.Error("Release must not be answered")
	}
}
