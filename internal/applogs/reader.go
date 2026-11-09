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
	namespace, err := kubeobjects.NamespaceName(accountID)
	if err != nil {
		return Result{}, err
	}
	if _, err := kubeobjects.AppName(appID); err != nil {
		return Result{}, err
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
		return Result{}, err
	}
	if pods.GetContinue() != "" {
		return Result{}, fmt.Errorf("too many app pods")
	}
	pod, err := selectPod(pods.Items, accountID, appID, requestedPod)
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
