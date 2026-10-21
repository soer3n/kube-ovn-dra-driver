/*
 * Copyright 2025 The Kubernetes Authors.
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

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/soer3n/kube-ovn-dra-driver/internal/profiles"
	"github.com/soer3n/kube-ovn-dra-driver/pkg/plumbing"
)

// ovnDHCPField is how a set Spec.OVNDHCP appears in the checkpoint's JSON.
const ovnDHCPField = `"OVNDHCP":true`

// The checkpoint's checksum is verified over the JSON the running version
// encodes, so a Spec field an older version does not know must not change the
// encoding while it is unset: a checkpoint written before the field existed
// still verifies after an upgrade, and the plugin starts.
func TestCheckpointVerifiesWithoutNewerSpecFields(t *testing.T) {
	written := newCheckpoint()
	nic := &plumbing.Spec{PodUID: testPodUID, IfaceName: testIface1, IP: testVMIP}
	written.V1.PreparedClaims[testClaimUID2] = profiles.PreparedDevices{{NIC: nic}}
	data, err := written.MarshalCheckpoint()
	require.NoError(t, err)
	assert.NotContains(t, string(data), "OVNDHCP", "an unset OVNDHCP must encode like a checkpoint without the field")

	read := newCheckpoint()
	require.NoError(t, read.UnmarshalCheckpoint(data))
	require.NoError(t, read.VerifyChecksum())

	nic.OVNDHCP = true
	data, err = written.MarshalCheckpoint()
	require.NoError(t, err)
	assert.Contains(t, string(data), ovnDHCPField)
	read = newCheckpoint()
	require.NoError(t, read.UnmarshalCheckpoint(data))
	require.NoError(t, read.VerifyChecksum())
	assert.True(t, read.V1.PreparedClaims[testClaimUID2][0].NIC.OVNDHCP)
}
