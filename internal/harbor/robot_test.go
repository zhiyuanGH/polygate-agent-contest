package harbor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestPullRobotHasOnlyProjectPullPermission(t *testing.T) {
	var created, deleted bool
	client, server := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v2.0/robots":
			var body struct {
				Level       string `json:"level"`
				Duration    int    `json:"duration"`
				Permissions []struct {
					Namespace string `json:"namespace"`
					Access    []struct {
						Resource string `json:"resource"`
						Action   string `json:"action"`
					} `json:"access"`
				} `json:"permissions"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.Level != "project" || body.Duration != 1 ||
				len(body.Permissions) != 1 || body.Permissions[0].Namespace != "test-project" ||
				len(body.Permissions[0].Access) != 1 ||
				body.Permissions[0].Access[0].Resource != "repository" ||
				body.Permissions[0].Access[0].Action != "pull" {
				t.Errorf("robot permissions were broader than project pull")
			}
			created = true
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":42,"name":"robot$test-project+pull","secret":"test-secret","level":"project","permissions":[{"kind":"project","namespace":"test-project","access":[{"resource":"repository","action":"pull"}]}]}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v2.0/robots/42":
			deleted = true
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2.0/robots/42":
			fmt.Fprint(w, `{"id":42,"name":"robot$test-project+pull","level":"project","permissions":[{"kind":"project","namespace":"test-project","access":[{"resource":"repository","action":"pull"}]}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	defer server.Close()
	robot, err := client.CreatePullRobot(context.Background(), "test-project", "pull", 1)
	if err != nil || robot.ID != 42 || robot.Secret != "test-secret" {
		t.Fatalf("robot creation: %+v %v", robot, err)
	}
	if err := client.DeleteRobot(context.Background(), robot.ID); err != nil {
		t.Fatal(err)
	}
	if !created || !deleted {
		t.Fatal("robot creation or cleanup was not exercised")
	}
	for _, request := range []struct {
		project, name string
		days          int
	}{{"../other", "pull", 1}, {"test-project", "pull", -1},
		{"test-project", "bad/name", 1}, {"test-project", "pull", 365}} {
		if _, err := client.CreatePullRobot(context.Background(), request.project,
			request.name, request.days); err == nil {
			t.Fatal("unsafe robot request accepted")
		}
	}
	if _, err := client.CreatePullRobot(context.Background(), "test-project",
		strings.Repeat("a", 129), 1); err == nil {
		t.Fatal("oversized robot name accepted")
	}
}
