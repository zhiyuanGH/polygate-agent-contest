package secretcatalog

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
	"polygate/internal/kubeobjects"
)

const accountA = "c8114f16-58dc-4f71-918b-8d21c1d661a0"
const accountB = "d2da6205-93d4-4089-a599-c145b7744fe8"

func testCatalog(t *testing.T) *Catalog {
	t.Helper()
	objects := make([]runtime.Object, 0, 2)
	for _, account := range []string{accountA, accountB} {
		ns, err := kubeobjects.NamespaceName(account)
		if err != nil {
			t.Fatal(err)
		}
		objects = append(objects, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "Secret",
			"metadata": map[string]any{"name": Name, "namespace": ns, "labels": map[string]any{
				"app.kubernetes.io/managed-by": "polygate", "polygate.io/account-id": account}},
			"type": "Opaque", "data": map[string]any{},
		}})
	}
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), objects...)
	return New(client)
}

func TestCatalogValuesStayPrivateAndAccountsStaySeparate(t *testing.T) {
	c := testCatalog(t)
	ctx := context.Background()
	if err := c.Put(ctx, accountA, "MODEL_API_KEY", "private-token"); err != nil {
		t.Fatal(err)
	}
	keys, err := c.List(ctx, accountA)
	if err != nil || len(keys) != 1 || keys[0] != "MODEL_API_KEY" {
		t.Fatalf("list=%v, err=%v", keys, err)
	}
	if strings.Contains(strings.Join(keys, ","), "private-token") {
		t.Fatal("secret value leaked through list")
	}
	if present, err := c.HasKeys(ctx, accountA, []string{"MODEL_API_KEY"}); err != nil || !present {
		t.Fatalf("own key: %v %v", present, err)
	}
	if present, err := c.HasKeys(ctx, accountB, []string{"MODEL_API_KEY"}); err != nil || present {
		t.Fatalf("other account key: %v %v", present, err)
	}
	if err := c.Delete(ctx, accountB, "MODEL_API_KEY"); !errors.Is(err, ErrMissing) {
		t.Fatalf("cross-account delete: %v", err)
	}
	if err := c.Put(ctx, accountA, "MODEL_API_KEY", "replacement"); err != nil {
		t.Fatal(err)
	}
	_, item, data, err := c.get(ctx, accountA)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(data["MODEL_API_KEY"])
	if err != nil || string(decoded) != "replacement" || item.GetNamespace() == "" {
		t.Fatalf("stored value or namespace invalid: %v", err)
	}
	if err := c.Delete(ctx, accountA, "MODEL_API_KEY"); err != nil {
		t.Fatal(err)
	}
	if keys, err = c.List(ctx, accountA); err != nil || len(keys) != 0 {
		t.Fatalf("deleted key listed: %v %v", keys, err)
	}
}

func TestCatalogLimitsAndOwnership(t *testing.T) {
	c := testCatalog(t)
	ctx := context.Background()
	if err := c.Put(ctx, accountA, "TOKEN", ""); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("empty value: %v", err)
	}
	if err := c.Put(ctx, accountA, "TOKEN", strings.Repeat("x", maxValueBytes+1)); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("oversize value: %v", err)
	}
	if err := c.Put(ctx, accountA, "POLYGATE_INTERNAL", "x"); err == nil {
		t.Fatal("reserved name accepted")
	}
	for _, key := range []string{"A", "B", "C", "D", "E", "F", "G", "H"} {
		if err := c.Put(ctx, accountA, key, "x"); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Put(ctx, accountA, "I", "x"); !errors.Is(err, ErrLimit) {
		t.Fatalf("ninth key: %v", err)
	}
	if err := c.Put(ctx, accountA, "A", "replacement"); err != nil {
		t.Fatalf("replacement should be allowed: %v", err)
	}
	resource, item, _, err := c.get(ctx, accountA)
	if err != nil {
		t.Fatal(err)
	}
	item.SetLabels(map[string]string{"polygate.io/account-id": accountB})
	if _, err := resource.Update(ctx, item, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := c.Put(ctx, accountA, "A", "x"); err == nil {
		t.Fatal("ownership mismatch accepted")
	}
}

func TestMutationMarkerTracksTheSameSecretUpdate(t *testing.T) {
	c := testCatalog(t)
	ctx := context.Background()
	if err := c.PutMarked(ctx, accountA, "MODEL_API_KEY", "first-value", "put-1"); err != nil {
		t.Fatal(err)
	}
	if applied, err := c.MutationApplied(ctx, accountA, "MODEL_API_KEY", "SECRET_UPSERT", "put-1"); err != nil || !applied {
		t.Fatalf("put marker: %v %v", applied, err)
	}
	if applied, err := c.MutationApplied(ctx, accountB, "MODEL_API_KEY", "SECRET_UPSERT", "put-1"); err != nil || applied {
		t.Fatalf("cross-account marker: %v %v", applied, err)
	}
	if err := c.PutMarked(ctx, accountA, "MODEL_API_KEY", "second-value", "put-2"); err != nil {
		t.Fatal(err)
	}
	conflicts := 0
	c.client.(*fake.FakeDynamicClient).PrependReactor("update", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		if conflicts == 0 {
			conflicts++
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, Name,
				errors.New("simulated competing update"))
		}
		return false, nil, nil
	})
	if applied, err := c.MutationApplied(ctx, accountA, "MODEL_API_KEY", "SECRET_UPSERT", "put-1"); err != nil || applied {
		t.Fatalf("stale put marker: %v %v", applied, err)
	}
	if conflicts != 1 {
		t.Fatal("recovery fence did not retry the resource version conflict")
	}
	_, fenced, data, err := c.get(ctx, accountA)
	if err != nil || fenced.GetAnnotations()["polygate.io/secret-recovery-fence"] != "put-1" || data["MODEL_API_KEY"] != base64.StdEncoding.EncodeToString([]byte("second-value")) {
		t.Fatalf("fence changed data or was not stored: %v", err)
	}
	if err := c.DeleteMarked(ctx, accountA, "MODEL_API_KEY", "delete-1"); err != nil {
		t.Fatal(err)
	}
	if applied, err := c.MutationApplied(ctx, accountA, "MODEL_API_KEY", "SECRET_DELETE", "delete-1"); err != nil || !applied {
		t.Fatalf("delete marker: %v %v", applied, err)
	}
	if applied, err := c.MutationApplied(ctx, accountA, "MODEL_API_KEY", "SECRET_UPSERT", "delete-1"); err == nil || applied {
		t.Fatalf("marker/data mismatch accepted: %v %v", applied, err)
	}
}
