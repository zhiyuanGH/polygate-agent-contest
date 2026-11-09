package volume

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"polygate/internal/domain"
	"polygate/internal/kubeobjects"
)

const StorageClass = "polygate-rwo"

var claims = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}

type Kubernetes struct{ client dynamic.Interface }

func NewKubernetes(kubeconfig string) (*Kubernetes, error) {
	var config *rest.Config
	var err error
	if kubeconfig == "" {
		config, err = rest.InClusterConfig()
	} else {
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	if err != nil {
		return nil, err
	}
	config.QPS, config.Burst = 3, 6
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return &Kubernetes{client: client}, nil
}

func ClaimName(id string) (string, error) {
	return kubeobjects.VolumeName(id)
}

func DesiredClaim(task Task) (*unstructured.Unstructured, error) {
	namespace, err := kubeobjects.NamespaceName(task.AccountID)
	if err != nil {
		return nil, err
	}
	name, err := ClaimName(task.ID)
	if err != nil {
		return nil, err
	}
	if task.SizeGiB < 1 || task.SizeGiB > domain.StudentVolumeGiBLimit {
		return nil, errors.New("volume size outside student limit")
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "PersistentVolumeClaim",
		"metadata": map[string]any{"name": name, "namespace": namespace,
			"labels": map[string]any{"app.kubernetes.io/managed-by": "polygate",
				"polygate.io/account-id": task.AccountID, "polygate.io/volume-id": task.ID,
				"recurring-job.longhorn.io/source": "enabled",
				"recurring-job.longhorn.io/backup-polygate-student-daily": "enabled",
				"recurring-job.longhorn.io/backup-polygate-student-weekly": "enabled"}},
		"spec": map[string]any{"accessModes": []any{"ReadWriteOnce"},
			"storageClassName": StorageClass,
			"resources":        map[string]any{"requests": map[string]any{"storage": fmt.Sprintf("%dGi", task.SizeGiB)}}},
	}}, nil
}

func (k *Kubernetes) EnsureClaim(ctx context.Context, task Task) (bool, error) {
	if k == nil || k.client == nil {
		return false, errors.New("Kubernetes client is missing")
	}
	wanted, err := DesiredClaim(task)
	if err != nil {
		return false, err
	}
	claimsClient := k.client.Resource(claims).Namespace(wanted.GetNamespace())
	actual, err := claimsClient.Get(ctx, wanted.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		actual, err = claimsClient.Create(ctx, wanted, metav1.CreateOptions{})
	}
	if err != nil {
		return false, err
	}
	if err := validateClaimBase(actual, wanted); err != nil {
		return false, err
	}
	storage, _, err := unstructured.NestedString(actual.Object, "spec", "resources", "requests", "storage")
	if err != nil {
		return false, err
	}
	current, err := resource.ParseQuantity(storage)
	if err != nil {
		return false, err
	}
	desired := resource.MustParse(fmt.Sprintf("%dGi", task.SizeGiB))
	if current.Cmp(desired) > 0 {
		return false, fmt.Errorf("PVC %s requests more storage than its reservation", wanted.GetName())
	}
	if current.Cmp(desired) < 0 {
		phase, _, err := unstructured.NestedString(actual.Object, "status", "phase")
		if err != nil || phase != "Bound" {
			return false, fmt.Errorf("PVC %s is not bound for expansion", wanted.GetName())
		}
		if err := unstructured.SetNestedField(actual.Object, fmt.Sprintf("%dGi", task.SizeGiB),
			"spec", "resources", "requests", "storage"); err != nil {
			return false, err
		}
		actual, err = claimsClient.Update(ctx, actual, metav1.UpdateOptions{})
		if err != nil {
			return false, err
		}
	}
	return ValidateClaim(actual, task)
}

// ValidateClaim verifies the object belongs to this exact account and matches
// the reserved size before it may be mounted or marked Ready.
func ValidateClaim(actual *unstructured.Unstructured, task Task) (bool, error) {
	wanted, err := DesiredClaim(task)
	if err != nil {
		return false, err
	}
	if err := validateClaimBase(actual, wanted); err != nil {
		return false, err
	}
	storage, _, err := unstructured.NestedString(actual.Object, "spec", "resources", "requests", "storage")
	requested := resource.MustParse(fmt.Sprintf("%dGi", task.SizeGiB))
	actualRequest, actualErr := resource.ParseQuantity(storage)
	if err != nil || actualErr != nil || requested.Cmp(actualRequest) != 0 {
		return false, fmt.Errorf("PVC %s has unexpected requested storage", wanted.GetName())
	}
	phase, _, err := unstructured.NestedString(actual.Object, "status", "phase")
	if err != nil {
		return false, err
	}
	volumeName, _, err := unstructured.NestedString(actual.Object, "spec", "volumeName")
	if err != nil || phase != "Bound" || volumeName == "" {
		return false, err
	}
	capacity, _, err := unstructured.NestedString(actual.Object, "status", "capacity", "storage")
	if err != nil || capacity == "" {
		return false, err
	}
	actualCapacity, err := resource.ParseQuantity(capacity)
	if err != nil {
		return false, err
	}
	return actualCapacity.Cmp(requested) >= 0, nil
}

func validateClaimBase(actual, wanted *unstructured.Unstructured) error {
	if actual.GetName() != wanted.GetName() || actual.GetNamespace() != wanted.GetNamespace() {
		return errors.New("PVC identity differs from volume reservation")
	}
	for key, value := range wanted.GetLabels() {
		if actual.GetLabels()[key] != value {
			return fmt.Errorf("PVC %s has unexpected %s label", wanted.GetName(), key)
		}
	}
	class, _, err := unstructured.NestedString(actual.Object, "spec", "storageClassName")
	if err != nil || class != StorageClass {
		return fmt.Errorf("PVC %s has unexpected StorageClass", wanted.GetName())
	}
	modes, _, err := unstructured.NestedStringSlice(actual.Object, "spec", "accessModes")
	if err != nil || len(modes) != 1 || modes[0] != "ReadWriteOnce" {
		return fmt.Errorf("PVC %s has unexpected access mode", wanted.GetName())
	}
	return nil
}
