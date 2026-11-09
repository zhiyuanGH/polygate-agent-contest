package volume

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func TestClaimOnlyBecomesReadyAfterOwnedPVCBinds(t *testing.T) {
	ctx := context.Background()
	task := Task{ID: "11111111-1111-4111-8111-111111111111",
		AccountID: "22222222-2222-4222-8222-222222222222", SizeGiB: 5}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{claims: "PersistentVolumeClaimList"})
	k := &Kubernetes{client: client}
	ready, err := k.EnsureClaim(ctx, task)
	if err != nil || ready {
		t.Fatalf("new claim: ready=%t err=%v", ready, err)
	}
	wanted, err := DesiredClaim(task)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := client.Resource(claims).Namespace(wanted.GetNamespace()).Get(ctx, wanted.GetName(), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if actual.Object["spec"].(map[string]any)["storageClassName"] != StorageClass {
		t.Fatal("wrong StorageClass")
	}
	for _, label := range []string{"recurring-job.longhorn.io/source",
		"recurring-job.longhorn.io/backup-polygate-student-daily",
		"recurring-job.longhorn.io/backup-polygate-student-weekly"} {
		if actual.GetLabels()[label] != "enabled" {
			t.Fatalf("student backup label %s missing", label)
		}
	}
	actual.Object["status"] = map[string]any{"phase": "Bound", "capacity": map[string]any{"storage": "5Gi"}}
	actual.Object["spec"].(map[string]any)["volumeName"] = "pvc-bound-volume"
	if _, err := client.Resource(claims).Namespace(wanted.GetNamespace()).Update(ctx, actual, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	ready, err = k.EnsureClaim(ctx, task)
	if err != nil || !ready {
		t.Fatalf("bound claim: ready=%t err=%v", ready, err)
	}
	task.SizeGiB = 8
	ready, err = k.EnsureClaim(ctx, task)
	if err != nil || ready {
		t.Fatalf("expansion before capacity catches up: ready=%t err=%v", ready, err)
	}
	actual, err = client.Resource(claims).Namespace(wanted.GetNamespace()).Get(ctx, wanted.GetName(), metav1.GetOptions{})
	if err != nil || actual.Object["spec"].(map[string]any)["resources"].(map[string]any)["requests"].(map[string]any)["storage"] != "8Gi" {
		t.Fatalf("PVC request not expanded: %+v %v", actual, err)
	}
	actual.Object["status"].(map[string]any)["capacity"] = map[string]any{"storage": "8Gi"}
	if _, err := client.Resource(claims).Namespace(wanted.GetNamespace()).Update(ctx, actual, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	ready, err = k.EnsureClaim(ctx, task)
	if err != nil || !ready {
		t.Fatalf("expanded claim: ready=%t err=%v", ready, err)
	}
	actual.SetLabels(map[string]string{"polygate.io/account-id": "foreign"})
	if _, err := client.Resource(claims).Namespace(wanted.GetNamespace()).Update(ctx, actual, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := k.EnsureClaim(ctx, task); err == nil {
		t.Fatal("foreign-owned PVC accepted")
	}
}
