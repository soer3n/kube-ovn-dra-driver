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

package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	drapb "k8s.io/kubelet/pkg/apis/dra/v1"
	registerapi "k8s.io/kubelet/pkg/apis/pluginregistration/v1"
)

type fakeRegistration struct{ registerapi.RegistrationClient }

func (fakeRegistration) GetInfo(context.Context, *registerapi.InfoRequest, ...grpc.CallOption) (*registerapi.PluginInfo, error) {
	return &registerapi.PluginInfo{}, nil
}

type fakeDRAPlugin struct{ drapb.DRAPluginClient }

func (fakeDRAPlugin) NodePrepareResources(context.Context, *drapb.NodePrepareResourcesRequest, ...grpc.CallOption) (*drapb.NodePrepareResourcesResponse, error) {
	return &drapb.NodePrepareResourcesResponse{}, nil
}

// The readiness service follows NRI; liveness does not, so a disconnected
// runtime does not restart the plugin.
func TestHealthcheckReadiness(t *testing.T) {
	ready := false
	h := &healthcheck{regClient: fakeRegistration{}, draClient: fakeDRAPlugin{}, ready: func() bool { return ready }}
	check := func(service string) grpc_health_v1.HealthCheckResponse_ServingStatus {
		t.Helper()
		resp, err := h.Check(context.Background(), &grpc_health_v1.HealthCheckRequest{Service: service})
		require.NoError(t, err)
		return resp.GetStatus()
	}

	assert.Equal(t, grpc_health_v1.HealthCheckResponse_NOT_SERVING, check(healthServiceReadiness))
	assert.Equal(t, grpc_health_v1.HealthCheckResponse_SERVING, check(healthServiceLiveness))
	ready = true
	assert.Equal(t, grpc_health_v1.HealthCheckResponse_SERVING, check(healthServiceReadiness))

	_, err := h.Check(context.Background(), &grpc_health_v1.HealthCheckRequest{Service: testOtherRequest})
	assert.Equal(t, codes.NotFound, status.Code(err))
}
