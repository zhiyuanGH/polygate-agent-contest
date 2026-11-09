package applogs

import (
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

const maxLogBytes int64 = 128 << 10

type Result struct {
	Pod  string `json:"pod"`
	Data string `json:"data"`
}

type PodInfo struct {
	Name     string `json:"name"`
	Phase    string `json:"phase"`
	Ready    bool   `json:"ready"`
	Restarts int64  `json:"restarts"`
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

func (r *Reader) Read(ctx context.Context, accountID, appID, requestedPod string) (Result, error) {
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
	endpoint := strings.TrimRight(r.host, "/") + "/api/v1/namespaces/" + namespace +
		"/pods/" + url.PathEscape(pod.GetName()) + "/log"
	query := url.Values{"container": {"app"}, "tailLines": {"200"},
		"limitBytes": {"131072"}, "timestamps": {"true"}}
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
		return Result{}, fmt.Errorf("pod log request returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxLogBytes+1))
	if err != nil {
		return Result{}, err
	}
	if len(data) > int(maxLogBytes) {
		data = data[:maxLogBytes]
	}
	return Result{Pod: pod.GetName(), Data: string(data)}, nil
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
