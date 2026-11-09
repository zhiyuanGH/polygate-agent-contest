package pullsecret

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"polygate/internal/harbor"
	"polygate/internal/kubeobjects"
	"polygate/internal/postgres"
)

const SecretName = "harbor-pull"
const secretType = "kubernetes.io/dockerconfigjson"
const managedLabel = "polygate.io/managed-by"
const accountLabel = "polygate.io/account-id"
const robotAnnotation = "polygate.io/pull-robot-id"
const retiredAnnotation = "polygate.io/retired-pull-robots"
const expiryAnnotation = "polygate.io/pull-expires-at"

var secretResource = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
var namespaceResource = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}

type Secret struct {
	ResourceVersion string
	Type            string
	Data            string
	Labels          map[string]string
	Annotations     map[string]string
}

type Tenant struct {
	Namespace string
	AccountID string
}

type SecretStore interface {
	ListTenants(context.Context) ([]Tenant, error)
	Get(context.Context, string) (*Secret, error)
	Put(context.Context, string, Secret) (*Secret, error)
	Delete(context.Context, string) error
}

type RobotStore interface {
	CreatePullRobot(context.Context, string, string, int) (harbor.RobotCredential, error)
	DeleteRobot(context.Context, int64) error
	ListPullRobots(context.Context, string) ([]int64, error)
}

type KubernetesStore struct{ Client dynamic.Interface }

func (k KubernetesStore) ListTenants(ctx context.Context) ([]Tenant, error) {
	list, err := k.Client.Resource(namespaceResource).List(ctx,
		metav1.ListOptions{LabelSelector: accountLabel})
	if err != nil {
		return nil, err
	}
	tenants := make([]Tenant, 0, len(list.Items))
	for _, item := range list.Items {
		id := item.GetLabels()[accountLabel]
		name, err := kubeobjects.NamespaceName(id)
		if err != nil || name != item.GetName() {
			return nil, errors.New("tenant namespace label does not match its name")
		}
		tenants = append(tenants, Tenant{Namespace: name, AccountID: id})
	}
	return tenants, nil
}

func (k KubernetesStore) Get(ctx context.Context, namespace string) (*Secret, error) {
	object, err := k.Client.Resource(secretResource).Namespace(namespace).Get(ctx, SecretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	data, _, err := unstructured.NestedString(object.Object, "data", ".dockerconfigjson")
	if err != nil {
		return nil, err
	}
	secretType, _, err := unstructured.NestedString(object.Object, "type")
	if err != nil {
		return nil, err
	}
	return &Secret{ResourceVersion: object.GetResourceVersion(), Type: secretType,
		Data: data, Labels: object.GetLabels(), Annotations: object.GetAnnotations()}, nil
}

func (k KubernetesStore) Put(ctx context.Context, namespace string, secret Secret) (*Secret, error) {
	meta := map[string]any{"name": SecretName, "namespace": namespace,
		"labels": secret.Labels, "annotations": secret.Annotations}
	if secret.ResourceVersion != "" {
		meta["resourceVersion"] = secret.ResourceVersion
	}
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Secret", "metadata": meta, "type": secretType,
		"data": map[string]any{".dockerconfigjson": secret.Data},
	}}
	var updated *unstructured.Unstructured
	var err error
	if secret.ResourceVersion == "" {
		updated, err = k.Client.Resource(secretResource).Namespace(namespace).
			Create(ctx, object, metav1.CreateOptions{})
	} else {
		updated, err = k.Client.Resource(secretResource).Namespace(namespace).
			Update(ctx, object, metav1.UpdateOptions{})
	}
	if err != nil {
		return nil, err
	}
	secret.ResourceVersion = updated.GetResourceVersion()
	return &secret, nil
}

func (k KubernetesStore) Delete(ctx context.Context, namespace string) error {
	err := k.Client.Resource(secretResource).Namespace(namespace).
		Delete(ctx, SecretName, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

type Controller struct {
	DB      *postgres.Store
	Harbor  RobotStore
	Secrets SecretStore
	Now     func() time.Time
}

func (c Controller) Run(ctx context.Context) error {
	if c.DB == nil || c.Harbor == nil || c.Secrets == nil {
		return errors.New("pull secret controller is incomplete")
	}
	tenants, err := c.Secrets.ListTenants(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, tenant := range tenants {
		var enabled, approved bool
		err := c.DB.Pool.QueryRow(ctx, `SELECT a.enabled,EXISTS(
            SELECT 1 FROM image_artifacts i WHERE i.account_id=a.id AND i.scan_status='Approved')
            FROM accounts a WHERE a.id=$1`, tenant.AccountID).Scan(&enabled, &approved)
		if errors.Is(err, pgx.ErrNoRows) {
			enabled, approved = false, false
		} else if err != nil {
			failures = append(failures, err)
			continue
		}
		if err := c.Reconcile(ctx, tenant, enabled && approved); err != nil {
			failures = append(failures, fmt.Errorf("tenant %s: %w", tenant.Namespace, err))
		}
	}
	return errors.Join(failures...)
}

func (c Controller) Reconcile(ctx context.Context, tenant Tenant, active bool) error {
	expected, err := kubeobjects.NamespaceName(tenant.AccountID)
	if err != nil || expected != tenant.Namespace {
		return errors.New("invalid tenant identity")
	}
	secret, err := c.Secrets.Get(ctx, tenant.Namespace)
	if err != nil {
		return err
	}
	if secret != nil && (secret.Type != secretType || secret.Labels[managedLabel] != "polygate" ||
		secret.Labels[accountLabel] != tenant.AccountID || secret.Data == "") {
		return errors.New("existing pull secret is not owned by this account")
	}
	project, err := harbor.AccountProjectName(tenant.AccountID)
	if err != nil {
		return err
	}
	referenced, err := robotIDs(secret)
	if err != nil {
		return err
	}
	known := make(map[int64]bool, len(referenced))
	for _, id := range referenced {
		known[id] = true
	}
	listed, err := c.Harbor.ListPullRobots(ctx, project)
	if err != nil {
		return err
	}
	for _, id := range listed {
		if !known[id] {
			if err := c.Harbor.DeleteRobot(ctx, id); err != nil {
				return fmt.Errorf("orphaned pull robot revocation failed: %w", err)
			}
		}
	}
	if !active {
		if secret == nil {
			return nil
		}
		ids, err := robotIDs(secret)
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := c.Harbor.DeleteRobot(ctx, id); err != nil {
				return err
			}
		}
		return c.Secrets.Delete(ctx, tenant.Namespace)
	}
	if secret != nil {
		secret, err = c.cleanupRetired(ctx, tenant.Namespace, secret)
		if err != nil {
			return err
		}
		expiry, err := time.Parse(time.RFC3339, secret.Annotations[expiryAnnotation])
		if err == nil && c.now().Add(12*time.Hour).Before(expiry) {
			return nil
		}
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	robot, err := c.Harbor.CreatePullRobot(ctx, project,
		"pull-"+hex.EncodeToString(random[:]), 1)
	if err != nil {
		return err
	}
	config, err := dockerConfig(robot)
	if err != nil {
		_ = c.Harbor.DeleteRobot(ctx, robot.ID)
		return err
	}
	next := Secret{Type: secretType, Data: base64.StdEncoding.EncodeToString(config),
		Labels: map[string]string{managedLabel: "polygate", accountLabel: tenant.AccountID},
		Annotations: map[string]string{robotAnnotation: strconv.FormatInt(robot.ID, 10),
			expiryAnnotation: c.now().Add(23 * time.Hour).UTC().Format(time.RFC3339)}}
	if secret != nil {
		ids, err := robotIDs(secret)
		if err != nil {
			_ = c.Harbor.DeleteRobot(ctx, robot.ID)
			return err
		}
		next.ResourceVersion = secret.ResourceVersion
		next.Annotations[retiredAnnotation] = joinIDs(ids)
	}
	updated, err := c.Secrets.Put(ctx, tenant.Namespace, next)
	if err != nil {
		_ = c.Harbor.DeleteRobot(ctx, robot.ID)
		return err
	}
	_, err = c.cleanupRetired(ctx, tenant.Namespace, updated)
	return err
}

func (c Controller) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func dockerConfig(robot harbor.RobotCredential) ([]byte, error) {
	if robot.ID <= 0 || robot.Name == "" || robot.Secret == "" {
		return nil, errors.New("Harbor pull robot credential is incomplete")
	}
	return json.Marshal(map[string]any{"auths": map[string]any{
		harbor.RegistryHost: map[string]string{"username": robot.Name,
			"password": robot.Secret,
			"auth":     base64.StdEncoding.EncodeToString([]byte(robot.Name + ":" + robot.Secret))},
	}})
}

func robotIDs(secret *Secret) ([]int64, error) {
	if secret == nil {
		return nil, nil
	}
	values := []string{secret.Annotations[robotAnnotation]}
	if retired := secret.Annotations[retiredAnnotation]; retired != "" {
		values = append(values, strings.Split(retired, ",")...)
	}
	if len(values) > 64 {
		return nil, errors.New("too many retired pull robots")
	}
	ids := make([]int64, 0, len(values))
	for _, value := range values {
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil || id <= 0 {
			return nil, errors.New("invalid pull robot ID annotation")
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func joinIDs(ids []int64) string {
	values := make([]string, 0, len(ids))
	for _, id := range ids {
		values = append(values, strconv.FormatInt(id, 10))
	}
	return strings.Join(values, ",")
}

func (c Controller) cleanupRetired(ctx context.Context, namespace string, secret *Secret) (*Secret, error) {
	retired := secret.Annotations[retiredAnnotation]
	if retired == "" {
		return secret, nil
	}
	ids, err := robotIDs(secret)
	if err != nil {
		return nil, err
	}
	remaining := make([]int64, 0, len(ids)-1)
	for _, id := range ids[1:] {
		if err := c.Harbor.DeleteRobot(ctx, id); err != nil {
			remaining = append(remaining, id)
		}
	}
	if len(remaining) == 0 {
		delete(secret.Annotations, retiredAnnotation)
	} else {
		secret.Annotations[retiredAnnotation] = joinIDs(remaining)
	}
	updated, err := c.Secrets.Put(ctx, namespace, *secret)
	if err != nil {
		return nil, err
	}
	if len(remaining) > 0 {
		return updated, errors.New("retired Harbor pull robots remain")
	}
	return updated, nil
}
