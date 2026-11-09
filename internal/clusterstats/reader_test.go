package clusterstats

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestReadMapsOnlyFixedPrometheusSamplesToNodes(t *testing.T) {
	now := float64(time.Now().Unix())
	instances := []string{"10.244.0.6:9100", "10.244.1.51:9100", "10.244.2.57:9100"}
	nodes := []string{"imcl-control-230", "imcl-worker-45", "imcl-worker-79"}
	seen := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		expression := r.URL.Query().Get("query")
		seen[expression] = true
		items := make([]map[string]any, 0, 3)
		for index, instance := range instances {
			labels := map[string]string{"instance": instance}
			value := "1"
			switch expression {
			case queryPods:
				labels = map[string]string{"namespace": "polygate-monitoring", "pod": "prometheus-prometheus-node-exporter-" + nodes[index],
					"pod_ip": instance[:len(instance)-5], "node": nodes[index]}
			case queryCPU:
				value = "0.25"
			case queryMemTotal:
				value = "1000"
			case queryMemAvail:
				value = "400"
			case queryRootTotal:
				value = "2000"
			case queryRootAvail:
				value = "500"
			default:
				t.Errorf("unexpected PromQL: %q", expression)
			}
			items = append(items, map[string]any{"metric": labels, "value": []any{now, value}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{
			"resultType": "vector", "result": items}})
	}))
	defer server.Close()
	reader, err := NewReader(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := reader.Read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 6 || len(snapshot.Nodes) != 3 {
		t.Fatalf("queries=%d nodes=%d", len(seen), len(snapshot.Nodes))
	}
	for index, node := range snapshot.Nodes {
		if node.Name != nodes[index] || node.CPUUsedPercent != 25 || node.MemoryUsedBytes != 600 ||
			node.RootAvailableBytes != 500 {
			t.Fatalf("wrong node sample: %#v", node)
		}
	}
}

func TestReadRejectsStalePodMapping(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{
			"resultType": "vector", "result": []any{map[string]any{
				"metric": map[string]string{"namespace": "polygate-monitoring", "pod": "prometheus-prometheus-node-exporter-one",
					"pod_ip": "10.244.0.6", "node": "imcl-control-230"},
				"value": []any{float64(time.Now().Add(-5 * time.Minute).Unix()), "1"}}}}})
	}))
	defer server.Close()
	reader, err := NewReader(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Read(t.Context()); err == nil {
		t.Fatal("stale mapping was accepted")
	}
}
