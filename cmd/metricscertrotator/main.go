// metricscertrotator renews the Metrics Server serving certificate in stages.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"polygate/internal/metricsrotate"
)

const (
	name      = "metrics-server"
	namespace = "kube-system"
	apiName   = "v1beta1.metrics.k8s.io"
	marker    = "polygate.io/metrics-cert-target-sha256"
)

var apiGVR = schema.GroupVersionResource{Group: "apiregistration.k8s.io", Version: "v1", Resource: "apiservices"}
var metricsGVR = schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "nodes"}

type rotator struct {
	client  kubernetes.Interface
	dynamic dynamic.Interface
	now     func() time.Time
}

func (r *rotator) getAPI(ctx context.Context) (*unstructured.Unstructured, error) {
	return r.dynamic.Resource(apiGVR).Get(ctx, apiName, metav1.GetOptions{})
}

func bundleOf(api *unstructured.Unstructured) ([]*x509.Certificate, error) {
	if value, _, _ := unstructured.NestedBool(api.Object, "spec", "insecureSkipTLSVerify"); value {
		return nil, fmt.Errorf("APIService skips TLS verification")
	}
	service, _, _ := unstructured.NestedMap(api.Object, "spec", "service")
	if service["name"] != name || service["namespace"] != namespace {
		return nil, fmt.Errorf("APIService target changed")
	}
	encoded, found, err := unstructured.NestedString(api.Object, "spec", "caBundle")
	if err != nil || !found {
		return nil, fmt.Errorf("APIService has no CA bundle")
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	return metricsrotate.Bundle(data)
}

func (r *rotator) updateAPI(ctx context.Context, api *unstructured.Unstructured, bundle []byte, target string) error {
	copy := api.DeepCopy()
	if err := unstructured.SetNestedField(copy.Object, base64.StdEncoding.EncodeToString(bundle), "spec", "caBundle"); err != nil {
		return err
	}
	annotations := copy.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	if target == "" {
		delete(annotations, marker)
	} else {
		annotations[marker] = target
	}
	copy.SetAnnotations(annotations)
	_, err := r.dynamic.Resource(apiGVR).Update(ctx, copy, metav1.UpdateOptions{})
	return err
}

func checkDeployment(deployment *appsv1.Deployment) error {
	if deployment == nil || deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 2 ||
		deployment.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType ||
		deployment.Spec.Strategy.RollingUpdate == nil ||
		deployment.Spec.Strategy.RollingUpdate.MaxSurge == nil ||
		deployment.Spec.Strategy.RollingUpdate.MaxSurge.IntValue() != 0 ||
		deployment.Spec.Strategy.RollingUpdate.MaxUnavailable == nil ||
		deployment.Spec.Strategy.RollingUpdate.MaxUnavailable.IntValue() != 1 {
		return fmt.Errorf("Metrics Server Deployment does not have a safe two-node rollout strategy")
	}
	return nil
}

func (r *rotator) run(ctx context.Context, force bool) error {
	now := r.now().UTC()
	secret, err := r.client.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if secret.Type != corev1.SecretTypeOpaque {
		return fmt.Errorf("Metrics Server Secret type changed")
	}
	currentPEM := secret.Data["tls.crt"]
	current, err := metricsrotate.Validate(currentPEM, secret.Data["tls.key"], now)
	if err != nil {
		return fmt.Errorf("current Metrics Server keypair: %w", err)
	}
	api, err := r.getAPI(ctx)
	if err != nil {
		return err
	}
	bundle, err := bundleOf(api)
	if err != nil {
		return err
	}
	deployment, err := r.client.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if err := checkDeployment(deployment); err != nil {
		return fmt.Errorf("safe Deployment preflight failed: %w", err)
	}
	target := api.GetAnnotations()[marker]
	currentHash := metricsrotate.Fingerprint(current)
	if target == "" {
		if len(bundle) != 1 || !bytes.Equal(bundle[0].Raw, current.Raw) {
			return fmt.Errorf("APIService CA and Secret differ without an active rotation")
		}
		remaining := current.NotAfter.Sub(now)
		if !force && remaining > 90*24*time.Hour {
			log.Printf("METRICS_CERT_HEALTHY expires=%s remainingDays=%.1f", current.NotAfter.Format(time.RFC3339), remaining.Hours()/24)
			return nil
		}
		newPEM, newKey, err := metricsrotate.Generate(now)
		if err != nil {
			return err
		}
		newCert, err := metricsrotate.Validate(newPEM, newKey, now)
		if err != nil {
			return err
		}
		target = metricsrotate.Fingerprint(newCert)
		if err := r.updateAPI(ctx, api, append(bytes.Clone(currentPEM), newPEM...), target); err != nil {
			return fmt.Errorf("stage dual CA trust: %w", err)
		}
		copy := secret.DeepCopy()
		copy.Data["tls.crt"] = newPEM
		copy.Data["tls.key"] = newKey
		if _, err := r.client.CoreV1().Secrets(namespace).Update(ctx, copy, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("update Metrics Server keypair after dual trust: %w", err)
		}
		currentPEM, current, currentHash = newPEM, newCert, target
		log.Printf("STAGED_METRICS_CERT target=%s", target)
	} else {
		if len(bundle) == 1 && metricsrotate.Fingerprint(bundle[0]) == currentHash && target == currentHash {
			if err := r.updateAPI(ctx, api, currentPEM, ""); err != nil {
				return err
			}
			log.Printf("FINISHED_METRICS_CERT marker=%s", target)
			return nil
		}
		if len(bundle) != 2 || metricsrotate.Fingerprint(bundle[1]) != target {
			return fmt.Errorf("staged CA bundle or marker is inconsistent")
		}
		if bytes.Equal(bundle[0].Raw, current.Raw) {
			if err := r.updateAPI(ctx, api, currentPEM, ""); err != nil {
				return err
			}
			return fmt.Errorf("abandoned rotation had not updated the Secret; restored old CA bundle")
		}
		if !bytes.Equal(bundle[1].Raw, current.Raw) || currentHash != target {
			return fmt.Errorf("Secret does not contain the staged target certificate")
		}
		log.Printf("RESUMING_METRICS_CERT target=%s", target)
	}
	if err := r.rollout(ctx, target); err != nil {
		return err
	}
	if err := r.waitServiceTLS(ctx, currentPEM, currentHash); err != nil {
		return err
	}
	if err := r.waitAggregation(ctx); err != nil {
		return err
	}
	api, err = r.getAPI(ctx)
	if err != nil {
		return err
	}
	if api.GetAnnotations()[marker] != target {
		return fmt.Errorf("rotation marker changed before finalizing")
	}
	if err := r.updateAPI(ctx, api, currentPEM, ""); err != nil {
		return fmt.Errorf("remove old CA trust: %w", err)
	}
	if err := r.waitAggregation(ctx); err != nil {
		return err
	}
	log.Printf("ROTATED_METRICS_CERT target=%s expires=%s", target, current.NotAfter.Format(time.RFC3339))
	return nil
}

func (r *rotator) rollout(ctx context.Context, target string) error {
	deployment, err := r.client.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if err := checkDeployment(deployment); err != nil {
		return err
	}
	if deployment.Spec.Template.Annotations[marker] != target {
		copy := deployment.DeepCopy()
		if copy.Spec.Template.Annotations == nil {
			copy.Spec.Template.Annotations = map[string]string{}
		}
		copy.Spec.Template.Annotations[marker] = target
		deployment, err = r.client.AppsV1().Deployments(namespace).Update(ctx, copy, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
	}
	deadline := time.Now().Add(180 * time.Second)
	for {
		live, err := r.client.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if live.Spec.Template.Annotations[marker] != target || checkDeployment(live) != nil {
			return fmt.Errorf("Metrics Server Deployment changed during rotation")
		}
		if live.Status.ObservedGeneration >= live.Generation && live.Status.UpdatedReplicas == 2 &&
			live.Status.AvailableReplicas == 2 && live.Status.Replicas == 2 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("Metrics Server rollout did not complete in three minutes")
		}
		if err := sleep(ctx, 3*time.Second); err != nil {
			return err
		}
	}
}

func (r *rotator) waitServiceTLS(ctx context.Context, certPEM []byte, expected string) error {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		return fmt.Errorf("new serving certificate is not a valid trust anchor")
	}
	deadline := time.Now().Add(45 * time.Second)
	for {
		var valid bool
		for range 3 {
			connection, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp",
				metricsrotate.ServiceName+":443", &tls.Config{RootCAs: pool,
					ServerName: metricsrotate.ServiceName, MinVersion: tls.VersionTLS12})
			if err != nil {
				valid = false
				break
			}
			certs := connection.ConnectionState().PeerCertificates
			_ = connection.Close()
			if len(certs) == 0 || metricsrotate.Fingerprint(certs[0]) != expected {
				valid = false
				break
			}
			valid = true
		}
		if valid {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("Metrics Server Service is not presenting the new trusted certificate")
		}
		if err := sleep(ctx, 3*time.Second); err != nil {
			return err
		}
	}
}

func (r *rotator) waitAggregation(ctx context.Context) error {
	deadline := time.Now().Add(60 * time.Second)
	for {
		api, err := r.getAPI(ctx)
		available := false
		if err == nil {
			conditions, _, _ := unstructured.NestedSlice(api.Object, "status", "conditions")
			for _, value := range conditions {
				item, ok := value.(map[string]any)
				if ok && item["type"] == "Available" && item["status"] == "True" {
					available = true
				}
			}
		}
		if available {
			metrics, err := r.dynamic.Resource(metricsGVR).List(ctx, metav1.ListOptions{})
			if err == nil && len(metrics.Items) == 3 {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("aggregated node metrics are unavailable after certificate rotation")
		}
		if err := sleep(ctx, 3*time.Second); err != nil {
			return err
		}
	}
}

func sleep(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func main() {
	force := flag.Bool("force", false, "rotate immediately for an attended acceptance test")
	flag.Parse()
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatal(err)
	}
	config.QPS, config.Burst = 5, 10
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatal(err)
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	r := &rotator{client: client, dynamic: dynamicClient, now: time.Now}
	if err := r.run(ctx, *force); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
