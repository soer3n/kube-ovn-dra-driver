#!/usr/bin/env bash

# Copyright 2023 The Kubernetes Authors.
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

# Shared environment for the release-artifact scripts (build-driver-image.sh,
# push-driver-image.sh, push-driver-chart.sh), invoked by `make
# push-release-artifacts`. The kind cluster lifecycle lives in the Makefile's
# kind-* targets (demo/kind/kind-no-cni.yaml), not here.

# A reference to the current directory where this script is located
SCRIPTS_DIR="$(cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd)"

# The name of the driver
: ${DRIVER_NAME:=kube-ovn-dra-driver}

# The registry, image and tag for the driver
: ${DRIVER_IMAGE_REGISTRY:="ghcr.io/soer3n"}
: ${DRIVER_IMAGE_NAME:="${DRIVER_NAME}"}
: ${DRIVER_IMAGE_TAG:="$(cat $(git rev-parse --show-toplevel)/deployments/helm/${DRIVER_NAME}/Chart.yaml | grep appVersion | sed 's/"//g' | sed -n 's/^appVersion: //p')"}
: ${DRIVER_IMAGE_PLATFORM:="ubuntu22.04"}

# The derived name of the driver image to build
: ${DRIVER_IMAGE:="${DRIVER_IMAGE_REGISTRY}/${DRIVER_IMAGE_NAME}:${DRIVER_IMAGE_TAG}"}

# Container tool, e.g. docker/podman
if [[ -z "${CONTAINER_TOOL}" ]]; then
    if [[ -n "$(which docker)" ]]; then
        echo "Docker found in PATH."
        CONTAINER_TOOL=docker
    elif [[ -n "$(which podman)" ]]; then
        echo "Podman found in PATH."
        CONTAINER_TOOL=podman
    else
        echo "No container tool detected. Please install Docker or Podman."
        return 1
    fi
fi
