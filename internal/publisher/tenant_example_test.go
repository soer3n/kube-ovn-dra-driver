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

package publisher

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
	dracel "k8s.io/dynamic-resource-allocation/cel"
)

const (
	tenantExample      = "../../demo/nic-example/examples/tenant-deviceclasses.yaml"
	exampleDriver      = "nic.kubeovn.io"
	exampleTenantA     = "tenant-a.nic.kubeovn.io"
	exampleTenantB     = "tenant-b.nic.kubeovn.io"
	exampleKindClass   = "DeviceClass"
	exampleVPC         = "tenant-a"
	exampleDefaultVPC  = "ovn-cluster"
	exampleSubnetA     = "tenant-a-1"
	exampleSubnetB     = "tenant-b-1"
	exampleSubnetOther = "other"
)

// exampleClasses reads the DeviceClasses of the tenant example.
func exampleClasses(t *testing.T) map[string]resourceapi.DeviceClass {
	t.Helper()
	f, err := os.Open(tenantExample)
	require.NoError(t, err)
	defer f.Close()
	decoder := yaml.NewYAMLOrJSONDecoder(f, 4096)
	classes := map[string]resourceapi.DeviceClass{}
	for {
		var class resourceapi.DeviceClass
		err := decoder.Decode(&class)
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		if class.Kind == exampleKindClass {
			classes[class.Name] = class
		}
	}
	return classes
}

// The selectors of the example's tenant classes match exactly their tenant's
// subnets and evaluate without error on every device, including one without a
// vpc attribute: in DRA a CEL error fails the allocation instead of skipping
// the device.
func TestTenantExampleSelectors(t *testing.T) {
	classes := exampleClasses(t)
	require.Len(t, classes, 2)
	subnets := map[string]Subnet{
		exampleSubnetA:     {Name: exampleSubnetA, Provider: exampleSubnetA + ".default.ovn", VPC: exampleVPC},
		exampleSubnetB:     {Name: exampleSubnetB, Provider: exampleSubnetB + ".default.ovn", VPC: exampleDefaultVPC},
		exampleSubnetOther: {Name: exampleSubnetOther},
	}
	want := map[string]string{exampleTenantA: exampleSubnetA, exampleTenantB: exampleSubnetB}

	compiler := dracel.GetCompiler(dracel.Features{EnableConsumableCapacity: true})
	for className, wantSubnet := range want {
		class, ok := classes[className]
		require.True(t, ok, "class %s", className)
		for name, subnet := range subnets {
			device := Device(subnet, true)
			matches := true
			for _, selector := range class.Spec.Selectors {
				result := compiler.CompileCELExpression(selector.CEL.Expression, dracel.Options{})
				require.Nil(t, result.Error, "class %s", className)
				match, _, err := result.DeviceMatches(context.Background(), dracel.Device{
					Driver:                   exampleDriver,
					AllowMultipleAllocations: device.AllowMultipleAllocations,
					Attributes:               device.Attributes,
					Capacity:                 device.Capacity,
				})
				require.NoError(t, err, "class %s on subnet %s", className, name)
				matches = matches && match
			}
			require.Equal(t, name == wantSubnet, matches, "class %s on subnet %s", className, name)
		}
	}
}
