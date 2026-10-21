#!/usr/bin/env bash
# Copyright The Kubernetes Authors.
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

# Brings up the e2e cluster for `make test-e2e`. Everything is driven through the
# Makefile's kind-*/kube-ovn targets, so all cluster and kube-ovn Helm values are
# sourced from a single place (the Makefile) — this script only orders them.
#
# Set E2E_CONTAINERLAB=1 to also wire the VLAN underlay (needs sudo + containerlab),
# which enables the Label("containerlab") specs.
#
# Set E2E_KUBEVIRT=1 for the Label("kubevirt") live migration specs: a cluster with
# two workers and KubeVirt, where a KubeVirt Plugin adds the DRA NIC to the domain
# (deployments/kubevirt, no binding sidecar) from the MAC the driver writes on the
# VMI (kubeletPlugin.annotateVMIMAC). E2E_RWX=1 also installs CDI and NFS-Ganesha
# with the NFS 4.1 storage class nfs4 for the Label("rwx") DataVolume spec. Without
# /dev/kvm, KubeVirt runs the VMs in software emulation. With E2E_CONTAINERLAB=1 as
# well, only the two workers are wired to the VLAN switch, like make
# kind-demo-2workers, and the provider network excludes the control plane.
#
# Set E2E_IP_FAMILY=dual for a dual-stack cluster (kind and kube-ovn), which
# enables the Label("dual") specs.
#
# Set E2E_PUBLISH_MODE=node to deploy the driver in node publish mode, where every
# kubelet plugin publishes the pool of its node, instead of central mode. The
# node mode specs then run instead of the central pool and capacity specs.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

export IP_FAMILY="${E2E_IP_FAMILY:-ipv4}"
export PUBLISH_MODE="${E2E_PUBLISH_MODE:-central}"

if [ "${E2E_KUBEVIRT:-0}" = "1" ]; then
  export KIND_CONFIG="$ROOT/demo/kind/kind-no-cni-2workers.yaml"
  export CONTAINERLAB_TOPOLOGY="$ROOT/demo/containerlab/vlan-topology-2workers.yaml"
  export HELM_EXTRA_ARGS="${HELM_EXTRA_ARGS:-} --set kubeletPlugin.annotateVMIMAC=true"
fi

make kind-create
if [ "${E2E_CONTAINERLAB:-0}" = "1" ]; then
  make clab-deploy
fi
make kind-deploy-kube-ovn
make kind-deploy-nic-prereqs
if [ "${E2E_KUBEVIRT:-0}" = "1" ] && [ "${E2E_CONTAINERLAB:-0}" = "1" ]; then
  # the control plane has no VLAN uplink in the two-worker topology
  kubectl patch provider-network external --type merge \
    -p "{\"spec\":{\"excludeNodes\":[\"${KIND_CLUSTER_NAME:-nic-dra-demo}-control-plane\"]}}"
fi
make kind-build-driver
make kind-deploy-driver
make wait-for-nic-devices

if [ "${E2E_KUBEVIRT:-0}" = "1" ]; then
  make kind-deploy-kubevirt
  if [ "${E2E_RWX:-0}" = "1" ]; then
    make kind-deploy-cdi kind-deploy-nfs
  fi
fi

# The e2e suite applies/removes its own ResourceClaim+pod fixture, so the base
# demo claim/pod (kind-deploy-nic-example) is intentionally NOT deployed here.

echo "e2e cluster ready. Run: make test-e2e"
