package applogs

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

func TestSelectPodChecksOwnershipEvenAfterLabelSelector(t *testing.T) {
	owned := func(name, account string, when time.Time) unstructured.Unstructured {
		pod := unstructured.Unstructured{}
		pod.SetName(name)
		pod.SetCreationTimestamp(metav1.NewTime(when))
		pod.SetLabels(map[string]string{
			"app.kubernetes.io/managed-by": "polygate", "polygate.io/account-id": account,
			"polygate.io/app-id": "app-a"})
		return pod
	}
	pods := []unstructured.Unstructured{owned("old", "account-a", time.Now().Add(-time.Hour)),
		owned("foreign", "account-b", time.Now()), owned("new", "account-a", time.Now())}
	selected, err := selectPod(pods, "account-a", "app-a", "")
	if err != nil || selected.GetName() != "new" {
		t.Fatalf("latest owned pod was not selected: %s %v", selected.GetName(), err)
	}
	if _, err := selectPod(pods, "account-a", "app-a", "foreign"); !errors.Is(err, ErrPodNotFound) {
		t.Fatalf("foreign pod accepted: %v", err)
	}
	if _, err := selectPod(pods, "account-a", "app-a", "old"); err != nil {
		t.Fatalf("owned pod rejected: %v", err)
	}
	if _, err := selectPod(pods, "account-c", "app-a", ""); !errors.Is(err, ErrNoPods) {
		t.Fatalf("missing app pod: %v", err)
	}
}

func TestReaderBoundsLogAndUsesOwnedNamespace(t *testing.T) {
	const account = "c8114f16-58dc-4f71-918b-8d21c1d661a0"
	const app = "a42cc824-7da4-47a6-90da-8d2c478e70c0"
	namespace := "pg-u-" + strings.ReplaceAll(account, "-", "")
	name := "app-" + strings.ReplaceAll(app, "-", "") + "-abc123-xyz99"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/namespaces/"+namespace+"/pods") {
			t.Errorf("wrong namespace: %s", r.URL.Path)
			http.Error(w, "wrong namespace", http.StatusNotFound)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/pods") {
			selector := r.URL.Query().Get("labelSelector")
			if !strings.Contains(selector, "polygate.io/account-id="+account) ||
				!strings.Contains(selector, "polygate.io/app-id="+app) {
				t.Errorf("wrong selector: %s", selector)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"PodList","metadata":{"resourceVersion":"1"},"items":[{"apiVersion":"v1","kind":"Pod","metadata":{"name":"` + name + `","labels":{"app.kubernetes.io/managed-by":"polygate","polygate.io/account-id":"` + account + `","polygate.io/app-id":"` + app + `"}}}]}`))
			return
		}
		if r.URL.Path != "/api/v1/namespaces/"+namespace+"/pods/"+name+"/log" ||
			r.URL.Query().Get("limitBytes") != "131072" || r.URL.Query().Get("tailLines") != "200" {
			t.Errorf("wrong Pod log request: %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(strings.Repeat("x", 200000)))
	}))
	defer server.Close()
	client, err := dynamic.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	reader := &Reader{client: client, http: server.Client(), host: server.URL}
	result, err := reader.Read(t.Context(), account, app, "")
	if err != nil || result.Pod != name || len(result.Data) != int(maxLogBytes) {
		t.Fatalf("bounded Pod log read failed: pod=%s bytes=%d err=%v", result.Pod, len(result.Data), err)
	}
}
