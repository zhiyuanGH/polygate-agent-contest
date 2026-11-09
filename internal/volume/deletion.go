package volume

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

var retainedVolumeName = regexp.MustCompile(`^pvc-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

var persistentVolumes = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumes"}
var longhornVolumes = schema.GroupVersionResource{Group: "longhorn.io", Version: "v1beta2", Resource: "volumes"}

func (k *Kubernetes) DiscoverDeletionTarget(ctx context.Context, task Task) (Task, error) {
	if k == nil || k.client == nil || task.State != "Deleting" || task.PVCUID != "" {
		return task, errors.New("invalid unrecorded deletion task")
	}
	wanted, err := DesiredClaim(task)
	if err != nil {
		return task, err
	}
	pvc, err := k.client.Resource(claims).Namespace(wanted.GetNamespace()).Get(ctx, wanted.GetName(), metav1.GetOptions{})
	if err != nil {
		return task, err // A Ready volume must still have its PVC before target recording.
	}
	ready, err := ValidateClaim(pvc, task)
	if err != nil {
		return task, fmt.Errorf("deletion PVC is not the ready owned claim: %w", err)
	}
	if !ready {
		return task, errors.New("deletion PVC capacity is not ready")
	}
	pvName, _, err := unstructured.NestedString(pvc.Object, "spec", "volumeName")
	if err != nil || !retainedVolumeName.MatchString(pvName) {
		return task, errors.New("deletion PVC has an unexpected retained PV name")
	}
	task.PVCUID = string(pvc.GetUID())
	if task.PVCUID == "" {
		return task, errors.New("deletion PVC has no UID")
	}
	task.PVName = pvName
	pv, err := k.client.Resource(persistentVolumes).Get(ctx, task.PVName, metav1.GetOptions{})
	if err != nil {
		return task, err
	}
	handle, err := validateDeletionPV(pv, task, wanted.GetNamespace(), wanted.GetName())
	if err != nil {
		return task, err
	}
	task.Handle = handle
	lh, err := k.client.Resource(longhornVolumes).Namespace("longhorn-system").Get(ctx, handle, metav1.GetOptions{})
	if err != nil {
		return task, err
	}
	if err := validateDeletionLonghorn(lh, task); err != nil {
		return task, err
	}
	return task, nil
}

func (k *Kubernetes) DestroyClaim(ctx context.Context, task Task) (bool, error) {
	if k == nil || k.client == nil || task.State != "Deleting" || task.PVCUID == "" ||
		!retainedVolumeName.MatchString(task.PVName) || task.Handle != task.PVName {
		return false, errors.New("deletion target is incomplete or invalid")
	}
	wanted, err := DesiredClaim(task)
	if err != nil {
		return false, err
	}
	pvcClient := k.client.Resource(claims).Namespace(wanted.GetNamespace())
	pvc, err := pvcClient.Get(ctx, wanted.GetName(), metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	if err == nil {
		if string(pvc.GetUID()) != task.PVCUID {
			return false, errors.New("PVC UID changed after deletion was requested")
		}
		if validateErr := validateClaimBase(pvc, wanted); validateErr != nil {
			return false, validateErr
		}
		boundPV, _, readErr := unstructured.NestedString(pvc.Object, "spec", "volumeName")
		if readErr != nil || boundPV != task.PVName {
			return false, errors.New("PVC binding changed after deletion was requested")
		}
		uid := types.UID(task.PVCUID)
		if err := pvcClient.Delete(ctx, wanted.GetName(), metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
		return false, nil
	}
	pvClient := k.client.Resource(persistentVolumes)
	pv, err := pvClient.Get(ctx, task.PVName, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	if err == nil {
		if _, err := validateDeletionPV(pv, task, wanted.GetNamespace(), wanted.GetName()); err != nil {
			return false, err
		}
		phase, _, readErr := unstructured.NestedString(pv.Object, "status", "phase")
		if readErr != nil || phase != "Released" {
			return false, nil
		}
		uid := pv.GetUID()
		if err := pvClient.Delete(ctx, task.PVName, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
		return false, nil
	}
	lhClient := k.client.Resource(longhornVolumes).Namespace("longhorn-system")
	lh, err := lhClient.Get(ctx, task.Handle, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if err := validateDeletionLonghorn(lh, task); err != nil {
		return false, err
	}
	state, _, readErr := unstructured.NestedString(lh.Object, "status", "state")
	if readErr != nil || state != "detached" {
		return false, nil
	}
	uid := lh.GetUID()
	if err := lhClient.Delete(ctx, task.Handle, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	return false, nil
}

func validateDeletionPV(pv *unstructured.Unstructured, task Task, namespace, claimName string) (string, error) {
	if pv.GetName() != task.PVName || !retainedVolumeName.MatchString(pv.GetName()) {
		return "", errors.New("PV identity differs from recorded target")
	}
	class, _, _ := unstructured.NestedString(pv.Object, "spec", "storageClassName")
	policy, _, _ := unstructured.NestedString(pv.Object, "spec", "persistentVolumeReclaimPolicy")
	driver, _, _ := unstructured.NestedString(pv.Object, "spec", "csi", "driver")
	handle, _, _ := unstructured.NestedString(pv.Object, "spec", "csi", "volumeHandle")
	refNamespace, _, _ := unstructured.NestedString(pv.Object, "spec", "claimRef", "namespace")
	refName, _, _ := unstructured.NestedString(pv.Object, "spec", "claimRef", "name")
	refUID, _, _ := unstructured.NestedString(pv.Object, "spec", "claimRef", "uid")
	if class != StorageClass || policy != "Retain" || driver != "driver.longhorn.io" ||
		handle != pv.GetName() || refNamespace != namespace || refName != claimName || refUID != task.PVCUID {
		return "", errors.New("PV ownership, reclaim policy or CSI identity differs from deletion target")
	}
	if task.Handle != "" && handle != task.Handle {
		return "", errors.New("PV Longhorn handle changed after deletion was requested")
	}
	return handle, nil
}

func validateDeletionLonghorn(lh *unstructured.Unstructured, task Task) error {
	if lh.GetName() != task.Handle || !retainedVolumeName.MatchString(lh.GetName()) {
		return errors.New("Longhorn volume identity differs from recorded target")
	}
	disks, _, err := unstructured.NestedStringSlice(lh.Object, "spec", "diskSelector")
	if err != nil || len(disks) != 1 || disks[0] != "polygate-student-data" {
		return errors.New("Longhorn disk selector differs from student storage")
	}
	nodes, _, err := unstructured.NestedStringSlice(lh.Object, "spec", "nodeSelector")
	if err != nil || len(nodes) != 1 || nodes[0] != "polygate-student-storage-45" {
		return errors.New("Longhorn node selector differs from student storage")
	}
	return nil
}
