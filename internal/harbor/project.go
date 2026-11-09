package harbor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

const AccountProjectStorageLimit = int64(2 << 30)

var accountIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// AccountProjectName gives each account an unambiguous Harbor namespace.
func AccountProjectName(accountID string) (string, error) {
	if !accountIDPattern.MatchString(accountID) {
		return "", errors.New("invalid account ID for Harbor project")
	}
	return "pg-u-" + strings.ReplaceAll(accountID, "-", ""), nil
}

func (c *Client) AccountProjectExists(ctx context.Context, accountID string) (bool, error) {
	project, err := AccountProjectName(accountID)
	if err != nil {
		return false, err
	}
	status, _, err := c.request(ctx, http.MethodGet, "/api/v2.0/projects/"+project, nil)
	if err != nil {
		return false, err
	}
	if status == http.StatusNotFound {
		return false, nil
	}
	if status != http.StatusOK {
		return false, fmt.Errorf("Harbor project lookup failed (status %d)", status)
	}
	return true, nil
}

// EnsureAccountProject creates or verifies a private, 2 GiB project. A
// conflicting project is accepted only after its privacy and hard quota have
// been read back from Harbor; project names are derived from account UUIDs.
func (c *Client) EnsureAccountProject(ctx context.Context, accountID string) (string, error) {
	project, err := AccountProjectName(accountID)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(map[string]any{
		"project_name":  project,
		"metadata":      map[string]string{"public": "false", "auto_scan": "false"},
		"storage_limit": AccountProjectStorageLimit,
	})
	if err != nil {
		return "", err
	}
	status, _, err := c.request(ctx, http.MethodPost, "/api/v2.0/projects", payload)
	if err != nil || (status != http.StatusCreated && status != http.StatusConflict) {
		return "", fmt.Errorf("Harbor project creation failed (status %d): %w", status, err)
	}
	status, body, err := c.request(ctx, http.MethodGet, "/api/v2.0/projects/"+project, nil)
	var state struct {
		ProjectID int64 `json:"project_id"`
		Metadata  struct {
			Public string `json:"public"`
		} `json:"metadata"`
	}
	if err != nil || status != http.StatusOK || json.Unmarshal(body, &state) != nil ||
		state.ProjectID <= 0 || state.Metadata.Public != "false" {
		return "", errors.New("Harbor account project is missing or not private")
	}
	status, body, err = c.request(ctx, http.MethodGet,
		fmt.Sprintf("/api/v2.0/quotas?reference=project&reference_id=%d", state.ProjectID), nil)
	var quotas []struct {
		Hard map[string]int64 `json:"hard"`
	}
	if err != nil || status != http.StatusOK || json.Unmarshal(body, &quotas) != nil ||
		len(quotas) != 1 || quotas[0].Hard["storage"] != AccountProjectStorageLimit {
		return "", errors.New("Harbor account project hard quota differs from 2 GiB")
	}
	return project, nil
}

// DeleteAccountProject is used when an account is removed or a disposable
// smoke account is cleaned up. Callers must first ensure no live app depends
// on artifacts in the project.
func (c *Client) DeleteAccountProject(ctx context.Context, accountID string) error {
	project, err := AccountProjectName(accountID)
	if err != nil {
		return err
	}
	status, _, err := c.request(ctx, http.MethodDelete, "/api/v2.0/projects/"+project, nil)
	if err != nil || (status != http.StatusOK && status != http.StatusAccepted &&
		status != http.StatusNoContent && status != http.StatusNotFound) {
		return fmt.Errorf("Harbor account project deletion failed (status %d): %w", status, err)
	}
	return nil
}

// DeleteAccountRepository removes a named repository before deleting its
// disposable account project. It is intentionally separate from project
// deletion so active student artifacts cannot be purged by a project call.
func (c *Client) DeleteAccountRepository(ctx context.Context, accountID, repository string) error {
	project, err := AccountProjectName(accountID)
	if err != nil || !simpleName.MatchString(repository) {
		return errors.New("invalid account repository")
	}
	status, _, err := c.request(ctx, http.MethodDelete,
		"/api/v2.0/projects/"+project+"/repositories/"+repository, nil)
	if err != nil || (status != http.StatusOK && status != http.StatusAccepted &&
		status != http.StatusNoContent && status != http.StatusNotFound) {
		return fmt.Errorf("Harbor account repository deletion failed (status %d): %w", status, err)
	}
	return nil
}

// DeleteAccountArtifact removes one digest from an account repository. The
// caller must verify that no other database artifact or app still uses it.
func (c *Client) DeleteAccountArtifact(ctx context.Context, accountID, repository, digest string) error {
	project, err := AccountProjectName(accountID)
	if err != nil {
		return err
	}
	path, err := artifactPath(project, repository, digest)
	if err != nil {
		return err
	}
	status, _, err := c.request(ctx, http.MethodDelete, path, nil)
	if err != nil || (status != http.StatusOK && status != http.StatusAccepted &&
		status != http.StatusNoContent && status != http.StatusNotFound) {
		return fmt.Errorf("Harbor account artifact deletion failed (status %d): %w", status, err)
	}
	return nil
}
