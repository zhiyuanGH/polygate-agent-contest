package appgateway

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"polygate/internal/server"
)

const testApp = "11111111-2222-3333-4444-555555555555"
const testAccount = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
const testSuffix = "apps.192.0.2.45.sslip.io"

type publicAccessFunc func(context.Context, string) (bool, error)

func (f publicAccessFunc) PublicAppAllowed(ctx context.Context, appID string) (bool, error) {
	return f(ctx, appID)
}

var allowPublic publicAccessFunc = func(_ context.Context, _ string) (bool, error) { return true, nil }

type privateAccessFake struct {
	mode    bool
	modeErr error
}

func (f privateAccessFake) PrivateAppAllowed(_ context.Context, _ string) (bool, error) {
	return f.mode, f.modeErr
}
func (f privateAccessFake) AppAccessSessionAllowed(_ context.Context, _ string, token string) (bool, error) {
	return token == "session-token", nil
}
func (f privateAccessFake) ConsumeAppAccessTicket(_ context.Context, _ string, ticket string) (string, string, error) {
	if ticket != "one-time-ticket" {
		return "", "", server.ErrForbidden
	}
	return "session-token", "/docs", nil
}

func service() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "app-" + strings.ReplaceAll(testApp, "-", ""),
			Namespace: "pg-u-" + strings.ReplaceAll(testAccount, "-", ""),
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "polygate",
				"polygate.io/app-id":           testApp,
				"polygate.io/account-id":       testAccount,
			},
		},
		Spec: corev1.ServiceSpec{
			Type:  corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{{Name: "http", Protocol: corev1.ProtocolTCP, Port: 8080}},
		},
	}
}

func TestReadableAliasUsesManagedServiceAndKeepsIDRoute(t *testing.T) {
	managed := service()
	managed.Annotations = map[string]string{
		"polygate.io/route-username": "student-a", "polygate.io/route-app-name": "test-agent-1"}
	paths, err := ParsePathRoutes("apps.polylink.org:443")
	if err != nil {
		t.Fatal(err)
	}
	var gotPath, gotPrefix string
	gateway := &Gateway{Resolver: NewResolver(fake.NewSimpleClientset(managed)), Access: allowPublic, PathRoutes: paths,
		Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
			gotPath, gotPrefix = req.URL.RequestURI(), req.Header.Get("X-Forwarded-Prefix")
			return &http.Response{StatusCode: 200, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader("ok"))}, nil
		})}
	for _, route := range []struct{ path, prefix string }{
		{"/student-a/test-agent-1/docs", "/student-a/test-agent-1"},
		{"/" + testApp + "/docs", "/" + testApp},
	} {
		response := httptest.NewRecorder()
		gateway.ServeHTTP(response, httptest.NewRequest("GET", "https://apps.polylink.org"+route.path, nil))
		if response.Code != 200 || gotPath != "/docs" || gotPrefix != route.prefix {
			t.Fatalf("route %s failed: status=%d upstream=%s prefix=%s", route.path,
				response.Code, gotPath, gotPrefix)
		}
	}
	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, httptest.NewRequest("GET", "https://apps.polylink.org/student-a/test-agent-1", nil))
	if response.Code != http.StatusPermanentRedirect || response.Header().Get("Location") != "/student-a/test-agent-1/" {
		t.Fatalf("alias slash redirect failed: %d %s", response.Code, response.Header().Get("Location"))
	}
	for _, path := range []string{"/student-a/other/", "/other/test-agent-1/", "/student-a/%2Fdocs/"} {
		response := httptest.NewRecorder()
		gateway.ServeHTTP(response, httptest.NewRequest("GET", "https://apps.polylink.org"+path, nil))
		if response.Code != http.StatusNotFound {
			t.Fatalf("unmanaged alias %s accepted: %d", path, response.Code)
		}
	}
}

func TestNumericStudentIDRoute(t *testing.T) {
	managed := service()
	managed.Annotations = map[string]string{
		"polygate.io/route-username": "24021919d", "polygate.io/route-app-name": "demo"}
	paths, err := ParsePathRoutes("apps.polylink.org:443")
	if err != nil {
		t.Fatal(err)
	}
	var prefix string
	gateway := &Gateway{Resolver: NewResolver(fake.NewSimpleClientset(managed)), Access: allowPublic, PathRoutes: paths,
		Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
			prefix = req.Header.Get("X-Forwarded-Prefix")
			return &http.Response{StatusCode: 200, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader("ok"))}, nil
		})}
	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, httptest.NewRequest("GET", "https://apps.polylink.org/24021919d/demo/docs", nil))
	if response.Code != 200 || prefix != "/24021919d/demo" {
		t.Fatalf("student ID alias failed: status=%d prefix=%s", response.Code, prefix)
	}
}

func TestPrivateAppCannotUsePublicOrLegacyRoutes(t *testing.T) {
	managed := service()
	managed.Annotations = map[string]string{
		"polygate.io/route-username": "student-a", "polygate.io/route-app-name": "test-agent-1"}
	paths, _ := ParsePathRoutes("apps.polylink.org:443")
	checked, proxied := 0, 0
	gateway := &Gateway{
		Resolver: NewResolver(fake.NewSimpleClientset(managed)),
		Access: publicAccessFunc(func(_ context.Context, appID string) (bool, error) {
			if appID != testApp {
				t.Errorf("authorization used alias %q instead of app ID", appID)
			}
			checked++
			return false, nil
		}),
		PathRoutes: paths, Suffixes: []string{testSuffix}, PublicPort: "30444",
		Transport: transportFunc(func(*http.Request) (*http.Response, error) {
			proxied++
			return &http.Response{StatusCode: 200, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader("leaked"))}, nil
		}),
	}
	for _, rawURL := range []string{
		"https://apps.polylink.org/student-a/test-agent-1/docs",
		"https://apps.polylink.org/" + testApp + "/docs",
		"https://" + testApp + "." + testSuffix + ":30444/docs",
		"https://apps.polylink.org/student-a/test-agent-1",
	} {
		response := httptest.NewRecorder()
		gateway.ServeHTTP(response, httptest.NewRequest("GET", rawURL, nil))
		if response.Code != http.StatusNotFound {
			t.Errorf("private app leaked at %s: %d", rawURL, response.Code)
		}
	}
	if checked != 4 || proxied != 0 {
		t.Fatalf("authorization checks=%d upstream calls=%d", checked, proxied)
	}
	gateway.Access = nil
	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, httptest.NewRequest("GET", "https://apps.polylink.org/"+testApp+"/", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing authorization service returned %d", response.Code)
	}
	gateway.Access = publicAccessFunc(func(context.Context, string) (bool, error) {
		return false, errors.New("database unavailable")
	})
	response = httptest.NewRecorder()
	gateway.ServeHTTP(response, httptest.NewRequest("GET", "https://apps.polylink.org/"+testApp+"/", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed authorization service returned %d", response.Code)
	}
}

func TestPrivateHostLoginCookieAndProxyIsolation(t *testing.T) {
	privateRoutes, err := ParseHostRoutes("polylink.org:443")
	if err != nil {
		t.Fatal(err)
	}
	paths, _ := ParsePathRoutes("apps.polylink.org:443")
	proxied := 0
	gateway := &Gateway{
		Resolver:      NewResolver(fake.NewSimpleClientset(service())),
		Access:        publicAccessFunc(func(context.Context, string) (bool, error) { return false, nil }),
		PrivateAccess: privateAccessFake{mode: true}, PrivateHostRoutes: privateRoutes,
		PathRoutes: paths, ConsoleOrigin: "https://polygate.polylink.org",
		Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
			proxied++
			if req.Header.Get("Cookie") != "app-cookie=ok" {
				t.Errorf("platform session reached app: %q", req.Header.Get("Cookie"))
			}
			response := &http.Response{StatusCode: http.StatusFound, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader(""))}
			response.Header.Set("Location", "http://"+req.URL.Host+"/next")
			response.Header.Add("Set-Cookie", "__Host-polygate-app-session=forged; Secure; Path=/")
			response.Header.Add("Set-Cookie", "app-cookie=ok; Secure; Path=/")
			return response, nil
		}),
	}
	privateURL := "https://" + testApp + ".polylink.org"
	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, httptest.NewRequest("GET", privateURL+"/_polygate/route-check", nil))
	if response.Code != http.StatusNoContent || response.Header().Get("X-PolyGate-Route-App-ID") != testApp {
		t.Fatalf("route preflight = %d, app=%q", response.Code, response.Header().Get("X-PolyGate-Route-App-ID"))
	}
	navigate := httptest.NewRequest("GET", privateURL+"/docs?x=1", nil)
	navigate.Header.Set("Accept", "text/html,application/xhtml+xml")
	response = httptest.NewRecorder()
	gateway.ServeHTTP(response, navigate)
	want := "https://polygate.polylink.org/auth/app?app=" + testApp + "&return=%2Fdocs%3Fx%3D1"
	if response.Code != http.StatusFound || response.Header().Get("Location") != want {
		t.Fatalf("private login redirect = %d %q", response.Code, response.Header().Get("Location"))
	}
	response = httptest.NewRecorder()
	gateway.ServeHTTP(response, httptest.NewRequest("GET", privateURL+"/api/data", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous API returned %d", response.Code)
	}
	crossOrigin := httptest.NewRequest("GET", privateURL+"/socket", nil)
	crossOrigin.Header.Set("Origin", "https://other-app.polylink.org")
	crossOrigin.Header.Set("Upgrade", "websocket")
	crossOrigin.Header.Set("Cookie", privateSessionCookie+"=session-token")
	response = httptest.NewRecorder()
	gateway.ServeHTTP(response, crossOrigin)
	if response.Code != http.StatusForbidden {
		t.Fatalf("cross-origin private WebSocket returned %d", response.Code)
	}
	response = httptest.NewRecorder()
	gateway.ServeHTTP(response, httptest.NewRequest("GET", privateURL+"/_polygate/auth/consume?ticket=one-time-ticket", nil))
	cookies := response.Result().Cookies()
	if response.Code != http.StatusFound || response.Header().Get("Location") != "/docs" ||
		len(cookies) != 1 || cookies[0].Name != privateSessionCookie || !cookies[0].Secure ||
		!cookies[0].HttpOnly || cookies[0].Domain != "" || cookies[0].Path != "/" {
		t.Fatalf("private ticket exchange = %d, cookies=%+v, location=%q",
			response.Code, cookies, response.Header().Get("Location"))
	}
	request := httptest.NewRequest("GET", privateURL+"/docs", nil)
	request.Header.Set("Cookie", privateSessionCookie+"=session-token; app-cookie=ok")
	response = httptest.NewRecorder()
	gateway.ServeHTTP(response, request)
	if response.Code != http.StatusFound || response.Header().Get("Location") != privateURL+"/next" || proxied != 1 {
		t.Fatalf("authorized private proxy = %d, location=%q, calls=%d",
			response.Code, response.Header().Get("Location"), proxied)
	}
	if got := response.Result().Header.Values("Set-Cookie"); len(got) != 1 || got[0] != "app-cookie=ok; Secure; Path=/" {
		t.Fatalf("private app response set platform cookie: %q", got)
	}
	for _, rawURL := range []string{privateURL + "/_polygate/other", "https://apps.polylink.org/" + testApp + "/"} {
		response = httptest.NewRecorder()
		gateway.ServeHTTP(response, httptest.NewRequest("GET", rawURL, nil))
		if response.Code != http.StatusNotFound {
			t.Fatalf("reserved or public route %s returned %d", rawURL, response.Code)
		}
	}
	gateway.PrivateAccess = privateAccessFake{mode: false}
	response = httptest.NewRecorder()
	gateway.ServeHTTP(response, httptest.NewRequest("GET", privateURL+"/", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("public app accepted on private host: %d", response.Code)
	}
}

func TestHostAndServiceValidation(t *testing.T) {
	good := testApp + "." + testSuffix + ":30444"
	if id, err := AppIDFromHost(good, "30444", []string{testSuffix}); err != nil || id != testApp {
		t.Fatalf("valid app host rejected: %q %v", id, err)
	}
	for _, host := range []string{
		"192.0.2.45:30444", testApp + "." + testSuffix + ":30443",
		"evil." + testApp + "." + testSuffix + ":30444",
		"not-a-uuid." + testSuffix + ":30444",
		testApp + ".apps.other.example:30444",
	} {
		if _, err := AppIDFromHost(host, "30444", []string{testSuffix}); err == nil {
			t.Errorf("unexpectedly accepted host %q", host)
		}
	}
	valid := service()
	if _, _, ok := targetFor(valid); !ok {
		t.Fatal("managed service rejected")
	}
	invalid := valid.DeepCopy()
	invalid.Namespace = "polygate-system"
	if _, _, ok := targetFor(invalid); ok {
		t.Fatal("platform namespace accepted as app backend")
	}
	invalid = valid.DeepCopy()
	delete(invalid.Labels, "app.kubernetes.io/managed-by")
	if _, _, ok := targetFor(invalid); ok {
		t.Fatal("unmanaged service accepted as app backend")
	}
}

func TestHostRoutesSupportInternalAndPublicCutover(t *testing.T) {
	routes, err := ParseHostRoutes(testSuffix + ":30444,example.org:443")
	if err != nil || len(routes) != 2 {
		t.Fatalf("valid routes rejected: %+v %v", routes, err)
	}
	for _, host := range []string{testApp + "." + testSuffix + ":30444", testApp + ".example.org"} {
		if appID, err := AppIDFromRoutes(host, routes); err != nil || appID != testApp {
			t.Fatalf("cutover host rejected: %s %s %v", host, appID, err)
		}
	}
	gateway := &Gateway{Resolver: NewResolver(fake.NewSimpleClientset(service())), Access: allowPublic, HostRoutes: routes,
		Transport: transportFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader("cutover-ok"))}, nil
		})}
	for _, host := range []string{testApp + "." + testSuffix + ":30444", testApp + ".example.org"} {
		response := httptest.NewRecorder()
		gateway.ServeHTTP(response, httptest.NewRequest("GET", "https://"+host+"/", nil))
		if response.Code != 200 || response.Body.String() != "cutover-ok" {
			t.Fatalf("gateway did not serve cutover host %s: %d %s", host, response.Code, response.Body.String())
		}
	}
	for _, host := range []string{testApp + ".example.org:30444", testApp + "." + testSuffix,
		"polygate.example.org", "evil." + testApp + ".example.org"} {
		if _, err := AppIDFromRoutes(host, routes); err == nil {
			t.Errorf("unexpectedly accepted host %s", host)
		}
	}
	for _, raw := range []string{"", "example.org", "example.org:0", "example.org:65536",
		"example.org:0443", "example.org:443,example.org:30444", "EXAMPLE.org:443"} {
		if _, err := ParseHostRoutes(raw); err == nil {
			t.Errorf("invalid route accepted: %q", raw)
		}
	}
}

func TestPathRoutesStripPrefixAndScopeRedirectsAndCookies(t *testing.T) {
	paths, err := ParsePathRoutes("apps.polylink.org:443")
	if err != nil {
		t.Fatal(err)
	}
	var gotPath, gotHost, gotPrefix, gotProto, gotPort string
	gateway := &Gateway{Resolver: NewResolver(fake.NewSimpleClientset(service())), Access: allowPublic,
		Suffixes: []string{testSuffix}, PublicPort: "30444", PathRoutes: paths,
		Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
			gotPath, gotHost = req.URL.RequestURI(), req.Host
			gotPrefix = req.Header.Get("X-Forwarded-Prefix")
			gotProto, gotPort = req.Header.Get("X-Forwarded-Proto"), req.Header.Get("X-Forwarded-Port")
			header := make(http.Header)
			header.Set("Location", "/login?next=%2Fdashboard")
			header.Add("Set-Cookie", "session=ok; Secure; HttpOnly; SameSite=Lax; Path=/")
			header.Add("Set-Cookie", "theme=dark; Path=/settings")
			header.Add("Set-Cookie", "cross=bad; Domain=polylink.org; Path=/")
			header.Add("Set-Cookie", "__Host-session=bad; Secure; Path=/")
			return &http.Response{StatusCode: http.StatusFound, Header: header,
				Body: io.NopCloser(strings.NewReader(""))}, nil
		})}
	request := httptest.NewRequest("GET", "https://apps.polylink.org/"+testApp+"/start?x=1", nil)
	request.Header.Set("X-Forwarded-Prefix", "/other-app")
	request.Header.Set("X-Forwarded-Port", "8080")
	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, request)
	if response.Code != http.StatusFound || gotPath != "/start?x=1" || gotHost != request.Host ||
		gotPrefix != "/"+testApp || gotProto != "https" || gotPort != "443" ||
		response.Header().Get("Location") != "/"+testApp+"/login?next=%2Fdashboard" {
		t.Fatalf("path gateway mismatch: status=%d upstream=%q host=%q forwarded=%q %q %q location=%q",
			response.Code, gotPath, gotHost, gotPrefix, gotProto, gotPort, response.Header().Get("Location"))
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 2 || cookies[0].Name != "session" || cookies[0].Path != "/"+testApp+"/" ||
		!cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode ||
		cookies[1].Name != "theme" || cookies[1].Path != "/"+testApp+"/settings" {
		t.Fatalf("path cookies escaped their application: %+v", cookies)
	}
	missingSlash := httptest.NewRecorder()
	gateway.ServeHTTP(missingSlash, httptest.NewRequest("GET", "https://apps.polylink.org/"+testApp+"?q=1", nil))
	if missingSlash.Code != http.StatusPermanentRedirect || missingSlash.Header().Get("Location") != "/"+testApp+"/?q=1" {
		t.Fatalf("app root was not canonicalized: %d %q", missingSlash.Code, missingSlash.Header().Get("Location"))
	}
	unknown := httptest.NewRecorder()
	gateway.ServeHTTP(unknown, httptest.NewRequest("GET", "https://apps.polylink.org/not-an-app/", nil))
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("invalid app ID was not rejected: %d", unknown.Code)
	}
	internal := httptest.NewRecorder()
	gateway.ServeHTTP(internal, httptest.NewRequest("GET", "https://"+testApp+"."+testSuffix+":30444/", nil))
	if internal.Code != http.StatusFound {
		t.Fatalf("legacy app host failed after path route added: %d", internal.Code)
	}
}

func TestPathRouteRejectsInvalidHostAndRewritesInternalAbsoluteRedirect(t *testing.T) {
	for _, raw := range []string{"", "apps.polylink.org", "apps.polylink.org:0", "apps.polylink.org:0443",
		"apps.polylink.org:443,apps.polylink.org:30444", "APPS.polylink.org:443"} {
		if _, err := ParsePathRoutes(raw); err == nil {
			t.Errorf("invalid path route accepted: %q", raw)
		}
	}
	paths, _ := ParsePathRoutes("apps.polylink.org:443")
	gateway := &Gateway{Resolver: NewResolver(fake.NewSimpleClientset(service())), Access: allowPublic, PathRoutes: paths,
		Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
			header := make(http.Header)
			if req.URL.Path == "/port" {
				header.Set("Location", "http://"+req.Host+":8080/next")
			} else {
				header.Set("Location", "http://"+req.URL.Host+"/next")
			}
			return &http.Response{StatusCode: 302, Header: header, Body: io.NopCloser(strings.NewReader(""))}, nil
		})}
	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, httptest.NewRequest("GET", "https://apps.polylink.org/"+testApp+"/", nil))
	if response.Header().Get("Location") != "https://apps.polylink.org/"+testApp+"/next" {
		t.Fatalf("internal absolute redirect leaked: %q", response.Header().Get("Location"))
	}
	internalPort := httptest.NewRecorder()
	gateway.ServeHTTP(internalPort, httptest.NewRequest("GET", "https://apps.polylink.org/"+testApp+"/port", nil))
	if internalPort.Header().Get("Location") != "https://apps.polylink.org/"+testApp+"/next" {
		t.Fatalf("app internal port leaked in redirect: %q", internalPort.Header().Get("Location"))
	}
	badHost := httptest.NewRecorder()
	gateway.ServeHTTP(badHost, httptest.NewRequest("GET", "https://evil.apps.polylink.org/"+testApp+"/", nil))
	if badHost.Code != http.StatusMisdirectedRequest {
		t.Fatalf("unexpected path host accepted: %d", badHost.Code)
	}
}

func TestPathRoutePreservesEscapedSuffixAndExternalRedirect(t *testing.T) {
	var gotURI string
	gateway := &Gateway{Resolver: NewResolver(fake.NewSimpleClientset(service())), Access: allowPublic,
		PathRoutes: []PathRoute{{Host: "apps.polylink.org", Port: "443"}},
		Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
			gotURI = req.URL.RequestURI()
			header := make(http.Header)
			header.Set("Location", "https://identity.example.org/login")
			return &http.Response{StatusCode: 302, Header: header,
				Body: io.NopCloser(strings.NewReader(""))}, nil
		})}
	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, httptest.NewRequest("GET",
		"https://apps.polylink.org/"+testApp+"/files/a%2Fb?download=1", nil))
	if response.Code != 302 || gotURI != "/files/a%2Fb?download=1" ||
		response.Header().Get("Location") != "https://identity.example.org/login" {
		t.Fatalf("escaped path or external redirect changed: %d %q %q",
			response.Code, gotURI, response.Header().Get("Location"))
	}
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestGatewayRoutesOnlyManagedAppAndOverwritesForwardedHeaders(t *testing.T) {
	resolver := NewResolver(fake.NewSimpleClientset(service()))
	var upstreamHost, upstreamPath, forwardedFor, forwardedProto, upstreamRequestHost string
	gateway := &Gateway{
		Resolver: resolver, Access: allowPublic, Suffixes: []string{testSuffix}, PublicPort: "30444",
		Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
			upstreamHost, upstreamPath = req.URL.Host, req.URL.RequestURI()
			forwardedFor, forwardedProto = req.Header.Get("X-Forwarded-For"), req.Header.Get("X-Forwarded-Proto")
			upstreamRequestHost = req.Host
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("app-ok"))}, nil
		}),
	}
	request := httptest.NewRequest("GET", "https://"+testApp+"."+testSuffix+":30444/a?q=1", nil)
	request.RemoteAddr = "192.0.2.10:1234"
	request.Header.Set("X-Forwarded-For", "198.51.100.200")
	request.Header.Set("X-Forwarded-Proto", "http")
	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, request)
	if response.Code != 200 || response.Body.String() != "app-ok" {
		t.Fatalf("unexpected proxy response: %d %q", response.Code, response.Body.String())
	}
	if upstreamHost != "app-11111111222233334444555555555555.pg-u-aaaaaaaabbbbccccddddeeeeeeeeeeee.svc.cluster.local:8080" ||
		upstreamPath != "/a?q=1" || upstreamRequestHost != request.Host ||
		forwardedFor != "192.0.2.10" || forwardedProto != "https" {
		t.Fatalf("unexpected proxy request: host=%q path=%q reqHost=%q xff=%q proto=%q",
			upstreamHost, upstreamPath, upstreamRequestHost, forwardedFor, forwardedProto)
	}
	bad := httptest.NewRequest("GET", "https://192.0.2.45:30444/", nil)
	denied := httptest.NewRecorder()
	gateway.ServeHTTP(denied, bad)
	if denied.Code != http.StatusMisdirectedRequest {
		t.Fatalf("raw IP host accepted: %d", denied.Code)
	}
	_, err := resolver.Resolve(context.Background(), "00000000-0000-0000-0000-000000000000")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing app should be not found: %v", err)
	}
}

func TestGatewayStripsPlatformCredentialsButPreservesAppCookies(t *testing.T) {
	resolver := NewResolver(fake.NewSimpleClientset(service()))
	gateway := &Gateway{Resolver: resolver, Access: allowPublic, Suffixes: []string{testSuffix}, PublicPort: "30444",
		Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
			if got := req.Header.Get("Cookie"); got != "app-session=ok; theme=dark" {
				t.Errorf("unexpected forwarded cookies: %q", got)
			}
			for _, name := range []string{"X-Polygate-Account", "X-Authenticated-User",
				"X-Remote-User", "X-Forwarded-User", "X-Forwarded-Email"} {
				if value := req.Header.Get(name); value != "" {
					t.Errorf("forwarded spoofable identity header %s=%q", name, value)
				}
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader("ok"))}, nil
		})}
	request := httptest.NewRequest("GET", "https://"+testApp+"."+testSuffix+":30444/", nil)
	request.Header.Set("Cookie", "__Host-polygate-session=secret; __Host-polygate-id-token=id; "+
		"KEYCLOAK_SESSION=kc; AUTH_SESSION_ID=auth; pg-dev-flow=flow; app-session=ok; theme=dark")
	for _, name := range []string{"X-Polygate-Account", "X-Authenticated-User",
		"X-Remote-User", "X-Forwarded-User", "X-Forwarded-Email"} {
		request.Header.Set(name, "spoofed")
	}
	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatalf("gateway returned %d: %s", response.Code, response.Body.String())
	}
}

func TestGatewayRejectsReservedAndParentDomainAppCookies(t *testing.T) {
	resolver := NewResolver(fake.NewSimpleClientset(service()))
	gateway := &Gateway{Resolver: resolver, Access: allowPublic, Suffixes: []string{testSuffix}, PublicPort: "30444",
		Transport: transportFunc(func(*http.Request) (*http.Response, error) {
			header := make(http.Header)
			header.Add("Set-Cookie", "app-session=ok; Secure; HttpOnly; Path=/")
			header.Add("Set-Cookie", "__Host-polygate-app-session=forged; Secure; Path=/")
			header.Add("Set-Cookie", "KEYCLOAK_SESSION=forged; Path=/")
			header.Add("Set-Cookie", "cross-app=forged; dOmAiN=apps.example.org; Path=/")
			return &http.Response{StatusCode: 200, Header: header,
				Body: io.NopCloser(strings.NewReader("ok"))}, nil
		})}
	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, httptest.NewRequest("GET", "https://"+testApp+"."+testSuffix+":30444/", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("gateway returned %d: %s", response.Code, response.Body.String())
	}
	cookies := response.Result().Header.Values("Set-Cookie")
	if len(cookies) != 1 || cookies[0] != "app-session=ok; Secure; HttpOnly; Path=/" {
		t.Fatalf("student app set reserved or parent-domain cookies: %q", cookies)
	}
}

func TestResolverKeepsVerifiedRouteDuringShortAPIOutage(t *testing.T) {
	client := fake.NewSimpleClientset(service())
	resolver := NewResolver(client)
	if _, err := resolver.Resolve(context.Background(), testApp); err != nil {
		t.Fatal(err)
	}
	client.PrependReactor("list", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("API unavailable")
	})
	resolver.expires = time.Now().Add(-time.Second)
	if _, err := resolver.Resolve(context.Background(), testApp); err != nil {
		t.Fatalf("verified route failed during short API outage: %v", err)
	}
	resolver.staleUntil = time.Now().Add(-time.Second)
	if _, err := resolver.Resolve(context.Background(), testApp); err == nil {
		t.Fatal("expired route continued indefinitely after API outage")
	}
}

func gatewayForBackend(backend *httptest.Server) *Gateway {
	return &Gateway{
		Resolver: NewResolver(fake.NewSimpleClientset(service())), Access: allowPublic,
		Suffixes: []string{testSuffix}, PublicPort: "30444",
		Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
			req.URL.Scheme = "http"
			req.URL.Host = backend.Listener.Addr().String()
			return http.DefaultTransport.RoundTrip(req)
		}),
	}
}

func TestGatewayStreamsSSEBeforeUpstreamFinishes(t *testing.T) {
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "data: second\n\n")
	}))
	defer backend.Close()
	server := httptest.NewTLSServer(gatewayForBackend(backend))
	defer server.Close()
	client := server.Client()
	client.Timeout = 5 * time.Second
	req, err := http.NewRequest("GET", server.URL+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = testApp + "." + testSuffix + ":30444"
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	first, err := reader.ReadString('\n')
	if err != nil || response.StatusCode != 200 || first != "data: first\n" {
		t.Fatalf("first SSE event was buffered or changed: status=%d line=%q err=%v", response.StatusCode, first, err)
	}
	close(release)
	released = true
	remainder, err := io.ReadAll(reader)
	if err != nil || !strings.Contains(string(remainder), "data: second\n\n") {
		t.Fatalf("second SSE event was lost: %q %v", remainder, err)
	}
}

func TestGatewayPassesWebSocketUpgradeAndBidirectionalBytes(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Upgrade") != "websocket" {
			http.Error(w, "missing upgrade", http.StatusBadRequest)
			return
		}
		connection, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer connection.Close()
		_, _ = io.WriteString(connection, "HTTP/1.1 101 Switching Protocols\r\n"+
			"Connection: Upgrade\r\nUpgrade: websocket\r\n"+
			"Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n\r\n")
		buffer := make([]byte, 4)
		if _, err := io.ReadFull(connection, buffer); err == nil {
			_, _ = connection.Write(buffer)
		}
	}))
	defer backend.Close()
	server := httptest.NewTLSServer(gatewayForBackend(backend))
	defer server.Close()
	// Only this local test server uses its generated certificate.
	connection, err := tls.Dial("tcp", server.Listener.Addr().String(),
		&tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
	_, err = fmt.Fprintf(connection, "GET /socket HTTP/1.1\r\nHost: %s.%s:30444\r\n"+
		"Connection: Upgrade\r\nUpgrade: websocket\r\n"+
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"+
		"Sec-WebSocket-Version: 13\r\n\r\n", testApp, testSuffix)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(connection)
	status, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(status, "101 Switching Protocols") {
		t.Fatalf("WebSocket upgrade failed: %q %v", status, err)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	if _, err := connection.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, 4)
	if _, err := io.ReadFull(reader, response); err != nil || string(response) != "ping" {
		t.Fatalf("WebSocket bytes did not pass both ways: %q %v", response, err)
	}
}

func TestPathGatewayPassesWebSocketUpgradeAndBidirectionalBytes(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/socket" || req.Header.Get("X-Forwarded-Prefix") != "/"+testApp ||
			req.Header.Get("Upgrade") != "websocket" {
			http.Error(w, "unexpected WebSocket request", http.StatusBadRequest)
			return
		}
		connection, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer connection.Close()
		_, _ = io.WriteString(connection, "HTTP/1.1 101 Switching Protocols\r\n"+
			"Connection: Upgrade\r\nUpgrade: websocket\r\n"+
			"Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n\r\n")
		buffer := make([]byte, 4)
		if _, err := io.ReadFull(connection, buffer); err == nil {
			_, _ = connection.Write(buffer)
		}
	}))
	defer backend.Close()
	gateway := gatewayForBackend(backend)
	gateway.PathRoutes = []PathRoute{{Host: "apps.polylink.org", Port: "443"}}
	server := httptest.NewTLSServer(gateway)
	defer server.Close()
	connection, err := tls.Dial("tcp", server.Listener.Addr().String(), &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
	_, err = fmt.Fprintf(connection, "GET /%s/socket HTTP/1.1\r\nHost: apps.polylink.org\r\n"+
		"Connection: Upgrade\r\nUpgrade: websocket\r\n"+
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"+
		"Sec-WebSocket-Version: 13\r\n\r\n", testApp)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(connection)
	status, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(status, "101 Switching Protocols") {
		t.Fatalf("path WebSocket upgrade failed: %q %v", status, err)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	if _, err := connection.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, 4)
	if _, err := io.ReadFull(reader, response); err != nil || string(response) != "ping" {
		t.Fatalf("path WebSocket bytes did not pass both ways: %q %v", response, err)
	}
}
