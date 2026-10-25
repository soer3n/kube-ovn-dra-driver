# Copyright 2023 The Kubernetes Authors.
# Copyright 2026 soer3n.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

CONTAINER_TOOL ?= docker
MKDIR    ?= mkdir
TR       ?= tr
DIST_DIR ?= $(CURDIR)/dist
HELM     ?= helm

export CHART_GIT_TAG ?= $(shell git describe --tags --always --dirty --match 'chart/*')

include $(CURDIR)/common.mk

BUILDIMAGE_TAG ?= golang$(GOLANG_VERSION)
BUILDIMAGE ?= $(IMAGE_NAME)-build:$(BUILDIMAGE_TAG)

CMDS := $(patsubst ./cmd/%/,%,$(sort $(dir $(wildcard ./cmd/*/))))
CMD_TARGETS := $(patsubst %,cmd-%, $(CMDS))

CHECK_TARGETS := assert-fmt vet lint ineffassign misspell
MAKE_TARGETS := binaries build check fmt test examples cmds coverage generate $(CHECK_TARGETS)

TARGETS := $(MAKE_TARGETS) $(CMD_TARGETS)

DOCKER_TARGETS := $(patsubst %,docker-%, $(TARGETS))
.PHONY: $(TARGETS) $(DOCKER_TARGETS)

GOOS ?= linux

binaries: cmds
ifneq ($(PREFIX),)
cmd-%: COMMAND_BUILD_OPTIONS = -o $(PREFIX)/$(*)
endif
cmds: $(CMD_TARGETS)
$(CMD_TARGETS): cmd-%:
	CGO_LDFLAGS_ALLOW='-Wl,--unresolved-symbols=ignore-in-object-files' GOOS=$(GOOS) \
		go build -ldflags "-s -w -X main.version=$(VERSION)" $(COMMAND_BUILD_OPTIONS) $(MODULE)/cmd/$(*)

build:
	GOOS=$(GOOS) go build ./...

examples: $(EXAMPLE_TARGETS)
$(EXAMPLE_TARGETS): example-%:
	GOOS=$(GOOS) go build ./examples/$(*)

all: check test build binary
check: $(CHECK_TARGETS)

# Apply go fmt to the codebase
fmt:
	go list -f '{{.Dir}}' $(MODULE)/... \
		| xargs gofmt -s -l -w

assert-fmt:
	go list -f '{{.Dir}}' $(MODULE)/... \
		| xargs gofmt -s -l > fmt.out
	@if [ -s fmt.out ]; then \
		echo "\nERROR: The following files are not formatted:\n"; \
		cat fmt.out; \
		rm fmt.out; \
		exit 1; \
	else \
		rm fmt.out; \
	fi

ineffassign:
	ineffassign $(MODULE)/...

lint:
	golangci-lint run ./...

misspell:
	misspell $(MODULE)/...

vet:
	go vet $(MODULE)/...

# Ensure that all log calls support contextual logging.
test: logcheck
.PHONY: logcheck
logcheck:
	(cd hack/tools && GOBIN=$(PWD) go install sigs.k8s.io/logtools/logcheck)
	./logcheck -check-contextual -check-deprecations ./...

COVERAGE_FILE := coverage.out
test: build cmds
	go test -v -coverprofile=$(COVERAGE_FILE) $(MODULE)/...

# The datapath tests in pkg/plumbing create network namespaces and need root;
# they skip otherwise.
.PHONY: test-privileged
test-privileged:
	go test -c -o $(CURDIR)/plumbing.test ./pkg/plumbing
	sudo $(CURDIR)/plumbing.test -test.v
	rm -f $(CURDIR)/plumbing.test

coverage: test
	cat $(COVERAGE_FILE) | grep -v "_mock.go" > $(COVERAGE_FILE).no-mocks
	go tool cover -func=$(COVERAGE_FILE).no-mocks

generate: generate-deepcopy

generate-deepcopy:
	for api in $(APIS); do \
		rm -f $(CURDIR)/api/$(VENDOR)/resource/$${api}/zz_generated.deepcopy.go; \
		controller-gen \
			object:headerFile=$(CURDIR)/hack/boilerplate.generatego.txt \
			paths=$(CURDIR)/api/$(VENDOR)/resource/$${api}/ \
			output:object:dir=$(CURDIR)/api/$(VENDOR)/resource/$${api}; \
	done

###############################################################################
# kind + kube-ovn + multus + NIC DRA driver demo
###############################################################################

KIND_CLUSTER_NAME ?= nic-dra-demo
KIND_CONFIG       ?= $(CURDIR)/demo/kind/kind-no-cni.yaml
# IP families of the cluster: ipv4, or dual for dual-stack. kind-create derives a
# dual-stack variant of KIND_CONFIG, kind-deploy-kube-ovn sets kube-ovn's
# NET_STACK and CIDRs to match.
IP_FAMILY         ?= ipv4
POD_CIDR_V4       = 10.244.0.0/16
POD_GATEWAY_V4    = 10.244.0.1
SVC_CIDR_V4       = 10.96.0.0/12
JOIN_CIDR_V4      = 100.64.0.0/16
POD_CIDR_V6       = fd00:10:244::/112
POD_GATEWAY_V6    = fd00:10:244::1
SVC_CIDR_V6       = fd00:10:96::/112
JOIN_CIDR_V6      = fd00:100:64::/112
# kube-controller-manager hands each node a /64 of the pod range, so kind's
# needs to be larger than kube-ovn's, which kube-ovn manages itself.
KIND_POD_CIDR_V6  = fd00:10:244::/56
ifeq ($(IP_FAMILY),dual)
# helm splits --set values at commas unless they are escaped
KUBE_OVN_STACK_ARGS = --set networking.NET_STACK=dual_stack \
	--set 'dual_stack.POD_CIDR=$(POD_CIDR_V4)\,$(POD_CIDR_V6)' \
	--set 'dual_stack.POD_GATEWAY=$(POD_GATEWAY_V4)\,$(POD_GATEWAY_V6)' \
	--set 'dual_stack.SVC_CIDR=$(SVC_CIDR_V4)\,$(SVC_CIDR_V6)' \
	--set 'dual_stack.JOIN_CIDR=$(JOIN_CIDR_V4)\,$(JOIN_CIDR_V6)'
else
KUBE_OVN_STACK_ARGS = --set networking.NET_STACK=ipv4 \
	--set ipv4.POD_CIDR=$(POD_CIDR_V4) \
	--set ipv4.POD_GATEWAY=$(POD_GATEWAY_V4) \
	--set ipv4.SVC_CIDR=$(SVC_CIDR_V4) \
	--set ipv4.JOIN_CIDR=$(JOIN_CIDR_V4)
endif
NIC_DEMO_DIR      ?= $(CURDIR)/demo/nic-example

# Kubernetes node image. The demo and e2e run on Kubernetes 1.36, where the
# DRAConsumableCapacity gate (shared subnets, IP-pool capacity) is beta and on by
# default; the driver itself needs 1.34 or later (k8s.io/* deps pinned to v0.35.x;
# the stable DRA API needs 1.34+). The image is the v1.36 image of kind v0.33.0,
# pinned by digest; use it with that kind release. Override to test another
# release, e.g. KIND_NODE_IMAGE=kindest/node:v1.35.8 (the gate is alpha there and
# the kind config below enables it explicitly).
KIND_NODE_IMAGE   ?= kindest/node:v1.36.4@sha256:099e049362a1526b2db71494e1947aae99bd16290d7c895f2b7ea312e3cbfaed

# kube-ovn ref to deploy the chart from. DRA NICs need kube-ovn-controller with
# --enable-dra-nic, which is not in a kube-ovn release yet; the default is the
# branch of the upstream pull request. A bare release tag also does not resolve
# against a typical local clone (tags aren't fetched by default) and silently
# falls through to curl'ing the release tarball below.
KUBE_OVN_VERSION  ?= dra-nic-upstream
# Local kube-ovn clone used to source the Helm chart offline (avoids fetching
# the whole source tarball from GitHub). The chart is read from the
# $(KUBE_OVN_VERSION) git ref; if the clone/ref is absent, the deploy falls back
# to curl'ing the GitHub release tarball (this fallback only makes sense for an
# actual upstream release tag, not the branch above — see KUBE_OVN_REPO).
KUBE_OVN_REPO     ?= $(CURDIR)/../kube-ovn
# Multus version (thick CNI daemonset).
MULTUS_VERSION    ?= v4.2.3

# Images preloaded into the kind cluster so the demo never has to pull them at
# runtime (mirrors the edge-router e2e image preload in kubermatic-virtualization).
#
# The demo deploys a kube-ovn image built from $(KUBE_OVN_VERSION), not the
# upstream release, which lacks --enable-dra-nic. By default it pulls
# docker.io/soer3n/kube-ovn:dra-driver-<version>. To use a local build instead,
# override the vars, e.g.
#   cd ../kube-ovn && make build-go && docker build -t docker.io/kubeovn/kube-ovn:dev -f dist/images/Dockerfile dist/images/
#   make ... KUBE_OVN_REGISTRY=docker.io/kubeovn KUBE_OVN_IMAGE_TAG=dev
# netshoot is pinned by digest.
KUBE_OVN_REGISTRY   ?= docker.io/soer3n
KUBE_OVN_IMAGE_REPO ?= kube-ovn
KUBE_OVN_IMAGE_TAG  ?= dra-driver-$(subst /,-,$(KUBE_OVN_VERSION:v%=%))
KUBE_OVN_IMAGE      ?= $(KUBE_OVN_REGISTRY)/$(KUBE_OVN_IMAGE_REPO):$(KUBE_OVN_IMAGE_TAG)
NETSHOOT_IMAGE    ?= docker.io/nicolaka/netshoot@sha256:47b907d662d139d1e2f22bfe14f4efca1e3f1feed283572f47c970c780c03b61
# Only the kube-ovn image is preloaded: it is single-arch, so `docker pull` +
# `kind load docker-image` (docker save) handles it cleanly, and preloading pins
# the exact DRA-patched build.
#
# netshoot is deliberately NOT preloaded. It is a public, multi-arch,
# digest-pinned image, and `kind load docker-image` (docker save of a manifest
# list) corrupts containerd's image store ("import-<date>", missing platform
# manifest -> CreateContainerError when a pod tries to start). The nodes have
# registry access, so pods pull netshoot at runtime. To warm it for benchmark
# timing, pull single-arch onto each node first, e.g.
#   docker pull --platform linux/amd64 $(NETSHOOT_IMAGE) && \
#   kind load docker-image --name $(KIND_CLUSTER_NAME) $(NETSHOOT_IMAGE)
PRELOAD_IMAGES    ?= $(KUBE_OVN_IMAGE)

# NIC driver name as registered in the DeviceClass.
NIC_DRIVER_NAME   ?= nic.kubeovn.io

# Benchmark knobs: compare DRA vs Multus secondary-NIC spin-up.
BENCH_MODE         ?= overlay
BENCH_COUNT        ?= 8
BENCH_COUNTS       ?= 2 4 8
BENCH_DIR          ?= $(CURDIR)/.bench
BENCH_TIMEOUT      ?= 300s
BENCH_REPS         ?= 1
# NICs the Multus hot-plug benchmark arm starts with before hot-plugging the
# rest via multus-dynamic-networks-controller (not deployed by kind-demo; see
# https://github.com/k8snetworkplumbingwg/multus-dynamic-networks-controller).
HOTPLUG_BASE       ?= 2

CONTAINERLAB_TOPOLOGY ?= $(CURDIR)/demo/containerlab/vlan-topology.yaml

.PHONY: kind-create kind-delete kind-preload-images kind-deploy-kube-ovn kind-deploy-multus \
        kind-kube-ovn-status kind-test-kube-ovn kind-test-kube-ovn-clean \
        kind-build-driver kind-deploy-driver kind-deploy-nic-prereqs kind-deploy-nic-example \
        kind-demo nic-example-deploy nic-example-clean kind-check-deps \
        kind-deploy-kube-ovn-fixtures kind-vlan-tag-workaround kind-demo-2workers \
        kind-test-vlan kind-deploy-vlan-peer \
        clab-deploy clab-destroy clab-clean-ports kind-vlan-status \
        kind-vlan-nat

## kind-check-deps: verify required tools are installed.
kind-check-deps:
	@for tool in kind kubectl helm docker curl tar; do \
		command -v $$tool >/dev/null 2>&1 || { echo "ERROR: '$$tool' not found in PATH"; exit 1; }; \
	done
	@echo "All required tools found."

## clab-deploy: wire eth1 into kind nodes via containerlab + start FRR gateway.
## Requires containerlab and sudo. Run after kind-create, before kind-deploy-kube-ovn.
clab-deploy: clab-clean-ports
	@echo "Recreating Linux bridge 'vlan-switch' on host..."
	sudo ip link delete vlan-switch 2>/dev/null || true
	sudo ip link add name vlan-switch type bridge
	sudo ip link set vlan-switch up
	@echo "Enabling VLAN filtering on bridge so 802.1q tags pass through..."
	sudo ip link set vlan-switch type bridge vlan_filtering 1
	@echo "Deploying containerlab VLAN topology + FRR gateway (requires sudo)..."
	sudo containerlab deploy -t $(CONTAINERLAB_TOPOLOGY) --reconfigure
	@echo "Allowing demo VLANs (100-800), the capacity demo VLAN (900) and benchmark VLANs (1101-1108) on bridge ports..."
	# 1101-1108 = demo/nic-example/examples/generate.py's BENCH_VLAN_BASE(1100) + 1..MAX_VLANS(8);
	# without these, a bench underlay pod's interface never comes up (not just "no traffic" as
	# docs/benchmarking.md's caveat implies) since kube-ovn can never bind an untagged VLAN.
	for vid in 100 200 300 400 500 600 700 800 900 1101 1102 1103 1104 1105 1106 1107 1108; do \
		for p in port1 port2 port3 port4; do \
			ip link show $$p >/dev/null 2>&1 || continue; \
			sudo bridge vlan add vid $$vid dev $$p; \
		done; \
	done
	@echo "FRR gateway is up. Check the VLAN gateways and host routes: make kind-vlan-status"
	$(MAKE) kind-vlan-nat

## kind-vlan-nat: SNAT the demo VLAN subnets (172.23-172.30.0.0/24) out the
## host's own default-route interface, so a VM attached to one of them (e.g.
## via vlan100-subnet) can reach the real internet, not just other hosts on
## the same VLAN trunk. frr-gw (clab-deploy above) is only the VLAN gateway in
## the host network namespace — it does not NAT — and these subnets are RFC1918 space with no other path out. Idempotent: skips a
## rule that's already present, so repeat `make clab-deploy` runs don't pile
## up duplicates. Run automatically by clab-deploy; safe to re-run standalone
## after a host reboot (iptables rules are NOT persisted across reboots by
## this target — install iptables-persistent or similar if you need that).
kind-vlan-nat:
	$(eval EGRESS_IF := $(shell ip route show default | awk '/^default/{print $$5; exit}'))
	@if [ -z "$(EGRESS_IF)" ]; then \
		echo "ERROR: could not determine the host's default-route interface"; \
		exit 1; \
	fi
	@echo "SNATting VLAN demo subnets out $(EGRESS_IF)..."
	@for subnet in 172.23.0.0/24 172.24.0.0/24 172.25.0.0/24 172.26.0.0/24 \
	               172.27.0.0/24 172.28.0.0/24 172.29.0.0/24 172.30.0.0/24; do \
		sudo iptables -t nat -C POSTROUTING -o $(EGRESS_IF) -s $$subnet -j MASQUERADE 2>/dev/null || \
			sudo iptables -t nat -A POSTROUTING -o $(EGRESS_IF) -s $$subnet -j MASQUERADE; \
	done
	@echo "Done."

## kind-vlan-status: show the gateway sub-interfaces of frr-gw and the host's
## connected routes to the VLAN subnets.
kind-vlan-status:
	@echo "=== VLAN gateway sub-interfaces (frr-gw, host network namespace) ==="
	-@ip -br addr show | grep -E '^eth1\.' || echo "  (none — is frr-gw running? make clab-deploy)"
	@echo ""
	@echo "=== Host routes to the VLAN subnets ==="
	-@ip route show | grep -E 'dev eth1\.' || echo "  (none)"

## clab-clean-ports: delete stale host-side veth ports from a previous deploy.
## containerlab names the host end of each link after the topology endpoint
## (vlan-switch:port1, :port2, :port3). Because the kind nodes are ext-containers
## whose lifecycle containerlab does not own, a teardown (including the implicit
## destroy in `deploy --reconfigure`) can leave these veths orphaned in the root
## netns. The next deploy then fails with "interface ... already exists".
## Deleting a veth removes both ends, clearing the matching eth1 in the kind node.
clab-clean-ports:
	@echo "Removing any stale containerlab veth ports (port1-4)..."
	@for p in port1 port2 port3 port4; do \
		sudo ip link delete $$p 2>/dev/null && echo "  deleted $$p" || true; \
	done

## clab-destroy: remove the containerlab VLAN topology and FRR gateway.
clab-destroy:
	@echo "Destroying containerlab VLAN topology (requires sudo)..."
	sudo containerlab destroy -t $(CONTAINERLAB_TOPOLOGY) --cleanup
	@echo "Removing host bridge 'vlan-switch'..."
	sudo ip link delete vlan-switch 2>/dev/null || true
	$(MAKE) clab-clean-ports

## kind-create: create a kind cluster with no CNI and DRA feature-gates enabled.
kind-create: kind-check-deps
	@set -e; cfg=$(KIND_CONFIG); \
	if [ "$(IP_FAMILY)" = dual ]; then \
		cfg=$$(mktemp --suffix=.yaml); \
		sed -e 's|^networking:|networking:\n  ipFamily: dual|' \
			-e 's|podSubnet: "$(POD_CIDR_V4)"|podSubnet: "$(POD_CIDR_V4),$(KIND_POD_CIDR_V6)"|' \
			-e 's|serviceSubnet: "$(SVC_CIDR_V4)"|serviceSubnet: "$(SVC_CIDR_V4),$(SVC_CIDR_V6)"|' $(KIND_CONFIG) > $$cfg; \
		grep -q 'ipFamily: dual' $$cfg && grep -q '$(KIND_POD_CIDR_V6)' $$cfg && grep -q '$(SVC_CIDR_V6)' $$cfg \
			|| { echo "could not derive a dual-stack config from $(KIND_CONFIG)"; exit 1; }; \
	fi; \
	echo "kind create cluster --name $(KIND_CLUSTER_NAME) --config $$cfg --image $(KIND_NODE_IMAGE)"; \
	kind create cluster --name $(KIND_CLUSTER_NAME) --config $$cfg --image $(KIND_NODE_IMAGE)
	@echo "Waiting for API server to be reachable..."
	until kubectl cluster-info --context kind-$(KIND_CLUSTER_NAME) >/dev/null 2>&1; do sleep 2; done
	@echo "Waiting for control-plane static pods (apiserver, scheduler, etcd)..."
	kubectl -n kube-system wait --for=condition=Ready pod \
		-l tier=control-plane --timeout=120s
	@echo "NRI is enabled by default in containerd v2.x (kind v1.35+) — no patching needed."
	@echo "NOTE: nodes will remain NotReady until kube-ovn is deployed (expected)."

## kind-delete: destroy the demo cluster.
kind-delete:
	kind delete cluster --name $(KIND_CLUSTER_NAME)

## kind-preload-images: pull (if missing) and load kube-ovn + netshoot images into the
## kind cluster so deployment and demo pods never pull from a remote registry at runtime.
kind-preload-images:
	@set -e; \
	for img in $(PRELOAD_IMAGES); do \
		if $(CONTAINER_TOOL) image inspect "$$img" >/dev/null 2>&1; then \
			echo "Image already present locally: $$img"; \
		else \
			echo "Pulling $$img ..."; \
			$(CONTAINER_TOOL) pull "$$img"; \
		fi; \
		echo "Loading $$img into kind cluster $(KIND_CLUSTER_NAME) ..."; \
		kind load docker-image "$$img" --name $(KIND_CLUSTER_NAME); \
	done
	@echo "Preloaded images: $(PRELOAD_IMAGES)"

## kind-deploy-kube-ovn: install kube-ovn CNI into the demo cluster via its own Helm chart.
kind-deploy-kube-ovn: kind-preload-images
	@set -e; \
	MASTER_NODE=$$(kubectl get nodes -l node-role.kubernetes.io/control-plane \
		-o jsonpath='{.items[0].metadata.name}'); \
	MASTER_IP=$$(kubectl get nodes -l node-role.kubernetes.io/control-plane \
		-o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}' | cut -d' ' -f1); \
	echo "Control-plane node: $$MASTER_NODE ($$MASTER_IP)"; \
	echo "Labelling control-plane node with kube-ovn/role=master ..."; \
	kubectl label node $$MASTER_NODE kube-ovn/role=master --overwrite; \
	CHART_TMP=$$(mktemp -d); \
	if git -C "$(KUBE_OVN_REPO)" cat-file -e "$(KUBE_OVN_VERSION):charts/kube-ovn/Chart.yaml" 2>/dev/null; then \
		echo "Extracting kube-ovn chart $(KUBE_OVN_VERSION) from local repo $(KUBE_OVN_REPO) ..."; \
		git -C "$(KUBE_OVN_REPO)" archive "$(KUBE_OVN_VERSION)" charts/kube-ovn \
			| tar -x -C $$CHART_TMP --strip-components=2; \
	else \
		echo "Local chart not found; fetching kube-ovn chart $(KUBE_OVN_VERSION) from GitHub into $$CHART_TMP ..."; \
		curl -fsSL https://github.com/kubeovn/kube-ovn/archive/refs/tags/$(KUBE_OVN_VERSION).tar.gz \
			| tar -xz -C $$CHART_TMP --strip-components=3 "kube-ovn-$(KUBE_OVN_VERSION:v%=%)/charts/kube-ovn"; \
	fi; \
	$(HELM) upgrade --install kube-ovn $$CHART_TMP \
		--namespace kube-system \
		--set global.registry.address=$(KUBE_OVN_REGISTRY) \
		--set global.images.kubeovn.repository=$(KUBE_OVN_IMAGE_REPO) \
		--set global.images.kubeovn.tag=$(KUBE_OVN_IMAGE_TAG) \
		--set global.images.pullPolicy=IfNotPresent \
		--set MASTER_NODES=$$MASTER_IP \
		$(KUBE_OVN_STACK_ARGS) \
		--set func.ENABLE_DRA_NIC=true \
		--timeout=300s --wait; \
	rm -rf $$CHART_TMP
	@echo "Waiting for nodes to become Ready (CNI is now up)..."
	kubectl wait --for=condition=Ready node --all --timeout=180s

## kind-kube-ovn-status: show kube-ovn component health and the default subnet.
kind-kube-ovn-status:
	@echo "=== kube-ovn pods ==="
	kubectl -n kube-system get pods -l app.kubernetes.io/part-of=kube-ovn -o wide 2>/dev/null \
		|| kubectl -n kube-system get pods | grep -E 'ovn|kube-ovn'
	@echo ""
	@echo "=== Nodes ==="
	kubectl get nodes -o wide
	@echo ""
	@echo "=== Subnets ==="
	kubectl get subnets 2>/dev/null || echo "  (kube-ovn CRDs not installed yet)"

## kind-test-kube-ovn: smoke-test the kube-ovn overlay with two pods + pod-to-pod ping.
## Run after kind-deploy-kube-ovn to confirm the CNI works before adding the NIC driver.
kind-test-kube-ovn:
	@echo "Creating two test pods on the default ovn overlay subnet..."
	kubectl apply -f $(CURDIR)/demo/kind/kube-ovn-smoke-test.yaml
	@echo "Waiting for test pods to be Ready..."
	kubectl wait --for=condition=Ready pod/kube-ovn-test-a pod/kube-ovn-test-b --timeout=120s
	$(eval B_IP := $(shell kubectl get pod kube-ovn-test-b -o jsonpath='{.status.podIP}'))
	@echo "Pinging kube-ovn-test-b ($(B_IP)) from kube-ovn-test-a..."
	kubectl exec kube-ovn-test-a -- ping -c 4 -W 2 $(B_IP) && \
		echo "  ✓ kube-ovn overlay pod-to-pod connectivity OK" || \
		echo "  ✗ kube-ovn pod-to-pod ping FAILED"

## kind-test-kube-ovn-clean: remove the kube-ovn smoke-test pods.
kind-test-kube-ovn-clean:
	kubectl delete pod -l app=kube-ovn-test --ignore-not-found

## kind-deploy-multus: install Multus CNI in thick mode.
kind-deploy-multus:
	@echo "Installing Multus $(MULTUS_VERSION) (thick)..."
	kubectl apply -f https://raw.githubusercontent.com/k8snetworkplumbingwg/multus-cni/$(MULTUS_VERSION)/deployments/multus-daemonset-thick.yml
	@echo "Waiting for multus to be ready..."
	kubectl -n kube-system wait --for=condition=Ready pod -l app=multus --timeout=120s

## kind-build-driver: build the NIC DRA driver image and load it into the kind cluster.
kind-build-driver:
	$(CONTAINER_TOOL) build \
		--build-arg GOLANG_VERSION="$(GOLANG_VERSION)" \
		-t $(IMAGE_NAME):dev \
		-f $(CURDIR)/Dockerfile \
		$(CURDIR)
	kind load docker-image $(IMAGE_NAME):dev --name $(KIND_CLUSTER_NAME)

# Who publishes the devices. "central" runs the publisher: one pool per Subnet,
# available on the nodes of the ProviderNetwork for a VLAN Subnet, with the free
# addresses of the Subnet as capacity. "node" lets every kubelet plugin publish a
# pool for its node, with the Subnets it can serve and no capacity. Both modes
# follow the chart value publisher.enabled, whose default is central.
PUBLISH_MODE      ?= central
# Extra helm arguments for kind-deploy-driver, e.g. --set kubeletPlugin.annotateVMIMAC=true
HELM_EXTRA_ARGS   ?=

## kind-deploy-driver: install the NIC DRA kubelet-plugin via Helm, and with
## PUBLISH_MODE=central the publisher.
## Tolerates the control-plane's NoSchedule taint so the demo's 2-node kind
## cluster has the plugin on BOTH nodes — needed for fixtures that pin pods to
## the control-plane (07-vlan-peer-pod.yaml, 08-ovn-peer-pod.yaml), which
## otherwise stay Pending (in node mode: "cannot allocate all claims", no device
## published for that node; in central mode the plugin that prepares the claim
## is missing).
kind-deploy-driver:
	$(HELM) upgrade --install kube-ovn-nic-dra \
		$(CURDIR)/deployments/helm/kube-ovn-dra-driver \
		--namespace kube-system \
		--set deviceProfile=nic \
		--set driverName=$(NIC_DRIVER_NAME) \
		--set publisher.enabled=$(if $(filter central,$(PUBLISH_MODE)),true,false) \
		--set image.repository=$(IMAGE_NAME) \
		--set image.tag=dev \
		--set image.pullPolicy=Never \
		--set-json 'kubeletPlugin.tolerations=[{"key":"node-role.kubernetes.io/control-plane","operator":"Exists","effect":"NoSchedule"}]' \
		$(HELM_EXTRA_ARGS) --wait

## kind-deploy-kube-ovn-fixtures: apply the kube-ovn underlay fixtures
## (ProviderNetwork, VLANs and the VLAN/overlay Subnets) and wait for reconcile.
kind-deploy-kube-ovn-fixtures:
	@echo "Applying kube-ovn provider network, VLANs and subnets..."
	kubectl apply -f $(NIC_DEMO_DIR)/00-kube-ovn-subnets.yaml
	@echo "Waiting 10s for subnet controllers to reconcile..."
	sleep 10
	@$(MAKE) --no-print-directory kind-vlan-tag-workaround

## kind-vlan-tag-workaround: set tag_request on the localnet port of every VLAN
## subnet. OVN 26.03's northd derives Logical_Switch_Port.tag from tag_request,
## and kube-ovn only writes tag, so VLAN subnets lose their 802.1q tag.
kind-vlan-tag-workaround:
	@set -e; \
	ip=$$(kubectl -n kube-system get pod -l app=ovn-central -o jsonpath='{.items[0].status.podIP}'); \
	for pair in $$(kubectl get subnets -o jsonpath='{range .items[?(@.spec.vlan)]}{.metadata.name}={.spec.vlan}{"\n"}{end}'); do \
		subnet=$${pair%%=*}; id=$$(kubectl get vlan $${pair#*=} -o jsonpath='{.spec.id}'); \
		for i in $$(seq 30); do \
			kubectl -n kube-system exec deploy/ovn-central -c ovn-central -- \
				ovn-nbctl --db=tcp:$$ip:6641 set logical_switch_port localnet.$$subnet tag_request=$$id 2>/dev/null && break; \
			sleep 2; \
		done; \
		echo "  localnet.$$subnet tag_request=$$id"; \
	done

## wait-for-nic-devices: wait until the driver publishes every kube-ovn subnet a
## DRA NIC can use as a device: an OVN provider other than the default one and a
## CIDR (join and ovn-default are not published). It picks up new subnets within
## seconds.
.PHONY: wait-for-nic-devices
wait-for-nic-devices:
	@echo "Waiting for the driver to publish all subnets..."
	@for subnet in $$(kubectl get subnets -o jsonpath='{range .items[*]}{.metadata.name} {.spec.provider} {.spec.cidrBlock}{"\n"}{end}' \
			| awk '$$2 != "ovn" && $$2 ~ /\.ovn$$/ && $$3 != "" {print $$1}'); do \
		for i in $$(seq 60); do \
			kubectl get resourceslices -o jsonpath='{range .items[?(@.spec.driver=="$(NIC_DRIVER_NAME)")]}{range .spec.devices[*]}{.name}{"\n"}{end}{end}' \
				| grep -qx "subnet-$$subnet" && break; \
			[ $$i -lt 60 ] || { echo "subnet $$subnet is not published"; exit 1; }; \
			sleep 2; \
		done; \
	done

## kind-deploy-nic-prereqs: apply the subnets the driver and any NIC claim
## depend on — but NOT a specific ResourceClaim/pod. The
## DeviceClass itself (nic.kubeovn.io) is Helm-managed by kind-deploy-driver,
## not applied here. The driver watches subnets, so the order does not matter.
kind-deploy-nic-prereqs: kind-deploy-kube-ovn-fixtures

## kind-deploy-nic-example: prerequisites + the demo ResourceClaim and pod.
kind-deploy-nic-example: kind-deploy-nic-prereqs
	@echo "Applying ResourceClaim..."
	kubectl apply -f $(NIC_DEMO_DIR)/02-resource-claim.yaml
	@echo "Launching demo pod..."
	kubectl apply -f $(NIC_DEMO_DIR)/03-pod.yaml
	@echo ""
	@echo "Demo pod started. Check interfaces with:"
	@echo "  kubectl exec multi-nic-demo -- ip addr"

## nic-example-deploy: deploy a scaling example with COUNT total NICs (2|4|8|16,
## equal VLAN/overlay split) and time pod creation -> Ready. COUNT defaults to 4.
COUNT ?= 4
nic-example-deploy: kind-deploy-kube-ovn-fixtures
	@$(MAKE) --no-print-directory wait-for-nic-devices
	@echo "Deploying $(COUNT)-NIC example (nic-demo-$(COUNT))..."
	kubectl apply -f $(NIC_DEMO_DIR)/examples/$(COUNT)nic.yaml
	@start=$$(date +%s); \
	kubectl wait --for=condition=Ready pod/nic-demo-$(COUNT) --timeout=300s; \
	end=$$(date +%s); \
	echo "=== nic-demo-$(COUNT): creation -> Ready in $$((end-start))s ==="
	kubectl exec nic-demo-$(COUNT) -- ip -br addr 2>/dev/null | grep -E 'net|eth0' || true

## nic-example-clean: delete the COUNT-NIC example pod and claim.
nic-example-clean:
	kubectl delete -f $(NIC_DEMO_DIR)/examples/$(COUNT)nic.yaml --ignore-not-found

.PHONY: nic-bench nic-bench-sweep nic-bench-clean
## nic-bench: compare DRA vs Multus secondary-NIC spin-up for BENCH_COUNT NICs.
## BENCH_MODE=overlay|underlay|mixed. overlay reuses the demo overlay subnets;
## underlay/mixed create benchmark subnets (need `make clab-deploy` for eth1) and
## wait until the plugin publishes them. Requires Multus (make
## kind-deploy-multus) and the driver running. Measures create -> Ready wall-clock.
nic-bench:
	@command -v python3 >/dev/null || { echo "python3 is required"; exit 1; }
	@set -e; OUT=$(BENCH_DIR); \
	HOTPLUG_BASE=$(HOTPLUG_BASE) python3 $(NIC_DEMO_DIR)/examples/generate.py bench --mode $(BENCH_MODE) --count $(BENCH_COUNT) --out $$OUT; \
	if [ -f $$OUT/subnets.yaml ]; then \
	  echo "==> applying benchmark subnets ($(BENCH_MODE))"; \
	  echo "    (left in place afterward, not deleted here — nic-bench-sweep reuses"; \
	  echo "     overlapping VLAN/subnet IDs across counts, and deleting-then-immediately-"; \
	  echo "     recreating them from the next count races kube-ovn's async CR deletion;"; \
	  echo "     run 'make nic-bench-clean' when done benchmarking)"; \
	  kubectl apply -f $$OUT/subnets.yaml; sleep 10; \
	  $(MAKE) --no-print-directory wait-for-nic-devices; \
	fi; \
	measure() { \
	  pod=$$1; file=$$2; \
	  kubectl delete -f $$file --ignore-not-found --wait=true >/dev/null 2>&1 || true; \
	  s=$$(date +%s); kubectl apply -f $$file >/dev/null; \
	  kubectl wait --for=condition=Ready pod/$$pod --timeout=$(BENCH_TIMEOUT) >/dev/null; \
	  e=$$(date +%s); echo $$((e-s)); \
	  kubectl delete -f $$file --ignore-not-found --wait=true >/dev/null 2>&1 || true; \
	}; \
	median() { \
	  printf '%s\n' "$$@" | sort -n | awk '{a[NR]=$$1} END{n=NR; if(n%2==1) printf "%d", a[(n+1)/2]; else printf "%.1f", (a[n/2]+a[n/2+1])/2}'; \
	}; \
	dra_vals=""; mu_vals=""; hp_base_vals=""; hp_hot_vals=""; \
	for rep in $$(seq 1 $(BENCH_REPS)); do \
	  echo "==> [$$rep/$(BENCH_REPS)] timing DRA ($(BENCH_COUNT) NICs, $(BENCH_MODE))"; \
	  d=$$(measure bench-dra $$OUT/dra.yaml); dra_vals="$$dra_vals $$d"; \
	  echo "==> [$$rep/$(BENCH_REPS)] timing Multus ($(BENCH_COUNT) NICs, $(BENCH_MODE))"; \
	  m=$$(measure bench-multus $$OUT/multus.yaml); mu_vals="$$mu_vals $$m"; \
	  if [ -f $$OUT/multus-hotplug.yaml ]; then \
	    echo "==> [$$rep/$(BENCH_REPS)] timing Multus+hotplug ($(HOTPLUG_BASE)->$(BENCH_COUNT) NICs, $(BENCH_MODE))"; \
	    kubectl delete -f $$OUT/multus-hotplug.yaml --ignore-not-found --wait=true >/dev/null 2>&1 || true; \
	    s=$$(date +%s); kubectl apply -f $$OUT/multus-hotplug.yaml >/dev/null; \
	    kubectl wait --for=condition=Ready pod/bench-multus-hotplug --timeout=$(BENCH_TIMEOUT) >/dev/null; \
	    e=$$(date +%s); hb=$$((e-s)); hp_base_vals="$$hp_base_vals $$hb"; \
	    full=$$(cat $$OUT/multus-hotplug-full-networks.txt); \
	    s=$$(date +%s); \
	    kubectl annotate pod bench-multus-hotplug k8s.v1.cni.cncf.io/networks="$$full" --overwrite >/dev/null; \
	    until [ "$$(kubectl exec bench-multus-hotplug -- sh -c 'ip -o link show 2>/dev/null | grep -c "^[0-9]*: net"' 2>/dev/null || echo 0)" -ge "$(BENCH_COUNT)" ]; do \
	      sleep 1; \
	      now=$$(date +%s); \
	      if [ $$((now-s)) -gt $(patsubst %s,%,$(BENCH_TIMEOUT)) ]; then echo "hot-plug timed out waiting for $(BENCH_COUNT) interfaces" >&2; break; fi; \
	    done; \
	    e=$$(date +%s); hh=$$((e-s)); hp_hot_vals="$$hp_hot_vals $$hh"; \
	    kubectl delete -f $$OUT/multus-hotplug.yaml --ignore-not-found --wait=true >/dev/null 2>&1 || true; \
	  fi; \
	done; \
	printf "\n=== secondary-NIC spin-up: %s NICs (%s), median of %s run(s) ===\n" "$(BENCH_COUNT)" "$(BENCH_MODE)" "$(BENCH_REPS)"; \
	printf "  DRA              : %ss  (raw:%s)\n" "$$(median $$dra_vals)" "$$dra_vals"; \
	printf "  Multus (static)  : %ss  (raw:%s)\n" "$$(median $$mu_vals)" "$$mu_vals"; \
	if [ -n "$$hp_hot_vals" ]; then \
	  printf "  Multus+hotplug   : %ss create->Ready(%s NICs) + %ss hot-plug(->%s NICs)  (raw base:%s hot:%s)\n" \
	    "$$(median $$hp_base_vals)" "$(HOTPLUG_BASE)" "$$(median $$hp_hot_vals)" "$(BENCH_COUNT)" "$$hp_base_vals" "$$hp_hot_vals"; \
	fi

## nic-bench-sweep: run nic-bench across BENCH_COUNTS (e.g. BENCH_COUNTS=\"2 4 8 16\").
nic-bench-sweep:
	@for c in $(BENCH_COUNTS); do \
		$(MAKE) --no-print-directory nic-bench BENCH_COUNT=$$c BENCH_MODE=$(BENCH_MODE); \
	done

## nic-bench-clean: delete benchmark subnets/vlans/pods left behind by nic-bench
## (underlay/mixed modes don't auto-delete between runs — see nic-bench comment).
## Deletes Subnets first and waits for them to actually terminate before deleting
## their Vlans: kube-ovn-controller's subnet-deletion finalizer needs the Vlan to
## still exist to finish cleanup, so deleting both at once (e.g. a single
## `kubectl delete -f subnets.yaml`) can permanently deadlock the Subnet in
## Terminating (needs a manual `kubectl patch ... finalizers:[]` to recover).
nic-bench-clean:
	kubectl delete pod bench-dra bench-multus bench-multus-hotplug --ignore-not-found
	kubectl delete resourceclaim bench-dra bench-multus bench-multus-hotplug --ignore-not-found
	@if [ -f $(BENCH_DIR)/subnets.yaml ]; then \
	  kubectl delete subnet -l bench=nic-dra --ignore-not-found --wait=true --timeout=60s; \
	  kubectl delete vlan -l bench=nic-dra --ignore-not-found; \
	fi

## kind-deploy-vlan-peer: deploy per-VLAN peer pods on the control-plane node.
kind-deploy-vlan-peer: kind-deploy-kube-ovn-fixtures
	@$(MAKE) --no-print-directory wait-for-nic-devices
	kubectl apply -f $(NIC_DEMO_DIR)/07-vlan-peer-pod.yaml
	@echo "Waiting for all VLAN peers + worker pod to be Ready..."
	kubectl wait --for=condition=Ready pod -l app=vlan-peer-test --timeout=300s

## kind-test-vlan: multi-VLAN tagged-traffic + isolation test across all demo VLANs.
##
## Topology (deployed by kind-deploy-vlan-peer):
##   vlan<N>-peer (control-plane)  net1 on vlan<N>-subnet
##   vlan-worker  (worker)         net1..netK across all VLAN subnets
##   frr-gw (host-net)             .253 gateway per VLAN
##
## Tests, looped over every VLAN (100..800):
##   1. Cross-node ping: vlan-worker netI -> vlan<I00>-peer (tagged traffic over the trunk).
##   2. VLAN isolation: vlan-worker net1 (VLAN100) must NOT reach the VLAN200 peer.
##   3. 802.1q tag capture on the bridge.
##   4. Bridge VLAN table + host VLAN routes.
VLAN_IDS ?= 100 200 300 400 500 600 700 800
kind-test-vlan: kind-deploy-vlan-peer
	@echo ""
	@echo "=== Cross-node tagged-traffic ping per VLAN (vlan-worker -> vlan<N>-peer) ==="
	@i=1; for vid in $(VLAN_IDS); do \
		peer_ip=$$(kubectl exec vlan$$vid-peer -- ip -4 addr show net1 2>/dev/null | awk '/inet /{split($$2,a,"/");print a[1]}'); \
		if [ -z "$$peer_ip" ]; then echo "  VLAN $$vid: peer IP not found, skipping"; i=$$((i+1)); continue; fi; \
		if kubectl exec vlan-worker -- ping -c 2 -W 2 -I net$$i $$peer_ip >/dev/null 2>&1; then \
			echo "  ✓ VLAN $$vid: vlan-worker net$$i -> vlan$$vid-peer ($$peer_ip) OK"; \
		else \
			echo "  ✗ VLAN $$vid: vlan-worker net$$i -> vlan$$vid-peer ($$peer_ip) FAILED"; \
		fi; \
		i=$$((i+1)); \
	done
	@echo ""
	@echo "=== VLAN isolation (vlan-worker net1/VLAN100 must NOT reach VLAN200 peer) ==="
	@v200_ip=$$(kubectl exec vlan200-peer -- ip -4 addr show net1 2>/dev/null | awk '/inet /{split($$2,a,"/");print a[1]}'); \
	if kubectl exec vlan-worker -- ping -c 2 -W 1 -I net1 $$v200_ip >/dev/null 2>&1; then \
		echo "  ✗ ISOLATION BROKEN: VLAN100 reached VLAN200 peer ($$v200_ip)"; \
	else \
		echo "  ✓ VLAN isolation confirmed: VLAN100 cannot reach VLAN200 peer ($$v200_ip)"; \
	fi
	@echo ""
	@echo "=== 802.1q tag capture on bridge (2s) ==="
	-sudo timeout 2 tcpdump -i vlan-switch -e -c 10 'vlan' 2>/dev/null || \
		echo "  (no tagged frames captured in window — re-run after pings)"
	@echo ""
	@echo "=== Bridge VLAN table ==="
	-bridge vlan show
	@echo ""
	@echo "=== Host routing table (VLAN subnets, connected through frr-gw's sub-interfaces) ==="
	-ip route show | grep -E '172\.(2[3-9]|3[01])\.' || echo "  (no VLAN routes — is frr-gw running?)"
	@echo ""
	@echo "=== Multi-VLAN test complete ==="

## kind-demo: full end-to-end: create cluster, wire eth1 via containerlab, deploy everything.
## kind-demo-2workers: a demo cluster with two workers, e.g. for live migration.
## Only the workers are wired to the VLAN switch (port2, port4), the provider
## network excludes the control plane, so VLAN pools select only the workers.
## No Multus (only needed for the Multus comparison and the benchmark).
TWO_WORKER_VARS = KIND_CONFIG=$(CURDIR)/demo/kind/kind-no-cni-2workers.yaml \
	CONTAINERLAB_TOPOLOGY=$(CURDIR)/demo/containerlab/vlan-topology-2workers.yaml
kind-demo-2workers:
	$(MAKE) kind-create clab-deploy kind-deploy-kube-ovn kind-deploy-nic-prereqs \
		kind-build-driver kind-deploy-driver kind-deploy-nic-example $(TWO_WORKER_VARS)
	kubectl patch provider-network external --type merge \
		-p '{"spec":{"excludeNodes":["$(KIND_CLUSTER_NAME)-control-plane"]}}'
	@for i in $$(seq 1 30); do \
	  r=$$(kubectl get provider-network external -o jsonpath='{.status.readyNodes}'); \
	  echo "$$r" | grep -q control-plane || break; sleep 2; done; \
	echo "provider network external ready on: $$r"

## KubeVirt with the kube-ovn-nic binding as a KubeVirt Plugin (VEP-190, alpha):
## a CEL hook adds the DRA NIC to the domain (deployments/kubevirt), so no binding
## sidecar is needed. The driver must run with kubeletPlugin.annotateVMIMAC=true,
## e.g. HELM_EXTRA_ARGS="--set kubeletPlugin.annotateVMIMAC=true".
# The latest KubeVirt release; v1.10.0-beta.0 works too.
KUBEVIRT_VERSION ?= v1.9.0
CDI_VERSION      ?= v1.66.1
KUBEVIRT_DIR     ?= $(CURDIR)/deployments/kubevirt
KUBEVIRT_RELEASE  = https://github.com/kubevirt/kubevirt/releases/download/$(KUBEVIRT_VERSION)
# deployments/kubevirt/plugin.yaml has the v1.9 form of the Plugin API. From v1.10
# on the hook list is called launcherHooks and each hook names its hookPoint; the
# CEL expression is the same.
KUBEVIRT_PLUGIN_SED = $(if $(filter v1.9.%,$(KUBEVIRT_VERSION)),-e '',-e 's/^  domainHooks:/  launcherHooks:/' -e '/^    - cel:$$/a\        hookPoint: GuestDefinition')
CDI_RELEASE       = https://github.com/kubevirt/containerized-data-importer/releases/download/$(CDI_VERSION)

## kind-deploy-kubevirt: install KubeVirt, register the kube-ovn-nic binding
## without a sidecar and apply the Plugin. Without /dev/kvm the VMs are emulated.
.PHONY: kind-deploy-kubevirt
kind-deploy-kubevirt:
	@# the v1.10 CRDs are too large for the last-applied annotation of a client-side apply
	kubectl apply --server-side --force-conflicts -f $(KUBEVIRT_RELEASE)/kubevirt-operator.yaml
	kubectl wait --for=condition=Established crd/kubevirts.kubevirt.io --timeout=120s
	kubectl apply -f $(KUBEVIRT_RELEASE)/kubevirt-cr.yaml
	kubectl patch kubevirt kubevirt -n kubevirt --type merge --patch-file $(KUBEVIRT_DIR)/kubevirt-config.yaml
	@[ -c /dev/kvm ] || kubectl patch kubevirt kubevirt -n kubevirt --type merge \
		-p '{"spec":{"configuration":{"developerConfiguration":{"useEmulation":true}}}}'
	kubectl wait --for=condition=Available -n kubevirt kv/kubevirt --timeout=600s
	@# virt-operator installs the Plugin CRD once the Plugins feature gate is on
	@for i in $$(seq 1 60); do kubectl get crd plugins.plugin.kubevirt.io >/dev/null 2>&1 && break; sleep 5; done
	kubectl wait --for=condition=Established crd/plugins.plugin.kubevirt.io --timeout=120s
	sed $(KUBEVIRT_PLUGIN_SED) $(KUBEVIRT_DIR)/plugin.yaml | kubectl apply -f -

## kind-deploy-cdi: install CDI, for VMs with DataVolume disks.
.PHONY: kind-deploy-cdi
kind-deploy-cdi:
	kubectl apply -f $(CDI_RELEASE)/cdi-operator.yaml
	kubectl wait --for=condition=Established crd/cdis.cdi.kubevirt.io --timeout=120s
	kubectl apply -f $(CDI_RELEASE)/cdi-cr.yaml
	kubectl wait --for=condition=Available cdi/cdi --timeout=600s

## kind-deploy-nfs: NFS-Ganesha as an in-cluster RWX provisioner, for VM disks
## that must live-migrate. Use the storage class nfs4 (NFS 4.1): with the
## chart's default nfs class (NFSv3), QEMU's image lock fails intermittently.
.PHONY: kind-deploy-nfs
kind-deploy-nfs:
	$(HELM) repo add nfs-ganesha-server-and-external-provisioner https://kubernetes-sigs.github.io/nfs-ganesha-server-and-external-provisioner/ 2>/dev/null || true
	$(HELM) upgrade --install nfs-ganesha nfs-ganesha-server-and-external-provisioner/nfs-server-provisioner \
		-n nfs-ganesha --create-namespace \
		--set persistence.enabled=true --set persistence.storageClass=standard --set persistence.size=15Gi \
		--set storageClass.name=nfs --set storageClass.defaultClass=false \
		--wait --timeout 300s
	printf '%s\n' 'apiVersion: storage.k8s.io/v1' 'kind: StorageClass' 'metadata:' '  name: nfs4' \
		'provisioner: cluster.local/nfs-ganesha-nfs-server-provisioner' 'reclaimPolicy: Delete' \
		'volumeBindingMode: Immediate' 'allowVolumeExpansion: true' 'mountOptions:' '  - vers=4.1' | kubectl apply -f -

## kind-delete-nfs: remove NFS-Ganesha. Delete the VMs and PVCs on nfs/nfs4
## first: if the server goes before its clients, their mounts hang and the kind
## nodes cannot be deleted anymore.
.PHONY: kind-delete-nfs
kind-delete-nfs:
	@left=$$(kubectl get pvc -A -o jsonpath='{range .items[?(@.spec.storageClassName=="nfs4")]}{.metadata.namespace}/{.metadata.name} {end}{range .items[?(@.spec.storageClassName=="nfs")]}{.metadata.namespace}/{.metadata.name} {end}'); \
	if [ -n "$$left" ]; then echo "PVCs still on NFS, delete them (and their VMs) first: $$left"; exit 1; fi
	kubectl delete storageclass nfs4 --ignore-not-found
	$(HELM) uninstall nfs-ganesha -n nfs-ganesha --wait || true
	kubectl delete namespace nfs-ganesha --ignore-not-found --wait=true

kind-demo: kind-create clab-deploy kind-deploy-kube-ovn kind-deploy-multus kind-deploy-nic-prereqs kind-build-driver kind-deploy-driver kind-deploy-nic-example
	@echo ""
	@echo "=== kube-ovn NIC DRA demo is running ==="
	@echo "Cluster:  $(KIND_CLUSTER_NAME)"
	@echo "Pod:      kubectl exec -it multi-nic-demo -- bash"
	@echo "Scaling:  make nic-example-deploy COUNT=8   (2|4|8|16)"
	@echo "Teardown: make kind-delete"

###############################################################################

setup-e2e:
	test/e2e/setup-e2e.sh

test-e2e:
	go run github.com/onsi/ginkgo/v2/ginkgo --tags=e2e ./test/e2e/...

teardown-e2e:
	test/e2e/teardown-e2e.sh

# Generate an image for containerized builds
# Note: This image is local only
.PHONY: .build-image
.build-image: docker/Dockerfile.devel
	if [ x"$(SKIP_IMAGE_BUILD)" = x"" ]; then \
		$(CONTAINER_TOOL) build \
			--progress=plain \
			--build-arg GOLANG_VERSION="$(GOLANG_VERSION)" \
			--tag $(BUILDIMAGE) \
			-f $(^) \
			docker; \
	fi

ifeq ($(CONTAINER_TOOL),podman)
CONTAINER_TOOL_OPTS=-v $(PWD):$(PWD):Z
else
CONTAINER_TOOL_OPTS=-v $(PWD):$(PWD):z --user $$(id -u):$$(id -g)
endif

$(DOCKER_TARGETS): docker-%: .build-image
	@echo "Running 'make $(*)' in container $(BUILDIMAGE)"
	$(CONTAINER_TOOL) run \
		--rm \
		-e HOME=$(PWD) \
		-e GOCACHE=$(PWD)/.cache/go \
		-e GOPATH=$(PWD)/.cache/gopath \
		$(CONTAINER_TOOL_OPTS) \
		-w $(PWD) \
		$(BUILDIMAGE) \
			make $(*)

# Start an interactive shell using the development image.
.PHONY: .shell
.shell:
	$(CONTAINER_TOOL) run \
		--rm \
		-ti \
		-e HOME=$(PWD) \
		-e GOCACHE=$(PWD)/.cache/go \
		-e GOPATH=$(PWD)/.cache/gopath \
		$(CONTAINER_TOOL_OPTS) \
		-w $(PWD) \
		$(BUILDIMAGE)

# Container images are built and pushed by .github/workflows/image.yaml.
.PHONY: push-chart
push-chart:
	CHART_VERSION="$${CHART_GIT_TAG##chart/}" \
		HELM=$(HELM) \
		demo/scripts/push-driver-chart.sh

## print-%: print a make variable, e.g. make -s print-KUBE_OVN_IMAGE (used by CI).
print-%:
	@echo '$($*)'
