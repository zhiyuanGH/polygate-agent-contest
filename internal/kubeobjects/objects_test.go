package kubeobjects

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"polygate/internal/domain"
)

const accountA = "c8114f16-58dc-4f71-918b-8d21c1d661a0"
const accountB = "d2da6205-93d4-4089-a599-c145b7744fe8"
const appID = "a42cc824-7da4-47a6-90da-8d2c478e70c0"

func TestTenantIsolationAndLeastPrivilege(t *testing.T) {
	a, err := Tenant(accountA, domain.StudentQuota)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Tenant(accountB, domain.StudentQuota)
	if err != nil {
		t.Fatal(err)
	}
	if a.Items[0]["metadata"].(Object)["name"] == b.Items[0]["metadata"].(Object)["name"] {
		t.Fatal("different accounts share a namespace")
	}
	foundGatewayPolicy := false
	for _, obj := range a.Items {
		if obj["kind"] == "Namespace" {
			labels := obj["metadata"].(Object)["labels"].(Object)
			if labels["pod-security.kubernetes.io/enforce"] != "restricted" {
				t.Fatal("tenant namespace does not enforce restricted Pod security")
			}
			continue
		}
		if obj["metadata"].(Object)["namespace"] != "pg-u-c8114f1658dc4f71918b8d21c1d661a0" {
			t.Fatalf("object escaped tenant namespace: %v", obj["kind"])
		}
		if obj["kind"] == "NetworkPolicy" && obj["metadata"].(Object)["name"] == "allow-app-gateway" {
			foundGatewayPolicy = true
			spec := obj["spec"].(Object)
			selector := spec["podSelector"].(Object)["matchLabels"].(Object)
			from := spec["ingress"].([]Object)[0]["from"].([]Object)[0]
			namespace := from["namespaceSelector"].(Object)["matchLabels"].(Object)
			pod := from["podSelector"].(Object)["matchLabels"].(Object)
			if selector["app.kubernetes.io/managed-by"] != "polygate" ||
				namespace["kubernetes.io/metadata.name"] != "polygate-system" || pod["app"] != "polygate-app-gateway" {
				t.Fatal("app gateway ingress policy has unexpected selectors")
			}
		}
		if obj["kind"] == "Role" && obj["metadata"].(Object)["name"] == "workload-controller" {
			for _, rule := range obj["rules"].([]Object) {
				for _, resource := range rule["resources"].([]string) {
					if resource == "secrets" {
						verbs := rule["verbs"].([]string)
						names, ok := rule["resourceNames"].([]string)
						if len(verbs) != 1 || verbs[0] != "get" || !ok || len(names) != 1 || names[0] != "harbor-pull" {
							t.Fatalf("workload identity can access an unrelated Secret: %+v", rule)
						}
					}
					if resource == "networkpolicies" || resource == "roles" || resource == "rolebindings" || resource == "resourcequotas" || resource == "limitranges" || resource == "serviceaccounts" {
						verbs := rule["verbs"].([]string)
						if len(verbs) != 1 || verbs[0] != "get" {
							t.Fatalf("workload identity can change a protected resource: %s", resource)
						}
					}
				}
			}
		}
		if obj["kind"] == "Role" && obj["metadata"].(Object)["name"] == "student-secret-editor" {
			rules := obj["rules"].([]Object)
			if len(rules) != 1 || !reflect.DeepEqual(rules[0]["resources"], []string{"secrets"}) ||
				!reflect.DeepEqual(rules[0]["resourceNames"], []string{"student-credentials"}) ||
				!reflect.DeepEqual(rules[0]["verbs"], []string{"get", "update"}) {
				t.Fatalf("API secret editor has excess permissions: %+v", rules)
			}
		}
		if obj["kind"] == "Role" && obj["metadata"].(Object)["name"] == "api-log-reader" {
			rules := obj["rules"].([]Object)
			wanted := map[string]string{"/pods": "list", "/pods/log": "get", "/events": "list",
				"apps/deployments": "get", "apps/replicasets": "list", "metrics.k8s.io/pods": "list"}
			if len(rules) != len(wanted) {
				t.Fatalf("API event reader has unexpected rule count: %+v", rules)
			}
			for _, rule := range rules {
				groups, resources, verbs := rule["apiGroups"].([]string), rule["resources"].([]string), rule["verbs"].([]string)
				if len(groups) != 1 || len(resources) != 1 || len(verbs) != 1 ||
					wanted[groups[0]+"/"+resources[0]] != verbs[0] {
					t.Fatalf("API event reader has unexpected privilege: %+v", rule)
				}
			}
		}
	}
	if !foundGatewayPolicy {
		t.Fatal("tenant missing app gateway ingress policy")
	}
	data, err := a.JSON()
	if err != nil || !json.Valid(data) {
		t.Fatalf("invalid Kubernetes JSON: %v", err)
	}
	quota := a.Items[1]["spec"].(Object)["hard"].(Object)
	if quota["count/deployments.apps"] != "2" {
		t.Fatalf("deployment object quota is missing: %v", quota)
	}
	if quota["limits.ephemeral-storage"] != "4Gi" {
		t.Fatalf("ephemeral storage quota is missing: %v", quota)
	}
	for key, expected := range map[string]string{
		"requests.storage": "10Gi", "persistentvolumeclaims": "2",
		"polygate-rwo.storageclass.storage.k8s.io/requests.storage":       "10Gi",
		"polygate-rwo.storageclass.storage.k8s.io/persistentvolumeclaims": "2",
	} {
		if quota[key] != expected {
			t.Fatalf("student storage quota %s is missing: %v", key, quota)
		}
	}
	limits := a.Items[2]["spec"].(Object)["limits"].([]Object)
	if len(limits) != 2 || limits[1]["type"] != "PersistentVolumeClaim" ||
		limits[1]["min"].(Object)["storage"] != "1Gi" || limits[1]["max"].(Object)["storage"] != "10Gi" {
		t.Fatalf("student PVC size limits are missing: %+v", limits)
	}
	for _, blocked := range []string{"158.132.0.0/16", "169.254.0.0/16", "10.0.0.0/8"} {
		if !strings.Contains(string(data), blocked) {
			t.Fatalf("missing protected range %s", blocked)
		}
	}
}

func TestTrustedWorkloadUsesPlatformDigestAndRestrictedPod(t *testing.T) {
	spec := domain.AppSpec{Name: "agent", Image: "student.example/untrusted:latest", Port: 8080,
		Resources: domain.Resources{CPURequestMilli: 250, CPULimitMilli: 1000, MemoryMiB: 1024, Replicas: 2}}
	digest := "registry.example/approved/agent@sha256:" + strings.Repeat("a", 64)
	list, err := TrustedWorkload(accountA, appID, spec, digest)
	if err != nil {
		t.Fatal(err)
	}
	data, err := list.JSON()
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, spec.Image) || !strings.Contains(text, digest) {
		t.Fatal("student image reached workload template or approved digest is missing")
	}
	for _, required := range []string{"runAsNonRoot", "RuntimeDefault", "allowPrivilegeEscalation", "readOnlyRootFilesystem", "automountServiceAccountToken", "node-role.kubernetes.io/worker", "Recreate"} {
		if !strings.Contains(text, required) {
			t.Fatalf("missing workload safety constraint %s", required)
		}
	}
	if strings.Contains(text, "hostNetwork") || strings.Contains(text, "hostPath") || strings.Contains(text, "privileged") {
		t.Fatal("workload contains host access")
	}
	if strings.Contains(text, "imagePullSecrets") {
		t.Fatal("public sample unexpectedly requires a Harbor credential")
	}
	privateDigest := "harbor.polygate-registry.svc.cluster.local/pg-u-" +
		strings.ReplaceAll(accountA, "-", "") + "/agent@sha256:" + strings.Repeat("b", 64)
	privateList, err := TrustedWorkload(accountA, appID, spec, privateDigest)
	if err != nil {
		t.Fatal(err)
	}
	privateJSON, err := privateList.JSON()
	if err != nil || !strings.Contains(string(privateJSON), `"imagePullSecrets"`) ||
		!strings.Contains(string(privateJSON), `"harbor-pull"`) {
		t.Fatalf("private Harbor image lacks pull secret: %v", err)
	}
	if _, err := TrustedWorkload(accountA, appID, spec, "registry.example/agent:latest"); err == nil {
		t.Fatal("mutable image tag accepted")
	}
	spec.Persistent = true
	if _, err := TrustedWorkload(accountA, appID, spec, digest); err == nil {
		t.Fatal("persistent workload accepted without storage design")
	}
}

func TestRollingWorkloadRequiresStatelessAppAndSurge(t *testing.T) {
	spec := domain.AppSpec{Name: "agent", Image: "ignored.example/agent:v1", Port: 8080,
		Resources: domain.Resources{CPURequestMilli: 250, CPULimitMilli: 1000, MemoryMiB: 1024, Replicas: 1}}
	digest := "registry.example/approved/agent@sha256:" + strings.Repeat("a", 64)
	list, err := RollingWorkload(accountA, appID, spec, digest)
	if err != nil {
		t.Fatal(err)
	}
	deployment := list.Items[0]
	strategy := deployment["spec"].(Object)["strategy"].(Object)
	if strategy["type"] != "RollingUpdate" {
		t.Fatalf("strategy=%v", strategy)
	}
	rolling := strategy["rollingUpdate"].(Object)
	if rolling["maxSurge"] != 1 || rolling["maxUnavailable"] != 0 {
		t.Fatalf("rolling budget=%v", rolling)
	}
	spec.Persistent = true
	if _, err := RollingWorkload(accountA, appID, spec, digest); err == nil {
		t.Fatal("persistent workload accepted for rolling update")
	}
}

func TestHTTPHealthPathBuildsBoundedProbes(t *testing.T) {
	spec := domain.AppSpec{Name: "agent", Image: "ignored.example/agent:v1", Port: 8080,
		HealthPath: "/health/ready",
		Resources:  domain.Resources{CPURequestMilli: 250, CPULimitMilli: 1000, MemoryMiB: 1024, Replicas: 1}}
	digest := "registry.example/approved/agent@sha256:" + strings.Repeat("a", 64)
	list, err := TrustedWorkload(accountA, appID, spec, digest)
	if err != nil {
		t.Fatal(err)
	}
	container := list.Items[0]["spec"].(Object)["template"].(Object)["spec"].(Object)["containers"].([]Object)[0]
	for _, kind := range []string{"startupProbe", "readinessProbe", "livenessProbe"} {
		probe := container[kind].(Object)
		get := probe["httpGet"].(Object)
		if get["path"] != spec.HealthPath || get["port"] != "http" || probe["failureThreshold"].(int) < 3 {
			t.Fatalf("%s=%v", kind, probe)
		}
	}
	spec.HealthPath = ""
	list, err = TrustedWorkload(accountA, appID, spec, digest)
	if err != nil {
		t.Fatal(err)
	}
	container = list.Items[0]["spec"].(Object)["template"].(Object)["spec"].(Object)["containers"].([]Object)[0]
	if _, hasLiveness := container["livenessProbe"]; hasLiveness {
		t.Fatal("TCP fallback should not restart a pod solely for a failed port probe")
	}
}

func TestStudentSecretsOnlyReachSelectedAppKeys(t *testing.T) {
	spec := domain.AppSpec{Name: "agent", Image: "ignored.example/agent:v1", Port: 8080,
		SecretKeys: []string{"OPENAI_API_KEY"},
		Resources:  domain.Resources{CPURequestMilli: 250, CPULimitMilli: 1000, MemoryMiB: 1024, Replicas: 1}}
	digest := "registry.example/approved/agent@sha256:" + strings.Repeat("a", 64)
	list, err := TrustedWorkload(accountA, appID, spec, digest)
	if err != nil {
		t.Fatal(err)
	}
	container := list.Items[0]["spec"].(Object)["template"].(Object)["spec"].(Object)["containers"].([]Object)[0]
	env := container["env"].([]Object)
	if len(env) != 1 || env[0]["name"] != "OPENAI_API_KEY" ||
		env[0]["valueFrom"].(Object)["secretKeyRef"].(Object)["name"] != "student-credentials" {
		t.Fatalf("selected Secret reference differs: %+v", env)
	}
	tenant, err := Tenant(accountA, domain.StudentQuota)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, object := range tenant.Items {
		if object["kind"] == "Secret" && object["metadata"].(Object)["name"] == "student-credentials" {
			found = true
			if _, managesData := object["data"]; managesData {
				t.Fatal("tenant template must not own student Secret data")
			}
		}
	}
	if !found {
		t.Fatal("student credentials Secret was not precreated")
	}
}

func TestInvalidAccountIDsRejected(t *testing.T) {
	for _, id := range []string{"", "../../kube-system", strings.ToUpper(accountA), accountA + "x"} {
		if _, err := NamespaceName(id); err == nil {
			t.Fatalf("accepted account ID %q", id)
		}
	}
}

func TestPersistentWorkloadMountsOnlyBoundClaim(t *testing.T) {
	spec := domain.AppSpec{Name: "agent", Image: "ignored.example/agent:v1", Port: 8080,
		Resources:  domain.Resources{CPURequestMilli: 250, CPULimitMilli: 1000, MemoryMiB: 1024, Replicas: 1},
		Persistent: true, VolumeID: "11111111-1111-4111-8111-111111111111", MountPath: "/data"}
	digest := "registry.example/approved/agent@sha256:" + strings.Repeat("a", 64)
	list, err := TrustedWorkload(accountA, appID, spec, digest)
	if err != nil {
		t.Fatal(err)
	}
	data, err := list.JSON()
	if err != nil {
		t.Fatal(err)
	}
	for _, wanted := range []string{"vol-11111111111141118111111111111111", "persistentVolumeClaim", "fsGroup", "OnRootMismatch", "/data", "Recreate"} {
		if !strings.Contains(string(data), wanted) {
			t.Fatalf("persistent template missing %s", wanted)
		}
	}
	if strings.Contains(string(data), "hostPath") {
		t.Fatal("persistent template gained host access")
	}
}

func TestPrecreatedBootstrapRestrictsNamespaceReadToNamedAccounts(t *testing.T) {
	bundle, err := PrecreatedTenants([]string{accountB, accountA}, domain.StudentQuota)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Items) != 10+2*20 {
		t.Fatalf("bootstrap object count=%d", len(bundle.Items))
	}
	var role Object
	for _, item := range bundle.Items {
		if item["kind"] == "ClusterRole" && item["metadata"].(Object)["name"] == "polygate-tenant-reader" {
			role = item
		}
	}
	if role == nil {
		t.Fatal("tenant reader missing")
	}
	rules := role["rules"].([]Object)
	if len(rules) != 1 || rules[0]["resources"].([]string)[0] != "namespaces" ||
		rules[0]["verbs"].([]string)[0] != "get" || len(rules[0]["resourceNames"].([]string)) != 2 {
		t.Fatalf("namespace reader is too broad: %+v", rules)
	}
	if _, err := PrecreatedTenants([]string{accountA, accountA}, domain.StudentQuota); err == nil {
		t.Fatal("duplicate account accepted")
	}
}

func TestQuotaControllerCanPatchOnlyNamedTenantQuota(t *testing.T) {
	bundle, err := PrecreatedTenants([]string{accountA}, domain.StudentQuota)
	if err != nil {
		t.Fatal(err)
	}
	var role, binding Object
	for _, item := range bundle.Items {
		meta := item["metadata"].(Object)
		if meta["name"] != "account-quota-controller" {
			continue
		}
		if item["kind"] == "Role" {
			role = item
		}
		if item["kind"] == "RoleBinding" {
			binding = item
		}
	}
	if role == nil || binding == nil {
		t.Fatal("quota controller RBAC missing")
	}
	rules := role["rules"].([]Object)
	if len(rules) != 1 || rules[0]["resources"].([]string)[0] != "resourcequotas" ||
		len(rules[0]["resourceNames"].([]string)) != 1 ||
		rules[0]["resourceNames"].([]string)[0] != "account-budget" ||
		len(rules[0]["verbs"].([]string)) != 2 {
		t.Fatalf("quota role is too broad: %+v", rules)
	}
	subject := binding["subjects"].([]Object)[0]
	if subject["name"] != "polygate-quota-controller" || subject["namespace"] != "polygate-system" {
		t.Fatalf("quota controller binding is wrong: %+v", subject)
	}
}

func TestPullSecretControllerHasOnlyTenantSecretPermissions(t *testing.T) {
	bundle, err := PrecreatedTenants([]string{accountA}, domain.StudentQuota)
	if err != nil {
		t.Fatal(err)
	}
	var pullRole, pullBinding Object
	for _, item := range bundle.Items {
		meta := item["metadata"].(Object)
		if meta["name"] != "harbor-pull-secret-controller" {
			continue
		}
		if item["kind"] == "Role" {
			pullRole = item
		}
		if item["kind"] == "RoleBinding" {
			pullBinding = item
		}
	}
	if pullRole == nil || pullBinding == nil ||
		pullRole["metadata"].(Object)["namespace"] != "pg-u-c8114f1658dc4f71918b8d21c1d661a0" {
		t.Fatal("pull credential RBAC is missing from the tenant namespace")
	}
	rules := pullRole["rules"].([]Object)
	if len(rules) != 2 || rules[0]["resources"].([]string)[0] != "secrets" ||
		rules[0]["verbs"].([]string)[0] != "create" ||
		rules[1]["resourceNames"].([]string)[0] != "harbor-pull" {
		t.Fatalf("pull credential RBAC is too broad: %+v", rules)
	}
	subjects := pullBinding["subjects"].([]Object)
	if len(subjects) != 1 || subjects[0]["name"] != "polygate-pull-secret-controller" ||
		subjects[0]["namespace"] != "polygate-system" {
		t.Fatalf("pull credential RoleBinding has wrong subject: %+v", subjects)
	}
}

func TestVolumeControllerCanOnlyManageClaims(t *testing.T) {
	bundle, err := PrecreatedTenants([]string{accountA}, domain.StudentQuota)
	if err != nil {
		t.Fatal(err)
	}
	var role, binding Object
	for _, item := range bundle.Items {
		if item["metadata"].(Object)["name"] != "volume-controller" {
			continue
		}
		switch item["kind"] {
		case "Role":
			role = item
		case "RoleBinding":
			binding = item
		}
	}
	if role == nil || binding == nil {
		t.Fatal("volume controller RBAC missing")
	}
	rules := role["rules"].([]Object)
	if len(rules) != 1 || rules[0]["resources"].([]string)[0] != "persistentvolumeclaims" ||
		!reflect.DeepEqual(rules[0]["verbs"], []string{"get", "create", "update", "delete"}) {
		t.Fatalf("volume controller permissions are too broad: %+v", rules)
	}
	subjects := binding["subjects"].([]Object)
	if len(subjects) != 1 || subjects[0]["name"] != "polygate-volume-controller" {
		t.Fatalf("volume controller subject: %+v", subjects)
	}
}

func TestWorkloadControllerCanOnlyReadManagedPullSecret(t *testing.T) {
	bundle, err := PrecreatedTenants([]string{accountA}, domain.StudentQuota)
	if err != nil {
		t.Fatal(err)
	}
	var role Object
	for _, item := range bundle.Items {
		if item["kind"] == "Role" && item["metadata"].(Object)["name"] == "workload-controller" {
			role = item
		}
	}
	if role == nil {
		t.Fatal("workload controller Role missing")
	}
	secretRules := 0
	for _, rule := range role["rules"].([]Object) {
		if rule["resources"].([]string)[0] != "secrets" {
			continue
		}
		secretRules++
		if names, ok := rule["resourceNames"].([]string); !ok || len(names) != 1 ||
			names[0] != "harbor-pull" || len(rule["verbs"].([]string)) != 1 ||
			rule["verbs"].([]string)[0] != "get" {
			t.Fatalf("workload secret access is too broad: %+v", rule)
		}
	}
	if secretRules != 1 {
		t.Fatalf("expected one named Secret read rule, got %d", secretRules)
	}
}
