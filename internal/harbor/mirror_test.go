package harbor

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	testregistry "github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"polygate/internal/registry"
)

func dialTestRegistry(address string) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, address)
	}
}

func TestMirrorAccountImagePinsPlatformDigest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sourceServer := httptest.NewTLSServer(testregistry.New())
	defer sourceServer.Close()
	sourceTransport := sourceServer.Client().Transport.(*http.Transport).Clone()
	sourceTransport.DialContext = dialTestRegistry(sourceServer.Listener.Addr().String())
	sourceTransport.TLSClientConfig = sourceTransport.TLSClientConfig.Clone()
	sourceTransport.TLSClientConfig.ServerName = "127.0.0.1"
	image, err := random.Image(1024, 1)
	if err != nil {
		t.Fatal(err)
	}
	config, err := image.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	config.OS, config.Architecture = "linux", "amd64"
	image, err = mutate.ConfigFile(image, config)
	if err != nil {
		t.Fatal(err)
	}
	sourceTag, err := name.NewTag("source.test/team/agent:v1", name.StrictValidation)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(sourceTag, image, remote.WithContext(ctx),
		remote.WithTransport(sourceTransport)); err != nil {
		t.Fatal(err)
	}
	resolver, err := registry.NewResolver(registry.Policy{Registries: []registry.Registry{
		{Host: "source.test"}}}, sourceTransport)
	if err != nil {
		t.Fatal(err)
	}
	source, err := resolver.Resolve(ctx, sourceTag.Name(), nil)
	if err != nil {
		t.Fatal(err)
	}
	project := "pg-u-" + strings.ReplaceAll(testAccountID, "-", "")
	targetRegistry := testregistry.New()
	targetServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v2.0/projects":
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2.0/projects/"+project:
			fmt.Fprint(w, `{"project_id":42,"metadata":{"public":"false"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2.0/quotas":
			fmt.Fprint(w, `[{"hard":{"storage":2147483648}}]`)
		default:
			targetRegistry.ServeHTTP(w, r)
		}
	}))
	defer targetServer.Close()
	cert, err := x509.ParseCertificate(targetServer.TLS.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	client, err := New(Config{BaseURL: "https://" + RegistryHost,
		Username: "admin", Password: "test-secret", CACertPEM: ca,
		DialContext: dialTestRegistry(targetServer.Listener.Addr().String())})
	if err != nil {
		t.Fatal(err)
	}
	client.http.Transport.(*http.Transport).TLSClientConfig.ServerName = "127.0.0.1"
	artifactID := "abcdefab-cdef-abcd-efab-cdefabcdefab"
	got, err := client.MirrorAccountImage(ctx, testAccountID, artifactID, source, resolver)
	if err != nil {
		t.Fatal(err)
	}
	want := RegistryHost + "/" + project + "/agent@" + source.PlatformDigest
	if got != want {
		t.Fatalf("mirrored reference %q, want %q", got, want)
	}
	bad := source
	bad.PlatformDigest = testDigest
	if _, err := client.MirrorAccountImage(ctx, testAccountID,
		"fedcbafe-dcba-fedc-bafe-dcbafedcbafe", bad, resolver); err == nil {
		t.Fatal("different source platform digest was mirrored")
	}
}
