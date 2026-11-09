package volume

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func TestRetainedVolumeDeletionRequiresRecordedOwnedBackend(t *testing.T) {
	ctx := context.Background()
	task := Task{ID: "11111111-1111-4111-8111-111111111111",
		AccountID: "22222222-2222-4222-8222-222222222222", SizeGiB: 2, State: "Deleting"}
	pvc, err := DesiredClaim(task)
	if err != nil {
		t.Fatal(err)
	}
	pvc.SetUID(types.UID("33333333-3333-4333-8333-333333333333"))
	pvName := "pvc-44444444-4444-4444-8444-444444444444"
	pvc.Object["status"] = map[string]any{"phase": "Bound", "capacity": map[string]any{"storage": "2Gi"}}
	pvc.Object["spec"].(map[string]any)["volumeName"] = pvName
	pv := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "PersistentVolume",
		"metadata": map[string]any{"name": pvName, "uid": "55555555-5555-4555-8555-555555555555"},
		"spec": map[string]any{"storageClassName": StorageClass, "persistentVolumeReclaimPolicy": "Retain",
			"claimRef": map[string]any{"namespace": pvc.GetNamespace(), "name": pvc.GetName(), "uid": string(pvc.GetUID())},
			"csi": map[string]any{"driver": "driver.longhorn.io", "volumeHandle": pvName}},
		"status": map[string]any{"phase": "Bound"},
	}}
	lh := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "longhorn.io/v1beta2", "kind": "Volume",
		"metadata": map[string]any{"name": pvName, "namespace": "longhorn-system", "uid": "66666666-6666-4666-8666-666666666666"},
		"spec": map[string]any{"diskSelector": []any{"polygate-student-data"},
			"nodeSelector": []any{"polygate-student-storage-45"}},
		"status": map[string]any{"state": "detached"},
	}}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		claims: "PersistentVolumeClaimList", persistentVolumes: "PersistentVolumeList", longhornVolumes: "VolumeList",
	}, pvc, pv, lh)
	k := &Kubernetes{client: client}
	wrong := task
	wrong.PVCUID, wrong.PVName, wrong.Handle = "foreign", pvName, pvName
	if _, err := k.DestroyClaim(ctx, wrong); err == nil {
		t.Fatal("foreign PVC UID could delete claim")
	}
	pv.Object["spec"].(map[string]any)["claimRef"].(map[string]any)["uid"] = "foreign"
	if _, err := client.Resource(persistentVolumes).Update(ctx, pv, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := k.DiscoverDeletionTarget(ctx, task); err == nil {
		t.Fatal("foreign PV claim reference was accepted")
	}
	pv.Object["spec"].(map[string]any)["claimRef"].(map[string]any)["uid"] = string(pvc.GetUID())
	if _, err := client.Resource(persistentVolumes).Update(ctx, pv, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	lh.Object["spec"].(map[string]any)["diskSelector"] = []any{"foreign"}
	if _, err := client.Resource(longhornVolumes).Namespace("longhorn-system").Update(ctx, lh, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := k.DiscoverDeletionTarget(ctx, task); err == nil {
		t.Fatal("foreign Longhorn placement was accepted")
	}
	lh.Object["spec"].(map[string]any)["diskSelector"] = []any{"polygate-student-data"}
	if _, err := client.Resource(longhornVolumes).Namespace("longhorn-system").Update(ctx, lh, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	target, err := k.DiscoverDeletionTarget(ctx, task)
	if err != nil || target.PVCUID != string(pvc.GetUID()) || target.PVName != pvName || target.Handle != pvName {
		t.Fatalf("discover deletion target: %+v %v", target, err)
	}
	if done, err := k.DestroyClaim(ctx, target); err != nil || done {
		t.Fatalf("delete PVC: %t %v", done, err)
	}
	if _, err := client.Resource(claims).Namespace(pvc.GetNamespace()).Get(ctx, pvc.GetName(), metav1.GetOptions{}); err == nil {
		t.Fatal("PVC was not removed")
	}
	if done, err := k.DestroyClaim(ctx, target); err != nil || done {
		t.Fatalf("PV must wait for Released: %t %v", done, err)
	}
	pv.Object["status"] = map[string]any{"phase": "Released"}
	if _, err := client.Resource(persistentVolumes).Update(ctx, pv, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if done, err := k.DestroyClaim(ctx, target); err != nil || done {
		t.Fatalf("delete retained PV: %t %v", done, err)
	}
	if done, err := k.DestroyClaim(ctx, target); err != nil || done {
		t.Fatalf("delete detached Longhorn volume: %t %v", done, err)
	}
	if done, err := k.DestroyClaim(ctx, target); err != nil || !done {
		t.Fatalf("backend not fully removed: %t %v", done, err)
	}
}
