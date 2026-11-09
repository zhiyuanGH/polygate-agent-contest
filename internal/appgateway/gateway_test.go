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
)

const testApp = "11111111-2222-3333-4444-555555555555"
const testAccount = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
const testSuffix = "apps.192.0.2.45.sslip.io"

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

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestGatewayRoutesOnlyManagedAppAndOverwritesForwardedHeaders(t *testing.T) {
	resolver := NewResolver(fake.NewSimpleClientset(service()))
	var upstreamHost, upstreamPath, forwardedFor, forwardedProto, upstreamRequestHost string
	gateway := &Gateway{
		Resolver: resolver, Suffixes: []string{testSuffix}, PublicPort: "30444",
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
		Resolver: NewResolver(fake.NewSimpleClientset(service())),
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
