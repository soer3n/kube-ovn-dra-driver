/*
Copyright 2025 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	admissionv1 "k8s.io/api/admission/v1"
	resourceapi "k8s.io/api/resource/v1"
	resourcev1beta1 "k8s.io/api/resource/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	configapi "github.com/soer3n/kube-ovn-dra-driver/api/kube-ovn.io/resource/nic/v1alpha1"
	"github.com/soer3n/kube-ovn-dra-driver/internal/profiles/nic"
)

const driverName = "nic.kubeovn.io"

func TestReadyEndpoint(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(readyHandler))
	t.Cleanup(s.Close)

	res, err := http.Get(s.URL)
	assert.NoError(t, err)
	assert.Equal(t, http.StatusOK, res.StatusCode)
}

func TestResourceClaimValidatingWebhook(t *testing.T) {
	unknownResource := metav1.GroupVersionResource{
		Group:    "resource.k8s.io",
		Version:  "v1",
		Resource: "unknownresources",
	}

	validNicConfig := &configapi.NicConfig{
		InterfaceName: "net1",
	}

	// An opaque config carrying an unknown field. The webhook decodes config
	// parameters with strict decoding enabled, so this is rejected before it
	// ever reaches NicConfig.Validate.
	unknownFieldConfig := []byte(`{"apiVersion":"nic.resource.kube-ovn.io/v1alpha1","kind":"NicConfig","bogusField":true}`)

	tests := map[string]struct {
		admissionReview      *admissionv1.AdmissionReview
		requestContentType   string
		expectedResponseCode int
		expectedAllowed      bool
		expectMessageSubstr  string
	}{
		"bad contentType": {
			requestContentType:   "invalid type",
			expectedResponseCode: http.StatusUnsupportedMediaType,
		},
		"invalid AdmissionReview": {
			admissionReview:      &admissionv1.AdmissionReview{},
			expectedResponseCode: http.StatusBadRequest,
		},
		"valid NicConfig in ResourceClaim": {
			admissionReview: admissionReviewWithObject(
				resourceClaimWithNicConfigs(validNicConfig),
				resourceClaimResourceV1,
			),
			expectedAllowed: true,
		},
		"valid NicConfig in ResourceClaimTemplate": {
			admissionReview: admissionReviewWithObject(
				resourceClaimTemplateWithNicConfigs(validNicConfig),
				resourceClaimTemplateResourceV1,
			),
			expectedAllowed: true,
		},
		"valid NicConfig in ResourceClaim v1beta1": {
			admissionReview: admissionReviewWithObject(
				toResourceClaimV1Beta1(resourceClaimWithNicConfigs(validNicConfig)),
				resourceClaimResourceV1Beta1,
			),
			expectedAllowed: true,
		},
		"unknown field in NicConfig rejected": {
			admissionReview: admissionReviewWithObject(
				resourceClaimWithRawConfig(unknownFieldConfig),
				resourceClaimResourceV1,
			),
			expectedAllowed:     false,
			expectMessageSubstr: "spec.devices.config[0].opaque.parameters",
		},
		"unknown resource type": {
			admissionReview: admissionReviewWithObject(
				resourceClaimWithNicConfigs(validNicConfig),
				unknownResource,
			),
			expectedAllowed:     false,
			expectMessageSubstr: "expected resource to be one of",
		},
	}

	configHandler := nic.Profile{}
	mux, err := newMux(configHandler, driverName)
	assert.NoError(t, err)

	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			requestBody, err := json.Marshal(test.admissionReview)
			require.NoError(t, err)

			contentType := test.requestContentType
			if contentType == "" {
				contentType = "application/json"
			}

			res, err := http.Post(s.URL+"/validate-resource-claim-parameters", contentType, bytes.NewReader(requestBody))
			require.NoError(t, err)
			expectedResponseCode := test.expectedResponseCode
			if expectedResponseCode == 0 {
				expectedResponseCode = http.StatusOK
			}
			assert.Equal(t, expectedResponseCode, res.StatusCode)
			if res.StatusCode != http.StatusOK {
				// We don't have an AdmissionReview to validate
				return
			}

			responseBody, err := io.ReadAll(res.Body)
			require.NoError(t, err)
			res.Body.Close()

			responseAdmissionReview, err := readAdmissionReview(responseBody)
			assert.NoError(t, err)
			assert.Equal(t, test.expectedAllowed, responseAdmissionReview.Response.Allowed)
			if !test.expectedAllowed && test.expectMessageSubstr != "" {
				assert.Contains(t, string(responseAdmissionReview.Response.Result.Message), test.expectMessageSubstr)
			}
		})
	}
}

func admissionReviewWithObject(obj runtime.Object, resource metav1.GroupVersionResource) *admissionv1.AdmissionReview {
	requestedAdmissionReview := &admissionv1.AdmissionReview{
		Request: &admissionv1.AdmissionRequest{
			Resource: resource,
			Object: runtime.RawExtension{
				Object: obj,
			},
		},
	}
	requestedAdmissionReview.SetGroupVersionKind(admissionv1.SchemeGroupVersion.WithKind("AdmissionReview"))
	return requestedAdmissionReview
}

func resourceClaimWithNicConfigs(nicConfigs ...*configapi.NicConfig) *resourceapi.ResourceClaim {
	resourceClaim := &resourceapi.ResourceClaim{
		Spec: resourceClaimSpecWithNicConfigs(nicConfigs...),
	}
	resourceClaim.SetGroupVersionKind(resourceapi.SchemeGroupVersion.WithKind("ResourceClaim"))
	return resourceClaim
}

func resourceClaimTemplateWithNicConfigs(nicConfigs ...*configapi.NicConfig) *resourceapi.ResourceClaimTemplate {
	resourceClaimTemplate := &resourceapi.ResourceClaimTemplate{
		Spec: resourceapi.ResourceClaimTemplateSpec{
			Spec: resourceClaimSpecWithNicConfigs(nicConfigs...),
		},
	}
	resourceClaimTemplate.SetGroupVersionKind(resourceapi.SchemeGroupVersion.WithKind("ResourceClaimTemplate"))
	return resourceClaimTemplate
}

func resourceClaimSpecWithNicConfigs(nicConfigs ...*configapi.NicConfig) resourceapi.ResourceClaimSpec {
	resourceClaimSpec := resourceapi.ResourceClaimSpec{}
	for _, nicConfig := range nicConfigs {
		nicConfig.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   configapi.GroupName,
			Version: configapi.Version,
			Kind:    configapi.NicConfigKind,
		})
		deviceConfig := resourceapi.DeviceClaimConfiguration{
			DeviceConfiguration: resourceapi.DeviceConfiguration{
				Opaque: &resourceapi.OpaqueDeviceConfiguration{
					Driver: driverName,
					Parameters: runtime.RawExtension{
						Object: nicConfig,
					},
				},
			},
		}
		resourceClaimSpec.Devices.Config = append(resourceClaimSpec.Devices.Config, deviceConfig)
	}
	return resourceClaimSpec
}

func resourceClaimWithRawConfig(raw []byte) *resourceapi.ResourceClaim {
	resourceClaim := &resourceapi.ResourceClaim{
		Spec: resourceapi.ResourceClaimSpec{
			Devices: resourceapi.DeviceClaim{
				Config: []resourceapi.DeviceClaimConfiguration{
					{
						DeviceConfiguration: resourceapi.DeviceConfiguration{
							Opaque: &resourceapi.OpaqueDeviceConfiguration{
								Driver:     driverName,
								Parameters: runtime.RawExtension{Raw: raw},
							},
						},
					},
				},
			},
		},
	}
	resourceClaim.SetGroupVersionKind(resourceapi.SchemeGroupVersion.WithKind("ResourceClaim"))
	return resourceClaim
}

func toResourceClaimV1Beta1(v1Claim *resourceapi.ResourceClaim) *resourcev1beta1.ResourceClaim {
	v1beta1Claim := &resourcev1beta1.ResourceClaim{}
	if err := scheme.Convert(v1Claim, v1beta1Claim, nil); err != nil {
		panic(fmt.Sprintf("failed to convert ResourceClaim to v1beta1: %v", err))
	}
	return v1beta1Claim
}
