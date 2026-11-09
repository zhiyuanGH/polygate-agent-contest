package appgateway

import (
	"context"
	"errors"
	"fmt"
	"log"
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
	PathRoutes []PathRoute
	Transport  http.RoundTripper
}

type HostRoute struct {
	Suffix string
	Port   string
}

type PathRoute struct {
	Host string
	Port string
}

func stripPlatformCredentials(req *http.Request) {
	// Console and Keycloak cookies must never reach student workloads, even
	// when a client explicitly sends them to an app hostname. Keep unrelated
	// application cookies so existing student sessions continue to work.
	cookies := req.Cookies()
	req.Header.Del("Cookie")
	for _, cookie := range cookies {
		if platformCookieName(cookie.Name) {
			continue
		}
		req.AddCookie(cookie)
	}
	for name := range req.Header {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-polygate-") ||
			strings.HasPrefix(lower, "x-authenticated-") ||
			strings.HasPrefix(lower, "x-remote-") ||
			strings.HasPrefix(lower, "x-forwarded-user") ||
			strings.HasPrefix(lower, "x-forwarded-email") ||
			strings.HasPrefix(lower, "x-forwarded-groups") {
			req.Header.Del(name)
		}
	}
}

func platformCookieName(name string) bool {
	name = strings.ToLower(name)
	return strings.HasPrefix(name, "__host-polygate-") ||
		strings.HasPrefix(name, "pg-dev-") ||
		strings.HasPrefix(name, "keycloak_") ||
		strings.HasPrefix(name, "kc_") ||
		name == "auth_session_id"
}

func stripAppSetCookies(response *http.Response) error {
	// A student app may set cookies for its own host. Keep platform-reserved
	// names and parent-domain cookies out of the browser so an app cannot
	// claim another app's future access session or share a cookie with peers.
	raw := response.Header.Values("Set-Cookie")
	response.Header.Del("Set-Cookie")
	for _, line := range raw {
		parts := strings.Split(line, ";")
		name, _, valid := strings.Cut(strings.TrimSpace(parts[0]), "=")
		if !valid || name == "" || platformCookieName(name) {
			continue
		}
		parentDomain := false
		for _, attribute := range parts[1:] {
			key, _, _ := strings.Cut(strings.TrimSpace(attribute), "=")
			if strings.EqualFold(key, "Domain") {
				parentDomain = true
				break
			}
		}
		if !parentDomain {
			response.Header.Add("Set-Cookie", line)
		}
	}
	return nil
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

// ParsePathRoutes accepts exact public hosts. A path route never matches a
// student-controlled subdomain, and old hostname routes remain independent.
func ParsePathRoutes(raw string) ([]PathRoute, error) {
	parts := strings.Split(raw, ",")
	if len(parts) < 1 || len(parts) > 4 {
		return nil, errors.New("expected one to four app path routes")
	}
	result := make([]PathRoute, 0, len(parts))
	seen := make(map[string]bool)
	for _, part := range parts {
		host, port, ok := strings.Cut(strings.TrimSpace(part), ":")
		if !ok || strings.Contains(port, ":") || seen[host] {
			return nil, errors.New("invalid or duplicate app path route")
		}
		if _, err := ParseSuffixes(host); err != nil {
			return nil, err
		}
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 || strconv.Itoa(value) != port {
			return nil, errors.New("invalid app path route port")
		}
		seen[host] = true
		result = append(result, PathRoute{Host: host, Port: port})
	}
	return result, nil
}

func pathAppID(req *http.Request, routes []PathRoute) (string, string, bool, bool) {
	for _, route := range routes {
		allowed := req.Host == net.JoinHostPort(route.Host, route.Port)
		if route.Port == "443" {
			allowed = allowed || req.Host == route.Host
		}
		if !allowed {
			continue
		}
		escaped := req.URL.EscapedPath()
		if !strings.HasPrefix(escaped, "/") {
			return "", "", true, false
		}
		segment, _, _ := strings.Cut(escaped[1:], "/")
		if !uuidPattern.MatchString(segment) {
			return "", "", true, false
		}
		prefix := "/" + segment
		if escaped == prefix {
			return segment, prefix, true, true
		}
		return segment, prefix, true, false
	}
	return "", "", false, false
}

func querySuffix(raw string) string {
	if raw == "" {
		return ""
	}
	return "?" + raw
}

func rewritePathLocation(response *http.Response, prefix string, target *url.URL, publicHost string) {
	raw := response.Header.Get("Location")
	if raw == "" {
		return
	}
	location, err := url.Parse(raw)
	if err != nil {
		return
	}
	publicName := publicHost
	if hostname, _, err := net.SplitHostPort(publicHost); err == nil {
		publicName = hostname
	}
	_, targetPort, _ := net.SplitHostPort(target.Host)
	samePublicHost := location.Hostname() == publicName &&
		(location.Port() == "" || location.Port() == "80" || location.Port() == "443" || location.Port() == targetPort)
	if location.Host != "" && location.Host != target.Host && !samePublicHost {
		return
	}
	if location.Host == target.Host || samePublicHost {
		location.Scheme = "https"
		location.Host = publicHost
	}
	if !strings.HasPrefix(location.Path, "/") || strings.HasPrefix(location.Path, "//") {
		return
	}
	if location.Path != prefix && !strings.HasPrefix(location.Path, prefix+"/") {
		location.Path = prefix + location.Path
		location.RawPath = ""
	}
	response.Header.Set("Location", location.String())
}

func scopePathCookies(response *http.Response, prefix string) {
	raw := response.Header.Values("Set-Cookie")
	response.Header.Del("Set-Cookie")
	for _, line := range raw {
		parsed := (&http.Response{Header: http.Header{"Set-Cookie": {line}}}).Cookies()
		if len(parsed) != 1 {
			continue
		}
		cookie := parsed[0]
		if cookie.Domain != "" || platformCookieName(cookie.Name) ||
			strings.HasPrefix(strings.ToLower(cookie.Name), "__host-") {
			continue
		}
		path := cookie.Path
		if path == "" || !strings.HasPrefix(path, "/") {
			path = "/"
		}
		if path != prefix && !strings.HasPrefix(path, prefix+"/") {
			path = prefix + path
		}
		cookie.Path = path
		response.Header.Add("Set-Cookie", cookie.String())
	}
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
	appID, prefix, pathMode, addSlash := pathAppID(req, g.PathRoutes)
	if !pathMode {
		var err error
		appID, err = AppIDFromHost(req.Host, g.PublicPort, g.Suffixes)
		if len(g.HostRoutes) > 0 {
			appID, err = AppIDFromRoutes(req.Host, g.HostRoutes)
		}
		if err != nil {
			http.Error(w, "Unknown app host", http.StatusMisdirectedRequest)
			return
		}
	} else if appID == "" {
		http.NotFound(w, req)
		return
	}
	if addSlash {
		http.Redirect(w, req, prefix+"/"+querySuffix(req.URL.RawQuery), http.StatusPermanentRedirect)
		return
	}
	if appID == "" {
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
	upstreamPath, upstreamRawPath := req.URL.Path, req.URL.RawPath
	if pathMode {
		upstreamPath = strings.TrimPrefix(upstreamPath, prefix)
		if upstreamPath == "" {
			upstreamPath = "/"
		}
		if upstreamRawPath != "" {
			upstreamRawPath = strings.TrimPrefix(upstreamRawPath, prefix)
			if upstreamRawPath == "" {
				upstreamRawPath = "/"
			}
		}
	}
	proxy := &httputil.ReverseProxy{
		Transport:     g.Transport,
		FlushInterval: -1,
		ModifyResponse: func(response *http.Response) error {
			if pathMode {
				rewritePathLocation(response, prefix, target, req.Host)
				scopePathCookies(response, prefix)
				return nil
			}
			return stripAppSetCookies(response)
		},
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.Path = upstreamPath
			pr.Out.URL.RawPath = upstreamRawPath
			pr.Out.Host = pr.In.Host
			stripPlatformCredentials(pr.Out)
			pr.Out.Header.Del("Forwarded")
			pr.Out.Header.Del("X-Forwarded-For")
			pr.Out.Header.Del("X-Forwarded-Host")
			pr.Out.Header.Del("X-Forwarded-Proto")
			pr.SetXForwarded()
			if pathMode {
				pr.Out.Header.Set("X-Forwarded-Prefix", prefix)
				pr.Out.Header.Set("X-Forwarded-Port", "443")
			}
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, proxyError error) {
			log.Printf("app gateway upstream failed: app=%s err=%v", appID, proxyError)
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
