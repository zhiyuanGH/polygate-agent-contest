package harbor

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
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

func TestVerifyBuiltImageReadsAccountTagAndDigest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	project, _ := AccountProjectName(testAccountID)
	backing := testregistry.New()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v2.0/projects":
			w.WriteHeader(http.StatusConflict)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2.0/projects/"+project:
			fmt.Fprint(w, `{"project_id":42,"metadata":{"public":"false"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2.0/quotas":
			fmt.Fprint(w, `[{"hard":{"storage":2147483648}}]`)
		default:
			backing.ServeHTTP(w, r)
		}
	}))
	defer server.Close()
	cert, err := x509.ParseCertificate(server.TLS.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	client, err := New(Config{BaseURL: "https://" + RegistryHost, Username: "admin",
		Password: "test-secret", CACertPEM: ca, DialContext: dialTestRegistry(server.Listener.Addr().String())})
	if err != nil {
		t.Fatal(err)
	}
	client.http.Transport.(*http.Transport).TLSClientConfig.ServerName = "127.0.0.1"
	resolver, err := registry.NewResolver(registry.Policy{Registries: []registry.Registry{{Host: RegistryHost}}}, client.http.Transport)
	if err != nil {
		t.Fatal(err)
	}
	img, err := random.Image(1024, 1)
	if err != nil {
		t.Fatal(err)
	}
	config, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	config.OS, config.Architecture = "linux", "amd64"
	img, err = mutate.ConfigFile(img, config)
	if err != nil {
		t.Fatal(err)
	}
	tagText, err := BuildTag(testAccountID, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	tag, err := name.NewTag(tagText, name.StrictValidation)
	if err != nil {
		t.Fatal(err)
	}
	if err = remote.Write(tag, img, remote.WithContext(ctx), remote.WithTransport(client.http.Transport)); err != nil {
		t.Fatal(err)
	}
	digest, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	verifier := BuildVerifier{Harbor: client, Resolver: resolver}
	verified, err := verifier.VerifyBuiltImage(ctx, testAccountID,
		"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", digest.String())
	if err != nil || verified.PinnedReference != RegistryHost+"/"+project+"/agent@"+digest.String() {
		t.Fatalf("verified image: %+v %v", verified, err)
	}
	mirrored, err := client.MirrorAccountImage(ctx, testAccountID,
		"eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", verified, resolver)
	if err != nil || mirrored != verified.PinnedReference {
		t.Fatalf("built image scan mirror: %q %v", mirrored, err)
	}
	foreign := verified
	foreign.PinnedReference = RegistryHost + "/pg-u-other/agent@" + digest.String()
	if _, err = client.MirrorAccountImage(ctx, testAccountID,
		"ffffffff-ffff-4fff-8fff-ffffffffffff", foreign, resolver); err == nil {
		t.Fatal("cross-account private source was mirrored")
	}
	if _, err = verifier.VerifyBuiltImage(ctx, testAccountID,
		"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "sha256:"+strings.Repeat("f", 64)); err == nil {
		t.Fatal("false digest accepted")
	}
}
