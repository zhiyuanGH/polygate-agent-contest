package pullsecret

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"polygate/internal/harbor"
)

const testAccount = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
const testNamespace = "pg-u-aaaaaaaaaaaa4aaa8aaaaaaaaaaaaaaa"

type fakeRobots struct {
	next    int64
	created int
	deleted []int64
	failNew bool
}

func (f *fakeRobots) CreatePullRobot(_ context.Context, _, _ string, days int) (harbor.RobotCredential, error) {
	if f.failNew || days != 1 {
		return harbor.RobotCredential{}, errors.New("robot unavailable")
	}
	f.next++
	f.created++
	return harbor.RobotCredential{ID: f.next, Name: "robot$pull-" + strconv.FormatInt(f.next, 10),
		Secret: "private-credential-" + strconv.FormatInt(f.next, 10)}, nil
}

func (f *fakeRobots) DeleteRobot(_ context.Context, id int64) error {
	f.deleted = append(f.deleted, id)
	return nil
}
func (f *fakeRobots) ListPullRobots(_ context.Context, _ string) ([]int64, error) {
	var ids []int64
	for id := int64(1); id <= f.next; id++ {
		deleted := false
		for _, old := range f.deleted {
			if id == old {
				deleted = true
				break
			}
		}
		if !deleted {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

type fakeSecrets struct {
	secret *Secret
	puts   int
	fail   bool
}

func (f *fakeSecrets) ListTenants(context.Context) ([]Tenant, error) { return nil, nil }
func (f *fakeSecrets) Get(context.Context, string) (*Secret, error) {
	if f.secret == nil {
		return nil, nil
	}
	copy := *f.secret
	copy.Labels = map[string]string{}
	for key, value := range f.secret.Labels {
		copy.Labels[key] = value
	}
	copy.Annotations = map[string]string{}
	for key, value := range f.secret.Annotations {
		copy.Annotations[key] = value
	}
	return &copy, nil
}
func (f *fakeSecrets) Put(_ context.Context, _ string, secret Secret) (*Secret, error) {
	if f.fail {
		return nil, errors.New("Kubernetes update unavailable")
	}
	f.puts++
	secret.ResourceVersion = strconv.Itoa(f.puts)
	f.secret = &secret
	return f.Get(context.Background(), "")
}
func (f *fakeSecrets) Delete(context.Context, string) error {
	f.secret = nil
	return nil
}

func TestPullCredentialRotationAndRevocation(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	robots := &fakeRobots{}
	secrets := &fakeSecrets{}
	controller := Controller{Harbor: robots, Secrets: secrets, Now: func() time.Time { return now }}
	tenant := Tenant{Namespace: testNamespace, AccountID: testAccount}
	if err := controller.Reconcile(ctx, tenant, true); err != nil {
		t.Fatal(err)
	}
	if robots.created != 1 || secrets.secret == nil ||
		secrets.secret.Annotations[robotAnnotation] != "1" || secrets.secret.Type != secretType {
		t.Fatalf("first credential not provisioned: robots=%d secret=%+v", robots.created, secrets.secret)
	}
	config, err := base64.StdEncoding.DecodeString(secrets.secret.Data)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Auths map[string]struct {
			Username string `json:"username"`
			Password string `json:"password"`
			Auth     string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(config, &decoded); err != nil || len(decoded.Auths) != 1 {
		t.Fatalf("Docker credential format invalid: %v", err)
	}
	auth := decoded.Auths[harbor.RegistryHost]
	if auth.Username != "robot$pull-1" || auth.Password != "private-credential-1" ||
		auth.Auth != base64.StdEncoding.EncodeToString([]byte(auth.Username+":"+auth.Password)) {
		t.Fatal("Docker credential did not match the scoped Harbor robot")
	}
	if err := controller.Reconcile(ctx, tenant, true); err != nil || robots.created != 1 {
		t.Fatalf("healthy credential rotated too early: robots=%d err=%v", robots.created, err)
	}
	now = now.Add(12 * time.Hour)
	if err := controller.Reconcile(ctx, tenant, true); err != nil {
		t.Fatal(err)
	}
	if robots.created != 2 || len(robots.deleted) != 1 || robots.deleted[0] != 1 ||
		secrets.secret.Annotations[robotAnnotation] != "2" ||
		secrets.secret.Annotations[retiredAnnotation] != "" {
		t.Fatalf("rotation did not replace and retire the old robot: %+v %+v", robots, secrets.secret)
	}
	secrets.fail = true
	now = now.Add(12 * time.Hour)
	if err := controller.Reconcile(ctx, tenant, true); err == nil || robots.created != 3 ||
		len(robots.deleted) != 2 || robots.deleted[1] != 3 {
		t.Fatalf("failed Secret write did not revoke the new robot: %+v err=%v", robots, err)
	}
	secrets.fail = false
	if err := controller.Reconcile(ctx, tenant, false); err != nil || secrets.secret != nil ||
		len(robots.deleted) != 3 || robots.deleted[2] != 2 {
		t.Fatalf("disabled account retained its pull credential: %+v err=%v", robots, err)
	}
}

func TestPullCredentialRejectsForeignSecretAndTenant(t *testing.T) {
	robots := &fakeRobots{}
	secrets := &fakeSecrets{secret: &Secret{Type: secretType, Data: "x",
		Labels: map[string]string{managedLabel: "other", accountLabel: testAccount}}}
	controller := Controller{Harbor: robots, Secrets: secrets}
	if err := controller.Reconcile(context.Background(), Tenant{Namespace: testNamespace,
		AccountID: testAccount}, true); err == nil || robots.created != 0 {
		t.Fatalf("foreign Secret was overwritten: %v", err)
	}
	if err := controller.Reconcile(context.Background(), Tenant{Namespace: "kube-system",
		AccountID: testAccount}, true); err == nil {
		t.Fatal("controller accepted a cross-namespace account")
	}
}

func TestPullCredentialRevokesOrphanAfterCrash(t *testing.T) {
	robots := &fakeRobots{next: 1}
	secrets := &fakeSecrets{}
	controller := Controller{Harbor: robots, Secrets: secrets}
	if err := controller.Reconcile(context.Background(), Tenant{Namespace: testNamespace,
		AccountID: testAccount}, true); err != nil {
		t.Fatal(err)
	}
	if len(robots.deleted) != 1 || robots.deleted[0] != 1 || robots.created != 1 ||
		secrets.secret.Annotations[robotAnnotation] != "2" {
		t.Fatalf("orphan was not revoked before provisioning: %+v", robots)
	}
}
