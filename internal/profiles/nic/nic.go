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
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/klog/v2"
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

var nodeGVR = schema.GroupVersionResource{Version: "v1", Resource: "nodes"}

// Profile implements profiles.Profile for kube-ovn virtual NICs.
type Profile struct {
	nodeName      string
	dynamicClient dynamic.Interface
	// subnets maps the enumerated device names to their Subnet names. A device
	// name does not always contain the Subnet name (see publisher.DeviceName).
	subnets *sync.Map
}

// NewProfile creates a NIC profile. dynamicClient is used to list kube-ovn
// Subnet CRs at enumeration time.
func NewProfile(nodeName string, dynamicClient dynamic.Interface) Profile {
	return Profile{
		nodeName:      nodeName,
		dynamicClient: dynamicClient,
		subnets:       &sync.Map{},
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

	// A VLAN underlay Subnet only works on the nodes where its ProviderNetwork
	// is ready, which have the provider bridge and the uplink. The central
	// publisher scopes its pool with a node selector; here the node simply
	// does not publish it.
	node, err := p.dynamicClient.Resource(nodeGVR).Get(ctx, p.nodeName, metav1.GetOptions{})
	if err != nil {
		return resourceslice.DriverResources{}, fmt.Errorf("get node %s: %w", p.nodeName, err)
	}
	nodeLabels := node.GetLabels()

	logger := klog.FromContext(ctx)
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
		if err := s.Publishable(); err != nil {
			logger.V(4).Info("Not publishing subnet", "subnet", s.Name, "reason", err)
			continue
		}
		if s.ProviderNetwork != "" && nodeLabels[publisher.ReadyLabel(s.ProviderNetwork)] != "true" {
			logger.V(4).Info("Not publishing subnet, its provider network is not ready on this node",
				"subnet", s.Name, "providerNetwork", s.ProviderNetwork, "node", p.nodeName)
			continue
		}
		device := publisher.Device(s, false)
		p.subnets.Store(device.Name, s.Name)
		devices = append(devices, device)
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
	// The node's labels say on which ProviderNetworks it is ready.
	nodeFactory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(p.dynamicClient, 0, "", func(opts *metav1.ListOptions) {
		opts.FieldSelector = fields.OneTermEqualSelector("metadata.name", p.nodeName).String()
	})
	nodeInformer := nodeFactory.ForResource(nodeGVR).Informer()
	if _, err := nodeInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj any) {
			oldU, ok1 := oldObj.(*unstructured.Unstructured)
			newU, ok2 := newObj.(*unstructured.Unstructured)
			if !ok1 || !ok2 || !equality.Semantic.DeepEqual(oldU.GetLabels(), newU.GetLabels()) {
				onChange()
			}
		},
	}); err != nil {
		return fmt.Errorf("watch node %s: %w", p.nodeName, err)
	}
	synced = append(synced, nodeInformer.HasSynced)
	factory.Start(ctx.Done())
	nodeFactory.Start(ctx.Done())
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

// SubnetOVNDHCP reports whether the Subnet has OVN's DHCP enabled
// (spec.enableDHCP), so the guest of a VM takes its addresses from OVN.
func (p Profile) SubnetOVNDHCP(ctx context.Context, subnetName string) (bool, error) {
	subnet, err := p.dynamicClient.Resource(subnetGVR).Get(ctx, subnetName, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("get subnet %s: %w", subnetName, err)
	}
	enabled, _, _ := unstructured.NestedBool(subnet.Object, "spec", "enableDHCP")
	return enabled, nil
}

// SubnetMTU returns the MTU of a NIC on the Subnet, the way kube-ovn-cni picks
// it for a pod: the Subnet's spec.mtu, else for a VLAN underlay Subnet without
// a logical gateway or U2O interconnection the MTU of its ProviderNetwork on
// this node. It returns 0 for an overlay Subnet: the NIC then takes the MTU of
// the pod's primary interface, which kube-ovn-cni set for the overlay.
func (p Profile) SubnetMTU(ctx context.Context, subnetName string) (int, error) {
	subnet, err := p.dynamicClient.Resource(subnetGVR).Get(ctx, subnetName, metav1.GetOptions{})
	if err != nil {
		return 0, fmt.Errorf("get subnet %s: %w", subnetName, err)
	}
	if mtu, _, _ := unstructured.NestedInt64(subnet.Object, "spec", "mtu"); mtu > 0 {
		return int(mtu), nil
	}
	if mtu, _, _ := unstructured.NestedFloat64(subnet.Object, "spec", "mtu"); mtu > 0 {
		return int(mtu), nil
	}
	vlanName, _, _ := unstructured.NestedString(subnet.Object, "spec", "vlan")
	logicalGateway, _, _ := unstructured.NestedBool(subnet.Object, "spec", "logicalGateway")
	u2o, _, _ := unstructured.NestedBool(subnet.Object, "spec", "u2oInterconnection")
	if vlanName == "" || logicalGateway || u2o {
		return 0, nil
	}
	vlan, err := p.dynamicClient.Resource(vlanGVR).Get(ctx, vlanName, metav1.GetOptions{})
	if err != nil {
		return 0, fmt.Errorf("get vlan %s of subnet %s: %w", vlanName, subnetName, err)
	}
	providerNetwork := publisher.ParseVlan(vlan).Provider
	if providerNetwork == "" {
		return 0, nil
	}
	node, err := p.dynamicClient.Resource(nodeGVR).Get(ctx, p.nodeName, metav1.GetOptions{})
	if err != nil {
		return 0, fmt.Errorf("get node %s: %w", p.nodeName, err)
	}
	value := node.GetLabels()[fmt.Sprintf(providerNetworkMTULabel, providerNetwork)]
	if value == "" {
		return 0, nil
	}
	mtu, err := strconv.Atoi(value)
	if err != nil || mtu <= 0 {
		return 0, fmt.Errorf("invalid MTU %q of provider network %s on node %s", value, providerNetwork, p.nodeName)
	}
	return mtu, nil
}

// providerNetworkMTULabel is the node label in which kube-ovn records the MTU
// of a ProviderNetwork's uplink on the node.
const providerNetworkMTULabel = "%s.provider-network.kubernetes.io/mtu"

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
		return nil, errors.New("runtime object is not a recognized NicConfig")
	}
	if err := nicConfig.Normalize(); err != nil {
		return nil, fmt.Errorf("normalizing NicConfig: %w", err)
	}

	perDeviceEdits := make(profiles.PerDeviceCDIContainerEdits)
	for _, result := range results {
		key := envKey(nicConfig.InterfaceName)
		envs := []string{
			fmt.Sprintf("KUBE_OVN_NIC_%s_SUBNET=%s", key, p.subnetNameFromDevice(result.Device)),
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

// subnetNameFromDevice returns the kube-ovn Subnet of an enumerated device.
func (p Profile) subnetNameFromDevice(deviceName string) string {
	if p.subnets != nil {
		if name, ok := p.subnets.Load(deviceName); ok {
			if subnet, ok := name.(string); ok {
				return subnet
			}
		}
	}
	return strings.TrimPrefix(deviceName, publisher.DeviceNamePrefix)
}
