package secretcatalog

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"polygate/internal/domain"
	"polygate/internal/kubeobjects"
)

const Name = "student-credentials"
const maxEntries = 8
const maxValueBytes = 8192
const maxTotalBytes = 32768

var ErrInvalidValue = errors.New("secret value must contain 1 to 8192 bytes")
var ErrLimit = errors.New("student secret capacity exceeded")
var ErrMissing = errors.New("student secret key not found")

type Catalog struct{ client dynamic.Interface }

func NewInCluster() (*Catalog, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	config.QPS, config.Burst = 5, 10
	config.Timeout = 10 * time.Second
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return &Catalog{client: client}, nil
}

func New(client dynamic.Interface) *Catalog { return &Catalog{client: client} }

func (c *Catalog) resource(accountID string) (dynamic.ResourceInterface, error) {
	ns, err := kubeobjects.NamespaceName(accountID)
	if err != nil {
		return nil, err
	}
	return c.client.Resource(schema.GroupVersionResource{Version: "v1", Resource: "secrets"}).Namespace(ns), nil
}

func (c *Catalog) get(ctx context.Context, accountID string) (dynamic.ResourceInterface, *unstructured.Unstructured, map[string]string, error) {
	resource, err := c.resource(accountID)
	if err != nil {
		return nil, nil, nil, err
	}
	item, err := resource.Get(ctx, Name, metav1.GetOptions{})
	if err != nil {
		return nil, nil, nil, err
	}
	if item.GetLabels()["app.kubernetes.io/managed-by"] != "polygate" ||
		item.GetLabels()["polygate.io/account-id"] != accountID || item.Object["type"] != "Opaque" {
		return nil, nil, nil, errors.New("student Secret ownership or type mismatch")
	}
	data, _, err := unstructured.NestedStringMap(item.Object, "data")
	if err != nil {
		return nil, nil, nil, err
	}
	if data == nil {
		data = map[string]string{}
	}
	return resource, item, data, nil
}

func (c *Catalog) List(ctx context.Context, accountID string) ([]string, error) {
	_, _, data, err := c.get(ctx, accountID)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(data))
	for key := range data {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys, nil
}

func (c *Catalog) HasKeys(ctx context.Context, accountID string, keys []string) (bool, error) {
	if err := domain.ValidateSecretKeys(keys); err != nil {
		return false, err
	}
	_, _, data, err := c.get(ctx, accountID)
	if err != nil {
		return false, err
	}
	for _, key := range keys {
		if _, ok := data[key]; !ok {
			return false, nil
		}
	}
	return true, nil
}

func (c *Catalog) Put(ctx context.Context, accountID, key, value string) error {
	return c.PutMarked(ctx, accountID, key, value, "")
}

// PutMarked records the durable database operation ID in the same Kubernetes
// update as the value, so a restarted API can resolve an uncertain response.
func (c *Catalog) PutMarked(ctx context.Context, accountID, key, value, operationID string) error {
	if err := domain.ValidateSecretKeys([]string{key}); err != nil {
		return err
	}
	if len(value) < 1 || len(value) > maxValueBytes {
		return ErrInvalidValue
	}
	for attempt := 0; attempt < 5; attempt++ {
		resource, item, data, err := c.get(ctx, accountID)
		if err != nil {
			return err
		}
		if _, exists := data[key]; !exists && len(data) >= maxEntries {
			return ErrLimit
		}
		encoded := base64.StdEncoding.EncodeToString([]byte(value))
		data[key] = encoded
		var total int
		for _, entry := range data {
			decoded, decodeErr := base64.StdEncoding.DecodeString(entry)
			if decodeErr != nil {
				return errors.New("student Secret contains malformed data")
			}
			total += len(decoded)
		}
		if total > maxTotalBytes {
			return ErrLimit
		}
		if err = unstructured.SetNestedStringMap(item.Object, data, "data"); err != nil {
			return err
		}
		if operationID != "" {
			annotations := item.GetAnnotations()
			if annotations == nil {
				annotations = map[string]string{}
			}
			annotations[operationAnnotation(key)] = operationID
			item.SetAnnotations(annotations)
		}
		_, err = resource.Update(ctx, item, metav1.UpdateOptions{})
		if apierrors.IsConflict(err) {
			continue
		}
		return err
	}
	return errors.New("student Secret update conflicted repeatedly")
}

func (c *Catalog) Delete(ctx context.Context, accountID, key string) error {
	return c.DeleteMarked(ctx, accountID, key, "")
}

func (c *Catalog) DeleteMarked(ctx context.Context, accountID, key, operationID string) error {
	if err := domain.ValidateSecretKeys([]string{key}); err != nil {
		return err
	}
	for attempt := 0; attempt < 5; attempt++ {
		resource, item, data, err := c.get(ctx, accountID)
		if err != nil {
			return err
		}
		if _, exists := data[key]; !exists {
			return ErrMissing
		}
		delete(data, key)
		if err = unstructured.SetNestedStringMap(item.Object, data, "data"); err != nil {
			return err
		}
		if operationID != "" {
			annotations := item.GetAnnotations()
			if annotations == nil {
				annotations = map[string]string{}
			}
			annotations[operationAnnotation(key)] = operationID
			item.SetAnnotations(annotations)
		}
		_, err = resource.Update(ctx, item, metav1.UpdateOptions{})
		if apierrors.IsConflict(err) {
			continue
		}
		return err
	}
	return errors.New("student Secret deletion conflicted repeatedly")
}

func operationAnnotation(key string) string {
	digest := sha256.Sum256([]byte(key))
	return "polygate.io/secret-operation-" + hex.EncodeToString(digest[:12])
}

// MutationApplied never decodes, returns, logs, or persists a Secret value. A
// matching marker proves the data and marker were one Kubernetes update. If
// absent, it writes a metadata fence using the current resourceVersion; any
// delayed update based on the earlier version must then fail with a conflict.
func (c *Catalog) MutationApplied(ctx context.Context, accountID, key, kind, operationID string) (bool, error) {
	if err := domain.ValidateSecretKeys([]string{key}); err != nil {
		return false, err
	}
	if operationID == "" || (kind != "SECRET_UPSERT" && kind != "SECRET_DELETE") {
		return false, errors.New("invalid student Secret operation")
	}
	for attempt := 0; attempt < 5; attempt++ {
		resource, item, data, err := c.get(ctx, accountID)
		if err != nil {
			return false, err
		}
		if item.GetAnnotations()[operationAnnotation(key)] == operationID {
			_, present := data[key]
			if present != (kind == "SECRET_UPSERT") {
				return false, errors.New("student Secret operation marker disagrees with data")
			}
			return true, nil
		}
		annotations := item.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations["polygate.io/secret-recovery-fence"] = operationID
		item.SetAnnotations(annotations)
		_, err = resource.Update(ctx, item, metav1.UpdateOptions{})
		if apierrors.IsConflict(err) {
			continue
		}
		return false, err
	}
	return false, errors.New("student Secret recovery fence conflicted repeatedly")
}
