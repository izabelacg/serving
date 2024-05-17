//go:build e2e
// +build e2e

/*
Copyright 2024 The Knative Authors

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

package ha

import (
	"context"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/watch"
	"knative.dev/serving/pkg/apis/autoscaling"
	"knative.dev/serving/pkg/apis/serving"
	rtesting "knative.dev/serving/pkg/testing/v1"
	"knative.dev/serving/test"
	"knative.dev/serving/test/e2e"
	"sync"
	"testing"
)

const (
	nodeTaintKey   = "knative-tests-evict"
	nodeTaintValue = "true"
	// Pods that do not tolerate the taint are evicted immediately
	nodeTaintEffect           = v1.TaintEffectNoExecute
	numberOfNodesWithUserPods = 1
)

// TODO
// TestResilienceDuringNodeDrainage
// Using all defaults for now.
// The test ensures that draining a node doesn't affect user applications.
// One service is probed during node drainage
// should not run in parallel / disruption to user workloads and kn components
// Starting with:
// - proxy mode: activator in the data path
// - minScale >= 1
func TestResilienceDuringNodeDrainage(t *testing.T) {
	clients := e2e.Setup(t)
	ctx := context.Background()

	// Create first service that we will continually probe during disruption scenario.
	names, resources := createPizzaPlanetService(t,
		rtesting.WithConfigAnnotations(map[string]string{
			autoscaling.MinScaleAnnotationKey:  "1",  // Make sure we don't scale to zero during the test.
			autoscaling.TargetBurstCapacityKey: "-1", // Make sure all requests go through the activator.
		}),
	)
	test.EnsureTearDown(t, clients, &names)

	//FIXME does probing makes it so terminating pod never reaches a deleted state?
	t.Log("Starting prober")
	prober := test.NewProberManager(t.Logf, clients, minProbes, test.AddRootCAtoTransport(context.Background(), t.Logf, clients, test.ServingFlags.HTTPS))

	prober.Spawn(resources.Service.Status.URL.URL())
	defer assertSLO(t, prober, 1)

	// Get nodes
	nodes, err := clients.KubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Failed to get nodes: %v", err)
	}
	if len(nodes.Items) == 1 {
		t.Fatalf("Insufficient nodes for testing")
		t.Fatalf("Only 1 kubernetes node available. Ensuring zero downtime can no longer be guaranteed.")
	}

	//FIXME find where pods are
	//choose a node
	//check if node is drained
	//check where pod is
	//confirm probing happening

	selector := labels.SelectorFromSet(labels.Set{
		serving.ServiceLabelKey: names.Service,
	})
	pods, err := clients.KubeClient.CoreV1().Pods(test.ServingFlags.TestNamespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		t.Fatalf("Unable to get pods: %v", err)
	}

	// Choose nodes with user pods
	candidateNodes := sets.Set[string]{}
	for _, pod := range pods.Items {
		candidateNodes.Insert(pod.Spec.NodeName)
	}
	if candidateNodes.Len() != numberOfNodesWithUserPods {
		t.Fatalf("Expected to have %d node(s) with a user pod running, but found %d.", numberOfNodesWithUserPods, candidateNodes.Len())
	}

	// Taint selected nodes
	for _, nodeName := range candidateNodes.UnsortedList() {
		t.Logf("Tainting node: %s", nodeName)
		if err := taintNode(ctx, clients, nodeName); err != nil {
			t.Fatalf("Failed to taint node %s: %v", nodeName, err)
		}
		//defer untaintNode(ctx, clients, nodeName)
	}

	defer func() {
		for _, nodeName := range candidateNodes.UnsortedList() {
			t.Logf("Untainting node: %s", nodeName)
			if err := untaintNode(ctx, clients, nodeName); err != nil {
				t.Fatalf("Failed to untaint node %s: %v", nodeName, err)
			}
		}
	}()

	t.Logf("Draining nodes: %v", candidateNodes)
	t.Logf("Watching pods during drainage of nodes: %v", candidateNodes)
	var wg sync.WaitGroup
	wg.Add(1)
	watcherTimeoutSeconds := int64(60 * 5)
	go watchPodEvents(t, ctx, clients, &wg, watcherTimeoutSeconds, selector, candidateNodes)
	wg.Wait()
}

func watchPodEvents(t *testing.T, ctx context.Context, clients *test.Clients, wg *sync.WaitGroup, timeoutSeconds int64, selector labels.Selector, nodes sets.Set[string]) {
	defer wg.Done()

	watcher, err := clients.KubeClient.CoreV1().Pods(test.ServingFlags.TestNamespace).Watch(ctx, metav1.ListOptions{
		LabelSelector:  selector.String(),
		Watch:          true,
		TimeoutSeconds: &timeoutSeconds,
	})
	if err != nil {
		t.Fatalf("Unable to watch pods: %v", err)
	}

	podEventsChan := watcher.ResultChan()
	defer watcher.Stop()

	for event := range podEventsChan {
		pod, ok := event.Object.(*v1.Pod)
		t.Logf("Pod %s received event: %s", pod.Name, event.Type)
		if !ok {
			continue
		}
		if event.Type == watch.Deleted && nodes.Has(pod.Spec.NodeName) {
			t.Logf("Pod %s deleted from node %s", pod.Name, pod.Spec.NodeName)
			break
		}
	}
	t.Logf("Exiting watchPodEvents function")

	// attempt 1
	//for {
	//	select {
	//	case event := <-watcher.ResultChan():
	//		item := event.Object.(*v1.Pod)
	//		if event.Type == watch.Deleted {
	//			if nodes.Has(item.Spec.NodeName) {
	//				t.Logf("Pod %s deleted from tainted node %s", item.Name, item.Spec.NodeName)
	//				return
	//			} else {
	//				t.Logf("Pod %s deleted from node %s", item.Name, item.Spec.NodeName)
	//			}
	//		} else if event.Type == watch.Added {
	//			//FIXME no node info is printed
	//			t.Logf("Pod %s created in the node %s", item.Name, item.Spec.NodeName)
	//		}
	//	case <-timeout:
	//		t.Fatalf("Timeout reached while waiting for pods to be deleted")
	//		return
	//	}
	//}

	// attempt 2
	//for event := range watcher.ResultChan() {
	//	item := event.Object.(*v1.Pod)
	//
	//	switch event.Type {
	//	case watch.Deleted:
	//		if nodes.Has(item.Spec.NodeName) {
	//			t.Logf("Pod %s deleted from tainted node: %s", item.Name, item.Spec.NodeName)
	//			break
	//		} else {
	//			t.Logf("Pod %s deleted from node: %s", item.Name, item.Spec.NodeName)
	//		}
	//	case watch.Added:
	//		t.Logf("Pod %s created in node: %s", item.Name, item.Spec.NodeName)
	//	}
	//}
}

func cordonNode(ctx context.Context, clients *test.Clients, name string) error {
	node, err := clients.KubeClient.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}

	node.Spec.Unschedulable = true
	_, err = clients.KubeClient.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	return err
}

func taintNode(ctx context.Context, clients *test.Clients, name string) error {
	node, err := clients.KubeClient.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}

	node.Spec.Taints = append(node.Spec.Taints, v1.Taint{
		Key:    nodeTaintKey,
		Value:  nodeTaintValue,
		Effect: nodeTaintEffect,
	})
	_, err = clients.KubeClient.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	return err
}

func untaintNode(ctx context.Context, clients *test.Clients, name string) error {
	node, err := clients.KubeClient.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}

	for i, taint := range node.Spec.Taints {
		if taint.Key == nodeTaintKey && taint.Value == nodeTaintValue {
			node.Spec.Taints = append(node.Spec.Taints[:i], node.Spec.Taints[i+1:]...)
		}
	}

	_, err = clients.KubeClient.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	return err
}
