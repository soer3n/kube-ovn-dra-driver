//go:build e2e

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

package e2e

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/soer3n/kube-ovn-dra-driver/internal/publisher"
)

// The capacity spec has failed now and then with a pod that stays unscheduled
// although the pool has room for it. The recorder samples what the scheduler and
// the publisher see while the spec runs, and prints it when the spec fails.

const (
	recorderInterval = 500 * time.Millisecond
	// publisherSelector selects the publisher pods of the driver's Helm release.
	publisherSelector = driverInstance + ",app.kubernetes.io/component=publisher"
	schedulerSelector = "component=kube-scheduler"
	recorderLogTail   = "--tail=300"
	flagFieldSelector = "--field-selector"
	flagPrefix        = "--prefix"
	flagTimestamps    = "--timestamps"
	resourceEvents    = "events"
	resourceSlices    = "resourceslices"
	resourceClaims    = "resourceclaims"
	kubectlLogs       = "logs"
	fieldSubnet       = "subnet"
)

// capacitySample is what the recorder saw at one point in time.
type capacitySample struct {
	at   time.Time
	line string
}

// capacityRecorder samples the Subnet, its pool and the spec's pods and claims,
// and keeps every change.
type capacityRecorder struct {
	mu      sync.Mutex
	samples []capacitySample
	stop    chan struct{}
	done    chan struct{}
}

// startCapacityRecorder samples until stop is called.
func startCapacityRecorder(ctx context.Context) *capacityRecorder {
	r := &capacityRecorder{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer GinkgoRecover()
		defer close(r.done)
		ticker := time.NewTicker(recorderInterval)
		defer ticker.Stop()
		for {
			r.record(capacitySnapshot(ctx))
			select {
			case <-r.stop:
				return
			case <-ticker.C:
			}
		}
	}()
	return r
}

func (r *capacityRecorder) record(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n := len(r.samples); n > 0 && r.samples[n-1].line == line {
		return
	}
	r.samples = append(r.samples, capacitySample{at: time.Now(), line: line})
}

// stopAndReport stops sampling. When the spec failed it prints the samples and
// what the cluster says about the spec's pods, claims and pool.
func (r *capacityRecorder) stopAndReport() {
	close(r.stop)
	<-r.done
	if !CurrentSpecReport().Failed() {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	GinkgoWriter.Printf("\n=== capacity timeline (free = Subnet status, cap = published capacity, alloc = allocated NICs, ips = ip objects; per pod: node or -, phase) ===\n")
	for _, s := range r.samples {
		GinkgoWriter.Printf("%s %s\n", s.at.Format("15:04:05.000"), s.line)
	}
	dump := func(title string, args ...string) {
		out, err := kubectl(args...)
		GinkgoWriter.Printf("\n=== %s ===\n%s", title, out)
		if err != nil {
			GinkgoWriter.Printf("(%v)\n", err)
		}
	}
	dump("events of the capacity pods", kubectlGet, resourceEvents, flagFieldSelector, "involvedObject.kind=Pod", flagOutput,
		"custom-columns=TIME:.lastTimestamp,POD:.involvedObject.name,REASON:.reason,MESSAGE:.message", "--sort-by=.lastTimestamp")
	dump("claims", kubectlGet, resourceClaims, flagSelector, capacityLabel, flagOutput,
		"custom-columns=NAME:.metadata.name,POOL:.status.allocation.devices.results[*].pool,CONSUMED:.status.allocation.devices.results[*].consumedCapacity,RESERVED:.status.reservedFor[*].name")
	dump("pool of "+capacitySubnet, kubectlGet, resourceSlices, flagOutput,
		"jsonpath={range .items[?(@.spec.pool.name==\""+capacitySubnet+"\")]}{.metadata.name} generation={.spec.pool.generation} capacity={.spec.devices[*].capacity}{\"\\n\"}{end}")
	dump("publisher log", flagNamespace, kubeSystemNS, kubectlLogs, flagSelector, publisherSelector, recorderLogTail, flagPrefix, flagTimestamps)
	dump("scheduler log", flagNamespace, kubeSystemNS, kubectlLogs, flagSelector, schedulerSelector, recorderLogTail, flagTimestamps)
}

// capacitySnapshot returns one line with the numbers the recorder follows. It
// does not fail the spec: a read error shows up in the line.
func capacitySnapshot(ctx context.Context) string {
	free, _ := subnetFree(ctx, capacitySubnet)

	capacity := "?"
	slices, err := clientset.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, slice := range slices.Items {
			if slice.Spec.Driver != driverName || slice.Spec.Pool.Name != capacitySubnet {
				continue
			}
			for _, device := range slice.Spec.Devices {
				if device.Name == publisher.DeviceName(capacitySubnet) {
					if c, ok := device.Capacity[capacityIPv4]; ok {
						capacity = fmt.Sprintf("%d (gen %d)", c.Value.Value(), slice.Spec.Pool.Generation)
					}
				}
			}
		}
	}

	allocated := 0
	claims, err := clientset.ResourceV1().ResourceClaims(namespace).List(ctx, metav1.ListOptions{LabelSelector: capacityLabel})
	if err == nil {
		for _, claim := range claims.Items {
			if claim.Status.Allocation == nil {
				continue
			}
			for _, result := range claim.Status.Allocation.Devices.Results {
				if result.Driver == driverName && result.Pool == capacitySubnet {
					allocated++
				}
			}
		}
	}

	ips := 0
	list, err := dynClient.Resource(ipGVR).List(ctx, metav1.ListOptions{})
	if err == nil {
		for i := range list.Items {
			if subnet, _, _ := unstructured.NestedString(list.Items[i].Object, fieldSpec, fieldSubnet); subnet == capacitySubnet {
				ips++
			}
		}
	}

	var pods []string
	podList, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: capacityLabel})
	if err == nil {
		for _, pod := range podList.Items {
			node := pod.Spec.NodeName
			if node == "" {
				node = "-"
			}
			pods = append(pods, fmt.Sprintf("%s:%s/%s", pod.Name, node, pod.Status.Phase))
		}
	}
	sort.Strings(pods)
	return fmt.Sprintf("free=%d cap=%s alloc=%d ips=%d %s", free, capacity, allocated, ips, strings.Join(pods, " "))
}
