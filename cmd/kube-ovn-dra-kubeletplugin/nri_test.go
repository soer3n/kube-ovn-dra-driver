/*
 * Copyright 2026 soer3n.
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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/containerd/nri/pkg/api"
	"github.com/containerd/nri/pkg/stub"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	resourceapi "k8s.io/api/resource/v1"

	"github.com/soer3n/kube-ovn-dra-driver/pkg/plumbing"
)

type recordingAttacher struct {
	mu       sync.Mutex
	attached []plumbing.Spec
	swept    []string
	// fail makes Attach fail while it is set.
	fail error
}

func (r *recordingAttacher) Attach(_ context.Context, spec plumbing.Spec) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return r.fail
	}
	r.attached = append(r.attached, spec)
	return nil
}

func (r *recordingAttacher) Detach(context.Context, plumbing.Spec) error { return nil }

func (r *recordingAttacher) DetachPodPorts(_ context.Context, podName, podNamespace string) error {
	r.swept = append(r.swept, podNamespace+"/"+podName)
	return nil
}

func sandbox(namespaces ...*api.LinuxNamespace) *api.PodSandbox {
	return &api.PodSandbox{
		Id: testSandboxID, Uid: testPodUID, Name: testPod, Namespace: testNS,
		Linux: &api.LinuxPodSandbox{Namespaces: namespaces},
	}
}

func TestNetworkNamespacePath(t *testing.T) {
	assert.Empty(t, networkNamespacePath(&api.PodSandbox{}))
	assert.Empty(t, networkNamespacePath(sandbox(&api.LinuxNamespace{Type: nsTypeIPC, Path: testIPCPath})))
	assert.Equal(t, testNetnsPath, networkNamespacePath(sandbox(
		&api.LinuxNamespace{Type: nsTypeIPC, Path: testIPCPath},
		&api.LinuxNamespace{Type: networkNamespaceType, Path: testNetnsPath},
	)))
}

func TestNRIPluginSandboxEvents(t *testing.T) {
	store := plumbing.NewNICStore()
	attacher := &recordingAttacher{}
	p := &nriPlugin{handler: plumbing.NewSandboxHandler(store, attacher)}
	store.Add(plumbing.Spec{PodUID: testPodUID, ClaimUID: testNRIClaimUID, IfaceName: testIface1})

	// A sandbox without a network namespace is skipped and keeps its NICs pending.
	require.NoError(t, p.RunPodSandbox(context.Background(), sandbox()))
	assert.Empty(t, attacher.attached)

	require.NoError(t, p.RunPodSandbox(context.Background(), sandbox(&api.LinuxNamespace{Type: networkNamespaceType, Path: testNetnsPath})))
	require.Len(t, attacher.attached, 1)
	assert.Equal(t, testSandboxID, attacher.attached[0].ContainerID)
	assert.Equal(t, testNetnsPath, attacher.attached[0].NetnsPath)

	require.NoError(t, p.StopPodSandbox(context.Background(), sandbox()))
	assert.Equal(t, []string{testNS + "/" + testPod}, attacher.swept)
}

func TestNRIPluginSynchronize(t *testing.T) {
	store := plumbing.NewNICStore()
	attacher := &recordingAttacher{}
	p := &nriPlugin{handler: plumbing.NewSandboxHandler(store, attacher)}
	store.Add(plumbing.Spec{PodUID: testPodUID, ClaimUID: testNRIClaimUID, IfaceName: testIface1})

	other := &api.PodSandbox{
		Id: "sandbox-2", Uid: "other-uid", Name: "other", Namespace: testNS,
		Linux: &api.LinuxPodSandbox{Namespaces: []*api.LinuxNamespace{{Type: networkNamespaceType, Path: "/var/run/netns/y"}}},
	}
	updates, err := p.Synchronize(context.Background(), []*api.PodSandbox{
		sandbox(), // no network namespace
		sandbox(&api.LinuxNamespace{Type: networkNamespaceType, Path: testNetnsPath}),
		other, // no DRA NICs
	}, nil)
	require.NoError(t, err)
	assert.Empty(t, updates)
	require.Len(t, attacher.attached, 1)
	assert.Equal(t, testNetnsPath, attacher.attached[0].NetnsPath)
	assert.True(t, p.Ready(), "a synchronized plugin is ready")
}

// An attach that fails in Synchronize must not drop the connection; it is
// retried until it succeeds, and forgotten when the sandbox stops.
func TestNRIPluginSynchronizeRetry(t *testing.T) {
	store := plumbing.NewNICStore()
	attacher := &recordingAttacher{fail: errors.New("ovs down")}
	p := &nriPlugin{handler: plumbing.NewSandboxHandler(store, attacher)}
	store.Add(plumbing.Spec{PodUID: testPodUID, ClaimUID: testNRIClaimUID, IfaceName: testIface1})
	sb := sandbox(&api.LinuxNamespace{Type: networkNamespaceType, Path: testNetnsPath})

	_, err := p.Synchronize(context.Background(), []*api.PodSandbox{sb}, nil)
	require.NoError(t, err)
	assert.True(t, p.Ready())
	require.Contains(t, p.failed, testPodUID)

	nriRetryInterval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.retryFailed(ctx)
	time.Sleep(50 * time.Millisecond)
	attacher.mu.Lock()
	assert.Empty(t, attacher.attached, "still failing")
	attacher.fail = nil
	attacher.mu.Unlock()
	require.Eventually(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.failed) == 0
	}, time.Second, 10*time.Millisecond)
	cancel()

	p.failed[testPodUID] = sb
	require.NoError(t, p.StopPodSandbox(context.Background(), sb))
	assert.Empty(t, p.failed, "a stopped sandbox is not retried")
}

// Claims are not prepared while NRI is not connected, so no pod starts
// without its NICs.
func TestPrepareRefusedWithoutNRI(t *testing.T) {
	d := &driver{state: newTestDeviceState(t, allocatedPod(testProvider)), nri: &nriPlugin{}}
	claim := testClaim(nil, testRequestA)
	results, err := d.PrepareResourceClaims(context.Background(), []*resourceapi.ResourceClaim{claim})
	require.NoError(t, err)
	assert.ErrorContains(t, results[claim.UID].Err, "NRI plugin is not connected")

	d.nri.ready.Store(true)
	results, err = d.PrepareResourceClaims(context.Background(), []*resourceapi.ResourceClaim{claim})
	require.NoError(t, err)
	assert.NoError(t, results[claim.UID].Err)
}

// fakeStub plays the NRI runtime connection. Every Run is one connection,
// which the runtime synchronizes. An error from conns drops it again, nil
// keeps it up until the context ends.
type fakeStub struct {
	stub.Stub
	plugin *nriPlugin
	conns  chan error
	calls  atomic.Int32
}

func (f *fakeStub) Run(ctx context.Context) error {
	f.calls.Add(1)
	select {
	case err := <-f.conns:
		if _, syncErr := f.plugin.Synchronize(ctx, nil, nil); syncErr != nil {
			return syncErr
		}
		if err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// A dropped connection makes the plugin not ready until the runtime has
// synchronized it again after the reconnect.
func TestNRIPluginReconnects(t *testing.T) {
	nriMinBackoff = 10 * time.Millisecond
	p := &nriPlugin{}
	fake := &fakeStub{plugin: p, conns: make(chan error)}
	p.stub = fake
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { p.run(ctx); close(done) }()

	assert.False(t, p.Ready(), "not ready before the first synchronization")
	fake.conns <- errors.New("connection closed")
	require.Eventually(t, func() bool { return fake.calls.Load() >= 2 }, time.Second, 5*time.Millisecond, "reconnects after the drop")
	assert.False(t, p.Ready(), "a dropped connection is not ready")

	fake.conns <- nil
	require.Eventually(t, p.Ready, time.Second, 5*time.Millisecond, "ready once the new connection is synchronized")

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("run did not return after the context was cancelled")
	}
	assert.False(t, p.Ready(), "not ready after the plugin stopped")
}
