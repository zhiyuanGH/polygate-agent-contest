package registry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
)

func digest(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func imageManifest(t *testing.T, architecture string) (string, []byte, string, []byte) {
	t.Helper()
	config, err := json.Marshal(map[string]any{"architecture": architecture, "os": "linux",
		"rootfs": map[string]any{"type": "layers", "diff_ids": []string{}},
		"config": map[string]any{"User": "65532"}})
	if err != nil {
		t.Fatal(err)
	}
	configDigest := digest(config)
	manifest, err := json.Marshal(map[string]any{"schemaVersion": 2,
		"mediaType": "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json",
			"digest": configDigest, "size": len(config)}, "layers": []any{}})
	if err != nil {
		t.Fatal(err)
	}
	return digest(manifest), manifest, configDigest, config
}

func testRegistry(t *testing.T, content map[string][]byte, requireAuth bool) *httptest.Server {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requireAuth && r.Header.Get("Authorization") != "Basic c3R1ZGVudDpzZWNyZXQ=" {
			w.Header().Set("WWW-Authenticate", `Basic realm="test-registry"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		body, ok := content[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if strings.Contains(r.URL.Path, "/manifests/") {
			w.Header().Set("Docker-Content-Digest", digest(body))
			if strings.Contains(string(body), "image.index.v1+json") {
				w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
			} else {
				w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			}
		} else {
			w.Header().Set("Content-Type", "application/octet-stream")
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	return server
}

func resolverFor(t *testing.T, server *httptest.Server) (*Resolver, string) {
	t.Helper()
	host := strings.TrimPrefix(server.URL, "https://")
	resolver, err := NewResolver(Policy{Registries: []Registry{{Host: host}}}, server.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	return resolver, host
}

func TestResolvePinnedSinglePlatformPrivateImage(t *testing.T) {
	manifestDigest, manifest, configDigest, config := imageManifest(t, "amd64")
	server := testRegistry(t, map[string][]byte{
		"/v2/test/app/manifests/v1":          manifest,
		"/v2/test/app/blobs/" + configDigest: config,
	}, true)
	resolver, host := resolverFor(t, server)
	ref := host + "/test/app:v1"
	if _, err := resolver.Resolve(context.Background(), ref, nil); err == nil {
		t.Fatal("anonymous access to the private image succeeded")
	}
	actual, err := resolver.Resolve(context.Background(), ref,
		authn.FromConfig(authn.AuthConfig{Username: "student", Password: "secret"}))
	if err != nil {
		t.Fatal(err)
	}
	if actual.PinnedReference != host+"/test/app@"+manifestDigest ||
		actual.ManifestDigest != manifestDigest || actual.PlatformDigest != manifestDigest ||
		actual.OS != "linux" || actual.Architecture != "amd64" || actual.ConfiguredUser != "65532" {
		t.Fatalf("unexpected resolved image: %+v", actual)
	}
}

func TestResolveIndexSelectsLinuxAMD64AndPinsIndex(t *testing.T) {
	armDigest, armManifest, armConfigDigest, armConfig := imageManifest(t, "arm64")
	amdDigest, amdManifest, amdConfigDigest, amdConfig := imageManifest(t, "amd64")
	index, err := json.Marshal(map[string]any{"schemaVersion": 2,
		"mediaType": "application/vnd.oci.image.index.v1+json",
		"manifests": []any{
			map[string]any{"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": armDigest,
				"size": len(armManifest), "platform": map[string]string{"os": "linux", "architecture": "arm64"}},
			map[string]any{"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": amdDigest,
				"size": len(amdManifest), "platform": map[string]string{"os": "linux", "architecture": "amd64"}},
		}})
	if err != nil {
		t.Fatal(err)
	}
	server := testRegistry(t, map[string][]byte{
		"/v2/test/app/manifests/v2":             index,
		"/v2/test/app/manifests/" + amdDigest:   amdManifest,
		"/v2/test/app/manifests/" + armDigest:   armManifest,
		"/v2/test/app/blobs/" + amdConfigDigest: amdConfig,
		"/v2/test/app/blobs/" + armConfigDigest: armConfig,
	}, false)
	resolver, host := resolverFor(t, server)
	actual, err := resolver.Resolve(context.Background(), host+"/test/app:v2", nil)
	if err != nil {
		t.Fatal(err)
	}
	if actual.ManifestDigest != digest(index) || actual.PlatformDigest != amdDigest ||
		actual.PinnedReference != host+"/test/app@"+digest(index) {
		t.Fatalf("index was not pinned with linux/amd64 selected: %+v", actual)
	}
}

func TestResolveRejectsUnsupportedPlatformAndUnapprovedRegistry(t *testing.T) {
	_, manifest, configDigest, config := imageManifest(t, "arm64")
	server := testRegistry(t, map[string][]byte{
		"/v2/test/app/manifests/v1":          manifest,
		"/v2/test/app/blobs/" + configDigest: config,
	}, false)
	resolver, host := resolverFor(t, server)
	if _, err := resolver.Resolve(context.Background(), host+"/test/app:v1", nil); err == nil ||
		!strings.Contains(err.Error(), "unsupported image platform") {
		t.Fatalf("arm64-only image should fail: %v", err)
	}
	if _, err := resolver.Resolve(context.Background(), "unapproved.example/test/app:v1", nil); err == nil ||
		!strings.Contains(err.Error(), "not approved") {
		t.Fatalf("unapproved registry should fail before a network request: %v", err)
	}
}

func TestResolveRejectsAmbiguousAndIncompleteReferences(t *testing.T) {
	resolver, err := NewResolver(Policy{Registries: []Registry{{Host: "registry.example"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"app:v1", "registry.example/app", "registry.example/app:v1@sha256:" + strings.Repeat("a", 64),
		"https://registry.example/app:v1", "registry.example/app:v1\n"} {
		if _, err := resolver.Resolve(context.Background(), ref, nil); err == nil {
			t.Errorf("accepted invalid reference %q", ref)
		}
	}
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestTokenChallengeCannotReachUnapprovedHost(t *testing.T) {
	base := transportFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "registry.example" {
			t.Fatalf("unapproved host reached base transport: %s", req.URL.Host)
		}
		return &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{
			"Www-Authenticate": []string{`Bearer realm="https://unapproved.example/token",service="registry.example",scope="repository:test/app:pull"`}},
			Body: io.NopCloser(strings.NewReader("unauthorized")), Request: req}, nil
	})
	resolver, err := NewResolver(Policy{Registries: []Registry{{Host: "registry.example"}}}, base)
	if err != nil {
		t.Fatal(err)
	}
	_, err = resolver.Resolve(context.Background(), "registry.example/test/app:v1", nil)
	if err == nil || !strings.Contains(err.Error(), "not approved") {
		t.Fatalf("token challenge escaped approved hosts: %v", err)
	}
}

func TestTokenHostsAreScopedToTheirRegistry(t *testing.T) {
	base := transportFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "registry-a.example" {
			t.Fatalf("another registry's token host was contacted: %s", req.URL.Host)
		}
		return &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{
			"Www-Authenticate": []string{`Bearer realm="https://auth-b.example/token",service="registry-a.example",scope="repository:test/app:pull"`}},
			Body: io.NopCloser(strings.NewReader("unauthorized")), Request: req}, nil
	})
	resolver, err := NewResolver(Policy{Registries: []Registry{
		{Host: "registry-a.example"}, {Host: "registry-b.example", AuthHosts: []string{"auth-b.example"}}}}, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.Resolve(context.Background(), "registry-a.example/test/app:v1", nil); err == nil ||
		!strings.Contains(err.Error(), "not approved") {
		t.Fatalf("cross-registry token host should be blocked: %v", err)
	}
}

func TestBlobMirrorDoesNotReceiveRegistryAuthorization(t *testing.T) {
	base := transportFunc(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Authorization") != "" {
			t.Fatal("registry credentials reached a blob mirror")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader("ok")), Request: req}, nil
	})
	transport := &restrictedTransport{base: base, registry: "registry.example",
		authHosts: map[string]bool{}, blobHosts: map[string]bool{"blob.example": true}}
	request, err := http.NewRequest(http.MethodGet, "https://blob.example/data", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer secret")
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if request.Header.Get("Authorization") != "Bearer secret" {
		t.Fatal("transport mutated its caller's request")
	}
}

func TestResolvePinsManifestContentDespiteIncorrectDigestHeader(t *testing.T) {
	manifestDigest, manifest, configDigest, config := imageManifest(t, "amd64")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path == "/v2/test/app/blobs/"+configDigest {
			_, _ = w.Write(config)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("0", 64))
		_, _ = w.Write(manifest)
	}))
	t.Cleanup(server.Close)
	resolver, host := resolverFor(t, server)
	image, err := resolver.Resolve(context.Background(), host+"/test/app:v1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if image.ManifestDigest != manifestDigest || image.PinnedReference != host+"/test/app@"+manifestDigest {
		t.Fatalf("header digest was trusted over manifest content: %+v", image)
	}
}

func TestRegistryMetadataBodyLimit(t *testing.T) {
	body := &boundedBody{ReadCloser: io.NopCloser(bytes.NewReader(bytes.Repeat([]byte{'x'}, maxResponseBytes+1)))}
	defer body.Close()
	if _, err := io.ReadAll(body); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("oversized metadata body should fail: %v", err)
	}
}
