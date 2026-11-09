package applogs

import (
	"context"
	"fmt"
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"polygate/internal/kubeobjects"
)

type PodMetric struct {
	Pod         string `json:"pod"`
	CPUMilli    int64  `json:"cpuMilli"`
	MemoryBytes int64  `json:"memoryBytes"`
	Timestamp   string `json:"timestamp"`
	Window      string `json:"window"`
}

// Metrics joins PodMetrics against Pods whose account and app labels were
// rechecked locally. The metrics API does not decide tenant ownership.
func (r *Reader) Metrics(ctx context.Context, accountID, appID string) ([]PodMetric, error) {
	namespace, err := kubeobjects.NamespaceName(accountID)
	if err != nil {
		return nil, err
	}
	if _, err := kubeobjects.AppName(appID); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	selector := labels.Set{"app.kubernetes.io/managed-by": "polygate", "polygate.io/account-id": accountID,
		"polygate.io/app-id": appID}.AsSelector().String()
	pods, err := r.client.Resource(schema.GroupVersionResource{Version: "v1", Resource: "pods"}).
		Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector, Limit: 20})
	if err != nil {
		return nil, err
	}
	if pods.GetContinue() != "" {
		return nil, fmt.Errorf("too many app pods")
	}
	owned := map[string]bool{}
	for _, pod := range filterOwnedPods(pods.Items, accountID, appID) {
		owned[pod.GetName()] = true
	}
	if len(owned) == 0 {
		return []PodMetric{}, nil
	}
	metrics, err := r.client.Resource(schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "pods"}).
		Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	result := make([]PodMetric, 0, len(owned))
	for _, metric := range metrics.Items {
		if !owned[metric.GetName()] {
			continue
		}
		entry := PodMetric{Pod: metric.GetName()}
		entry.Timestamp, _, _ = unstructured.NestedString(metric.Object, "timestamp")
		entry.Window, _, _ = unstructured.NestedString(metric.Object, "window")
		containers, found, err := unstructured.NestedSlice(metric.Object, "containers")
		if err != nil || !found {
			return nil, fmt.Errorf("invalid pod metrics container list")
		}
		for _, container := range containers {
			item, ok := container.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid pod metrics container")
			}
			usage, ok := item["usage"].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid pod metrics usage")
			}
			cpuText, cpuOK := usage["cpu"].(string)
			memoryText, memoryOK := usage["memory"].(string)
			if !cpuOK || !memoryOK {
				return nil, fmt.Errorf("incomplete pod metrics usage")
			}
			cpu, err := resource.ParseQuantity(cpuText)
			if err != nil {
				return nil, err
			}
			memory, err := resource.ParseQuantity(memoryText)
			if err != nil {
				return nil, err
			}
			entry.CPUMilli += cpu.MilliValue()
			entry.MemoryBytes += memory.Value()
		}
		result = append(result, entry)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Pod < result[j].Pod })
	return result, nil
}
