package applogs

import (
	"context"
	"fmt"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"polygate/internal/kubeobjects"
)

type Event struct {
	Type    string    `json:"type"`
	Reason  string    `json:"reason"`
	Message string    `json:"message"`
	Object  string    `json:"object"`
	Count   int64     `json:"count"`
	At      time.Time `json:"at"`
}

func (r *Reader) Events(ctx context.Context, accountID, appID string) ([]Event, error) {
	namespace, err := kubeobjects.NamespaceName(accountID)
	if err != nil {
		return nil, err
	}
	deployment, err := kubeobjects.AppName(appID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	selector := labels.Set{
		"app.kubernetes.io/managed-by": "polygate",
		"polygate.io/account-id":       accountID,
		"polygate.io/app-id":           appID,
	}.AsSelector().String()
	pods, err := r.client.Resource(schema.GroupVersionResource{Version: "v1", Resource: "pods"}).
		Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector, Limit: 20})
	if err != nil {
		return nil, err
	}
	if pods.GetContinue() != "" {
		return nil, fmt.Errorf("too many app pods")
	}
	events := make([]Event, 0)
	seen := make(map[string]bool)
	resource := r.client.Resource(schema.GroupVersionResource{Version: "v1", Resource: "events"}).Namespace(namespace)
	collect := func(selector string, matches func(unstructured.Unstructured) bool) error {
		continueToken := ""
		for page := 0; page < 10; page++ {
			list, err := resource.List(ctx, metav1.ListOptions{FieldSelector: selector, Limit: 100, Continue: continueToken})
			if err != nil {
				return err
			}
			for _, item := range list.Items {
				if !matches(item) || seen[item.GetName()] {
					continue
				}
				seen[item.GetName()] = true
				events = append(events, eventSummary(item))
			}
			continueToken = list.GetContinue()
			if continueToken == "" {
				return nil
			}
		}
		return fmt.Errorf("too many app events")
	}
	for _, pod := range filterOwnedPods(pods.Items, accountID, appID) {
		uid := string(pod.GetUID())
		name := pod.GetName()
		if uid == "" {
			continue
		}
		if err := collect(fields.OneTermEqualSelector("involvedObject.uid", uid).String(),
			func(item unstructured.Unstructured) bool {
				kind, _, _ := unstructured.NestedString(item.Object, "involvedObject", "kind")
				actualUID, _, _ := unstructured.NestedString(item.Object, "involvedObject", "uid")
				actualName, _, _ := unstructured.NestedString(item.Object, "involvedObject", "name")
				return kind == "Pod" && actualUID == uid && actualName == name
			}); err != nil {
			return nil, err
		}
	}
	workload, err := r.client.Resource(schema.GroupVersionResource{
		Group: "apps", Version: "v1", Resource: "deployments",
	}).Namespace(namespace).Get(ctx, deployment, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	if err == nil {
		labels := workload.GetLabels()
		if labels["app.kubernetes.io/managed-by"] != "polygate" ||
			labels["polygate.io/account-id"] != accountID || labels["polygate.io/app-id"] != appID {
			return nil, fmt.Errorf("app Deployment ownership labels do not match")
		}
		replicasets, err := r.client.Resource(schema.GroupVersionResource{
			Group: "apps", Version: "v1", Resource: "replicasets",
		}).Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector, Limit: 50})
		if err != nil {
			return nil, err
		}
		if replicasets.GetContinue() != "" {
			return nil, fmt.Errorf("too many app ReplicaSets")
		}
		for _, rs := range replicasets.Items {
			rsLabels := rs.GetLabels()
			if rsLabels["app.kubernetes.io/managed-by"] != "polygate" ||
				rsLabels["polygate.io/account-id"] != accountID || rsLabels["polygate.io/app-id"] != appID {
				continue
			}
			owned := false
			for _, owner := range rs.GetOwnerReferences() {
				if owner.Kind == "Deployment" && owner.UID == workload.GetUID() &&
					owner.Name == deployment && owner.Controller != nil && *owner.Controller {
					owned = true
				}
			}
			if !owned || rs.GetUID() == "" {
				continue
			}
			uid, name := string(rs.GetUID()), rs.GetName()
			if err := collect(fields.OneTermEqualSelector("involvedObject.uid", uid).String(),
				func(item unstructured.Unstructured) bool {
					kind, _, _ := unstructured.NestedString(item.Object, "involvedObject", "kind")
					actualUID, _, _ := unstructured.NestedString(item.Object, "involvedObject", "uid")
					actualName, _, _ := unstructured.NestedString(item.Object, "involvedObject", "name")
					return kind == "ReplicaSet" && actualUID == uid && actualName == name
				}); err != nil {
				return nil, err
			}
		}
	}
	if err := collect(fields.OneTermEqualSelector("involvedObject.name", deployment).String(),
		func(item unstructured.Unstructured) bool {
			kind, _, _ := unstructured.NestedString(item.Object, "involvedObject", "kind")
			name, _, _ := unstructured.NestedString(item.Object, "involvedObject", "name")
			return kind == "Deployment" && name == deployment
		}); err != nil {
		return nil, err
	}
	sort.Slice(events, func(i, j int) bool { return events[i].At.After(events[j].At) })
	if len(events) > 50 {
		events = events[:50]
	}
	return events, nil
}

func eventSummary(item unstructured.Unstructured) Event {
	value := func(path ...string) string {
		text, _, _ := unstructured.NestedString(item.Object, path...)
		return text
	}
	at := item.GetCreationTimestamp().Time
	for _, candidate := range []string{value("lastTimestamp"), value("eventTime")} {
		if parsed, err := time.Parse(time.RFC3339Nano, candidate); err == nil {
			at = parsed
			break
		}
	}
	count, _, _ := unstructured.NestedInt64(item.Object, "count")
	message := []rune(value("message"))
	if len(message) > 1000 {
		message = append(message[:1000], '…')
	}
	return Event{Type: value("type"), Reason: value("reason"), Message: string(message),
		Object: value("involvedObject", "name"), Count: count, At: at}
}
