package harbor

import (
	"context"
	"errors"
	"net/url"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"polygate/internal/registry"
)

// BuildTag is a fixed, account-scoped destination. The build guest does not
// choose a registry, project, repository or tag from Dockerfile contents.
func BuildTag(accountID, taskID string) (string, error) {
	project, err := AccountProjectName(accountID)
	if err != nil || !accountIDPattern.MatchString(taskID) {
		return "", errors.New("invalid build image identity")
	}
	return RegistryHost + "/" + project + "/agent:b" + strings.ReplaceAll(taskID, "-", ""), nil
}

type BuildVerifier struct {
	Harbor   *Client
	Resolver *registry.Resolver
}

// EnsureBuildProject creates or checks the private account namespace before
// the isolated guest starts. The host-side publisher needs an existing target.
func (v BuildVerifier) EnsureBuildProject(ctx context.Context, accountID string) error {
	if v.Harbor == nil {
		return errors.New("Harbor client is absent")
	}
	_, err := v.Harbor.EnsureAccountProject(ctx, accountID)
	return err
}

// VerifyBuiltImage checks the task-specific tag in Harbor, then resolves the
// immutable digest and linux/amd64 configuration using the registry policy.
// A guest's claimed digest alone never creates an image artifact.
func (v BuildVerifier) VerifyBuiltImage(ctx context.Context, accountID, taskID, expectedDigest string) (registry.Image, error) {
	var empty registry.Image
	if v.Harbor == nil || v.Resolver == nil || !digestPattern.MatchString(expectedDigest) {
		return empty, errors.New("invalid build verifier configuration or digest")
	}
	tag, err := BuildTag(accountID, taskID)
	if err != nil {
		return empty, err
	}
	if _, err = v.Harbor.EnsureAccountProject(ctx, accountID); err != nil {
		return empty, err
	}
	u, err := url.Parse(v.Harbor.base)
	if err != nil || u.Hostname() != RegistryHost {
		return empty, errors.New("Harbor client is not bound to the internal registry host")
	}
	ref, err := name.NewTag(tag, name.StrictValidation)
	if err != nil {
		return empty, err
	}
	transport := &harborOnlyTransport{base: v.Harbor.http.Transport, host: u.Host}
	auth := authn.FromConfig(authn.AuthConfig{Username: v.Harbor.username, Password: v.Harbor.password})
	desc, err := remote.Get(ref, remote.WithContext(ctx), remote.WithAuth(auth), remote.WithTransport(transport))
	if err != nil || desc.Digest.String() != expectedDigest {
		return empty, errors.New("build tag is missing or points to a different digest")
	}
	pinned := ref.Context().Name() + "@" + expectedDigest
	image, err := v.Resolver.Resolve(ctx, pinned, auth)
	if err != nil || image.RegistryHost != RegistryHost || image.ManifestDigest != expectedDigest ||
		image.PlatformDigest != expectedDigest {
		return empty, errors.New("built image digest, platform or registry is invalid")
	}
	return image, nil
}
