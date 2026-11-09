package applogs

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

func TestMetricsOnlyIncludesOwnedPods(t *testing.T) {
	const account = "c8114f16-58dc-4f71-918b-8d21c1d661a0"
	const app = "a42cc824-7da4-47a6-90da-8d2c478e70c0"
	name := "app-" + strings.ReplaceAll(app, "-", "") + "-abc123-xyz99"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/apis/metrics.k8s.io/") {
			_, _ = w.Write([]byte(`{"apiVersion":"metrics.k8s.io/v1beta1","kind":"PodMetricsList","items":[` +
				`{"apiVersion":"metrics.k8s.io/v1beta1","kind":"PodMetrics","metadata":{"name":"` + name + `"},` +
				`"timestamp":"2026-11-07T11:00:00Z","window":"15s","containers":[{"name":"app",` +
				`"usage":{"cpu":"25m","memory":"2Mi"}}]},` +
				`{"apiVersion":"metrics.k8s.io/v1beta1","kind":"PodMetrics","metadata":{"name":"foreign"},` +
				`"containers":[{"name":"app","usage":{"cpu":"500m","memory":"4Gi"}}]}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"PodList","items":[` +
			`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"` + name + `",` +
			`"labels":{"app.kubernetes.io/managed-by":"polygate","polygate.io/account-id":"` + account +
			`","polygate.io/app-id":"` + app + `"}}},` +
			`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"foreign",` +
			`"labels":{"app.kubernetes.io/managed-by":"polygate","polygate.io/account-id":"other",` +
			`"polygate.io/app-id":"` + app + `"}}}]}`))
	}))
	defer server.Close()
	client, err := dynamic.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	reader := &Reader{client: client}
	items, err := reader.Metrics(t.Context(), account, app)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Pod != name || items[0].CPUMilli != 25 || items[0].MemoryBytes != 2*1024*1024 {
		t.Fatalf("incorrect or foreign metrics: %+v", items)
	}
}
