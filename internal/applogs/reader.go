package applogs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"polygate/internal/kubeobjects"
)

var ErrNoPods = errors.New("no app pods")
var ErrPodNotFound = errors.New("pod is not part of this app")
var ErrNoPreviousLogs = errors.New("no previous container logs")

const maxLogBytes int64 = 128 << 10
const maxLogResponseBytes int64 = 4 << 20

type Result struct {
	Pod      string `json:"pod"`
	Data     string `json:"data"`
	Previous bool   `json:"previous"`
}

type PodInfo struct {
	Name                  string `json:"name"`
	Phase                 string `json:"phase"`
	Ready                 bool   `json:"ready"`
	Restarts              int64  `json:"restarts"`
	Reason                string `json:"reason,omitempty"`
	Message               string `json:"message,omitempty"`
	WaitingReason         string `json:"waitingReason,omitempty"`
	LastTerminationReason string `json:"lastTerminationReason,omitempty"`
	LastExitCode          *int64 `json:"lastExitCode,omitempty"`
}

type Reader struct {
	client   dynamic.Interface
	http     *http.Client
	host     string
	lokiURL  string
	lokiHTTP *http.Client
}

func NewInCluster() (*Reader, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	config.QPS, config.Burst = 3, 6
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	httpClient, err := rest.HTTPClientFor(config)
	if err != nil {
		return nil, err
	}
	return &Reader{client: client, http: httpClient, host: config.Host,
		lokiURL: os.Getenv("POLYGATE_LOKI_URL"), lokiHTTP: &http.Client{Timeout: 8 * time.Second}}, nil
}

func (r *Reader) Read(ctx context.Context, accountID, appID, requestedPod string, previous bool) (Result, error) {
	pods, namespace, err := r.listOwnedPods(ctx, accountID, appID)
	if err != nil {
		return Result{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	pod, err := selectPod(pods, accountID, appID, requestedPod)
	if err != nil {
		return Result{}, err
	}
	if previous {
		statuses, _, _ := unstructured.NestedSlice(pod.Object, "status", "containerStatuses")
		available := false
		for _, value := range statuses {
			status, ok := value.(map[string]any)
			if !ok || status["name"] != "app" {
				continue
			}
			count, _ := status["restartCount"].(int64)
			available = count > 0
		}
		if !available {
			return Result{}, ErrNoPreviousLogs
		}
	}
	endpoint := strings.TrimRight(r.host, "/") + "/api/v1/namespaces/" + namespace +
		"/pods/" + url.PathEscape(pod.GetName()) + "/log"
	query := url.Values{"container": {"app"}, "tailLines": {"200"}, "timestamps": {"true"}}
	if previous {
		query.Set("previous", "true")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+query.Encode(), nil)
	if err != nil {
		return Result{}, err
	}
	response, err := r.http.Do(request)
	if err != nil {
		return Result{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		if previous && response.StatusCode == http.StatusBadRequest {
			return Result{}, ErrNoPreviousLogs
		}
		return Result{}, fmt.Errorf("pod log request returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxLogResponseBytes+1))
	if err != nil {
		return Result{}, err
	}
	if len(data) > int(maxLogResponseBytes) {
		return Result{}, fmt.Errorf("pod log response exceeds 4 MiB")
	}
	if len(data) > int(maxLogBytes) {
		data = data[len(data)-int(maxLogBytes):]
		if firstNewline := bytes.IndexByte(data, '\n'); firstNewline >= 0 && firstNewline+1 < len(data) {
			data = data[firstNewline+1:]
		}
	}
	return Result{Pod: pod.GetName(), Data: string(data), Previous: previous}, nil
}

func (r *Reader) List(ctx context.Context, accountID, appID string) ([]PodInfo, error) {
	pods, _, err := r.listOwnedPods(ctx, accountID, appID)
	if err != nil {
		return nil, err
	}
	sort.Slice(pods, func(i, j int) bool {
		return pods[i].GetCreationTimestamp().After(pods[j].GetCreationTimestamp().Time)
	})
	result := make([]PodInfo, 0, len(pods))
	for _, pod := range pods {
		phase, _, _ := unstructured.NestedString(pod.Object, "status", "phase")
		statuses, _, _ := unstructured.NestedSlice(pod.Object, "status", "containerStatuses")
		item := PodInfo{Name: pod.GetName(), Phase: phase, Ready: len(statuses) > 0}
		item.Reason, _, _ = unstructured.NestedString(pod.Object, "status", "reason")
		item.Message, _, _ = unstructured.NestedString(pod.Object, "status", "message")
		if item.Reason == "" {
			conditions, _, _ := unstructured.NestedSlice(pod.Object, "status", "conditions")
			for _, value := range conditions {
				condition, ok := value.(map[string]any)
				if !ok || condition["type"] != "PodScheduled" || condition["status"] != "False" {
					continue
				}
				item.Reason, _ = condition["reason"].(string)
				item.Message, _ = condition["message"].(string)
				break
			}
		}
		if len(item.Message) > 256 {
			item.Message = item.Message[:256]
		}
		for _, value := range statuses {
			status, ok := value.(map[string]any)
			if !ok {
				item.Ready = false
				continue
			}
			if ready, ok := status["ready"].(bool); !ok || !ready {
				item.Ready = false
			}
			if count, ok := status["restartCount"].(int64); ok {
				item.Restarts += count
			}
			if status["name"] == "app" {
				if state, ok := status["state"].(map[string]any); ok {
					if waiting, ok := state["waiting"].(map[string]any); ok {
						item.WaitingReason, _ = waiting["reason"].(string)
					}
				}
				if last, ok := status["lastState"].(map[string]any); ok {
					if terminated, ok := last["terminated"].(map[string]any); ok {
						item.LastTerminationReason, _ = terminated["reason"].(string)
						if code, ok := terminated["exitCode"].(int64); ok {
							item.LastExitCode = &code
						}
					}
				}
			}
		}
		result = append(result, item)
	}
	return result, nil
}

func (r *Reader) listOwnedPods(ctx context.Context, accountID, appID string) ([]unstructured.Unstructured, string, error) {
	namespace, err := kubeobjects.NamespaceName(accountID)
	if err != nil {
		return nil, "", err
	}
	if _, err := kubeobjects.AppName(appID); err != nil {
		return nil, "", err
	}
	selector := labels.Set{
		"app.kubernetes.io/managed-by": "polygate",
		"polygate.io/account-id":       accountID,
		"polygate.io/app-id":           appID,
	}.AsSelector().String()
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	pods, err := r.client.Resource(schema.GroupVersionResource{Version: "v1", Resource: "pods"}).
		Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector, Limit: 20})
	if err != nil {
		return nil, "", err
	}
	if pods.GetContinue() != "" {
		return nil, "", fmt.Errorf("too many app pods")
	}
	return filterOwnedPods(pods.Items, accountID, appID), namespace, nil
}

func selectPod(pods []unstructured.Unstructured, accountID, appID, requested string) (unstructured.Unstructured, error) {
	owned := filterOwnedPods(pods, accountID, appID)
	if requested != "" {
		for _, pod := range owned {
			if pod.GetName() == requested {
				return pod, nil
			}
		}
		return unstructured.Unstructured{}, ErrPodNotFound
	}
	if len(owned) == 0 {
		return unstructured.Unstructured{}, ErrNoPods
	}
	sort.Slice(owned, func(i, j int) bool {
		return owned[i].GetCreationTimestamp().After(owned[j].GetCreationTimestamp().Time)
	})
	return owned[0], nil
}

func filterOwnedPods(pods []unstructured.Unstructured, accountID, appID string) []unstructured.Unstructured {
	owned := make([]unstructured.Unstructured, 0, len(pods))
	for _, pod := range pods {
		labels := pod.GetLabels()
		if labels["app.kubernetes.io/managed-by"] == "polygate" &&
			labels["polygate.io/account-id"] == accountID &&
			labels["polygate.io/app-id"] == appID {
			owned = append(owned, pod)
		}
	}
	return owned
}
