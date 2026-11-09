package clusterstats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	queryPods      = `kube_pod_info{namespace="polygate-monitoring",pod=~"prometheus-prometheus-node-exporter-.*"}`
	queryCPU       = `1 - avg by (instance) (rate(node_cpu_seconds_total{job="node-exporter",mode="idle"}[5m]))`
	queryMemTotal  = `node_memory_MemTotal_bytes{job="node-exporter"}`
	queryMemAvail  = `node_memory_MemAvailable_bytes{job="node-exporter"}`
	queryRootTotal = `node_filesystem_size_bytes{job="node-exporter",mountpoint="/",fstype!~"tmpfs|overlay"}`
	queryRootAvail = `node_filesystem_avail_bytes{job="node-exporter",mountpoint="/",fstype!~"tmpfs|overlay"}`
)

type Reader struct {
	base   *url.URL
	client *http.Client
}

type Node struct {
	Name               string  `json:"name"`
	CPUUsedPercent     float64 `json:"cpuUsedPercent"`
	MemoryUsedBytes    float64 `json:"memoryUsedBytes"`
	MemoryTotalBytes   float64 `json:"memoryTotalBytes"`
	RootAvailableBytes float64 `json:"rootAvailableBytes"`
	RootTotalBytes     float64 `json:"rootTotalBytes"`
}

type Snapshot struct {
	Nodes     []Node    `json:"nodes"`
	SampledAt time.Time `json:"sampledAt"`
}

type vectorItem struct {
	Metric map[string]string `json:"metric"`
	Value  []json.RawMessage `json:"value"`
}

type queryResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string       `json:"resultType"`
		Result     []vectorItem `json:"result"`
	} `json:"data"`
}

func NewReader(raw string) (*Reader, error) {
	base, err := url.Parse(raw)
	if err != nil || base.Scheme != "http" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || base.Path != "" {
		return nil, errors.New("invalid internal Prometheus URL")
	}
	return &Reader{base: base, client: &http.Client{Timeout: 5 * time.Second}}, nil
}

func (r *Reader) query(ctx context.Context, expression string) ([]vectorItem, error) {
	endpoint := *r.base
	endpoint.Path = "/api/v1/query"
	params := url.Values{"query": {expression}}
	endpoint.RawQuery = params.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	response, err := r.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Prometheus query returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 256*1024+1))
	if err != nil || len(body) > 256*1024 {
		return nil, errors.New("Prometheus query response is too large or unreadable")
	}
	var result queryResponse
	if err := json.Unmarshal(body, &result); err != nil || result.Status != "success" || result.Data.ResultType != "vector" {
		return nil, errors.New("Prometheus query response is invalid")
	}
	return result.Data.Result, nil
}

func sample(item vectorItem, now time.Time) (float64, error) {
	if len(item.Value) != 2 {
		return 0, errors.New("Prometheus sample is incomplete")
	}
	var timestamp float64
	var valueText string
	if err := json.Unmarshal(item.Value[0], &timestamp); err != nil {
		return 0, err
	}
	if err := json.Unmarshal(item.Value[1], &valueText); err != nil {
		return 0, err
	}
	value, err := strconv.ParseFloat(valueText, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0, errors.New("Prometheus sample is not a finite nonnegative number")
	}
	if age := now.Sub(time.Unix(int64(timestamp), 0)); age < -time.Minute || age > 2*time.Minute {
		return 0, errors.New("Prometheus sample is stale")
	}
	return value, nil
}

func values(items []vectorItem, expected map[string]string, now time.Time) (map[string]float64, error) {
	result := make(map[string]float64, len(expected))
	for _, item := range items {
		instance := item.Metric["instance"]
		if _, exists := expected[instance]; !exists {
			continue
		}
		if _, duplicate := result[instance]; duplicate {
			return nil, errors.New("duplicate Prometheus node sample")
		}
		value, err := sample(item, now)
		if err != nil {
			return nil, err
		}
		result[instance] = value
	}
	if len(result) != len(expected) {
		return nil, errors.New("Prometheus node samples are missing")
	}
	return result, nil
}

func (r *Reader) Read(ctx context.Context) (Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	now := time.Now().UTC()
	pods, err := r.query(ctx, queryPods)
	if err != nil {
		return Snapshot{}, err
	}
	names := make(map[string]string)
	for _, item := range pods {
		if !strings.HasPrefix(item.Metric["pod"], "prometheus-prometheus-node-exporter-") ||
			item.Metric["namespace"] != "polygate-monitoring" {
			continue
		}
		ip := net.ParseIP(item.Metric["pod_ip"])
		name := item.Metric["node"]
		if ip == nil || name == "" || len(name) > 253 {
			return Snapshot{}, errors.New("Prometheus exporter identity is invalid")
		}
		if _, err := sample(item, now); err != nil {
			return Snapshot{}, err
		}
		instance := net.JoinHostPort(ip.String(), "9100")
		if _, duplicate := names[instance]; duplicate {
			return Snapshot{}, errors.New("duplicate Prometheus exporter identity")
		}
		names[instance] = name
	}
	if len(names) != 3 {
		return Snapshot{}, errors.New("expected metrics from all three cluster nodes")
	}
	queries := []string{queryCPU, queryMemTotal, queryMemAvail, queryRootTotal, queryRootAvail}
	metrics := make([]map[string]float64, len(queries))
	for index, expression := range queries {
		items, err := r.query(ctx, expression)
		if err != nil {
			return Snapshot{}, err
		}
		metrics[index], err = values(items, names, now)
		if err != nil {
			return Snapshot{}, err
		}
	}
	result := Snapshot{Nodes: make([]Node, 0, len(names)), SampledAt: now}
	for instance, name := range names {
		cpu, totalMem, availMem := metrics[0][instance], metrics[1][instance], metrics[2][instance]
		totalRoot, availRoot := metrics[3][instance], metrics[4][instance]
		if cpu > 1 || totalMem <= 0 || availMem > totalMem || totalRoot <= 0 || availRoot > totalRoot {
			return Snapshot{}, errors.New("Prometheus node metrics are inconsistent")
		}
		result.Nodes = append(result.Nodes, Node{Name: name, CPUUsedPercent: cpu * 100,
			MemoryUsedBytes: totalMem - availMem, MemoryTotalBytes: totalMem,
			RootAvailableBytes: availRoot, RootTotalBytes: totalRoot})
	}
	sort.Slice(result.Nodes, func(i, j int) bool { return result.Nodes[i].Name < result.Nodes[j].Name })
	return result, nil
}
