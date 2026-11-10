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
	"polygate/internal/server"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var aliasSegmentPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

var ErrNotFound = errors.New("app service not found")

type Resolver struct {
	client     kubernetes.Interface
	mu         sync.Mutex
	expires    time.Time
	staleUntil time.Time
	services   map[string]*url.URL
	appIDs     map[string]string
}

func NewResolver(client kubernetes.Interface) *Resolver {
	return &Resolver{client: client}
}

// Resolve accepts only Services created by PolyGate with matching app and
// account labels. No user-controlled hostname or URL is used as an upstream.
func validRouteKey(key string) bool {
	if uuidPattern.MatchString(key) {
		return true
	}
	username, name, ok := strings.Cut(key, "/")
	return ok && aliasSegmentPattern.MatchString(username) && aliasSegmentPattern.MatchString(name)
}

func (r *Resolver) Resolve(ctx context.Context, key string) (*url.URL, error) {
	_, target, err := r.ResolveWithID(ctx, key)
	return target, err
}

// ResolveWithID returns the immutable app ID even for a readable alias.
func (r *Resolver) ResolveWithID(ctx context.Context, key string) (string, *url.URL, error) {
	if !validRouteKey(key) {
		return "", nil, ErrNotFound
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Now().After(r.expires) {
		list, err := r.client.CoreV1().Services("").List(ctx, metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/managed-by=polygate",
		})
		if err != nil {
			if cached := r.services[key]; cached != nil && time.Now().Before(r.staleUntil) {
				copy := *cached
				return r.appIDs[key], &copy, nil
			}
			return "", nil, err
		}
		services := make(map[string]*url.URL, len(list.Items))
		appIDs := make(map[string]string, len(list.Items))
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
			appIDs[id] = id
			if alias := serviceAlias(service); alias != "" {
				if services[alias] != nil {
					conflicts[alias] = true
				}
				services[alias] = target
				appIDs[alias] = id
			}
		}
		for id := range conflicts {
			delete(services, id)
			delete(appIDs, id)
		}
		r.services = services
		r.appIDs = appIDs
		r.expires = time.Now().Add(5 * time.Second)
		r.staleUntil = time.Now().Add(5 * time.Minute)
	}
	target := r.services[key]
	if target == nil {
		return "", nil, ErrNotFound
	}
	copy := *target
	return r.appIDs[key], &copy, nil
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

func serviceAlias(service *corev1.Service) string {
	annotations := service.GetAnnotations()
	username, name := annotations["polygate.io/route-username"], annotations["polygate.io/route-app-name"]
	if !aliasSegmentPattern.MatchString(username) || !aliasSegmentPattern.MatchString(name) {
		return ""
	}
	return username + "/" + name
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
	Resolver          *Resolver
	Access            PublicAccessChecker
	PrivateAccess     PrivateAccessChecker
	Suffixes          []string
	PublicPort        string
	HostRoutes        []HostRoute
	PrivateHostRoutes []HostRoute
	PathRoutes        []PathRoute
	ConsoleOrigin     string
	Transport         http.RoundTripper
}

type PublicAccessChecker interface {
	PublicAppAllowed(context.Context, string) (bool, error)
}

type PrivateAccessChecker interface {
	PrivateAppAllowed(context.Context, string) (bool, error)
	AppAccessSessionAllowed(context.Context, string, string) (bool, error)
	ConsumeAppAccessTicket(context.Context, string, string) (string, string, error)
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
		segment, rest, hasSlash := strings.Cut(escaped[1:], "/")
		key, prefix := segment, "/"+segment
		if !uuidPattern.MatchString(segment) {
			if !aliasSegmentPattern.MatchString(segment) || !hasSlash {
				return "", "", true, false
			}
			name, _, _ := strings.Cut(rest, "/")
			if !aliasSegmentPattern.MatchString(name) {
				return "", "", true, false
			}
			key, prefix = segment+"/"+name, "/"+segment+"/"+name
		}
		if escaped == prefix {
			return key, prefix, true, true
		}
		return key, prefix, true, false
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

const privateSessionCookie = "__Host-polygate-app-session"

func (g *Gateway) authorizePrivate(w http.ResponseWriter, req *http.Request, appID string) bool {
	if g.PrivateAccess == nil || g.ConsoleOrigin == "" {
		http.Error(w, "App authorization unavailable", http.StatusServiceUnavailable)
		return false
	}
	private, err := g.PrivateAccess.PrivateAppAllowed(req.Context(), appID)
	if err != nil {
		http.Error(w, "App authorization temporarily unavailable", http.StatusServiceUnavailable)
		return false
	}
	if !private {
		http.NotFound(w, req)
		return false
	}
	if req.URL.Path == "/_polygate" || strings.HasPrefix(req.URL.Path, "/_polygate/") {
		if req.URL.Path != "/_polygate/auth/consume" {
			http.NotFound(w, req)
			return false
		}
		if req.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return false
		}
		session, returnPath, err := g.PrivateAccess.ConsumeAppAccessTicket(req.Context(), appID,
			req.URL.Query().Get("ticket"))
		if errors.Is(err, server.ErrForbidden) {
			http.Error(w, "App sign-in link expired or invalid", http.StatusForbidden)
			return false
		}
		if err != nil {
			http.Error(w, "App authorization temporarily unavailable", http.StatusServiceUnavailable)
			return false
		}
		http.SetCookie(w, &http.Cookie{Name: privateSessionCookie, Value: session, Path: "/",
			MaxAge: int((8 * time.Hour).Seconds()), Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		http.Redirect(w, req, returnPath, http.StatusFound)
		return false
	}
	expectedHost := req.Host
	if hostname, port, err := net.SplitHostPort(req.Host); err == nil && port == "443" {
		expectedHost = hostname
	}
	origin := req.Header.Get("Origin")
	if origin != "" && origin != "https://"+expectedHost ||
		strings.EqualFold(req.Header.Get("Upgrade"), "websocket") && origin == "" {
		http.Error(w, "Cross-origin app request denied", http.StatusForbidden)
		return false
	}
	session := ""
	if cookie, err := req.Cookie(privateSessionCookie); err == nil {
		session = cookie.Value
	}
	allowed, err := g.PrivateAccess.AppAccessSessionAllowed(req.Context(), appID, session)
	if err != nil {
		http.Error(w, "App authorization temporarily unavailable", http.StatusServiceUnavailable)
		return false
	}
	if allowed {
		return true
	}
	if req.Method == http.MethodGet && strings.Contains(req.Header.Get("Accept"), "text/html") &&
		!strings.EqualFold(req.Header.Get("Upgrade"), "websocket") {
		next := g.ConsoleOrigin + "/auth/app?app=" + url.QueryEscape(appID) +
			"&return=" + url.QueryEscape(req.URL.RequestURI())
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, req, next, http.StatusFound)
		return false
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, "Sign in to access this app", http.StatusUnauthorized)
	return false
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
	privateMode := false
	if !pathMode {
		var err error
		if len(g.PrivateHostRoutes) > 0 {
			appID, err = AppIDFromRoutes(req.Host, g.PrivateHostRoutes)
			privateMode = err == nil
		}
		if !privateMode {
			appID, err = AppIDFromHost(req.Host, g.PublicPort, g.Suffixes)
			if len(g.HostRoutes) > 0 {
				appID, err = AppIDFromRoutes(req.Host, g.HostRoutes)
			}
		}
		if err != nil {
			http.Error(w, "Unknown app host", http.StatusMisdirectedRequest)
			return
		}
	} else if appID == "" {
		http.NotFound(w, req)
		return
	}
	if appID == "" {
		http.Error(w, "Unknown app host", http.StatusMisdirectedRequest)
		return
	}
	canonicalID, target, err := g.Resolver.ResolveWithID(req.Context(), appID)
	if errors.Is(err, ErrNotFound) {
		http.NotFound(w, req)
		return
	}
	if err != nil {
		http.Error(w, "App routing temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	if privateMode && req.Method == http.MethodGet && req.URL.Path == "/_polygate/route-check" {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-PolyGate-Route-App-ID", canonicalID)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if privateMode {
		if !g.authorizePrivate(w, req, canonicalID) {
			return
		}
	} else {
		if g.Access == nil {
			http.Error(w, "App authorization unavailable", http.StatusServiceUnavailable)
			return
		}
		public, err := g.Access.PublicAppAllowed(req.Context(), canonicalID)
		if err != nil {
			http.Error(w, "App authorization temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		if !public {
			http.NotFound(w, req)
			return
		}
	}
	if addSlash {
		http.Redirect(w, req, prefix+"/"+querySuffix(req.URL.RawQuery), http.StatusPermanentRedirect)
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
			if privateMode {
				rewritePathLocation(response, "", target, req.Host)
				return stripAppSetCookies(response)
			}
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
			pr.Out.Header.Del("X-Forwarded-Prefix")
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
