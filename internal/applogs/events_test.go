package applogs

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

func TestEventsRecheckInvolvedObjects(t *testing.T) {
	const account = "c8114f16-58dc-4f71-918b-8d21c1d661a0"
	const app = "a42cc824-7da4-47a6-90da-8d2c478e70c0"
	podName := "app-" + strings.ReplaceAll(app, "-", "") + "-abc123-xyz99"
	deployment := "app-" + strings.ReplaceAll(app, "-", "")
	event := func(name, kind, object, uid string) string {
		return `{"apiVersion":"v1","kind":"Event","metadata":{"name":"` + name + `"},` +
			`"involvedObject":{"kind":"` + kind + `","name":"` + object + `","uid":"` + uid + `"},` +
			`"reason":"Scheduled","type":"Normal","message":"ok","count":1}`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/deployments/") {
			_, _ = w.Write([]byte(`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"` + deployment +
				`","uid":"deployment-uid","labels":{"app.kubernetes.io/managed-by":"polygate",` +
				`"polygate.io/account-id":"` + account + `","polygate.io/app-id":"` + app + `"}}}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/replicasets") {
			_, _ = w.Write([]byte(`{"apiVersion":"apps/v1","kind":"ReplicaSetList","items":[` +
				`{"apiVersion":"apps/v1","kind":"ReplicaSet","metadata":{"name":"owned-rs",` +
				`"uid":"owned-rs-uid","labels":{"app.kubernetes.io/managed-by":"polygate",` +
				`"polygate.io/account-id":"` + account + `","polygate.io/app-id":"` + app + `"},` +
				`"ownerReferences":[{"kind":"Deployment","name":"` + deployment +
				`","uid":"deployment-uid","controller":true}]}},` +
				`{"apiVersion":"apps/v1","kind":"ReplicaSet","metadata":{"name":"foreign-rs",` +
				`"uid":"foreign-rs-uid","labels":{"app.kubernetes.io/managed-by":"polygate",` +
				`"polygate.io/account-id":"` + account + `","polygate.io/app-id":"` + app + `"},` +
				`"ownerReferences":[{"kind":"Deployment","name":"` + deployment +
				`","uid":"other-deployment-uid","controller":true}]}}]}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/pods") {
			_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"PodList","items":[` +
				`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"` + podName + `","uid":"pod-uid",` +
				`"labels":{"app.kubernetes.io/managed-by":"polygate","polygate.io/account-id":"` + account +
				`","polygate.io/app-id":"` + app + `"}}}]}`))
			return
		}
		if strings.Contains(r.URL.Query().Get("fieldSelector"), "owned-rs-uid") {
			_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"EventList","items":[` +
				event("owned-rs-event", "ReplicaSet", "owned-rs", "owned-rs-uid") + `,` +
				event("foreign-rs-event", "ReplicaSet", "foreign-rs", "owned-rs-uid") + `]}`))
			return
		}
		if strings.Contains(r.URL.Query().Get("fieldSelector"), "foreign-rs-uid") {
			_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"EventList","items":[` +
				event("foreign-only", "ReplicaSet", "foreign-rs", "foreign-rs-uid") + `]}`))
			return
		}
		if strings.Contains(r.URL.Query().Get("fieldSelector"), "involvedObject.uid") {
			_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"EventList","items":[` +
				event("owned-pod", "Pod", podName, "pod-uid") + `,` +
				event("foreign-pod", "Pod", "other-pod", "pod-uid") + `]}`))
			return
		}
		_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"EventList","items":[` +
			event("owned-deployment", "Deployment", deployment, "deployment-uid") + `,` +
			event("same-name-service", "Service", deployment, "service-uid") + `]}`))
	}))
	defer server.Close()
	client, err := dynamic.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	reader := &Reader{client: client, http: server.Client(), host: server.URL}
	items, err := reader.Events(t.Context(), account, app)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("foreign events were included: %+v", items)
	}
	seen := map[string]bool{items[0].Object: true, items[1].Object: true, items[2].Object: true}
	if !seen[podName] || !seen[deployment] || !seen["owned-rs"] || seen["foreign-rs"] {
		t.Fatalf("owned Pod, ReplicaSet or Deployment events missing: %+v", items)
	}
}
