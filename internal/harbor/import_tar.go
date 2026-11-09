package harbor

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"polygate/internal/registry"
)

var ErrInvalidImageTar = errors.New("invalid docker-save image archive")

// ImportImageTar accepts one docker-save image, publishes it to a fixed
// account-scoped Harbor tag, and returns metadata for the existing scan flow.
func (c *Client) ImportImageTar(ctx context.Context, accountID, uploadID, path string) (registry.Image, error) {
	var empty registry.Image
	if !accountIDPattern.MatchString(uploadID) || path == "" {
		return empty, errors.New("invalid image upload identity")
	}
	img, err := tarball.ImageFromPath(path, nil)
	if err != nil {
		return empty, fmt.Errorf("%w: %v", ErrInvalidImageTar, err)
	}
	config, err := img.ConfigFile()
	if err != nil || config.OS != "linux" || config.Architecture != "amd64" {
		return empty, fmt.Errorf("%w: image must be linux/amd64", ErrInvalidImageTar)
	}
	digest, err := img.Digest()
	if err != nil {
		return empty, fmt.Errorf("%w: image manifest digest is unavailable", ErrInvalidImageTar)
	}
	project, err := c.EnsureAccountProject(ctx, accountID)
	if err != nil {
		return empty, err
	}
	tag, err := name.NewTag(RegistryHost+"/"+project+"/agent:u"+
		strings.ReplaceAll(uploadID, "-", ""), name.StrictValidation)
	if err != nil {
		return empty, err
	}
	u, err := url.Parse(c.base)
	if err != nil || u.Hostname() != RegistryHost {
		return empty, errors.New("Harbor client is not bound to the internal registry host")
	}
	transport := &harborOnlyTransport{base: c.http.Transport, host: u.Host}
	auth := authn.FromConfig(authn.AuthConfig{Username: c.username, Password: c.password})
	options := []remote.Option{remote.WithContext(ctx), remote.WithAuth(auth),
		remote.WithTransport(transport), remote.WithJobs(1)}
	if err = remote.Write(tag, img, options...); err != nil {
		return empty, fmt.Errorf("Harbor image upload failed: %w", err)
	}
	remoteImage, err := remote.Get(tag, options...)
	if err != nil || remoteImage.Digest != digest {
		return empty, errors.New("uploaded image digest verification failed")
	}
	pinned := tag.Context().Name() + "@" + digest.String()
	return registry.Image{SubmittedReference: pinned, PinnedReference: pinned,
		ManifestDigest: digest.String(), PlatformDigest: digest.String(),
		RegistryHost: RegistryHost, OS: config.OS, Architecture: config.Architecture,
		ConfiguredUser: config.Config.User}, nil
}
