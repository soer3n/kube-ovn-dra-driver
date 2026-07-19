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

ARG GOLANG_VERSION=1.25
FROM golang:${GOLANG_VERSION} AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_LDFLAGS_ALLOW='-Wl,--unresolved-symbols=ignore-in-object-files' \
    CGO_ENABLED=0 GOOS=linux \
    go build -ldflags "-s -w" -o /bin/kube-ovn-dra-kubeletplugin \
    github.com/soer3n/kube-ovn-dra-driver/cmd/kube-ovn-dra-kubeletplugin

# NIC variant: includes ovs-vsctl so the Multus-free attach datapath can manage
# OVS ports. Build with `--target nic`. ovs-vsctl is a client that talks to the
# host ovsdb socket (mounted by the chart when kubeletPlugin.nicAttach.enabled).
FROM debian:stable-slim AS nic
RUN apt-get update \
    && apt-get install -y --no-install-recommends openvswitch-switch iproute2 \
    && rm -rf /var/lib/apt/lists/*
COPY --from=builder /bin/kube-ovn-dra-kubeletplugin /usr/local/bin/kube-ovn-dra-kubeletplugin
ENTRYPOINT ["/usr/local/bin/kube-ovn-dra-kubeletplugin"]

# Default (IPAM-only mode, no host OVS needed): minimal distroless image. Kept
# last so a plain `docker build` with no --target produces this.
FROM gcr.io/distroless/static:nonroot
COPY --from=builder /bin/kube-ovn-dra-kubeletplugin /usr/local/bin/kube-ovn-dra-kubeletplugin
ENTRYPOINT ["/usr/local/bin/kube-ovn-dra-kubeletplugin"]
