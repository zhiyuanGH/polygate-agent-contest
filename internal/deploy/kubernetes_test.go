package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"polygate/internal/domain"
	"polygate/internal/harbor"
	"polygate/internal/kubeobjects"
	"polygate/internal/volume"
)

func TestPrecreatedTenantDoesNotReadUnrelatedComponentObjects(t *testing.T) {
	requests := 0
	testAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Error(w, "secret read is forbidden", http.StatusForbidden)
	}))
	defer testAPI.Close()
	client, err := dynamic.NewForConfig(&rest.Config{Host: testAPI.URL})
	if err != nil {
		t.Fatal(err)
	}
	objects := kubeobjects.List{Items: []kubeobjects.Object{
		{"kind": "Secret", "metadata": kubeobjects.Object{
			"name": "student-credentials", "namespace": "pg-u-test"}},
		{"kind": "Role", "metadata": kubeobjects.Object{
			"name": "student-secret-editor", "namespace": "pg-u-test"}},
		{"kind": "RoleBinding", "metadata": kubeobjects.Object{
			"name": "api-log-reader", "namespace": "pg-u-test"}},
	}}
	if err := verifyObjects(t.Context(), client, objects); err != nil || requests != 0 {
		t.Fatalf("unrelated tenant object was read by workload controller: requests=%d err=%v", requests, err)
	}
	objects.Items = append(objects.Items, kubeobjects.Object{"kind": "Role", "metadata": kubeobjects.Object{
		"name": "workload-controller", "namespace": "pg-u-test"}})
	if err := verifyObjects(t.Context(), client, objects); err == nil || requests != 1 {
		t.Fatalf("workload controller's own Role was not verified: requests=%d err=%v", requests, err)
	}
}

func TestKubernetesAdapterAppliesOnlyGeneratedObjectsAndReadsDeployment(t *testing.T) {
	const accountID = "c8114f16-58dc-4f71-918b-8d21c1d661a0"
	const appID = "a42cc824-7da4-47a6-90da-8d2c478e70c0"
	const digest = "registry.example/approved/agent@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	var patched []string
	testAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPatch {
			if r.URL.Query().Get("fieldManager") == "" ||
				r.Header.Get("Content-Type") != "application/apply-patch+yaml" {
				http.Error(w, "server-side apply required", http.StatusBadRequest)
				return
			}
			patched = append(patched, r.URL.Path)
			var object map[string]any
			if err := json.NewDecoder(r.Body).Decode(&object); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(object)
			return
		}
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/deployments/") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"apiVersion": "apps/v1", "kind": "Deployment",
				"metadata": map[string]any{"name": "app-" + strings.ReplaceAll(appID, "-", ""), "generation": 3,
					"labels": map[string]any{"app.kubernetes.io/managed-by": "polygate",
						"polygate.io/account-id": accountID, "polygate.io/app-id": appID}},
				"spec": map[string]any{"replicas": 1, "template": map[string]any{"spec": map[string]any{
					"containers": []any{map[string]any{"name": "app", "image": digest}},
				}}},
				"status": map[string]any{"observedGeneration": 3, "updatedReplicas": 1, "readyReplicas": 1},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer testAPI.Close()
	client, err := dynamic.NewForConfig(&rest.Config{Host: testAPI.URL})
	if err != nil {
		t.Fatal(err)
	}
	cluster := &KubernetesCluster{provisioner: client, workloads: client}
	task := Task{AccountID: accountID, AppID: appID, ImageDigest: digest,
		Spec: domain.AppSpec{Name: "agent", Image: "student.example/untrusted:latest", Port: 8080,
			Resources: domain.Resources{CPURequestMilli: 250, CPULimitMilli: 1000, MemoryMiB: 512, Replicas: 1}}}
	if err := cluster.EnsureTenant(context.Background(), accountID, domain.StudentQuota); err != nil {
		t.Fatal(err)
	}
	if err := cluster.EnsureWorkload(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if len(patched) != 22 || !strings.Contains(patched[0], "/namespaces/pg-u-") ||
		!strings.Contains(patched[18], "/networkpolicies/allow-app-gateway") ||
		!strings.Contains(patched[20], "/deployments/app-") ||
		!strings.Contains(patched[21], "/services/app-") {
		t.Fatalf("unexpected Kubernetes writes: %v", patched)
	}
	observed, err := cluster.ObserveWorkload(context.Background(), task)
	if err != nil || !observed.Ready() || observed.ImageDigest != digest || observed.DesiredReplicas != 1 {
		t.Fatalf("deployment observation: %+v, %v", observed, err)
	}
}

func TestPersistentWorkloadWaitsForBoundClaimBeforeAnyWrite(t *testing.T) {
	const accountID = "c8114f16-58dc-4f71-918b-8d21c1d661a0"
	const appID = "a42cc824-7da4-47a6-90da-8d2c478e70c0"
	const digest = "registry.example/approved/agent@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	writes := 0
	testAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes++
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`))
	}))
	defer testAPI.Close()
	client, err := dynamic.NewForConfig(&rest.Config{Host: testAPI.URL})
	if err != nil {
		t.Fatal(err)
	}
	cluster := &KubernetesCluster{workloads: client}
	task := Task{AccountID: accountID, AppID: appID, ImageDigest: digest, VolumeSizeGiB: 5,
		Spec: domain.AppSpec{Name: "agent", Image: "ignored.example/agent:v1", Port: 8080, Persistent: true,
			VolumeID: "11111111-1111-4111-8111-111111111111", MountPath: "/data",
			Resources: domain.Resources{CPURequestMilli: 250, CPULimitMilli: 1000, MemoryMiB: 512, Replicas: 1}}}
	if err := cluster.EnsureWorkload(context.Background(), task); !errors.Is(err, ErrVolumePending) || writes != 0 {
		t.Fatalf("missing PVC wrote workload or returned wrong error: %v writes=%d", err, writes)
	}
}

func TestPersistentImageUpdateAppliesRecreateAfterBoundClaim(t *testing.T) {
	const accountID = "c8114f16-58dc-4f71-918b-8d21c1d661a0"
	const appID = "a42cc824-7da4-47a6-90da-8d2c478e70c0"
	const volumeID = "11111111-1111-4111-8111-111111111111"
	digest := "registry.example/approved/agent@sha256:" + strings.Repeat("a", 64)
	claim, err := volume.DesiredClaim(volume.Task{ID: volumeID, AccountID: accountID, SizeGiB: 5})
	if err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedField(claim.Object, "pvc-test", "spec", "volumeName"); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedField(claim.Object, "Bound", "status", "phase"); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedField(claim.Object, "5Gi", "status", "capacity", "storage"); err != nil {
		t.Fatal(err)
	}
	var strategy string
	patches := 0
	testAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/persistentvolumeclaims/") {
			_ = json.NewEncoder(w).Encode(claim.Object)
			return
		}
		if r.Method == http.MethodPatch {
			patches++
			var object map[string]any
			if json.NewDecoder(r.Body).Decode(&object) != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if object["kind"] == "Deployment" {
				strategy = object["spec"].(map[string]any)["strategy"].(map[string]any)["type"].(string)
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(object)
			return
		}
		http.NotFound(w, r)
	}))
	defer testAPI.Close()
	client, err := dynamic.NewForConfig(&rest.Config{Host: testAPI.URL})
	if err != nil {
		t.Fatal(err)
	}
	task := Task{Kind: "Update", AccountID: accountID, AppID: appID, ImageDigest: digest, VolumeSizeGiB: 5,
		Spec: domain.AppSpec{Name: "agent", Image: "ignored.example/agent:v1", Port: 8080, Persistent: true,
			VolumeID: volumeID, MountPath: "/data",
			Resources: domain.Resources{CPURequestMilli: 250, CPULimitMilli: 1000, MemoryMiB: 512, Replicas: 1}}}
	cluster := &KubernetesCluster{workloads: client}
	if err := cluster.EnsureWorkload(context.Background(), task); err != nil || patches != 2 || strategy != "Recreate" {
		t.Fatalf("persistent update: patches=%d strategy=%q err=%v", patches, strategy, err)
	}
}

func TestStatelessResourceUpdateCanRecreateWithoutSurge(t *testing.T) {
	const accountID = "c8114f16-58dc-4f71-918b-8d21c1d661a0"
	const appID = "a42cc824-7da4-47a6-90da-8d2c478e70c0"
	const digest = "registry.example/approved/agent@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	var strategy string
	patched := 0
	testAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPatch {
			http.NotFound(w, r)
			return
		}
		patched++
		var object map[string]any
		if err := json.NewDecoder(r.Body).Decode(&object); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if object["kind"] == "Deployment" {
			strategy = object["spec"].(map[string]any)["strategy"].(map[string]any)["type"].(string)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(object)
	}))
	defer testAPI.Close()
	client, err := dynamic.NewForConfig(&rest.Config{Host: testAPI.URL})
	if err != nil {
		t.Fatal(err)
	}
	task := Task{Kind: "Update", Strategy: "Recreate", AccountID: accountID, AppID: appID, ImageDigest: digest,
		Spec: domain.AppSpec{Name: "agent", Image: "ignored.example/agent:v1", Port: 8080,
			Resources: domain.Resources{CPURequestMilli: 250, CPULimitMilli: 1000, MemoryMiB: 512, Replicas: 1}}}
	if err := (&KubernetesCluster{workloads: client}).EnsureWorkload(context.Background(), task); err != nil || patched != 2 || strategy != "Recreate" {
		t.Fatalf("stateless Recreate: patches=%d strategy=%q err=%v", patched, strategy, err)
	}
}

func TestRestartWaitsForNewPodAndRollbackWaitsForOldTemplate(t *testing.T) {
	const accountID = "c8114f16-58dc-4f71-918b-8d21c1d661a0"
	const appID = "a42cc824-7da4-47a6-90da-8d2c478e70c0"
	const taskID = "41111111-1111-4111-8111-111111111111"
	const digest = "registry.example/approved/agent@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	templateID, podID := "", ""
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPatch {
			var object map[string]any
			if err := json.NewDecoder(r.Body).Decode(&object); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if object["kind"] == "Deployment" {
				metadata := object["spec"].(map[string]any)["template"].(map[string]any)["metadata"].(map[string]any)
				templateID = ""
				if annotations, ok := metadata["annotations"].(map[string]any); ok {
					templateID, _ = annotations["polygate.io/restart-task-id"].(string)
				}
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(object)
			return
		}
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/deployments/") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"apiVersion": "apps/v1", "kind": "Deployment",
				"metadata": map[string]any{"generation": 2, "labels": map[string]any{
					"app.kubernetes.io/managed-by": "polygate", "polygate.io/account-id": accountID,
					"polygate.io/app-id": appID}},
				"spec": map[string]any{"replicas": 1, "template": map[string]any{
					"metadata": map[string]any{"annotations": map[string]any{"polygate.io/restart-task-id": templateID}},
					"spec":     map[string]any{"containers": []any{map[string]any{"image": digest}}}}},
				"status": map[string]any{"observedGeneration": 2, "replicas": 1,
					"updatedReplicas": 1, "readyReplicas": 1},
			})
			return
		}
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pods") {
			_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "v1", "kind": "PodList",
				"items": []any{map[string]any{"apiVersion": "v1", "kind": "Pod",
					"metadata": map[string]any{"annotations": map[string]any{"polygate.io/restart-task-id": podID}},
					"spec":     map[string]any{"containers": []any{map[string]any{"image": digest}}}}}})
			return
		}
		http.NotFound(w, r)
	}))
	defer api.Close()
	client, err := dynamic.NewForConfig(&rest.Config{Host: api.URL})
	if err != nil {
		t.Fatal(err)
	}
	cluster := &KubernetesCluster{workloads: client}
	task := Task{ID: taskID, Kind: "Update", UpdateKind: "Restart", Strategy: "Recreate",
		AccountID: accountID, AppID: appID, ImageDigest: digest,
		Spec: domain.AppSpec{Name: "agent", Image: "ignored.example/agent:v1", Port: 8080,
			Resources: domain.Resources{CPURequestMilli: 250, CPULimitMilli: 1000, MemoryMiB: 512, Replicas: 1}}}
	if err := cluster.EnsureWorkload(context.Background(), task); err != nil || templateID != taskID {
		t.Fatalf("restart template marker: %q %v", templateID, err)
	}
	if observed, err := cluster.ObserveWorkload(context.Background(), task); err != nil || observed.OldPods != 1 {
		t.Fatalf("old Pod was accepted as restarted: %+v %v", observed, err)
	}
	podID = taskID
	if observed, err := cluster.ObserveWorkload(context.Background(), task); err != nil || observed.OldPods != 0 || !observed.Ready() {
		t.Fatalf("new Pod was not accepted: %+v %v", observed, err)
	}
	task.Kind = "Rollback"
	if err := cluster.EnsureWorkload(context.Background(), task); err != nil || templateID != "" {
		t.Fatalf("rollback template marker: %q %v", templateID, err)
	}
	if observed, err := cluster.ObserveWorkload(context.Background(), task); err != nil || observed.OldPods != 1 {
		t.Fatalf("failed restart Pod was accepted as restored: %+v %v", observed, err)
	}
	podID = ""
	if observed, err := cluster.ObserveWorkload(context.Background(), task); err != nil || observed.OldPods != 0 || !observed.Ready() {
		t.Fatalf("restored Pod was not accepted: %+v %v", observed, err)
	}
}

func TestPrivateWorkloadWaitsForAccountPullSecret(t *testing.T) {
	const accountID = "c8114f16-58dc-4f71-918b-8d21c1d661a0"
	const appID = "a42cc824-7da4-47a6-90da-8d2c478e70c0"
	image := harbor.RegistryHost + "/pg-u-" + strings.ReplaceAll(accountID, "-", "") +
		"/agent@sha256:" + strings.Repeat("a", 64)
	ready := false
	patched := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/secrets/harbor-pull") {
			if !ready {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "v1", "kind": "Status",
					"status": "Failure", "reason": "NotFound", "code": 404})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "v1", "kind": "Secret",
				"metadata": map[string]any{"name": "harbor-pull",
					"labels": map[string]string{"polygate.io/managed-by": "polygate",
						"polygate.io/account-id": accountID},
					"annotations": map[string]string{"polygate.io/pull-expires-at": time.Now().Add(time.Hour).Format(time.RFC3339)}},
				"type": "kubernetes.io/dockerconfigjson", "data": map[string]string{".dockerconfigjson": "e30="}})
			return
		}
		if r.Method == http.MethodPatch {
			patched++
			var object map[string]any
			if json.NewDecoder(r.Body).Decode(&object) != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(object)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	client, err := dynamic.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	cluster := &KubernetesCluster{workloads: client}
	task := Task{AccountID: accountID, AppID: appID, ImageDigest: image,
		Spec: domain.AppSpec{Name: "agent", Image: "registry.example/agent:v1", Port: 3000,
			Resources: domain.Resources{CPURequestMilli: 250, CPULimitMilli: 1000, MemoryMiB: 512, Replicas: 1}}}
	if err := cluster.EnsureWorkload(context.Background(), task); !errors.Is(err, ErrPullCredentialPending) || patched != 0 {
		t.Fatalf("workload started before pull credential: err=%v patches=%d", err, patched)
	}
	ready = true
	if err := cluster.EnsureWorkload(context.Background(), task); err != nil || patched != 2 {
		t.Fatalf("workload did not start after pull credential: err=%v patches=%d", err, patched)
	}
}

func TestKubernetesStopWaitsForPodExitAndChecksOwnership(t *testing.T) {
	const accountID = "c8114f16-58dc-4f71-918b-8d21c1d661a0"
	const appID = "a42cc824-7da4-47a6-90da-8d2c478e70c0"
	deploymentExists, serviceExists, podExists := true, true, true
	owner := true
	deletes := 0
	testAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		kind := ""
		exists := false
		switch {
		case strings.Contains(r.URL.Path, "/deployments/"):
			kind, exists = "Deployment", deploymentExists
		case strings.Contains(r.URL.Path, "/services/"):
			kind, exists = "Service", serviceExists
		case strings.HasSuffix(r.URL.Path, "/pods"):
			items := []any{}
			if podExists {
				items = append(items, map[string]any{"metadata": map[string]any{"name": "terminating-pod"}})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "v1", "kind": "PodList", "items": items})
			return
		}
		if !exists {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "v1", "kind": "Status",
				"status": "Failure", "reason": "NotFound", "code": 404})
			return
		}
		if r.Method == http.MethodDelete {
			deletes++
			if kind == "Deployment" {
				deploymentExists = false
			} else {
				serviceExists = false
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "v1", "kind": "Status", "status": "Success"})
			return
		}
		labels := map[string]any{"app.kubernetes.io/managed-by": "polygate",
			"polygate.io/account-id": accountID, "polygate.io/app-id": appID}
		if !owner {
			labels["polygate.io/account-id"] = "other-account"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "v1", "kind": kind,
			"metadata": map[string]any{"name": "app-" + strings.ReplaceAll(appID, "-", ""), "labels": labels}})
	}))
	defer testAPI.Close()
	client, err := dynamic.NewForConfig(&rest.Config{Host: testAPI.URL})
	if err != nil {
		t.Fatal(err)
	}
	cluster := &KubernetesCluster{workloads: client}
	task := Task{AccountID: accountID, AppID: appID}
	owner = false
	if err := cluster.DeleteWorkload(context.Background(), task); err == nil || deletes != 0 {
		t.Fatalf("foreign workload was deleted: error=%v deletes=%d", err, deletes)
	}
	owner = true
	if err := cluster.DeleteWorkload(context.Background(), task); err != nil || deletes != 2 {
		t.Fatalf("owned workload delete: error=%v deletes=%d", err, deletes)
	}
	if gone, err := cluster.WorkloadGone(context.Background(), task); err != nil || gone {
		t.Fatalf("pod still exists: gone=%v err=%v", gone, err)
	}
	podExists = false
	if gone, err := cluster.WorkloadGone(context.Background(), task); err != nil || !gone {
		t.Fatalf("workload should be gone: gone=%v err=%v", gone, err)
	}
}

func TestTenantVerificationComparesQuantitiesAndPolicies(t *testing.T) {
	if !equalJSON(map[string]any{"hard": map[string]any{"limits.cpu": "4000m", "limits.memory": "4096Mi"}},
		map[string]any{"hard": map[string]any{"limits.cpu": "4", "limits.memory": "4Gi"}}) {
		t.Fatal("equivalent Kubernetes resource quantities were rejected")
	}
	if equalJSON(map[string]any{"egress": []any{map[string]any{"port": 443}}},
		map[string]any{"egress": []any{map[string]any{"port": 80}}}) {
		t.Fatal("a changed network policy port was accepted")
	}
}
