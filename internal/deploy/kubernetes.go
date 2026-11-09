package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"polygate/internal/domain"
	"polygate/internal/harbor"
	"polygate/internal/kubeobjects"
	"polygate/internal/volume"
)

type KubernetesCluster struct {
	provisioner dynamic.Interface
	workloads   dynamic.Interface
	precreated  bool
}

// Separate kubeconfigs keep cluster-scoped account provisioning apart from
// Deployment/Service reconciliation. Neither credential should be an admin
// kubeconfig in production.
func NewKubernetesCluster(provisionerKubeconfig, workloadKubeconfig string) (*KubernetesCluster, error) {
	if provisionerKubeconfig == "" || workloadKubeconfig == "" {
		return nil, errors.New("both provisioner and workload kubeconfig paths are required")
	}
	provisionerInfo, err := os.Stat(provisionerKubeconfig)
	if err != nil {
		return nil, fmt.Errorf("provisioner kubeconfig: %w", err)
	}
	workloadInfo, err := os.Stat(workloadKubeconfig)
	if err != nil {
		return nil, fmt.Errorf("workload kubeconfig: %w", err)
	}
	if os.SameFile(provisionerInfo, workloadInfo) {
		return nil, errors.New("provisioner and workload kubeconfig must be separate files")
	}
	open := func(path string) (dynamic.Interface, error) {
		config, err := clientcmd.BuildConfigFromFlags("", path)
		if err != nil {
			return nil, err
		}
		config.QPS = 5
		config.Burst = 10
		return dynamic.NewForConfig(config)
	}
	provisioner, err := open(provisionerKubeconfig)
	if err != nil {
		return nil, fmt.Errorf("provisioner kubeconfig: %w", err)
	}
	workloads, err := open(workloadKubeconfig)
	if err != nil {
		return nil, fmt.Errorf("workload kubeconfig: %w", err)
	}
	return &KubernetesCluster{provisioner: provisioner, workloads: workloads}, nil
}

// NewPrecreatedKubernetesCluster uses one workload identity with no tenant
// write privileges. The operator creates account namespaces from the template
// before enrollment. In a Pod, the short-lived projected ServiceAccount token
// is used; a kubeconfig path is accepted for isolated acceptance tests.
func NewPrecreatedKubernetesCluster(workloadKubeconfig string) (*KubernetesCluster, error) {
	var config *rest.Config
	var err error
	if workloadKubeconfig == "" {
		config, err = rest.InClusterConfig()
	} else {
		config, err = clientcmd.BuildConfigFromFlags("", workloadKubeconfig)
	}
	if err != nil {
		return nil, fmt.Errorf("workload Kubernetes credentials: %w", err)
	}
	config.QPS, config.Burst = 5, 10
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return &KubernetesCluster{workloads: client, precreated: true}, nil
}

func (k *KubernetesCluster) EnsureTenant(ctx context.Context, accountID string, quota domain.Quota) error {
	objects, err := kubeobjects.Tenant(accountID, quota)
	if err != nil {
		return err
	}
	if k.precreated {
		return verifyObjects(ctx, k.workloads, objects)
	}
	return applyObjects(ctx, k.provisioner, objects, "polygate-tenant")
}

func verifyObjects(ctx context.Context, client dynamic.Interface, objects kubeobjects.List) error {
	for _, wanted := range objects.Items {
		kind, _ := wanted["kind"].(string)
		resource, namespaced, err := objectResource(kind)
		if err != nil {
			return err
		}
		metadata, ok := wanted["metadata"].(kubeobjects.Object)
		if !ok {
			return errors.New("tenant object metadata is missing")
		}
		name, _ := metadata["name"].(string)
		namespace, _ := metadata["namespace"].(string)
		// The workload controller verifies only objects required to run tenant
		// apps. Other platform components manage their own tenant Roles; tying
		// deployment to those Roles would stall apps during unrelated rollouts.
		if (kind == "Role" || kind == "RoleBinding") && name != "workload-controller" {
			continue
		}
		// Student credentials are managed by the API feature gate. This worker
		// must not read their contents, and the Secret is absent while disabled.
		if kind == "Secret" && name == "student-credentials" {
			continue
		}
		var target dynamic.ResourceInterface = client.Resource(resource)
		if namespaced {
			target = client.Resource(resource).Namespace(namespace)
		}
		actual, err := target.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("precreated tenant %s %s/%s: %w", kind, namespace, name, err)
		}
		if kind == "Namespace" {
			wantLabels := metadata["labels"].(kubeobjects.Object)
			labels := actual.GetLabels()
			for key, value := range wantLabels {
				if labels[key] != value {
					return fmt.Errorf("precreated Namespace %s has an invalid %s label", name, key)
				}
			}
			continue
		}
		if kind == "Secret" {
			if actual.Object["type"] != wanted["type"] {
				return fmt.Errorf("precreated tenant %s %s/%s has a different type", kind, namespace, name)
			}
			wantLabels := metadata["labels"].(kubeobjects.Object)
			for key, value := range wantLabels {
				if actual.GetLabels()[key] != value {
					return fmt.Errorf("precreated tenant %s %s/%s has an invalid %s label", kind, namespace, name, key)
				}
			}
		}
		for _, field := range []string{"spec", "rules", "roleRef", "subjects", "automountServiceAccountToken"} {
			want, specified := wanted[field]
			if !specified {
				continue
			}
			actualValue, present := actual.Object[field]
			if !present || !equalJSON(want, actualValue) {
				return fmt.Errorf("precreated tenant %s %s/%s has a different %s", kind, namespace, name, field)
			}
		}
	}
	return nil
}

func equalJSON(a, b any) bool {
	left, leftErr := json.Marshal(a)
	right, rightErr := json.Marshal(b)
	if leftErr != nil || rightErr != nil {
		return false
	}
	var leftValue, rightValue any
	if json.Unmarshal(left, &leftValue) != nil || json.Unmarshal(right, &rightValue) != nil {
		return false
	}
	return equivalentKubernetesValue(leftValue, rightValue)
}

func equivalentKubernetesValue(want, actual any) bool {
	switch expected := want.(type) {
	case map[string]any:
		got, ok := actual.(map[string]any)
		if !ok || len(expected) != len(got) {
			return false
		}
		for key, value := range expected {
			if !equivalentKubernetesValue(value, got[key]) {
				return false
			}
		}
		return true
	case []any:
		got, ok := actual.([]any)
		if !ok || len(expected) != len(got) {
			return false
		}
		for i, value := range expected {
			if !equivalentKubernetesValue(value, got[i]) {
				return false
			}
		}
		return true
	case string:
		got, ok := actual.(string)
		if !ok {
			return false
		}
		if expected == got {
			return true
		}
		wantQuantity, wantErr := resource.ParseQuantity(expected)
		gotQuantity, gotErr := resource.ParseQuantity(got)
		return wantErr == nil && gotErr == nil && wantQuantity.Cmp(gotQuantity) == 0
	default:
		return reflect.DeepEqual(want, actual)
	}
}

func (k *KubernetesCluster) EnsureWorkload(ctx context.Context, task Task) error {
	var objects kubeobjects.List
	var err error
	if (task.Kind == "Update" || task.Kind == "Rollback") && !task.Spec.Persistent && task.Strategy != "Recreate" {
		objects, err = kubeobjects.RollingWorkload(task.AccountID, task.AppID, task.Spec, task.ImageDigest, task.Username)
	} else {
		objects, err = kubeobjects.TrustedWorkload(task.AccountID, task.AppID, task.Spec, task.ImageDigest, task.Username)
	}
	if err != nil {
		return err
	}
	if task.Kind == "Update" && task.UpdateKind == "Restart" {
		deployment := objects.Items[0]
		spec := deployment["spec"].(kubeobjects.Object)
		template := spec["template"].(kubeobjects.Object)
		metadata := template["metadata"].(kubeobjects.Object)
		metadata["annotations"] = kubeobjects.Object{"polygate.io/restart-task-id": task.ID}
	}
	if task.Spec.Persistent {
		namespace, err := kubeobjects.NamespaceName(task.AccountID)
		if err != nil {
			return err
		}
		name, err := kubeobjects.VolumeName(task.Spec.VolumeID)
		if err != nil {
			return err
		}
		claim, err := k.workloads.Resource(schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}).
			Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return ErrVolumePending
		}
		if err != nil {
			return err
		}
		ready, err := volume.ValidateClaim(claim, volume.Task{
			ID: task.Spec.VolumeID, AccountID: task.AccountID, SizeGiB: task.VolumeSizeGiB})
		if err != nil {
			return err
		}
		if !ready {
			return ErrVolumePending
		}
	}
	if strings.HasPrefix(task.ImageDigest, harbor.RegistryHost+"/") {
		namespace, err := kubeobjects.NamespaceName(task.AccountID)
		if err != nil {
			return err
		}
		secret, err := k.workloads.Resource(schema.GroupVersionResource{Version: "v1", Resource: "secrets"}).
			Namespace(namespace).Get(ctx, "harbor-pull", metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return ErrPullCredentialPending
		}
		if err != nil {
			return err
		}
		data, _, err := unstructured.NestedString(secret.Object, "data", ".dockerconfigjson")
		if err != nil {
			return err
		}
		expires, parseErr := time.Parse(time.RFC3339, secret.GetAnnotations()["polygate.io/pull-expires-at"])
		if secret.Object["type"] != "kubernetes.io/dockerconfigjson" ||
			secret.GetLabels()["polygate.io/managed-by"] != "polygate" ||
			secret.GetLabels()["polygate.io/account-id"] != task.AccountID ||
			data == "" || parseErr != nil || time.Until(expires) < 10*time.Minute {
			return ErrPullCredentialPending
		}
	}
	return applyObjects(ctx, k.workloads, objects, "polygate-workload")
}

func (k *KubernetesCluster) ObserveWorkload(ctx context.Context, task Task) (Observation, error) {
	ns, err := kubeobjects.NamespaceName(task.AccountID)
	if err != nil {
		return Observation{}, err
	}
	name, err := kubeobjects.AppName(task.AppID)
	if err != nil {
		return Observation{}, err
	}
	deployment, err := k.workloads.Resource(schema.GroupVersionResource{
		Group: "apps", Version: "v1", Resource: "deployments",
	}).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Observation{}, nil
	}
	if err != nil {
		return Observation{}, err
	}
	labels := deployment.GetLabels()
	if labels["app.kubernetes.io/managed-by"] != "polygate" ||
		labels["polygate.io/account-id"] != task.AccountID ||
		labels["polygate.io/app-id"] != task.AppID {
		return Observation{}, errors.New("deployment ownership labels do not match task")
	}
	containers, found, err := unstructured.NestedSlice(deployment.Object, "spec", "template", "spec", "containers")
	if err != nil || !found || len(containers) != 1 {
		return Observation{}, errors.New("deployment container list is invalid")
	}
	container, ok := containers[0].(map[string]any)
	if !ok {
		return Observation{}, errors.New("deployment container is invalid")
	}
	image, ok := container["image"].(string)
	if !ok {
		return Observation{}, errors.New("deployment image is missing")
	}
	var result Observation
	result.Generation = deployment.GetGeneration()
	result.ObservedGeneration, _, err = unstructured.NestedInt64(deployment.Object, "status", "observedGeneration")
	if err != nil {
		return Observation{}, err
	}
	result.DesiredReplicas, _, err = unstructured.NestedInt64(deployment.Object, "spec", "replicas")
	if err != nil {
		return Observation{}, err
	}
	result.UpdatedReplicas, _, err = unstructured.NestedInt64(deployment.Object, "status", "updatedReplicas")
	if err != nil {
		return Observation{}, err
	}
	result.ReadyReplicas, _, err = unstructured.NestedInt64(deployment.Object, "status", "readyReplicas")
	if err != nil {
		return Observation{}, err
	}
	result.TotalReplicas, _, err = unstructured.NestedInt64(deployment.Object, "status", "replicas")
	if err != nil {
		return Observation{}, err
	}
	result.ImageDigest = image
	if task.Kind == "Update" || task.Kind == "Rollback" {
		expectedRestart := ""
		if task.Kind == "Update" && task.UpdateKind == "Restart" {
			expectedRestart = task.ID
		}
		if task.UpdateKind == "Restart" {
			actual, _, err := unstructured.NestedString(deployment.Object, "spec", "template", "metadata", "annotations", "polygate.io/restart-task-id")
			if err != nil {
				return Observation{}, err
			}
			if actual != expectedRestart {
				result.OldPods++
			}
		}
		pods, err := k.workloads.Resource(schema.GroupVersionResource{Version: "v1", Resource: "pods"}).
			Namespace(ns).List(ctx, metav1.ListOptions{LabelSelector: "polygate.io/account-id=" + task.AccountID +
			",polygate.io/app-id=" + task.AppID})
		if err != nil {
			return Observation{}, err
		}
		result.PodCount = int64(len(pods.Items))
		for _, pod := range pods.Items {
			containers, found, err := unstructured.NestedSlice(pod.Object, "spec", "containers")
			if err != nil || !found || len(containers) != 1 {
				return Observation{}, errors.New("managed pod container list is invalid")
			}
			container, ok := containers[0].(map[string]any)
			if !ok || container["image"] != task.ImageDigest || pod.GetDeletionTimestamp() != nil ||
				(task.UpdateKind == "Restart" && pod.GetAnnotations()["polygate.io/restart-task-id"] != expectedRestart) {
				result.OldPods++
			}
		}
	}
	return result, nil
}

func (k *KubernetesCluster) DeleteWorkload(ctx context.Context, task Task) error {
	ns, err := kubeobjects.NamespaceName(task.AccountID)
	if err != nil {
		return err
	}
	name, err := kubeobjects.AppName(task.AppID)
	if err != nil {
		return err
	}
	for _, kind := range []string{"Deployment", "Service"} {
		resource, _, err := objectResource(kind)
		if err != nil {
			return err
		}
		api := k.workloads.Resource(resource).Namespace(ns)
		object, err := api.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		labels := object.GetLabels()
		if labels["app.kubernetes.io/managed-by"] != "polygate" ||
			labels["polygate.io/account-id"] != task.AccountID ||
			labels["polygate.io/app-id"] != task.AppID {
			return fmt.Errorf("%s ownership labels do not match stop task", kind)
		}
		policy := metav1.DeletePropagationForeground
		if err = api.Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &policy}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (k *KubernetesCluster) WorkloadGone(ctx context.Context, task Task) (bool, error) {
	ns, err := kubeobjects.NamespaceName(task.AccountID)
	if err != nil {
		return false, err
	}
	name, err := kubeobjects.AppName(task.AppID)
	if err != nil {
		return false, err
	}
	for _, kind := range []string{"Deployment", "Service"} {
		resource, _, err := objectResource(kind)
		if err != nil {
			return false, err
		}
		_, err = k.workloads.Resource(resource).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			return false, nil
		}
		if !apierrors.IsNotFound(err) {
			return false, err
		}
	}
	pods, err := k.workloads.Resource(schema.GroupVersionResource{Version: "v1", Resource: "pods"}).
		Namespace(ns).List(ctx, metav1.ListOptions{LabelSelector: "polygate.io/account-id=" + task.AccountID +
		",polygate.io/app-id=" + task.AppID})
	if err != nil {
		return false, err
	}
	return len(pods.Items) == 0, nil
}

func applyObjects(ctx context.Context, client dynamic.Interface, objects kubeobjects.List, manager string) error {
	force := true
	for _, object := range objects.Items {
		kind, _ := object["kind"].(string)
		resource, namespaced, err := objectResource(kind)
		if err != nil {
			return err
		}
		metadata, ok := object["metadata"].(kubeobjects.Object)
		if !ok {
			return errors.New("object metadata is missing")
		}
		name, _ := metadata["name"].(string)
		ns, _ := metadata["namespace"].(string)
		if name == "" || (namespaced && ns == "") || (!namespaced && ns != "") {
			return fmt.Errorf("invalid metadata for %s", kind)
		}
		data, err := json.Marshal(object)
		if err != nil {
			return err
		}
		var target dynamic.ResourceInterface = client.Resource(resource)
		if namespaced {
			target = client.Resource(resource).Namespace(ns)
		}
		_, err = target.Patch(ctx, name, types.ApplyPatchType, data, metav1.PatchOptions{
			FieldManager: manager, Force: &force, FieldValidation: "Strict",
		})
		if err != nil {
			return fmt.Errorf("apply %s %s/%s: %w", kind, ns, name, err)
		}
	}
	return nil
}

func objectResource(kind string) (schema.GroupVersionResource, bool, error) {
	switch kind {
	case "Namespace":
		return schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}, false, nil
	case "ResourceQuota":
		return schema.GroupVersionResource{Version: "v1", Resource: "resourcequotas"}, true, nil
	case "LimitRange":
		return schema.GroupVersionResource{Version: "v1", Resource: "limitranges"}, true, nil
	case "ServiceAccount":
		return schema.GroupVersionResource{Version: "v1", Resource: "serviceaccounts"}, true, nil
	case "Secret":
		return schema.GroupVersionResource{Version: "v1", Resource: "secrets"}, true, nil
	case "Role":
		return schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}, true, nil
	case "RoleBinding":
		return schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}, true, nil
	case "NetworkPolicy":
		return schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "networkpolicies"}, true, nil
	case "Deployment":
		return schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, true, nil
	case "Service":
		return schema.GroupVersionResource{Version: "v1", Resource: "services"}, true, nil
	default:
		return schema.GroupVersionResource{}, false, fmt.Errorf("unsupported Kubernetes object kind %q", kind)
	}
}
