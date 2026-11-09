package adminquota

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"polygate/internal/domain"
)

func TestApplyQuotaPreservesStorageAndIsIdempotent(t *testing.T) {
	const accountID = "c8114f16-58dc-4f71-918b-8d21c1d661a0"
	namespace := "pg-u-" + strings.ReplaceAll(accountID, "-", "")
	quota := map[string]any{
		"apiVersion": "v1", "kind": "ResourceQuota",
		"metadata": map[string]any{"name": "account-budget", "namespace": namespace, "resourceVersion": "1"},
		"spec": map[string]any{"hard": map[string]any{
			"pods": "4", "count/deployments.apps": "2", "services": "2",
			"requests.cpu": "2", "limits.cpu": "4", "requests.memory": "4Gi", "limits.memory": "4Gi",
			"requests.ephemeral-storage": "512Mi", "limits.ephemeral-storage": "4Gi",
			"requests.storage": "10Gi", "persistentvolumeclaims": "2",
		}},
	}
	patches := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/namespaces/"+namespace+"/resourcequotas/account-budget") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(quota)
		case http.MethodPatch:
			if r.Header.Get("Content-Type") != "application/merge-patch+json" {
				http.Error(w, "wrong patch type", http.StatusBadRequest)
				return
			}
			var patch map[string]any
			if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if patch["metadata"].(map[string]any)["resourceVersion"] != quota["metadata"].(map[string]any)["resourceVersion"] {
				http.Error(w, "conflict", http.StatusConflict)
				return
			}
			hard := quota["spec"].(map[string]any)["hard"].(map[string]any)
			for key, value := range patch["spec"].(map[string]any)["hard"].(map[string]any) {
				hard[key] = value
			}
			quota["metadata"].(map[string]any)["resourceVersion"] = "2"
			patches++
			_ = json.NewEncoder(w).Encode(quota)
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
	}))
	defer api.Close()
	client, err := dynamic.NewForConfig(&rest.Config{Host: api.URL})
	if err != nil {
		t.Fatal(err)
	}
	controller := Kubernetes{Client: client}
	desired := domain.Quota{CPURequestMilli: 3000, CPULimitMilli: 6000, MemoryMiB: 8192, Pods: 5, Apps: 3}
	if err := controller.Apply(context.Background(), accountID, desired); err != nil {
		t.Fatal(err)
	}
	if err := controller.Apply(context.Background(), accountID, desired); err != nil {
		t.Fatal(err)
	}
	if patches != 1 {
		t.Fatalf("patch count = %d", patches)
	}
	hard := quota["spec"].(map[string]any)["hard"].(map[string]any)
	if hard["requests.storage"] != "10Gi" || hard["persistentvolumeclaims"] != "2" ||
		hard["requests.memory"] != "8192Mi" || hard["pods"] != "5" {
		t.Fatalf("quota patch changed wrong limits: %+v", hard)
	}
}
