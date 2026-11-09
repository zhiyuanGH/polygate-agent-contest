package harbor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"polygate/internal/registry"
)

const RegistryHost = "harbor.polygate-registry.svc.cluster.local"

var ErrMirrorDigestMismatch = errors.New("Harbor mirror digest differs from inspected digest")

type harborOnlyTransport struct {
	base http.RoundTripper
	host string
}

func (t *harborOnlyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" || !strings.EqualFold(req.URL.Host, t.host) {
		return nil, errors.New("Harbor registry request left the approved HTTPS host")
	}
	return t.base.RoundTrip(req)
}

// MirrorAccountImage copies only the inspected linux/amd64 child image into
// the account's private project. It returns an immutable Harbor digest; a
// changed source manifest or a different uploaded digest is never accepted.
func (c *Client) MirrorAccountImage(ctx context.Context, accountID, artifactID string,
	source registry.Image, resolver *registry.Resolver) (string, error) {
	if !accountIDPattern.MatchString(artifactID) || resolver == nil {
		return "", errors.New("invalid image artifact or source resolver")
	}
	project, err := c.EnsureAccountProject(ctx, accountID)
	if err != nil {
		return "", err
	}
	var sourceAuth authn.Authenticator
	if source.RegistryHost == RegistryHost {
		// Built images are already private in this account's project. Only
		// that exact namespace may use the Harbor credential for fetching.
		if !strings.HasPrefix(source.PinnedReference, RegistryHost+"/"+project+"/agent@") {
			return "", errors.New("built image is outside its account project")
		}
		sourceAuth = authn.FromConfig(authn.AuthConfig{Username: c.username, Password: c.password})
	}
	img, err := resolver.FetchAMD64(ctx, source, sourceAuth)
	if err != nil {
		return "", err
	}
	target, err := name.NewTag(RegistryHost+"/"+project+"/agent:a"+
		strings.ReplaceAll(artifactID, "-", ""), name.StrictValidation)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(c.base)
	if err != nil || u.Hostname() != RegistryHost {
		return "", errors.New("Harbor client is not bound to the internal registry host")
	}
	transport := &harborOnlyTransport{base: c.http.Transport, host: u.Host}
	auth := authn.FromConfig(authn.AuthConfig{Username: c.username, Password: c.password})
	options := []remote.Option{remote.WithContext(ctx), remote.WithAuth(auth),
		remote.WithTransport(transport)}
	if err := remote.Write(target, img, options...); err != nil {
		return "", fmt.Errorf("Harbor image copy failed: %w", err)
	}
	uploaded, err := remote.Get(target, options...)
	if err != nil || uploaded.Digest.String() != source.PlatformDigest {
		return "", ErrMirrorDigestMismatch
	}
	return target.Context().Name() + "@" + uploaded.Digest.String(), nil
}
