//go:build e2e

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

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// The network state of a pod as iproute2 reports it with -j, like the iproute
// package of Kube-OVN's e2e framework (test/e2e/framework/iproute), reduced to
// the fields the specs check.

// addrInfo is an address of a link.
type addrInfo struct {
	Family    string `json:"family"`
	Local     string `json:"local"`
	PrefixLen int    `json:"prefixlen"`
}

// link is a network interface.
type link struct {
	IfName    string     `json:"ifname"`
	Mtu       int        `json:"mtu"`
	OperState string     `json:"operstate"`
	Address   string     `json:"address"`
	AddrInfo  []addrInfo `json:"addr_info"`
}

// route is a route of the main table.
type route struct {
	Dst     string `json:"dst"`
	Gateway string `json:"gateway,omitempty"`
	Dev     string `json:"dev"`
}

// ips returns the addresses of the link.
func (l link) ips() []string {
	ips := make([]string, 0, len(l.AddrInfo))
	for _, addr := range l.AddrInfo {
		ips = append(ips, addr.Local)
	}
	return ips
}

// podIPJSON runs ip with -j and the arguments in the pod and decodes the result.
// Only stdout is decoded: kubectl prints notices such as "Defaulted container"
// on stderr.
func podIPJSON(pod string, result any, args ...string) error {
	cmd := exec.Command(kubectlBinary, append([]string{kubectlExec, pod, argsEnd, "ip", "-j"}, args...)...)
	cmd.Dir = repoRoot()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ip -j %s in pod %s: %w: %s", strings.Join(args, " "), pod, err, stderr.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), result); err != nil {
		return fmt.Errorf("decode ip -j %s of pod %s: %w: %s", strings.Join(args, " "), pod, err, stdout.String())
	}
	return nil
}

// linkShow returns the link dev of the pod with its addresses.
func linkShow(pod, dev string) (link, error) {
	var links []link
	if err := podIPJSON(pod, &links, "addr", "show", "dev", dev); err != nil {
		return link{}, err
	}
	if len(links) != 1 {
		return link{}, fmt.Errorf("pod %s has %d links named %s", pod, len(links), dev)
	}
	return links[0], nil
}

// routeShow returns the IPv4 routes of the pod through dev.
func routeShow(pod, dev string) ([]route, error) {
	var routes []route
	err := podIPJSON(pod, &routes, "route", "show", "dev", dev)
	return routes, err
}
