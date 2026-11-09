package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

const maxResponseBytes = 8 << 20
const MaxImageTransferBytes int64 = (2 << 30) - (64 << 20)

// Policy is set by the operator. Student input cannot add registry, token or
// blob hosts. Hosts are scoped to one registry so credentials cannot be sent
// to an authentication service approved for a different registry.
type Policy struct {
	Registries []Registry `json:"registries"`
}

type Registry struct {
	Host      string   `json:"host"`
	AuthHosts []string `json:"authHosts"`
	BlobHosts []string `json:"blobHosts"`
}

type Resolver struct {
	registries map[string]Registry
	base       http.RoundTripper
}

type Image struct {
	SubmittedReference string
	PinnedReference    string
	ManifestDigest     string
	PlatformDigest     string
	RegistryHost       string
	OS                 string
	Architecture       string
	ConfiguredUser     string
}

func NewResolver(policy Policy, base http.RoundTripper) (*Resolver, error) {
	if len(policy.Registries) == 0 {
		return nil, errors.New("at least one operator-approved registry is required")
	}
	registries := make(map[string]Registry)
	for _, entry := range policy.Registries {
		if !validHost(entry.Host) {
			return nil, fmt.Errorf("invalid registry host %q", entry.Host)
		}
		if _, exists := registries[entry.Host]; exists {
			return nil, fmt.Errorf("duplicate registry host %q", entry.Host)
		}
		for _, host := range append(append([]string{}, entry.AuthHosts...), entry.BlobHosts...) {
			if !validHost(host) {
				return nil, fmt.Errorf("invalid registry network host %q", host)
			}
		}
		registries[entry.Host] = entry
	}
	if base == nil {
		base = http.DefaultTransport
	}
	return &Resolver{registries: registries, base: base}, nil
}

func validHost(host string) bool {
	if host == "" || host != strings.ToLower(host) || strings.ContainsAny(host, "/@?#\\ \t\r\n") {
		return false
	}
	// Disallow wildcard entries. An operator must list exact registry and token hosts.
	return !strings.Contains(host, "*") && !strings.HasPrefix(host, ".") && !strings.HasSuffix(host, ".")
}

type restrictedTransport struct {
	base       http.RoundTripper
	registry   string
	authHosts  map[string]bool
	blobHosts  map[string]bool
	blobBudget *byteBudget
}

func (t *restrictedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	host := strings.ToLower(req.URL.Host)
	if req.URL.Scheme != "https" ||
		(host != t.registry && !t.authHosts[host] && !t.blobHosts[host]) {
		return nil, fmt.Errorf("registry request host is not approved: %s", req.URL.Host)
	}
	if t.blobHosts[host] && host != t.registry && !t.authHosts[host] && req.Header.Get("Authorization") != "" {
		// Blob mirrors use signed URLs. Do not forward registry credentials there.
		req = req.Clone(req.Context())
		req.Header.Del("Authorization")
	}
	response, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	limit := int64(maxResponseBytes)
	blob := t.blobBudget != nil &&
		(strings.Contains(req.URL.Path, "/blobs/") || t.blobHosts[host])
	if blob {
		limit = t.blobBudget.limit
	}
	if response.ContentLength > limit {
		response.Body.Close()
		return nil, errors.New("registry response exceeds the permitted size limit")
	}
	if blob {
		response.Body = &budgetedBody{ReadCloser: response.Body, budget: t.blobBudget}
	}
	response.Body = &boundedBody{ReadCloser: response.Body, limit: limit}
	return response, nil
}

type boundedBody struct {
	io.ReadCloser
	read  int64
	limit int64
}

func (b *boundedBody) Read(buffer []byte) (int, error) {
	limit := b.limit
	if limit == 0 {
		limit = maxResponseBytes
	}
	if b.read == limit {
		var probe [1]byte
		n, err := b.ReadCloser.Read(probe[:])
		if n > 0 {
			return 0, errors.New("registry response exceeds the permitted size limit")
		}
		return 0, err
	}
	if int64(len(buffer)) > limit-b.read {
		buffer = buffer[:limit-b.read]
	}
	n, err := b.ReadCloser.Read(buffer)
	b.read += int64(n)
	return n, err
}

// byteBudget caps aggregate source blob traffic, including retries and
// multiple layers. The lock keeps concurrent readers within one hard bound.
type byteBudget struct {
	mu        sync.Mutex
	limit     int64
	remaining int64
}

type budgetedBody struct {
	io.ReadCloser
	budget *byteBudget
}

func (b *budgetedBody) Read(buffer []byte) (int, error) {
	b.budget.mu.Lock()
	defer b.budget.mu.Unlock()
	if b.budget.remaining == 0 {
		var probe [1]byte
		n, err := b.ReadCloser.Read(probe[:])
		if n > 0 {
			return 0, errors.New("image transfer exceeds the byte budget")
		}
		return 0, err
	}
	if int64(len(buffer)) > b.budget.remaining {
		buffer = buffer[:b.budget.remaining]
	}
	n, err := b.ReadCloser.Read(buffer)
	b.budget.remaining -= int64(n)
	return n, err
}

// Resolve reads only manifests and configuration, checks linux/amd64, and
// returns an immutable top-level manifest or index digest for Kubernetes.
// Credentials are passed per call; no Docker config or ambient keychain is read.
func (r *Resolver) Resolve(ctx context.Context, submitted string, credentials authn.Authenticator) (Image, error) {
	var result Image
	if len(submitted) == 0 || len(submitted) > 512 || strings.TrimSpace(submitted) != submitted ||
		strings.ContainsAny(submitted, " \t\r\n") {
		return result, errors.New("invalid image reference")
	}
	ref, err := name.ParseReference(submitted, name.StrictValidation)
	if err != nil {
		return result, fmt.Errorf("image reference must include an explicit registry and tag or digest: %w", err)
	}
	entry, approved := r.registries[ref.Context().RegistryStr()]
	if !approved {
		return result, errors.New("image registry is not approved")
	}
	if _, ok := ref.(name.Digest); ok {
		if colon := strings.LastIndex(submitted[:strings.IndexByte(submitted, '@')], ":"); colon > strings.LastIndexByte(submitted[:strings.IndexByte(submitted, '@')], '/') {
			return result, errors.New("image reference cannot contain both a tag and digest")
		}
	}
	if credentials == nil {
		credentials = authn.Anonymous
	}
	transport := &restrictedTransport{base: r.base, registry: entry.Host,
		authHosts: make(map[string]bool), blobHosts: make(map[string]bool)}
	for _, host := range entry.AuthHosts {
		transport.authHosts[host] = true
	}
	for _, host := range entry.BlobHosts {
		transport.blobHosts[host] = true
	}
	desc, err := remote.Get(ref, remote.WithContext(ctx), remote.WithAuth(credentials),
		remote.WithPlatform(v1.Platform{OS: "linux", Architecture: "amd64"}), remote.WithTransport(transport))
	if err != nil {
		return result, fmt.Errorf("registry manifest lookup failed: %w", err)
	}
	if desc.Digest.Algorithm != "sha256" {
		return result, errors.New("only sha256 image digests are supported")
	}
	actualDigest := sha256.Sum256(desc.Manifest)
	if desc.Digest.String() != "sha256:"+hex.EncodeToString(actualDigest[:]) {
		return result, errors.New("registry manifest digest does not match its content")
	}
	switch desc.MediaType {
	case types.OCIManifestSchema1, types.DockerManifestSchema2, types.OCIImageIndex, types.DockerManifestList:
	default:
		return result, fmt.Errorf("unsupported image manifest type %s", desc.MediaType)
	}
	image, err := desc.Image()
	if err != nil {
		return result, fmt.Errorf("linux/amd64 image is unavailable: %w", err)
	}
	config, err := image.ConfigFile()
	if err != nil {
		return result, fmt.Errorf("image configuration lookup failed: %w", err)
	}
	if config.OS != "linux" || config.Architecture != "amd64" {
		return result, fmt.Errorf("unsupported image platform %s/%s", config.OS, config.Architecture)
	}
	if len(config.Config.User) > 256 {
		return result, errors.New("image configured user is too long")
	}
	platformDigest, err := image.Digest()
	if err != nil {
		return result, fmt.Errorf("platform manifest digest unavailable: %w", err)
	}
	if platformDigest.Algorithm != "sha256" {
		return result, errors.New("only sha256 image digests are supported")
	}
	return Image{SubmittedReference: submitted,
		PinnedReference: ref.Context().Name() + "@" + desc.Digest.String(),
		ManifestDigest:  desc.Digest.String(), PlatformDigest: platformDigest.String(),
		RegistryHost: entry.Host,
		OS:           config.OS, Architecture: config.Architecture, ConfiguredUser: config.Config.User}, nil
}
