package registry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

var ErrSourcePolicy = errors.New("image source violates registry policy")
var ErrSourceChanged = errors.New("source image digest or platform changed")
var ErrImageTooLarge = errors.New("source image exceeds the copy limit")

// FetchAMD64 returns a lazy image whose blobs can only be read from the
// operator-approved source hosts. The selected child manifest must match the
// platform digest recorded at inspection time. Blob traffic and manifest
// sizes are bounded before the image is given to a copy operation.
func (r *Resolver) FetchAMD64(ctx context.Context, source Image,
	credentials authn.Authenticator) (v1.Image, error) {
	ref, err := name.NewDigest(source.PinnedReference, name.StrictValidation)
	entry, approved := r.registries[source.RegistryHost]
	if err != nil || !approved || ref.Context().RegistryStr() != source.RegistryHost ||
		!strings.HasPrefix(ref.DigestStr(), "sha256:") ||
		!strings.HasPrefix(source.PlatformDigest, "sha256:") {
		return nil, ErrSourcePolicy
	}
	if credentials == nil {
		credentials = authn.Anonymous
	}
	budget := &byteBudget{limit: MaxImageTransferBytes, remaining: MaxImageTransferBytes}
	transport := &restrictedTransport{base: r.base, registry: entry.Host,
		authHosts: make(map[string]bool), blobHosts: make(map[string]bool), blobBudget: budget}
	for _, host := range entry.AuthHosts {
		transport.authHosts[host] = true
	}
	for _, host := range entry.BlobHosts {
		transport.blobHosts[host] = true
	}
	img, err := remote.Image(ref, remote.WithContext(ctx), remote.WithAuth(credentials),
		remote.WithPlatform(v1.Platform{OS: "linux", Architecture: "amd64"}),
		remote.WithTransport(transport))
	if err != nil {
		return nil, fmt.Errorf("source image manifest unavailable: %w", err)
	}
	digest, err := img.Digest()
	if err != nil || digest.String() != source.PlatformDigest {
		return nil, ErrSourceChanged
	}
	config, err := img.ConfigFile()
	if err != nil || config.OS != "linux" || config.Architecture != "amd64" ||
		len(config.Config.User) > 256 {
		return nil, ErrSourcePolicy
	}
	manifest, err := img.Manifest()
	if err != nil || manifest.Config.Size <= 0 || manifest.Config.Size > maxResponseBytes ||
		len(manifest.Layers) > 128 {
		return nil, ErrImageTooLarge
	}
	total := manifest.Config.Size
	for _, layer := range manifest.Layers {
		if layer.Size < 0 || layer.Size > MaxImageTransferBytes-total {
			return nil, ErrImageTooLarge
		}
		total += layer.Size
	}
	return img, nil
}

// Only copy traffic may use a larger blob limit; Resolve retains its 8 MiB
// metadata-only bound and never downloads image layers.
var _ http.RoundTripper = (*restrictedTransport)(nil)
