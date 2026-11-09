package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"polygate/internal/metricsrotate"
)

func TestInterruptedBeforeSecretUpdateRestoresOldTrust(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	oldPEM, oldKey, err := metricsrotate.Generate(now)
	if err != nil {
		t.Fatal(err)
	}
	newPEM, newKey, err := metricsrotate.Generate(now)
	if err != nil {
		t.Fatal(err)
	}
	newCert, err := metricsrotate.Validate(newPEM, newKey, now)
	if err != nil {
		t.Fatal(err)
	}
	target := metricsrotate.Fingerprint(newCert)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"tls.crt": oldPEM, "tls.key": oldKey}}
	replicas := int32(2)
	zero, one := intstr.FromInt32(0), intstr.FromInt32(1)
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: appsv1.DeploymentSpec{Replicas: &replicas, Strategy: appsv1.DeploymentStrategy{
			Type:          appsv1.RollingUpdateDeploymentStrategyType,
			RollingUpdate: &appsv1.RollingUpdateDeployment{MaxSurge: &zero, MaxUnavailable: &one}}}}
	api := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiregistration.k8s.io/v1", "kind": "APIService",
		"metadata": map[string]any{"name": apiName, "annotations": map[string]any{marker: target}},
		"spec": map[string]any{
			"service":  map[string]any{"name": name, "namespace": namespace},
			"caBundle": base64.StdEncoding.EncodeToString(append(bytes.Clone(oldPEM), newPEM...)),
		},
	}}
	r := &rotator{client: kubefake.NewSimpleClientset(secret, deployment),
		dynamic: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), api), now: func() time.Time { return now }}
	err = r.run(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "abandoned rotation") {
		t.Fatalf("expected safe rollback of abandoned rotation, got %v", err)
	}
	live, err := r.getAPI(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if live.GetAnnotations()[marker] != "" {
		t.Fatal("abandoned rotation marker remained on APIService")
	}
	bundle, err := bundleOf(live)
	if err != nil || len(bundle) != 1 {
		t.Fatalf("old trust bundle was not restored: %v", err)
	}
	oldCert, err := metricsrotate.Validate(oldPEM, oldKey, now)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bundle[0].Raw, oldCert.Raw) {
		t.Fatal("old certificate was not restored as the sole trust anchor")
	}
}
