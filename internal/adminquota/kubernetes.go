package adminquota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"polygate/internal/domain"
	"polygate/internal/kubeobjects"
)

var quotaGVR = schema.GroupVersionResource{Version: "v1", Resource: "resourcequotas"}

type Kubernetes struct{ Client dynamic.Interface }

func InCluster() (*Kubernetes, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	config.QPS, config.Burst = 2, 4
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return &Kubernetes{Client: client}, nil
}

func desiredHard(accountID string, quota domain.Quota) (map[string]string, error) {
	objects, err := kubeobjects.Tenant(accountID, quota)
	if err != nil {
		return nil, err
	}
	for _, object := range objects.Items {
		if object["kind"] != "ResourceQuota" {
			continue
		}
		spec, ok := object["spec"].(kubeobjects.Object)
		if !ok {
			return nil, errors.New("tenant quota spec is invalid")
		}
		hard, ok := spec["hard"].(kubeobjects.Object)
		if !ok {
			return nil, errors.New("tenant quota hard limits are invalid")
		}
		keys := []string{"pods", "count/deployments.apps", "services", "requests.cpu", "limits.cpu", "requests.memory", "limits.memory", "requests.ephemeral-storage", "limits.ephemeral-storage"}
		wanted := make(map[string]string, len(keys))
		for _, key := range keys {
			value, ok := hard[key].(string)
			if !ok {
				return nil, fmt.Errorf("tenant quota %s is invalid", key)
			}
			wanted[key] = value
		}
		return wanted, nil
	}
	return nil, errors.New("tenant quota object is missing")
}

func matchesHard(actual map[string]any, desired map[string]string) bool {
	for key, want := range desired {
		got, ok := actual[key].(string)
		if !ok {
			return false
		}
		wantQuantity, wantErr := resource.ParseQuantity(want)
		gotQuantity, gotErr := resource.ParseQuantity(got)
		if wantErr != nil || gotErr != nil || wantQuantity.Cmp(gotQuantity) != 0 {
			return false
		}
	}
	return true
}

// Apply changes only the account's compute-related quota keys. Storage limits
// stay under the storage controller and are preserved by the merge patch.
func (k *Kubernetes) Apply(ctx context.Context, accountID string, quota domain.Quota) error {
	namespace, err := kubeobjects.NamespaceName(accountID)
	if err != nil {
		return err
	}
	desired, err := desiredHard(accountID, quota)
	if err != nil {
		return err
	}
	resource := k.Client.Resource(quotaGVR).Namespace(namespace)
	current, err := resource.Get(ctx, "account-budget", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read account quota %s: %w", namespace, err)
	}
	hard, found, err := unstructuredHard(current.Object)
	if err != nil || !found {
		return fmt.Errorf("account quota %s has invalid hard limits: %w", namespace, err)
	}
	if matchesHard(hard, desired) {
		return nil
	}
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"resourceVersion": current.GetResourceVersion()},
		"spec":     map[string]any{"hard": desired},
	})
	if err != nil {
		return err
	}
	if _, err = resource.Patch(ctx, "account-budget", types.MergePatchType, patch, metav1.PatchOptions{FieldValidation: "Strict"}); err != nil {
		return fmt.Errorf("patch account quota %s: %w", namespace, err)
	}
	updated, err := resource.Get(ctx, "account-budget", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("verify account quota %s: %w", namespace, err)
	}
	hard, found, err = unstructuredHard(updated.Object)
	if err != nil || !found || !matchesHard(hard, desired) {
		return fmt.Errorf("account quota %s did not converge after patch: %w", namespace, err)
	}
	return nil
}

func unstructuredHard(object map[string]any) (map[string]any, bool, error) {
	spec, ok := object["spec"].(map[string]any)
	if !ok {
		return nil, false, errors.New("spec missing")
	}
	hard, ok := spec["hard"].(map[string]any)
	if !ok {
		return nil, false, errors.New("hard missing")
	}
	return hard, true, nil
}
