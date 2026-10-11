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
	"os/signal"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/urfave/cli/v2"
	resourceapi "k8s.io/api/resource/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/klog/v2"

	"github.com/soer3n/kube-ovn-dra-driver/internal/publisher"
	"github.com/soer3n/kube-ovn-dra-driver/pkg/flags"
)

const (
	// publisherCommand is the first argument that starts the publisher instead of
	// the kubelet plugin. Both run from the same image.
	publisherCommand = "publisher"

	// publishModeNode and publishModeCentral select who publishes the devices.
	// In node mode every kubelet plugin publishes a pool for its node. In central
	// mode the publisher publishes one pool per Subnet, and the kubelet plugins
	// only prepare claims.
	publishModeNode    = "node"
	publishModeCentral = "central"

	// publisherRefreshDelay batches bursts of changes, for example a Subnet
	// status that kube-ovn updates on every address it allocates, into one
	// update of the pools.
	publisherRefreshDelay = 3 * time.Second

	podOwnersTTL = time.Minute
)

type publisherFlags struct {
	kubeClientConfig flags.KubeClientConfig
	loggingConfig    *flags.LoggingConfig

	driverName      string
	namespace       string
	leaseName       string
	identity        string
	ownerDeployment string
	capacity        bool
}

func newPublisherApp() *cli.App {
	f := &publisherFlags{loggingConfig: flags.NewLoggingConfig()}
	cliFlags := []cli.Flag{
		&cli.StringFlag{
			Name:        "driver-name",
			Usage:       "Name of the DRA driver the pools are published for.",
			Value:       "nic.kubeovn.io",
			Destination: &f.driverName,
			EnvVars:     []string{"DRIVER_NAME"},
		},
		&cli.StringFlag{
			Name:        "namespace",
			Usage:       "Namespace of the publisher, where the lease and the owner Deployment are.",
			Required:    true,
			Destination: &f.namespace,
			EnvVars:     []string{"NAMESPACE"},
		},
		&cli.StringFlag{
			Name:        "lease-name",
			Usage:       "Name of the lease that elects the publisher that publishes.",
			Value:       "kube-ovn-dra-publisher",
			Destination: &f.leaseName,
			EnvVars:     []string{"LEASE_NAME"},
		},
		&cli.StringFlag{
			Name:        "identity",
			Usage:       "Identity of this publisher in the leader election, normally the pod name.",
			Required:    true,
			Destination: &f.identity,
			EnvVars:     []string{"POD_NAME"},
		},
		&cli.StringFlag{
			Name:        "owner-deployment",
			Usage:       "Name of the Deployment that owns the published ResourceSlices, so they are removed with it.",
			Required:    true,
			Destination: &f.ownerDeployment,
			EnvVars:     []string{"OWNER_DEPLOYMENT"},
		},
		&cli.BoolFlag{
			Name:        "capacity",
			Usage:       "Publish the free addresses of a Subnet as consumable capacity, so the scheduler refuses a pod when the pool is exhausted.",
			Value:       true,
			Destination: &f.capacity,
			EnvVars:     []string{"PUBLISH_CAPACITY"},
		},
	}
	cliFlags = append(cliFlags, f.kubeClientConfig.Flags()...)
	cliFlags = append(cliFlags, f.loggingConfig.Flags()...)

	return &cli.App{
		Name:            "kube-ovn-dra-kubeletplugin " + publisherCommand,
		Usage:           "publishes the kube-ovn Subnets as cluster-wide DRA pools",
		ArgsUsage:       " ",
		HideHelpCommand: true,
		Flags:           cliFlags,
		Before: func(c *cli.Context) error {
			if c.Args().Len() > 0 {
				return fmt.Errorf("arguments not supported: %v", c.Args().Slice())
			}
			return f.loggingConfig.Apply()
		},
		Action: func(c *cli.Context) error {
			clientSets, err := f.kubeClientConfig.NewClientSets()
			if err != nil {
				return fmt.Errorf("create client: %w", err)
			}
			ctx, stop := signal.NotifyContext(c.Context, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
			defer stop()
			return runPublisher(ctx, f, clientSets)
		},
	}
}

// runPublisher takes the lease and publishes while it holds it. It returns when
// ctx is done or the lease is lost, so the Deployment restarts it.
func runPublisher(ctx context.Context, f *publisherFlags, clientSets flags.ClientSets) error {
	logger := klog.FromContext(ctx)
	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Name: f.leaseName, Namespace: f.namespace},
		Client:     clientSets.Core.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: f.identity},
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	failed := make(chan error, 1)
	elector, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock:            lock,
		ReleaseOnCancel: true,
		LeaseDuration:   15 * time.Second,
		RenewDeadline:   10 * time.Second,
		RetryPeriod:     2 * time.Second,
		Name:            f.leaseName,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				logger.Info("Became the publisher", "identity", f.identity)
				if err := publish(ctx, f, clientSets); err != nil && ctx.Err() == nil {
					failed <- err
					cancel()
				}
			},
			OnStoppedLeading: func() {
				logger.Info("Stopped being the publisher", "identity", f.identity)
			},
		},
	})
	if err != nil {
		return fmt.Errorf("create leader elector: %w", err)
	}
	elector.Run(ctx)

	select {
	case err := <-failed:
		return err
	default:
		return nil
	}
}

// publish runs the publisher while the caller leads: it follows the Subnets,
// Vlans, ip objects and claims, and keeps the pools up to date.
func publish(ctx context.Context, f *publisherFlags, clientSets flags.ClientSets) error {
	logger := klog.FromContext(ctx)

	owner, err := ownerOf(ctx, clientSets.Core, f.namespace, f.ownerDeployment)
	if err != nil {
		return err
	}

	dynamicFactory := dynamicinformer.NewDynamicSharedInformerFactory(clientSets.Dynamic, 0)
	subnets := dynamicFactory.ForResource(publisher.SubnetGVR).Informer()
	vlans := dynamicFactory.ForResource(publisher.VlanGVR).Informer()
	ips := dynamicFactory.ForResource(publisher.IPGVR).Informer()
	coreFactory := informers.NewSharedInformerFactory(clientSets.Core, 0)
	claims := coreFactory.Resource().V1().ResourceClaims().Informer()

	trigger := make(chan struct{}, 1)
	onChange := func(any) {
		select {
		case trigger <- struct{}{}:
		default:
		}
	}
	handler := cache.ResourceEventHandlerFuncs{
		AddFunc:    onChange,
		UpdateFunc: func(_, newObj any) { onChange(newObj) },
		DeleteFunc: onChange,
	}
	synced := []cache.InformerSynced{subnets.HasSynced, vlans.HasSynced, ips.HasSynced, claims.HasSynced}
	for _, informer := range []cache.SharedIndexInformer{subnets, vlans, ips, claims} {
		if _, err := informer.AddEventHandler(handler); err != nil {
			return fmt.Errorf("watch: %w", err)
		}
	}
	dynamicFactory.Start(ctx.Done())
	coreFactory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), synced...) {
		return fmt.Errorf("sync informers: %w", ctx.Err())
	}

	snapshot := &snapshotter{
		driverName: f.driverName,
		capacity:   f.capacity,
		subnets:    subnets.GetStore(),
		vlans:      vlans.GetStore(),
		ips:        ips.GetStore(),
		claims:     claims.GetStore(),
		owners:     newPodOwners(clientSets.Core),
	}
	initial, err := snapshot.resources()
	if err != nil {
		return err
	}
	controller, err := resourceslice.StartController(ctx, resourceslice.Options{
		DriverName: f.driverName,
		KubeClient: clientSets.Core,
		Owner:      owner,
		Resources:  &initial,
	})
	if err != nil {
		return fmt.Errorf("start resource slice controller: %w", err)
	}
	defer controller.Stop()
	logger.Info("Published the Subnet pools", "pools", len(initial.Pools), "capacity", f.capacity)

	last := initial
	refreshDevicesLoop(ctx, trigger, publisherRefreshDelay,
		func() (resourceslice.DriverResources, bool, error) {
			resources, err := snapshot.resources()
			if err != nil {
				return resourceslice.DriverResources{}, false, err
			}
			return resources, !apiequality.Semantic.DeepEqual(resources, last), nil
		},
		func(_ context.Context, resources resourceslice.DriverResources) error {
			last = resources
			controller.Update(&resources)
			return nil
		})
	return nil
}

// ownerOf returns the Deployment that owns the ResourceSlices: they are removed
// with it, for example by a Helm uninstall.
func ownerOf(ctx context.Context, client kubernetes.Interface, namespace, name string) (*resourceslice.Owner, error) {
	deployment, err := client.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get owner Deployment %s/%s: %w", namespace, name, err)
	}
	return &resourceslice.Owner{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       deployment.Name,
		UID:        deployment.UID,
	}, nil
}

// snapshotter turns the informer stores into the pools.
type snapshotter struct {
	driverName string
	capacity   bool
	subnets    cache.Store
	vlans      cache.Store
	ips        cache.Store
	claims     cache.Store
	owners     publisher.OwnersOf
}

func (s *snapshotter) resources() (resourceslice.DriverResources, error) {
	vlans := func(name string) (publisher.VlanInfo, error) {
		obj, exists, err := s.vlans.GetByKey(name)
		if err != nil {
			return publisher.VlanInfo{}, err
		}
		if !exists {
			return publisher.VlanInfo{}, apierrors.NewNotFound(publisher.VlanGVR.GroupResource(), name)
		}
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			return publisher.VlanInfo{}, fmt.Errorf("vlan %q has unexpected type %T", name, obj)
		}
		return publisher.ParseVlan(u), nil
	}

	logger := klog.Background()
	var subnets []publisher.Subnet
	for _, obj := range s.subnets.List() {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		if spec, _ := u.Object["spec"].(map[string]any); spec == nil {
			continue
		}
		subnet, err := publisher.ParseSubnet(u, vlans)
		if err != nil {
			// A Vlan that is not there yet, for example while a manifest is
			// applied, must not keep the other Subnets from being published.
			logger.Error(err, "Skipping a Subnet", "subnet", u.GetName())
			continue
		}
		subnets = append(subnets, subnet)
	}
	sort.Slice(subnets, func(i, j int) bool { return subnets[i].Name < subnets[j].Name })

	if s.capacity {
		var ipRefs []publisher.IPRef
		for _, obj := range s.ips.List() {
			if u, ok := obj.(*unstructured.Unstructured); ok {
				if ref, ok := publisher.ParseIP(u); ok {
					ipRefs = append(ipRefs, ref)
				}
			}
		}
		var claims []*resourceapi.ResourceClaim
		for _, obj := range s.claims.List() {
			if c, ok := obj.(*resourceapi.ResourceClaim); ok {
				claims = append(claims, c)
			}
		}
		publisher.ApplyHeld(subnets, s.driverName, claims, ipRefs, s.owners)
	}
	return resourceslice.DriverResources{Pools: publisher.Pools(subnets, s.capacity)}, nil
}

// podOwners finds the names the address of a pod's NIC can carry: the pod, and
// the VM of a KubeVirt virt-launcher pod. Pods do not change their VM, so the
// answer is cached for a while instead of reading the pod on every update.
type podOwners struct {
	client kubernetes.Interface
	mu     sync.Mutex
	cache  map[string]podOwnersEntry
}

type podOwnersEntry struct {
	names   []string
	expires time.Time
}

func newPodOwners(client kubernetes.Interface) publisher.OwnersOf {
	p := &podOwners{client: client, cache: make(map[string]podOwnersEntry)}
	return p.owners
}

func (p *podOwners) owners(namespace, pod string) []string {
	key := namespace + "/" + pod
	p.mu.Lock()
	entry, ok := p.cache[key]
	p.mu.Unlock()
	if ok && time.Now().Before(entry.expires) {
		return entry.names
	}

	names := []string{pod}
	got, err := p.client.CoreV1().Pods(namespace).Get(context.Background(), pod, metav1.GetOptions{})
	switch {
	case err == nil:
		if vm := got.Labels["vm.kubevirt.io/name"]; vm != "" {
			names = append(names, vm)
		}
	case !apierrors.IsNotFound(err):
		// Do not cache a failure, and do not drop the pod's own name.
		klog.Background().Error(err, "Reading a pod to find its VM", "pod", key)
		return names
	}

	p.mu.Lock()
	p.cache[key] = podOwnersEntry{names: names, expires: time.Now().Add(podOwnersTTL)}
	p.mu.Unlock()
	return names
}

// runPublisherIfRequested starts the publisher when the first argument asks for
// it. It reports whether it did, and its error.
func runPublisherIfRequested(args []string) (bool, error) {
	if len(args) < 2 || args[1] != publisherCommand {
		return false, nil
	}
	err := newPublisherApp().Run(append([]string{args[0]}, args[2:]...))
	if errors.Is(err, context.Canceled) {
		err = nil
	}
	return true, err
}
