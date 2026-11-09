package harbor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

const testAccountID = "12345678-1234-1234-1234-123456789abc"

func TestEnsureAccountProjectPrivateAndQuota(t *testing.T) {
	project, err := AccountProjectName(testAccountID)
	if err != nil || project != "pg-u-12345678123412341234123456789abc" {
		t.Fatalf("project name: %q %v", project, err)
	}
	for _, created := range []bool{true, false} {
		t.Run(fmt.Sprint(created), func(t *testing.T) {
			posts := 0
			client, server := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/api/v2.0/projects":
					posts++
					var payload struct {
						Name         string            `json:"project_name"`
						Metadata     map[string]string `json:"metadata"`
						StorageLimit int64             `json:"storage_limit"`
					}
					if json.NewDecoder(r.Body).Decode(&payload) != nil || payload.Name != project ||
						payload.Metadata["public"] != "false" ||
						payload.StorageLimit != AccountProjectStorageLimit {
						t.Errorf("unsafe project creation request: %+v", payload)
					}
					if created {
						w.WriteHeader(http.StatusCreated)
					} else {
						w.WriteHeader(http.StatusConflict)
					}
				case r.Method == http.MethodGet && r.URL.Path == "/api/v2.0/projects/"+project:
					fmt.Fprint(w, `{"project_id":42,"metadata":{"public":"false"}}`)
				case r.Method == http.MethodGet && r.URL.Path == "/api/v2.0/quotas" &&
					r.URL.Query().Get("reference") == "project" && r.URL.Query().Get("reference_id") == "42":
					fmt.Fprint(w, `[{"hard":{"storage":2147483648}}]`)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			})
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			got, err := client.EnsureAccountProject(ctx, testAccountID)
			if err != nil || got != project || posts != 1 {
				t.Fatalf("ensure project: %q posts=%d err=%v", got, posts, err)
			}
		})
	}
}

func TestEnsureAccountProjectFailsClosed(t *testing.T) {
	if _, err := AccountProjectName("../other"); err == nil {
		t.Fatal("unsafe account ID accepted")
	}
	for _, response := range []string{
		`{"project_id":42,"metadata":{"public":"true"}}`,
		`{"project_id":0,"metadata":{"public":"false"}}`,
	} {
		client, server := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				w.WriteHeader(http.StatusConflict)
				return
			}
			if strings.Contains(r.URL.Path, "/quotas") {
				fmt.Fprint(w, `[{"hard":{"storage":0}}]`)
				return
			}
			fmt.Fprint(w, response)
		})
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		if _, err := client.EnsureAccountProject(ctx, testAccountID); err == nil {
			t.Fatal("misconfigured project accepted")
		}
		cancel()
		server.Close()
	}
}

func TestDeleteAccountArtifactScopedByAccountAndDigest(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	project, _ := AccountProjectName(testAccountID)
	deletes := 0
	client, server := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path !=
			"/api/v2.0/projects/"+project+"/repositories/agent/artifacts/"+digest {
			t.Errorf("unexpected Harbor request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		deletes++
		w.WriteHeader(http.StatusNoContent)
	})
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.DeleteAccountArtifact(ctx, testAccountID, "agent", digest); err != nil || deletes != 1 {
		t.Fatalf("delete account artifact: deletes=%d err=%v", deletes, err)
	}
	for _, bad := range []struct{ account, repository, digest string }{
		{"../other", "agent", digest}, {testAccountID, "../agent", digest},
		{testAccountID, "agent", "latest"},
	} {
		if err := client.DeleteAccountArtifact(ctx, bad.account, bad.repository, bad.digest); err == nil {
			t.Fatalf("unsafe artifact deletion accepted: %+v", bad)
		}
	}
	if deletes != 1 {
		t.Fatalf("unsafe request reached Harbor: %d", deletes)
	}
}
