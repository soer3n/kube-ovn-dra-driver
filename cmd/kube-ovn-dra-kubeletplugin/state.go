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
	"errors"
	"fmt"
	"slices"
	"sync"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/client-go/dynamic"
	coreclientset "k8s.io/client-go/kubernetes"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	drapbv1 "k8s.io/kubelet/pkg/apis/dra/v1beta1"
	"k8s.io/kubernetes/pkg/kubelet/checkpointmanager"

	nicconfig "github.com/soer3n/kube-ovn-dra-driver/api/kube-ovn.io/resource/nic/v1alpha1"
	"github.com/soer3n/kube-ovn-dra-driver/internal/profiles"
	nicprofile "github.com/soer3n/kube-ovn-dra-driver/internal/profiles/nic"
	"github.com/soer3n/kube-ovn-dra-driver/pkg/nicprepare"
	"github.com/soer3n/kube-ovn-dra-driver/pkg/plumbing"
)

type AllocatableDevices map[string]resourceapi.Device
type PreparedClaims map[string]profiles.PreparedDevices

// maxPrepareConcurrency bounds how many per-NIC IPAM reservations run at once
// during PrepareResourceClaims. Concurrency is what makes prepare latency ~flat
// in NIC count (vs. Multus's serial per-NIC CNI chain); the cap keeps a
// many-NIC pod from flooding the kube-ovn controller with simultaneous requests.
const maxPrepareConcurrency = 16

type OpaqueDeviceConfig struct {
	Requests []string
	Config   runtime.Object
}

type DeviceState struct {
	sync.Mutex
	driverName        string
	cdi               *CDIHandler
	driverResources   resourceslice.DriverResources
	allocatable       AllocatableDevices
	checkpointManager checkpointmanager.CheckpointManager
	configDecoder     runtime.Decoder
	configHandler     profiles.ConfigHandler
	// NIC-specific: set when the profile is nicprofile.ProfileName
	coreclient    coreclientset.Interface
	dynamicClient dynamic.Interface
	isNIC         bool
	// nriStore receives one plumbing.Spec per claimed NIC, keyed by pod UID, so
	// the NRI sandbox hook can perform the attach once the netns exists.
	nriStore *plumbing.PendingStore
}

func NewDeviceState(config *Config) (*DeviceState, error) {
	driverResources, err := config.profile.EnumerateDevices()
	if err != nil {
		return nil, fmt.Errorf("error enumerating all possible devices: %v", err)
	}

	cdi, err := NewCDIHandler(config.flags.cdiRoot, config.flags.driverName, config.flags.profile)
	if err != nil {
		return nil, fmt.Errorf("unable to create CDI handler: %v", err)
	}

	err = cdi.CreateCommonSpecFile()
	if err != nil {
		return nil, fmt.Errorf("unable to create CDI spec file for common edits: %v", err)
	}

	checkpointManager, err := checkpointmanager.NewCheckpointManager(config.DriverPluginPath())
	if err != nil {
		return nil, fmt.Errorf("unable to create checkpoint manager: %v", err)
	}

	configScheme := runtime.NewScheme()
	configHandler := config.profile
	sb := configHandler.SchemeBuilder()
	if err := sb.AddToScheme(configScheme); err != nil {
		return nil, fmt.Errorf("create config scheme: %w", err)
	}

	// Set up a json serializer to decode our types.
	decoder := json.NewSerializerWithOptions(
		json.DefaultMetaFactory,
		configScheme,
		configScheme,
		json.SerializerOptions{
			Pretty: true, Strict: true,
		},
	)

	allocatable := make(AllocatableDevices)
	for _, slice := range driverResources.Pools[config.flags.nodeName].Slices {
		for _, device := range slice.Devices {
			allocatable[device.Name] = device
		}
	}

	state := &DeviceState{
		driverName:        config.flags.driverName,
		cdi:               cdi,
		driverResources:   driverResources,
		allocatable:       allocatable,
		checkpointManager: checkpointManager,
		configDecoder:     decoder,
		configHandler:     configHandler,
		coreclient:        config.coreclient,
		dynamicClient:     config.dynamicClient,
		isNIC:             config.flags.profile == nicprofile.ProfileName,
		nriStore:          config.nriStore,
	}

	checkpoints, err := state.checkpointManager.ListCheckpoints()
	if err != nil {
		return nil, fmt.Errorf("unable to list checkpoints: %v", err)
	}

	for _, c := range checkpoints {
		if c == DriverPluginCheckpointFile {
			return state, nil
		}
	}

	checkpoint := newCheckpoint()
	if err := state.checkpointManager.CreateCheckpoint(DriverPluginCheckpointFile, checkpoint); err != nil {
		return nil, fmt.Errorf("unable to sync to checkpoint: %v", err)
	}

	return state, nil
}

func (s *DeviceState) Prepare(claim *resourceapi.ResourceClaim) ([]*drapbv1.Device, error) {
	s.Lock()
	defer s.Unlock()

	claimUID := string(claim.UID)

	checkpoint := newCheckpoint()
	if err := s.checkpointManager.GetCheckpoint(DriverPluginCheckpointFile, checkpoint); err != nil {
		return nil, fmt.Errorf("unable to sync from checkpoint: %v", err)
	}
	preparedClaims := checkpoint.V1.PreparedClaims

	if preparedClaims[claimUID] != nil {
		return preparedClaims[claimUID].GetDevices(), nil
	}
	preparedDevices, err := s.prepareDevices(claim)
	if err != nil {
		return nil, fmt.Errorf("prepare failed: %v", err)
	}

	if err = s.cdi.CreateClaimSpecFile(claimUID, preparedDevices); err != nil {
		return nil, fmt.Errorf("unable to create CDI spec file for claim: %v", err)
	}

	preparedClaims[claimUID] = preparedDevices
	if err := s.checkpointManager.CreateCheckpoint(DriverPluginCheckpointFile, checkpoint); err != nil {
		return nil, fmt.Errorf("unable to sync to checkpoint: %v", err)
	}

	return preparedClaims[claimUID].GetDevices(), nil
}

func (s *DeviceState) Unprepare(claimUID string) error {
	s.Lock()
	defer s.Unlock()

	checkpoint := newCheckpoint()
	if err := s.checkpointManager.GetCheckpoint(DriverPluginCheckpointFile, checkpoint); err != nil {
		checkpoint = newCheckpoint()
		if err := s.checkpointManager.CreateCheckpoint(DriverPluginCheckpointFile, checkpoint); err != nil {
			return fmt.Errorf("unable to create new checkpoint: %v", err)
		}
	}
	preparedClaims := checkpoint.V1.PreparedClaims

	if preparedClaims[claimUID] == nil {
		return nil
	}

	if err := s.unprepareDevices(claimUID, preparedClaims[claimUID]); err != nil {
		return fmt.Errorf("unprepare failed: %v", err)
	}

	err := s.cdi.DeleteClaimSpecFile(claimUID)
	if err != nil {
		return fmt.Errorf("unable to delete CDI spec file for claim: %v", err)
	}

	delete(preparedClaims, claimUID)
	if err := s.checkpointManager.CreateCheckpoint(DriverPluginCheckpointFile, checkpoint); err != nil {
		return fmt.Errorf("unable to sync to checkpoint: %v", err)
	}

	return nil
}

func (s *DeviceState) prepareDevices(claim *resourceapi.ResourceClaim) (profiles.PreparedDevices, error) {
	if claim.Status.Allocation == nil {
		return nil, fmt.Errorf("claim not yet allocated")
	}
	// Check if any device request has admin access
	hasAdminAccess := s.checkAdminAccess(claim)

	// Retrieve the full set of device configs for the driver.
	configs, err := GetOpaqueDeviceConfigs(
		s.configDecoder,
		s.driverName,
		claim.Status.Allocation.Devices.Config,
	)
	if err != nil {
		return nil, fmt.Errorf("error getting opaque device configs: %v", err)
	}

	// Add the default device Config to the front of the config list with the
	// lowest precedence. This guarantees there will be at least one config in
	// the list with len(Requests) == 0 for the lookup below.
	configs = slices.Insert(configs, 0, &OpaqueDeviceConfig{})

	// Look through the configs and figure out which one will be applied to
	// each device allocation result based on their order of precedence.
	configResultsMap := make(map[runtime.Object][]*resourceapi.DeviceRequestAllocationResult)
	for _, result := range claim.Status.Allocation.Devices.Results {
		// The claim may include allocations meant for other drivers.
		if result.Driver != s.driverName {
			continue
		}
		if _, exists := s.allocatable[result.Device]; !exists {
			return nil, fmt.Errorf("requested device is not allocatable: %v", result.Device)
		}
		for _, c := range slices.Backward(configs) {
			if len(c.Requests) == 0 || slices.Contains(c.Requests, result.Request) {
				configResultsMap[c.Config] = append(configResultsMap[c.Config], &result)
				break
			}
		}
	}

	// Apply all configs associated with devices that need to be prepared.
	// Track container edits generated from applying the config to the set
	// of device allocation results.
	perDeviceCDIContainerEdits := make(profiles.PerDeviceCDIContainerEdits)
	for config, results := range configResultsMap {
		// Apply the config to the list of results associated with it.
		containerEdits, err := s.configHandler.ApplyConfig(config, results)
		if err != nil {
			return nil, fmt.Errorf("error applying config: %w", err)
		}

		// Merge any new container edits with the overall per device map.
		for k, v := range containerEdits {
			perDeviceCDIContainerEdits[k] = v
		}
	}

	// Flatten configResultsMap into an ordered list of (result, ifaceName) items.
	// Map iteration order isn't stable, so fix it into a slice before doing the
	// concurrent IPAM below and the (order-sensitive) device build after.
	type prepItem struct {
		result    *resourceapi.DeviceRequestAllocationResult
		ifaceName string
	}
	var items []prepItem
	for cfg, results := range configResultsMap {
		// Determine interface name from NicConfig for this batch.
		ifaceName := "net1"
		if s.isNIC {
			if nicCfg, ok := cfg.(*nicconfig.NicConfig); ok && nicCfg != nil && nicCfg.InterfaceName != "" {
				ifaceName = nicCfg.InterfaceName
			}
		}
		for _, result := range results {
			items = append(items, prepItem{result: result, ifaceName: ifaceName})
		}
	}

	// NIC IPAM (a kube-ovn controller round-trip per NIC, via the ips.kubeovn.io
	// CRD — Multus-free) is the slow step. Run it concurrently across all claimed
	// NICs so prepare latency is ~one round-trip instead of N — the structural win
	// over Multus's serial per-NIC CNI chain.
	nicCfgs := make([]*nicprepare.NicDeviceConfig, len(items))
	if s.isNIC {
		itemErrs := make([]error, len(items))
		var wg sync.WaitGroup
		sem := make(chan struct{}, maxPrepareConcurrency)
		for i := range items {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				it := items[i]
				dev := s.allocatable[it.result.Device]
				cfg, err := nicprepare.RequestIPAMViaIPObject(context.Background(), s.coreclient, s.dynamicClient, claim, it.result, dev, it.ifaceName)
				if err != nil {
					itemErrs[i] = fmt.Errorf("NIC IPAM for device %s: %w", it.result.Device, err)
					return
				}
				nicCfgs[i] = cfg
			}(i)
		}
		wg.Wait()
		if err := errors.Join(itemErrs...); err != nil {
			// Best-effort rollback of the reservations that did succeed, so a
			// retry of PrepareResourceClaims isn't blocked by half-created IP
			// objects (their names are deterministic per pod+provider).
			for _, cfg := range nicCfgs {
				if cfg != nil {
					_ = nicprepare.ReleaseIPAMViaIPObject(context.Background(), s.dynamicClient, cfg)
				}
			}
			return nil, err
		}
	}

	// Build the prepared devices in item order, wiring the NRI attach spec and the
	// release info from the (now-complete) IPAM results.
	preparedDevices := make(profiles.PreparedDevices, 0, len(items))
	for i, it := range items {
		result := it.result
		nicCfg := nicCfgs[i]
		// Stash the attach Spec for the NRI sandbox hook to consume once the pod
		// netns exists. NetnsPath/ContainerID are filled in there.
		if nicCfg != nil && s.nriStore != nil {
			s.nriStore.Add(nicCfg.PodUID, nicSpecFromConfig(nicCfg, s.allocatable[result.Device]))
		}

		device := &profiles.PreparedDevice{
			Device: drapbv1.Device{
				RequestNames: []string{result.Request},
				PoolName:     result.Pool,
				DeviceName:   result.Device,
				CdiDeviceIds: s.cdi.GetClaimDevices(string(claim.UID), []string{result.Device}),
			},
			ContainerEdits: perDeviceCDIContainerEdits[result.Device],
			AdminAccess:    hasAdminAccess,
		}
		// Persist what release needs so UnprepareResourceClaims can delete the
		// ips.kubeovn.io object (which makes kube-ovn GC the overlay LSP).
		if nicCfg != nil {
			device.NIC = &profiles.NicReleaseInfo{
				PodName:      nicCfg.PodName,
				PodNamespace: nicCfg.PodNamespace,
				Provider:     nicCfg.Provider,
			}
		}
		preparedDevices = append(preparedDevices, device)
	}

	return preparedDevices, nil
}

func (s *DeviceState) unprepareDevices(claimUID string, devices profiles.PreparedDevices) error {
	if !s.isNIC {
		return nil
	}
	// Release each NIC's IPAM reservation by deleting its ips.kubeovn.io object.
	// kube-ovn's reserved-IP delete path then removes the overlay LSP. The veth
	// and OVS port (when the nicAttach datapath is enabled) are torn down
	// separately by the NRI StopPodSandbox hook. kubeovnip.Release tolerates
	// NotFound, so a repeated unprepare is a no-op.
	var errs []error
	for _, d := range devices {
		if d.NIC == nil {
			continue
		}
		cfg := &nicprepare.NicDeviceConfig{
			PodName:      d.NIC.PodName,
			PodNamespace: d.NIC.PodNamespace,
			Provider:     d.NIC.Provider,
		}
		if err := nicprepare.ReleaseIPAMViaIPObject(context.Background(), s.dynamicClient, cfg); err != nil {
			errs = append(errs, fmt.Errorf("release IPAM for device %s (claim %s): %w", d.DeviceName, claimUID, err))
		}
	}
	return errors.Join(errs...)
}

// nicSpecFromConfig builds the plumbing.Spec for one NIC from the IPAM result
// and the device's subnetType/vlanId attributes. NetnsPath and ContainerID are
// left empty — the NRI sandbox hook fills them in when the sandbox appears.
func nicSpecFromConfig(cfg *nicprepare.NicDeviceConfig, dev resourceapi.Device) plumbing.Spec {
	spec := plumbing.Spec{
		PodUID:       cfg.PodUID,
		PodName:      cfg.PodName,
		PodNamespace: cfg.PodNamespace,
		IfaceName:    cfg.IfaceName,
		IP:           cfg.IP,
		MAC:          cfg.MAC,
		Gateway:      cfg.Gateway,
		Provider:     cfg.Provider,
		Type:         plumbing.SubnetTypeOVN,
	}
	if dev.Attributes != nil {
		if v := dev.Attributes["nic.kubeovn.io/subnetType"]; v.StringValue != nil && *v.StringValue == "vlan" {
			spec.Type = plumbing.SubnetTypeVLAN
		}
		if v := dev.Attributes["nic.kubeovn.io/vlanId"]; v.IntValue != nil {
			spec.VlanID = int(*v.IntValue)
		}
		// For VLAN underlay the bridge is br-<ProviderNetwork>, which is a
		// different identifier than the subnet provider used for IPAM/IP-CR
		// naming (cfg.Provider). Override Provider here so providerBridge()
		// resolves to e.g. br-external rather than br-<subnet-provider>.
		if spec.Type == plumbing.SubnetTypeVLAN {
			if v := dev.Attributes["nic.kubeovn.io/providerNetwork"]; v.StringValue != nil && *v.StringValue != "" {
				spec.Provider = *v.StringValue
			}
		}
	}
	// OVN overlay ports bind via external_ids:iface-id = the OVN LSP name.
	if spec.Type == plumbing.SubnetTypeOVN {
		spec.IfaceID = spec.PortName()
	}
	return spec
}

// checkAdminAccess determines if a resource claim requires admin access.
func (s *DeviceState) checkAdminAccess(claim *resourceapi.ResourceClaim) bool {
	if claim != nil && claim.Status.Allocation != nil {
		for _, result := range claim.Status.Allocation.Devices.Results {
			if result.AdminAccess != nil && *result.AdminAccess {
				return true
			}
		}
	}
	return false
}

// GetOpaqueDeviceConfigs returns an ordered list of the configs contained in possibleConfigs for this driver.
//
// Configs can either come from the resource claim itself or from the device
// class associated with the request. Configs coming directly from the resource
// claim take precedence over configs coming from the device class. Moreover,
// configs found later in the list of configs attached to its source take
// precedence over configs found earlier in the list for that source.
//
// All of the configs relevant to the driver from the list of possibleConfigs
// will be returned in order of precedence (from lowest to highest). If no
// configs are found, nil is returned.
func GetOpaqueDeviceConfigs(
	decoder runtime.Decoder,
	driverName string,
	possibleConfigs []resourceapi.DeviceAllocationConfiguration,
) ([]*OpaqueDeviceConfig, error) {
	// Collect all configs in order of reverse precedence.
	var classConfigs []resourceapi.DeviceAllocationConfiguration
	var claimConfigs []resourceapi.DeviceAllocationConfiguration
	var candidateConfigs []resourceapi.DeviceAllocationConfiguration
	for _, config := range possibleConfigs {
		switch config.Source {
		case resourceapi.AllocationConfigSourceClass:
			classConfigs = append(classConfigs, config)
		case resourceapi.AllocationConfigSourceClaim:
			claimConfigs = append(claimConfigs, config)
		default:
			return nil, fmt.Errorf("invalid config source: %v", config.Source)
		}
	}
	candidateConfigs = append(candidateConfigs, classConfigs...)
	candidateConfigs = append(candidateConfigs, claimConfigs...)

	// Decode all configs that are relevant for the driver.
	var resultConfigs []*OpaqueDeviceConfig
	for _, config := range candidateConfigs {
		// If this is nil, the driver doesn't support some future API extension
		// and needs to be updated.
		if config.Opaque == nil {
			return nil, fmt.Errorf("only opaque parameters are supported by this driver")
		}

		// Configs for different drivers may have been specified because a
		// single request can be satisfied by different drivers. This is not
		// an error -- drivers must skip over other driver's configs in order
		// to support this.
		if config.Opaque.Driver != driverName {
			continue
		}

		decodedConfig, err := runtime.Decode(decoder, config.Opaque.Parameters.Raw)
		if err != nil {
			return nil, fmt.Errorf("error decoding config parameters: %w", err)
		}

		resultConfig := &OpaqueDeviceConfig{
			Requests: config.Requests,
			Config:   decodedConfig,
		}

		resultConfigs = append(resultConfigs, resultConfig)
	}

	return resultConfigs, nil
}
