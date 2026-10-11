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

// Package nic implements a DRA device profile that exposes kube-ovn Subnets
// as allocatable NIC devices. Each Subnet becomes one device in the
// ResourceSlice for the node. kube-ovn-controller allocates a claimed device's
// address from the Subnet named by its subnetName attribute; the driver then
// plumbs the NIC into the pod.
package nic

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdispec "tags.cncf.io/container-device-interface/specs-go"

	configapi "github.com/soer3n/kube-ovn-dra-driver/api/kube-ovn.io/resource/nic/v1alpha1"
	"github.com/soer3n/kube-ovn-dra-driver/internal/profiles"
	"github.com/soer3n/kube-ovn-dra-driver/internal/publisher"
)

const ProfileName = "nic"

// kube-ovn Subnet GVR.
var subnetGVR = schema.GroupVersionResource{
	Group:    "kubeovn.io",
	Version:  "v1",
	Resource: "subnets",
}

// kube-ovn Vlan GVR. A Subnet references a Vlan by name via spec.vlan; the Vlan
// holds the 802.1q id (spec.id) and the ProviderNetwork name (spec.provider).
var vlanGVR = schema.GroupVersionResource{
	Group:    "kubeovn.io",
	Version:  "v1",
	Resource: "vlans",
}

// Profile implements profiles.Profile for kube-ovn virtual NICs.
type Profile struct {
	nodeName      string
	dynamicClient dynamic.Interface
}

// NewProfile creates a NIC profile. dynamicClient is used to list kube-ovn
// Subnet CRs at enumeration time.
func NewProfile(nodeName string, dynamicClient dynamic.Interface) Profile {
	return Profile{
		nodeName:      nodeName,
		dynamicClient: dynamicClient,
	}
}

// EnumerateDevices implements profiles.Profile.
// It lists all kube-ovn Subnets and exposes each one as a DRA device in the pool
// of the node. This is the layout of the node publish mode, in which every
// kubelet plugin publishes the devices of its node. The central publish mode
// builds its pools with the same publisher package.
func (p Profile) EnumerateDevices() (resourceslice.DriverResources, error) {
	ctx := context.Background()
	subnets, err := p.dynamicClient.Resource(subnetGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return resourceslice.DriverResources{}, fmt.Errorf("list kube-ovn subnets: %w", err)
	}

	// A VLAN underlay subnet does not carry the id on the Subnet itself; it
	// references a Vlan CR via spec.vlan. Resolve that Vlan so claims can
	// select underlay subnets by type, VLAN id or ProviderNetwork.
	vlans := func(name string) (publisher.VlanInfo, error) {
		vlan, err := p.dynamicClient.Resource(vlanGVR).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return publisher.VlanInfo{}, err
		}
		return publisher.ParseVlan(vlan), nil
	}

	var devices []resourceapi.Device
	for i := range subnets.Items {
		subnet := &subnets.Items[i]
		if spec, _ := subnet.Object["spec"].(map[string]interface{}); spec == nil {
			continue
		}
		s, err := publisher.ParseSubnet(subnet, vlans)
		if err != nil {
			return resourceslice.DriverResources{}, err
		}
		devices = append(devices, publisher.Device(s, false))
	}

	resources := resourceslice.DriverResources{
		Pools: map[string]resourceslice.Pool{
			p.nodeName: {
				Slices: []resourceslice.Slice{
					{Devices: devices},
				},
			},
		},
	}
	return resources, nil
}

// WatchDevices implements profiles.Profile. It watches kube-ovn Subnets and
// Vlans and calls onChange when one is added, deleted or its spec changes.
// Status updates, which kube-ovn writes on every address allocation, are
// ignored.
func (p Profile) WatchDevices(ctx context.Context, onChange func()) error {
	factory := dynamicinformer.NewDynamicSharedInformerFactory(p.dynamicClient, 0)
	handler := cache.ResourceEventHandlerFuncs{
		AddFunc: func(any) { onChange() },
		UpdateFunc: func(oldObj, newObj any) {
			if !specEqual(oldObj, newObj) {
				onChange()
			}
		},
		DeleteFunc: func(any) { onChange() },
	}
	var synced []cache.InformerSynced
	for _, gvr := range []schema.GroupVersionResource{subnetGVR, vlanGVR} {
		informer := factory.ForResource(gvr).Informer()
		if _, err := informer.AddEventHandler(handler); err != nil {
			return fmt.Errorf("watch %s: %w", gvr.Resource, err)
		}
		synced = append(synced, informer.HasSynced)
	}
	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), synced...) {
		return fmt.Errorf("sync kube-ovn subnet and vlan informers: %w", ctx.Err())
	}
	return nil
}

// specEqual reports whether two unstructured objects have the same spec.
func specEqual(oldObj, newObj any) bool {
	oldU, ok1 := oldObj.(*unstructured.Unstructured)
	newU, ok2 := newObj.(*unstructured.Unstructured)
	if !ok1 || !ok2 {
		return false
	}
	return equality.Semantic.DeepEqual(oldU.Object["spec"], newU.Object["spec"])
}

// SchemeBuilder implements profiles.ConfigHandler.
func (p Profile) SchemeBuilder() runtime.SchemeBuilder {
	return runtime.NewSchemeBuilder(configapi.AddToScheme)
}

// Validate implements profiles.ConfigHandler.
func (p Profile) Validate(config runtime.Object) error {
	nicConfig, ok := config.(*configapi.NicConfig)
	if !ok {
		return fmt.Errorf("expected v1alpha1.NicConfig but got: %T", config)
	}
	return nicConfig.Validate()
}

// ApplyConfig implements profiles.ConfigHandler.
// For NIC devices, configuration is expressed as environment variables injected
// via CDI that tell the pod the subnet and device behind each interface. They
// are keyed by the interface name, which is unique in the pod, unlike the
// device, which several NICs on one subnet share.
func (p Profile) ApplyConfig(config runtime.Object, results []*resourceapi.DeviceRequestAllocationResult) (profiles.PerDeviceCDIContainerEdits, error) {
	if config == nil {
		config = configapi.DefaultNicConfig()
	}
	nicConfig, ok := config.(*configapi.NicConfig)
	if !ok {
		return nil, fmt.Errorf("runtime object is not a recognized NicConfig")
	}
	if err := nicConfig.Normalize(); err != nil {
		return nil, fmt.Errorf("normalizing NicConfig: %w", err)
	}

	perDeviceEdits := make(profiles.PerDeviceCDIContainerEdits)
	for _, result := range results {
		key := envKey(nicConfig.InterfaceName)
		envs := []string{
			fmt.Sprintf("KUBE_OVN_NIC_%s_SUBNET=%s", key, subnetNameFromDevice(result.Device)),
			fmt.Sprintf("KUBE_OVN_NIC_%s_DEVICE=%s", key, result.Device),
		}
		edits := &cdispec.ContainerEdits{Env: envs}
		perDeviceEdits[profiles.ResultKey(result)] = &cdiapi.ContainerEdits{ContainerEdits: edits}
	}
	return perDeviceEdits, nil
}

var nonEnvChars = regexp.MustCompile(`[^A-Z0-9_]`)

// envKey turns an interface name into an environment variable name part.
func envKey(ifaceName string) string {
	return nonEnvChars.ReplaceAllString(strings.ToUpper(ifaceName), "_")
}

// subnetNameFromDevice strips the "subnet-" prefix from the device name to
// recover the original kube-ovn Subnet name.
func subnetNameFromDevice(deviceName string) string {
	return publisher.SubnetName(deviceName)
}
