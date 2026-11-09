package harbor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var managedPullRobotName = regexp.MustCompile(`(?:^|\+)pull-[0-9a-f]{16}$`)

type RobotCredential struct {
	ID     int64
	Name   string
	Secret string
}

// CreatePullRobot creates a one-day project robot with repository
// pull permission only. The caller must keep the returned secret out of logs.
func (c *Client) CreatePullRobot(ctx context.Context, project, name string,
	durationDays int) (RobotCredential, error) {
	if !simpleName.MatchString(project) || !simpleName.MatchString(name) ||
		durationDays != 1 {
		return RobotCredential{}, errors.New("invalid pull robot request")
	}
	payload, err := json.Marshal(map[string]any{
		"name": name, "level": "project", "duration": durationDays,
		"description": "PolyGate temporary pull-only credential",
		"permissions": []any{map[string]any{
			"kind": "project", "namespace": project,
			"access": []any{map[string]string{"resource": "repository", "action": "pull"}},
		}},
	})
	if err != nil {
		return RobotCredential{}, err
	}
	status, body, err := c.request(ctx, http.MethodPost, "/api/v2.0/robots", payload)
	if err != nil || status != http.StatusCreated {
		return RobotCredential{}, fmt.Errorf("Harbor pull robot creation failed (status %d)", status)
	}
	var response struct {
		ID     int64  `json:"id"`
		Name   string `json:"name"`
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(body, &response); err != nil ||
		response.ID <= 0 || response.Name == "" || response.Secret == "" {
		if response.ID > 0 {
			_ = c.DeleteRobot(ctx, response.ID)
		}
		return RobotCredential{}, errors.New("Harbor did not return the robot credential")
	}
	status, body, err = c.request(ctx, http.MethodGet,
		fmt.Sprintf("/api/v2.0/robots/%d", response.ID), nil)
	var metadata struct {
		Name        string `json:"name"`
		Level       string `json:"level"`
		Permissions []struct {
			Kind      string `json:"kind"`
			Namespace string `json:"namespace"`
			Access    []struct {
				Resource string `json:"resource"`
				Action   string `json:"action"`
			} `json:"access"`
		} `json:"permissions"`
	}
	if err != nil || status != http.StatusOK || json.Unmarshal(body, &metadata) != nil ||
		metadata.Name != response.Name || metadata.Level != "project" ||
		len(metadata.Permissions) != 1 ||
		metadata.Permissions[0].Kind != "project" ||
		metadata.Permissions[0].Namespace != project ||
		len(metadata.Permissions[0].Access) != 1 ||
		metadata.Permissions[0].Access[0].Resource != "repository" ||
		metadata.Permissions[0].Access[0].Action != "pull" {
		_ = c.DeleteRobot(ctx, response.ID)
		return RobotCredential{}, fmt.Errorf("Harbor returned an unexpected pull robot: id=%d status=%d level=%q permissions=%v",
			response.ID, status, metadata.Level, metadata.Permissions)
	}
	return RobotCredential{ID: response.ID, Name: response.Name, Secret: response.Secret}, nil
}

func (c *Client) DeleteRobot(ctx context.Context, id int64) error {
	if id <= 0 {
		return errors.New("invalid Harbor robot ID")
	}
	status, _, err := c.request(ctx, http.MethodDelete, fmt.Sprintf("/api/v2.0/robots/%d", id), nil)
	if err != nil || (status != http.StatusOK && status != http.StatusNotFound) {
		return fmt.Errorf("Harbor robot deletion failed (status %d)", status)
	}
	return nil
}

func (c *Client) RobotExists(ctx context.Context, id int64) (bool, error) {
	if id <= 0 {
		return false, errors.New("invalid Harbor robot ID")
	}
	status, _, err := c.request(ctx, http.MethodGet,
		fmt.Sprintf("/api/v2.0/robots/%d", id), nil)
	if err != nil {
		return false, err
	}
	if status == http.StatusNotFound {
		return false, nil
	}
	if status != http.StatusOK {
		return false, fmt.Errorf("Harbor robot lookup failed (status %d)", status)
	}
	return true, nil
}

// ListPullRobots returns only PolyGate's project-scoped, pull-only robots.
// It lets the controller revoke a robot left behind if it crashes after
// Harbor creation but before the Kubernetes Secret is written.
func (c *Client) ListPullRobots(ctx context.Context, project string) ([]int64, error) {
	if !simpleName.MatchString(project) {
		return nil, errors.New("invalid Harbor project name")
	}
	status, body, err := c.request(ctx, http.MethodGet, "/api/v2.0/projects/"+project, nil)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, nil
	}
	var projectState struct {
		ID int64 `json:"project_id"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &projectState) != nil || projectState.ID <= 0 {
		return nil, fmt.Errorf("Harbor project lookup failed (status %d)", status)
	}
	const pageSize = 100
	var ids []int64
	for page := 1; page <= 20; page++ {
		query := url.QueryEscape("Level=project,ProjectID=" + strconv.FormatInt(projectState.ID, 10))
		path := fmt.Sprintf("/api/v2.0/robots?q=%s&page=%d&page_size=%d", query, page, pageSize)
		status, body, err = c.request(ctx, http.MethodGet, path, nil)
		var robots []struct {
			ID          int64  `json:"id"`
			Name        string `json:"name"`
			Level       string `json:"level"`
			Permissions []struct {
				Kind      string `json:"kind"`
				Namespace string `json:"namespace"`
				Access    []struct {
					Resource string `json:"resource"`
					Action   string `json:"action"`
				} `json:"access"`
			} `json:"permissions"`
		}
		if err != nil || status != http.StatusOK || json.Unmarshal(body, &robots) != nil {
			return nil, fmt.Errorf("Harbor project robot listing failed (status %d)", status)
		}
		for _, robot := range robots {
			if robot.ID <= 0 || robot.Level != "project" || len(robot.Permissions) != 1 {
				return nil, errors.New("Harbor returned invalid project robot metadata")
			}
			permission := robot.Permissions[0]
			if permission.Kind != "project" || permission.Namespace != project {
				return nil, errors.New("Harbor returned a robot from another project")
			}
			if !managedPullRobotName.MatchString(robot.Name) {
				continue
			}
			if len(permission.Access) != 1 || permission.Access[0].Resource != "repository" ||
				permission.Access[0].Action != "pull" || !strings.Contains(robot.Name, project+"+") {
				return nil, errors.New("managed Harbor robot has unexpected permissions or name")
			}
			ids = append(ids, robot.ID)
		}
		if len(robots) < pageSize {
			return ids, nil
		}
	}
	return nil, errors.New("Harbor project robot listing exceeded 20 pages")
}
