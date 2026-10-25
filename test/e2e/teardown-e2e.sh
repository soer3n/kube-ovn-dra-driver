#!/usr/bin/env bash
# Copyright The Kubernetes Authors.
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

# Tears down the e2e cluster created by setup-e2e.sh.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

if [ "${E2E_RWX:-0}" = "1" ]; then
  # NFS clients hang when their server goes first, and the kind nodes with them:
  # remove the VMs and their volumes on NFS (not the server's own volume), then
  # the NFS server
  kubectl delete vm --all -A --wait=true --timeout=5m || true
  kubectl get pvc -A -o jsonpath='{range .items[?(@.spec.storageClassName=="nfs4")]}{.metadata.namespace} {.metadata.name}{"\n"}{end}{range .items[?(@.spec.storageClassName=="nfs")]}{.metadata.namespace} {.metadata.name}{"\n"}{end}' |
    while read -r ns name; do
      kubectl delete pvc -n "$ns" "$name" --wait=true --timeout=5m || true
    done || true
  make kind-delete-nfs || true
fi
if [ "${E2E_CONTAINERLAB:-0}" = "1" ]; then
  if [ "${E2E_KUBEVIRT:-0}" = "1" ]; then
    export CONTAINERLAB_TOPOLOGY="$ROOT/demo/containerlab/vlan-topology-2workers.yaml"
  fi
  make clab-destroy || true
fi
make kind-delete
