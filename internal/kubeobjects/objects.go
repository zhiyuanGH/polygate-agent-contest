package kubeobjects

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"polygate/internal/domain"
)

type Object map[string]any

type List struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Items      []Object `json:"items"`
}

func (l List) JSON() ([]byte, error) { return json.MarshalIndent(l, "", "  ") }

var accountPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var digestPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._:/-]+@sha256:[0-9a-f]{64}$`)
var routeAliasSegmentPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func ValidatePlatformDigest(image string) error {
	if !digestPattern.MatchString(image) {
		return errors.New("platform image must be pinned by sha256 digest")
	}
	return nil
}

func NamespaceName(accountID string) (string, error) {
	if !accountPattern.MatchString(accountID) {
		return "", errors.New("account ID must be a canonical lowercase UUID")
	}
	return "pg-u-" + strings.ReplaceAll(accountID, "-", ""), nil
}

func AppName(appID string) (string, error) {
	if !accountPattern.MatchString(appID) {
		return "", errors.New("app ID must be a canonical lowercase UUID")
	}
	return "app-" + strings.ReplaceAll(appID, "-", ""), nil
}

func VolumeName(volumeID string) (string, error) {
	if !accountPattern.MatchString(volumeID) {
		return "", errors.New("volume ID must be a canonical lowercase UUID")
	}
	return "vol-" + strings.ReplaceAll(volumeID, "-", ""), nil
}

func Tenant(accountID string, quota domain.Quota) (List, error) {
	ns, err := NamespaceName(accountID)
	if err != nil {
		return List{}, err
	}
	if quota.CPURequestMilli < 250 || quota.CPULimitMilli < 1000 || quota.MemoryMiB < 1024 || quota.Pods < 1 || quota.Apps < 1 {
		return List{}, errors.New("account quota is below the tenant template minimum")
	}
	if quota.CPURequestMilli > quota.CPULimitMilli {
		return List{}, errors.New("CPU request quota exceeds CPU limit quota")
	}
	labels := Object{
		"polygate.io/account-id":             accountID,
		"pod-security.kubernetes.io/enforce": "restricted", "pod-security.kubernetes.io/enforce-version": "v1.37",
		"pod-security.kubernetes.io/audit": "restricted", "pod-security.kubernetes.io/audit-version": "v1.37",
		"pod-security.kubernetes.io/warn": "restricted", "pod-security.kubernetes.io/warn-version": "v1.37",
	}
	metadata := func(name string) Object { return Object{"name": name, "namespace": ns} }
	appGatewayPolicy := Object{"podSelector": Object{"matchLabels": Object{"app.kubernetes.io/managed-by": "polygate"}},
		"policyTypes": []string{"Ingress"}, "ingress": []Object{{"from": []Object{{
			"namespaceSelector": Object{"matchLabels": Object{"kubernetes.io/metadata.name": "polygate-system"}},
			"podSelector":       Object{"matchLabels": Object{"app": "polygate-app-gateway"}},
		}}, "ports": []Object{{"protocol": "TCP", "port": "http"}}}}}
	objects := []Object{
		{"apiVersion": "v1", "kind": "Namespace", "metadata": Object{"name": ns, "labels": labels}},
		{"apiVersion": "v1", "kind": "ResourceQuota", "metadata": metadata("account-budget"), "spec": Object{"hard": Object{
			"pods": fmt.Sprint(quota.Pods), "count/deployments.apps": fmt.Sprint(quota.Apps), "services": fmt.Sprint(quota.Apps),
			"requests.cpu": fmt.Sprintf("%dm", quota.CPURequestMilli), "limits.cpu": fmt.Sprintf("%dm", quota.CPULimitMilli),
			"requests.memory": fmt.Sprintf("%dMi", quota.MemoryMiB), "limits.memory": fmt.Sprintf("%dMi", quota.MemoryMiB),
			"requests.ephemeral-storage": fmt.Sprintf("%dMi", quota.Pods*128), "limits.ephemeral-storage": fmt.Sprintf("%dGi", quota.Pods),
			"requests.storage": "10Gi", "persistentvolumeclaims": "2",
			"polygate-rwo.storageclass.storage.k8s.io/requests.storage":       "10Gi",
			"polygate-rwo.storageclass.storage.k8s.io/persistentvolumeclaims": "2",
		}}},
		{"apiVersion": "v1", "kind": "LimitRange", "metadata": metadata("container-resources"), "spec": Object{"limits": []Object{{
			"type": "Container", "max": Object{"cpu": "2", "memory": "4Gi", "ephemeral-storage": "1Gi"},
			"default":        Object{"cpu": "1", "memory": "1Gi", "ephemeral-storage": "1Gi"},
			"defaultRequest": Object{"cpu": "250m", "memory": "512Mi", "ephemeral-storage": "128Mi"},
		}, {"type": "PersistentVolumeClaim", "min": Object{"storage": "1Gi"},
			"max": Object{"storage": "10Gi"}}}}},
		{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": metadata("app-runtime"), "automountServiceAccountToken": false},
		{"apiVersion": "v1", "kind": "Secret", "metadata": Object{"name": "student-credentials", "namespace": ns,
			"labels": Object{"app.kubernetes.io/managed-by": "polygate", "polygate.io/account-id": accountID}},
			"type": "Opaque"},
		{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role", "metadata": metadata("workload-controller"), "rules": []Object{
			{"apiGroups": []string{"apps"}, "resources": []string{"deployments"}, "verbs": []string{"get", "list", "watch", "create", "update", "patch", "delete"}},
			{"apiGroups": []string{""}, "resources": []string{"services"}, "verbs": []string{"get", "list", "watch", "create", "update", "patch", "delete"}},
			{"apiGroups": []string{""}, "resources": []string{"persistentvolumeclaims"}, "verbs": []string{"get"}},
			{"apiGroups": []string{""}, "resources": []string{"pods", "events"}, "verbs": []string{"get", "list", "watch"}},
			{"apiGroups": []string{"apps"}, "resources": []string{"replicasets"}, "verbs": []string{"get", "list", "watch"}},
			{"apiGroups": []string{""}, "resources": []string{"resourcequotas", "limitranges", "serviceaccounts"}, "verbs": []string{"get"}},
			{"apiGroups": []string{""}, "resources": []string{"secrets"}, "resourceNames": []string{"harbor-pull"}, "verbs": []string{"get"}},
			{"apiGroups": []string{"rbac.authorization.k8s.io"}, "resources": []string{"roles", "rolebindings"}, "verbs": []string{"get"}},
			{"apiGroups": []string{"networking.k8s.io"}, "resources": []string{"networkpolicies"}, "verbs": []string{"get"}},
		}},
		{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding", "metadata": metadata("workload-controller"),
			"roleRef":  Object{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "workload-controller"},
			"subjects": []Object{{"kind": "ServiceAccount", "name": "workload-controller", "namespace": "polygate-system"}}},
		{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role", "metadata": metadata("student-secret-editor"), "rules": []Object{
			{"apiGroups": []string{""}, "resources": []string{"secrets"}, "resourceNames": []string{"student-credentials"}, "verbs": []string{"get", "update"}},
		}},
		{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding", "metadata": metadata("student-secret-editor"),
			"roleRef":  Object{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "student-secret-editor"},
			"subjects": []Object{{"kind": "ServiceAccount", "name": "polygate-api", "namespace": "polygate-system"}}},
		{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role", "metadata": metadata("account-quota-controller"), "rules": []Object{
			{"apiGroups": []string{""}, "resources": []string{"resourcequotas"},
				"resourceNames": []string{"account-budget"}, "verbs": []string{"get", "patch"}},
		}},
		{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding", "metadata": metadata("account-quota-controller"),
			"roleRef":  Object{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "account-quota-controller"},
			"subjects": []Object{{"kind": "ServiceAccount", "name": "polygate-quota-controller", "namespace": "polygate-system"}}},
		{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role", "metadata": metadata("api-log-reader"), "rules": []Object{
			{"apiGroups": []string{""}, "resources": []string{"pods"}, "verbs": []string{"list"}},
			{"apiGroups": []string{""}, "resources": []string{"pods/log"}, "verbs": []string{"get"}},
			{"apiGroups": []string{""}, "resources": []string{"events"}, "verbs": []string{"list"}},
			{"apiGroups": []string{"apps"}, "resources": []string{"deployments"}, "verbs": []string{"get"}},
			{"apiGroups": []string{"apps"}, "resources": []string{"replicasets"}, "verbs": []string{"list"}},
			{"apiGroups": []string{"metrics.k8s.io"}, "resources": []string{"pods"}, "verbs": []string{"list"}},
		}},
		{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding", "metadata": metadata("api-log-reader"),
			"roleRef":  Object{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "api-log-reader"},
			"subjects": []Object{{"kind": "ServiceAccount", "name": "polygate-api", "namespace": "polygate-system"}}},
		{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role", "metadata": metadata("volume-controller"), "rules": []Object{
			{"apiGroups": []string{""}, "resources": []string{"persistentvolumeclaims"}, "verbs": []string{"get", "create", "update", "delete"}},
		}},
		{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding", "metadata": metadata("volume-controller"),
			"roleRef":  Object{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "volume-controller"},
			"subjects": []Object{{"kind": "ServiceAccount", "name": "polygate-volume-controller", "namespace": "polygate-system"}}},
		{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role", "metadata": metadata("harbor-pull-secret-controller"), "rules": []Object{
			{"apiGroups": []string{""}, "resources": []string{"secrets"}, "verbs": []string{"create"}},
			{"apiGroups": []string{""}, "resources": []string{"secrets"}, "resourceNames": []string{"harbor-pull"}, "verbs": []string{"get", "update", "delete"}},
		}},
		{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding", "metadata": metadata("harbor-pull-secret-controller"),
			"roleRef":  Object{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "harbor-pull-secret-controller"},
			"subjects": []Object{{"kind": "ServiceAccount", "name": "polygate-pull-secret-controller", "namespace": "polygate-system"}}},
		{"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy", "metadata": metadata("default-deny"),
			"spec": Object{"podSelector": Object{}, "policyTypes": []string{"Ingress", "Egress"}}},
		{"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy", "metadata": metadata("allow-app-gateway"),
			"spec": appGatewayPolicy},
		{"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy", "metadata": metadata("allow-dns-and-public-https"),
			"spec": Object{"podSelector": Object{}, "policyTypes": []string{"Egress"}, "egress": []Object{
				{"to": []Object{{"namespaceSelector": Object{"matchLabels": Object{"kubernetes.io/metadata.name": "kube-system"}}, "podSelector": Object{"matchLabels": Object{"k8s-app": "kube-dns"}}}},
					"ports": []Object{{"protocol": "UDP", "port": 53}, {"protocol": "TCP", "port": 53}}},
				{"to": []Object{{"ipBlock": Object{"cidr": "0.0.0.0/0", "except": []string{
					"10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.168.0.0/16", "158.132.0.0/16",
				}}}}, "ports": []Object{{"protocol": "TCP", "port": 443}}},
			}}},
	}
	return List{APIVersion: "v1", Kind: "List", Items: objects}, nil
}

// PrecreatedTenants is an operator-applied bootstrap bundle. The long-running
// worker receives only namespaced workload permissions and read access to the
// explicitly listed Namespace objects; it cannot create tenant namespaces.
func PrecreatedTenants(accountIDs []string, quota domain.Quota) (List, error) {
	if len(accountIDs) == 0 || len(accountIDs) > 50 {
		return List{}, errors.New("bootstrap requires between 1 and 50 account IDs")
	}
	ids := append([]string(nil), accountIDs...)
	sort.Strings(ids)
	names := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(accountIDs))
	for _, id := range ids {
		name, err := NamespaceName(id)
		if err != nil {
			return List{}, err
		}
		if seen[name] {
			return List{}, errors.New("duplicate account ID in bootstrap")
		}
		seen[name] = true
		names = append(names, name)
	}
	items := []Object{
		{"apiVersion": "v1", "kind": "Namespace", "metadata": Object{"name": "polygate-system", "labels": Object{
			"pod-security.kubernetes.io/enforce": "restricted", "pod-security.kubernetes.io/enforce-version": "v1.37"}}},
		{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": Object{"name": "workload-controller", "namespace": "polygate-system"}},
		{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": Object{"name": "polygate-volume-controller", "namespace": "polygate-system"}},
		{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": Object{"name": "polygate-pull-secret-controller", "namespace": "polygate-system"}},
		{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": Object{"name": "polygate-api", "namespace": "polygate-system"}},
		{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": Object{"name": "polygate-quota-controller", "namespace": "polygate-system"}},
		{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole", "metadata": Object{"name": "polygate-tenant-reader"},
			"rules": []Object{{"apiGroups": []string{""}, "resources": []string{"namespaces"}, "resourceNames": names, "verbs": []string{"get"}}}},
		{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRoleBinding", "metadata": Object{"name": "polygate-tenant-reader"},
			"roleRef":  Object{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "polygate-tenant-reader"},
			"subjects": []Object{{"kind": "ServiceAccount", "name": "workload-controller", "namespace": "polygate-system"}}},
		{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole", "metadata": Object{"name": "polygate-pull-tenant-reader"},
			"rules": []Object{{"apiGroups": []string{""}, "resources": []string{"namespaces"}, "verbs": []string{"list"}}}},
		{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRoleBinding", "metadata": Object{"name": "polygate-pull-tenant-reader"},
			"roleRef":  Object{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "polygate-pull-tenant-reader"},
			"subjects": []Object{{"kind": "ServiceAccount", "name": "polygate-pull-secret-controller", "namespace": "polygate-system"}}},
	}
	for _, id := range ids {
		tenant, err := Tenant(id, quota)
		if err != nil {
			return List{}, err
		}
		items = append(items, tenant.Items...)
	}
	return List{APIVersion: "v1", Kind: "List", Items: items}, nil
}

// TrustedWorkload ignores spec.Image. The platform must supply a digest from
// its own approved image catalog; mutable tags are never accepted here.
func TrustedWorkload(accountID, appID string, spec domain.AppSpec, platformImageDigest string, username ...string) (List, error) {
	return workload(accountID, appID, spec, platformImageDigest, false, username...)
}

// RollingWorkload uses one surge Pod and keeps every existing replica available
// while replacing a stateless Deployment. The caller must reserve that extra
// Pod's resources before applying this template.
func RollingWorkload(accountID, appID string, spec domain.AppSpec, platformImageDigest string, username ...string) (List, error) {
	if spec.Persistent {
		return List{}, errors.New("persistent workloads require Recreate updates")
	}
	return workload(accountID, appID, spec, platformImageDigest, true, username...)
}

func workload(accountID, appID string, spec domain.AppSpec, platformImageDigest string, rolling bool, username ...string) (List, error) {
	ns, err := NamespaceName(accountID)
	if err != nil {
		return List{}, err
	}
	name, err := AppName(appID)
	if err != nil {
		return List{}, err
	}
	if err := domain.ValidateSpec(spec); err != nil {
		return List{}, err
	}
	if err := ValidatePlatformDigest(platformImageDigest); err != nil {
		return List{}, err
	}
	labels := Object{"app.kubernetes.io/managed-by": "polygate", "polygate.io/account-id": accountID, "polygate.io/app-id": appID}
	meta := Object{"name": name, "namespace": ns, "labels": labels}
	serviceMeta := Object{"name": name, "namespace": ns, "labels": labels}
	if len(username) > 0 && routeAliasSegmentPattern.MatchString(username[0]) &&
		routeAliasSegmentPattern.MatchString(spec.Name) {
		serviceMeta["annotations"] = Object{"polygate.io/route-username": username[0],
			"polygate.io/route-app-name": spec.Name}
	}
	resources := Object{"requests": Object{"cpu": fmt.Sprintf("%dm", spec.Resources.CPURequestMilli), "memory": fmt.Sprintf("%dMi", spec.Resources.MemoryMiB), "ephemeral-storage": "128Mi"},
		"limits": Object{"cpu": fmt.Sprintf("%dm", spec.Resources.CPULimitMilli), "memory": fmt.Sprintf("%dMi", spec.Resources.MemoryMiB), "ephemeral-storage": "1Gi"}}
	container := Object{"name": "app", "image": platformImageDigest, "imagePullPolicy": "IfNotPresent",
		"ports": []Object{{"name": "http", "containerPort": spec.Port, "protocol": "TCP"}}, "resources": resources,
		"securityContext": Object{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": Object{"drop": []string{"ALL"}}},
		"volumeMounts":    []Object{{"name": "tmp", "mountPath": "/tmp"}},
		"startupProbe":    Object{"tcpSocket": Object{"port": "http"}, "periodSeconds": 5, "failureThreshold": 12},
		"readinessProbe":  Object{"tcpSocket": Object{"port": "http"}, "periodSeconds": 5, "timeoutSeconds": 2}}
	if spec.HealthPath != "" {
		check := Object{"path": spec.HealthPath, "port": "http", "scheme": "HTTP"}
		container["startupProbe"] = Object{"httpGet": check, "periodSeconds": 5, "timeoutSeconds": 2, "failureThreshold": 12}
		container["readinessProbe"] = Object{"httpGet": check, "periodSeconds": 5, "timeoutSeconds": 2, "failureThreshold": 3}
		container["livenessProbe"] = Object{"httpGet": check, "periodSeconds": 10, "timeoutSeconds": 2, "failureThreshold": 6}
	}
	if len(spec.SecretKeys) > 0 {
		keys := append([]string(nil), spec.SecretKeys...)
		sort.Strings(keys)
		env := make([]Object, 0, len(keys))
		for _, key := range keys {
			env = append(env, Object{"name": key, "valueFrom": Object{"secretKeyRef": Object{
				"name": "student-credentials", "key": key}}})
		}
		container["env"] = env
	}
	pod := Object{"serviceAccountName": "app-runtime", "automountServiceAccountToken": false,
		"nodeSelector":    Object{"node-role.kubernetes.io/worker": "", "kubernetes.io/arch": "amd64"},
		"securityContext": Object{"runAsNonRoot": true, "runAsUser": 65532, "runAsGroup": 65532, "seccompProfile": Object{"type": "RuntimeDefault"}},
		"containers":      []Object{container}, "volumes": []Object{{"name": "tmp", "emptyDir": Object{"sizeLimit": "256Mi"}}}}
	if spec.Persistent {
		claim, err := VolumeName(spec.VolumeID)
		if err != nil {
			return List{}, err
		}
		container["volumeMounts"] = []Object{{"name": "tmp", "mountPath": "/tmp"},
			{"name": "data", "mountPath": spec.MountPath}}
		pod["volumes"] = []Object{{"name": "tmp", "emptyDir": Object{"sizeLimit": "256Mi"}},
			{"name": "data", "persistentVolumeClaim": Object{"claimName": claim}}}
		pod["securityContext"].(Object)["fsGroup"] = 65532
		pod["securityContext"].(Object)["fsGroupChangePolicy"] = "OnRootMismatch"
	}
	if strings.HasPrefix(platformImageDigest, "harbor.polygate-registry.svc.cluster.local/") {
		pod["imagePullSecrets"] = []Object{{"name": "harbor-pull"}}
	}
	strategy := Object{"type": "Recreate"}
	if rolling {
		strategy = Object{"type": "RollingUpdate", "rollingUpdate": Object{"maxSurge": 1, "maxUnavailable": 0}}
	}
	return List{APIVersion: "v1", Kind: "List", Items: []Object{
		{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": meta,
			"spec": Object{"replicas": spec.Resources.Replicas, "strategy": strategy,
				"selector": Object{"matchLabels": labels}, "template": Object{"metadata": Object{"labels": labels}, "spec": pod}}},
		{"apiVersion": "v1", "kind": "Service", "metadata": serviceMeta,
			"spec": Object{"type": "ClusterIP", "selector": labels, "ports": []Object{{"name": "http", "port": spec.Port, "targetPort": "http", "protocol": "TCP"}}}},
	}}, nil
}
