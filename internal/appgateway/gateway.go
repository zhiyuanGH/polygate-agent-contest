package appgateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

var ErrNotFound = errors.New("app service not found")

type Resolver struct {
	client     kubernetes.Interface
	mu         sync.Mutex
	expires    time.Time
	staleUntil time.Time
	services   map[string]*url.URL
}

func NewResolver(client kubernetes.Interface) *Resolver {
	return &Resolver{client: client}
}

// Resolve accepts only Services created by PolyGate with matching app and
// account labels. No user-controlled hostname or URL is used as an upstream.
func (r *Resolver) Resolve(ctx context.Context, appID string) (*url.URL, error) {
	if !uuidPattern.MatchString(appID) {
		return nil, ErrNotFound
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Now().After(r.expires) {
		list, err := r.client.CoreV1().Services("").List(ctx, metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/managed-by=polygate",
		})
		if err != nil {
			if cached := r.services[appID]; cached != nil && time.Now().Before(r.staleUntil) {
				copy := *cached
				return &copy, nil
			}
			return nil, err
		}
		services := make(map[string]*url.URL, len(list.Items))
		conflicts := make(map[string]bool)
		for i := range list.Items {
			service := &list.Items[i]
			id, target, ok := targetFor(service)
			if !ok {
				continue
			}
			if services[id] != nil {
				conflicts[id] = true
			}
			services[id] = target
		}
		for id := range conflicts {
			delete(services, id)
		}
		r.services = services
		r.expires = time.Now().Add(5 * time.Second)
		r.staleUntil = time.Now().Add(5 * time.Minute)
	}
	target := r.services[appID]
	if target == nil {
		return nil, ErrNotFound
	}
	copy := *target
	return &copy, nil
}

func targetFor(service *corev1.Service) (string, *url.URL, bool) {
	labels := service.GetLabels()
	appID, accountID := labels["polygate.io/app-id"], labels["polygate.io/account-id"]
	if labels["app.kubernetes.io/managed-by"] != "polygate" ||
		!uuidPattern.MatchString(appID) || !uuidPattern.MatchString(accountID) ||
		service.GetNamespace() != "pg-u-"+strings.ReplaceAll(accountID, "-", "") ||
		service.GetName() != "app-"+strings.ReplaceAll(appID, "-", "") ||
		service.Spec.Type != corev1.ServiceTypeClusterIP || len(service.Spec.Ports) != 1 ||
		service.Spec.Ports[0].Name != "http" || service.Spec.Ports[0].Protocol != corev1.ProtocolTCP ||
		service.Spec.Ports[0].Port < 1 {
		return "", nil, false
	}
	target := &url.URL{Scheme: "http", Host: net.JoinHostPort(
		service.GetName()+"."+service.GetNamespace()+".svc.cluster.local",
		strconv.Itoa(int(service.Spec.Ports[0].Port))),
	}
	return appID, target, true
}

func AppIDFromHost(host, port string, suffixes []string) (string, error) {
	hostname, gotPort, err := net.SplitHostPort(host)
	if err != nil && port == "443" {
		hostname, gotPort, err = host, "443", nil
	}
	if err != nil || gotPort != port || hostname != strings.ToLower(hostname) {
		return "", ErrNotFound
	}
	for _, suffix := range suffixes {
		prefix, ok := strings.CutSuffix(hostname, "."+suffix)
		if ok && uuidPattern.MatchString(prefix) {
			return prefix, nil
		}
	}
	return "", ErrNotFound
}

type Gateway struct {
	Resolver   *Resolver
	Suffixes   []string
	PublicPort string
	HostRoutes []HostRoute
	Transport  http.RoundTripper
}

type HostRoute struct {
	Suffix string
	Port   string
}

// ParseHostRoutes lets the old NodePort and a new public HTTPS hostname work
// during a domain cutover. Each suffix has exactly one expected external port.
func ParseHostRoutes(raw string) ([]HostRoute, error) {
	parts := strings.Split(raw, ",")
	if len(parts) < 1 || len(parts) > 8 {
		return nil, errors.New("expected one to eight app host routes")
	}
	routes := make([]HostRoute, 0, len(parts))
	seen := make(map[string]bool, len(parts))
	for _, part := range parts {
		suffix, port, ok := strings.Cut(strings.TrimSpace(part), ":")
		if !ok || strings.Contains(port, ":") || seen[suffix] {
			return nil, errors.New("invalid or duplicate app host route")
		}
		if _, err := ParseSuffixes(suffix); err != nil {
			return nil, err
		}
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 || strconv.Itoa(value) != port {
			return nil, errors.New("invalid app host route port")
		}
		seen[suffix] = true
		routes = append(routes, HostRoute{Suffix: suffix, Port: port})
	}
	return routes, nil
}

func AppIDFromRoutes(host string, routes []HostRoute) (string, error) {
	for _, route := range routes {
		if appID, err := AppIDFromHost(host, route.Port, []string{route.Suffix}); err == nil {
			return appID, nil
		}
	}
	return "", ErrNotFound
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path == "/healthz" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
		return
	}
	if req.TLS == nil {
		http.Error(w, "HTTPS required", http.StatusBadRequest)
		return
	}
	appID, err := AppIDFromHost(req.Host, g.PublicPort, g.Suffixes)
	if len(g.HostRoutes) > 0 {
		appID, err = AppIDFromRoutes(req.Host, g.HostRoutes)
	}
	if err != nil {
		http.Error(w, "Unknown app host", http.StatusMisdirectedRequest)
		return
	}
	target, err := g.Resolver.Resolve(req.Context(), appID)
	if errors.Is(err, ErrNotFound) {
		http.NotFound(w, req)
		return
	}
	if err != nil {
		http.Error(w, "App routing temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	proxy := &httputil.ReverseProxy{
		Transport:     g.Transport,
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = pr.In.Host
			pr.Out.Header.Del("Forwarded")
			pr.Out.Header.Del("X-Forwarded-For")
			pr.Out.Header.Del("X-Forwarded-Host")
			pr.Out.Header.Del("X-Forwarded-Proto")
			pr.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "App backend unavailable", http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, req)
}

func ParseSuffixes(raw string) ([]string, error) {
	fields := strings.Split(raw, ",")
	if len(fields) < 1 || len(fields) > 4 {
		return nil, errors.New("expected one to four app host suffixes")
	}
	var result []string
	for _, field := range fields {
		value := strings.TrimSpace(field)
		if value == "" || value != strings.ToLower(value) || strings.ContainsAny(value, "/: ") ||
			!strings.Contains(value, ".") {
			return nil, fmt.Errorf("invalid app host suffix")
		}
		result = append(result, value)
	}
	return result, nil
}
