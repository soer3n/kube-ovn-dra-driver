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
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/containerd/nri/pkg/api"
	"github.com/containerd/nri/pkg/stub"
	"k8s.io/klog/v2"

	"github.com/soer3n/kube-ovn-dra-driver/pkg/plumbing"
)

// nriPluginName / nriPluginIdx identify this plugin to the NRI runtime. The
// index orders plugins; a high value runs us late, after the primary CNI has
// set up eth0.
const (
	nriPluginName = "dra-nic"
	nriPluginIdx  = "90"
)

// networkNamespaceType is the OCI runtime-spec namespace type for the network
// namespace. NRI carries it verbatim in PodSandbox.Linux.Namespaces[].Type;
// the api package defines no constant for it.
const networkNamespaceType = "network"

// nriRetryInterval is how often attaches that failed in Synchronize are
// retried; nriMaxBackoff caps the delay between reconnects to the runtime.
var (
	nriRetryInterval = 10 * time.Second
	nriMinBackoff    = time.Second
	nriMaxBackoff    = 30 * time.Second
)

// nriPlugin bridges NRI pod-sandbox events to the plumbing.SandboxHandler. It
// implements the stub's Synchronize / RunPodSandbox / StopPodSandbox
// interfaces (detected by reflection in stub.New).
//
// It fails closed: the plugin is ready only once the runtime has synchronized
// it, and the driver refuses to prepare claims while it is not, so kubelet
// retries instead of starting a pod whose NICs nobody would attach. When the
// connection drops, the plugin reconnects; the runtime then synchronizes all
// running sandboxes again, which attaches the NICs of sandboxes created in
// between. Attaches that fail in Synchronize are retried in the background.
type nriPlugin struct {
	stub    stub.Stub
	handler *plumbing.SandboxHandler
	ready   atomic.Bool

	mu sync.Mutex
	// failed holds the running sandboxes whose NICs could not be attached in
	// Synchronize, by pod UID.
	failed map[string]*api.PodSandbox
}

// startNRIPlugin constructs the plugin and keeps it connected to the NRI
// socket in the background until ctx is done.
func startNRIPlugin(ctx context.Context, handler *plumbing.SandboxHandler) (*nriPlugin, error) {
	p := &nriPlugin{handler: handler, failed: make(map[string]*api.PodSandbox)}

	s, err := stub.New(p,
		stub.WithPluginName(nriPluginName),
		stub.WithPluginIdx(nriPluginIdx),
		stub.WithOnClose(func() { p.ready.Store(false) }),
	)
	if err != nil {
		return nil, fmt.Errorf("create NRI stub: %w", err)
	}
	p.stub = s

	go p.run(ctx)
	go p.retryFailed(ctx)
	return p, nil
}

// run serves the NRI connection and reconnects with a backoff when it ends.
func (p *nriPlugin) run(ctx context.Context) {
	logger := klog.FromContext(ctx)
	backoff := nriMinBackoff
	for {
		err := p.stub.Run(ctx)
		p.ready.Store(false)
		if ctx.Err() != nil {
			return
		}
		logger.Error(err, "NRI plugin disconnected, DRA NIC claims are not prepared until it reconnects", "retryIn", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, nriMaxBackoff)
	}
}

// Ready reports whether the runtime has synchronized the plugin, so it will
// receive the RunPodSandbox events of new pods.
func (p *nriPlugin) Ready() bool {
	return p != nil && p.ready.Load()
}

// stop tears down the NRI connection.
func (p *nriPlugin) stop() {
	if p != nil && p.stub != nil {
		p.stub.Stop()
	}
}

// Synchronize is invoked when the plugin (re)connects to the runtime, with all
// existing pod sandboxes. It attaches the NICs of running pods again, which
// covers a sandbox started while the plugin was down; Attach is idempotent.
// Failing Synchronize would drop the NRI connection, so failed attaches are
// retried in the background instead.
func (p *nriPlugin) Synchronize(ctx context.Context, pods []*api.PodSandbox, _ []*api.Container) ([]*api.ContainerUpdate, error) {
	for _, sb := range pods {
		p.syncSandbox(ctx, sb)
	}
	p.ready.Store(true)
	return nil, nil
}

// syncSandbox attaches the NICs of a running sandbox and records it for a
// retry if that fails.
func (p *nriPlugin) syncSandbox(ctx context.Context, sb *api.PodSandbox) {
	netnsPath := networkNamespacePath(sb)
	if netnsPath == "" {
		return
	}
	err := p.handler.OnSynchronizeSandbox(ctx, sb.GetUid(), sb.GetId(), netnsPath, sb.GetLabels())
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		klog.FromContext(ctx).Error(err, "Synchronize: attach failed, retrying",
			"pod", sb.GetName(), "namespace", sb.GetNamespace(), "uid", sb.GetUid(), "netns", netnsPath)
		if p.failed == nil {
			p.failed = make(map[string]*api.PodSandbox)
		}
		p.failed[sb.GetUid()] = sb
		return
	}
	delete(p.failed, sb.GetUid())
}

// retryFailed retries the attaches that failed in Synchronize until they
// succeed or the sandbox stops.
func (p *nriPlugin) retryFailed(ctx context.Context) {
	ticker := time.NewTicker(nriRetryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		p.mu.Lock()
		sandboxes := make([]*api.PodSandbox, 0, len(p.failed))
		for _, sb := range p.failed {
			sandboxes = append(sandboxes, sb)
		}
		p.mu.Unlock()
		for _, sb := range sandboxes {
			p.syncSandbox(ctx, sb)
		}
	}
}

// RunPodSandbox is invoked by the runtime when a pod sandbox is created — after
// its network namespace exists. We attach the NICs the prepare path added for
// this pod.
func (p *nriPlugin) RunPodSandbox(ctx context.Context, sb *api.PodSandbox) error {
	logger := klog.FromContext(ctx).WithValues("pod", sb.GetName(), "namespace", sb.GetNamespace(), "uid", sb.GetUid())

	netnsPath := networkNamespacePath(sb)
	if netnsPath == "" {
		// No netns reported (host-network pod, or runtime didn't populate it).
		// Nothing we can attach into; skip quietly.
		logger.V(4).Info("RunPodSandbox: no network namespace, skipping")
		return nil
	}

	if err := p.handler.OnRunPodSandbox(ctx, sb.GetUid(), sb.GetId(), netnsPath, sb.GetLabels()); err != nil {
		// Failing the sandbox makes kubelet retry; a pod with missing NICs would
		// otherwise run without ever noticing.
		logger.Error(err, "RunPodSandbox: attach failed", "netns", netnsPath)
		return err
	}
	return nil
}

// StopPodSandbox is invoked when the sandbox is torn down. We detach the NICs
// of the pod; kube-ovn-controller releases their addresses.
func (p *nriPlugin) StopPodSandbox(ctx context.Context, sb *api.PodSandbox) error {
	p.mu.Lock()
	delete(p.failed, sb.GetUid())
	p.mu.Unlock()
	logger := klog.FromContext(ctx).WithValues("pod", sb.GetName(), "namespace", sb.GetNamespace(), "uid", sb.GetUid())
	if err := p.handler.OnStopPodSandbox(ctx, sb.GetUid(), sb.GetName(), sb.GetNamespace()); err != nil {
		logger.Error(err, "StopPodSandbox: detach failed")
		return err
	}
	return nil
}

// networkNamespacePath returns the pod sandbox's network namespace path, or ""
// if the sandbox has none (e.g. host networking).
func networkNamespacePath(sb *api.PodSandbox) string {
	linux := sb.GetLinux()
	if linux == nil {
		return ""
	}
	for _, ns := range linux.GetNamespaces() {
		if ns.GetType() == networkNamespaceType {
			return ns.GetPath()
		}
	}
	return ""
}
