package keycloakadmin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCreateTemporaryUserReplayAndPasswordReset(t *testing.T) {
	const userID = "2df493c3-60c9-4936-aa67-d3195a5ac6cc"
	const accountID = "bb128b15-9215-43c8-89b6-48e2fb855877"
	const password = "first-strong-temporary-password"
	var user *User
	tokens, reset := 0, 0
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/realms/polygate/protocol/openid-connect/token":
			id, secret, ok := r.BasicAuth()
			if !ok || id != "account-manager" || secret != "private-client-secret" ||
				r.FormValue("grant_type") != "client_credentials" {
				http.Error(w, "bad client authentication", http.StatusUnauthorized)
				return
			}
			tokens++
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "test-token", "expires_in": 300})
		case r.URL.Path == "/admin/realms/polygate/users" && r.Method == http.MethodPost:
			if r.Header.Get("Authorization") != "Bearer test-token" {
				http.Error(w, "missing token", http.StatusUnauthorized)
				return
			}
			if user != nil {
				w.WriteHeader(http.StatusConflict)
				return
			}
			var input struct {
				Username        string              `json:"username"`
				Enabled         bool                `json:"enabled"`
				RequiredActions []string            `json:"requiredActions"`
				Attributes      map[string][]string `json:"attributes"`
				Credentials     []struct {
					Type      string `json:"type"`
					Value     string `json:"value"`
					Temporary bool   `json:"temporary"`
				} `json:"credentials"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil ||
				input.Username != "student-c" || input.Enabled || len(input.RequiredActions) != 1 ||
				len(input.Attributes["polygateAccountId"]) != 1 || input.Attributes["polygateAccountId"][0] != accountID ||
				input.RequiredActions[0] != "UPDATE_PASSWORD" || len(input.Credentials) != 1 ||
				input.Credentials[0].Type != "password" || input.Credentials[0].Value != password ||
				!input.Credentials[0].Temporary {
				http.Error(w, "bad user", http.StatusBadRequest)
				return
			}
			user = &User{ID: userID, Username: input.Username, RequiredActions: input.RequiredActions, Attributes: input.Attributes}
			w.WriteHeader(http.StatusCreated)
		case r.URL.Path == "/admin/realms/polygate/users" && r.Method == http.MethodGet:
			if user != nil && r.URL.Query().Get("username") == user.Username && r.URL.Query().Get("exact") == "true" {
				_ = json.NewEncoder(w).Encode([]User{*user})
			} else {
				_ = json.NewEncoder(w).Encode([]User{})
			}
		case r.URL.Path == "/admin/realms/polygate/users/"+userID && r.Method == http.MethodGet:
			if user == nil {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(user)
		case r.URL.Path == "/admin/realms/polygate/users/"+userID && r.Method == http.MethodPut:
			var input struct {
				Enabled bool `json:"enabled"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				http.Error(w, "invalid state", http.StatusBadRequest)
				return
			}
			user.Enabled = input.Enabled
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/admin/realms/polygate/users/"+userID+"/reset-password" && r.Method == http.MethodPut:
			var credential struct {
				Type      string `json:"type"`
				Value     string `json:"value"`
				Temporary bool   `json:"temporary"`
			}
			if err := json.NewDecoder(r.Body).Decode(&credential); err != nil ||
				credential.Type != "password" || credential.Value != "replacement-temporary-password" || !credential.Temporary {
				http.Error(w, "bad reset", http.StatusBadRequest)
				return
			}
			reset++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer service.Close()
	client, err := New(service.URL, "polygate", "account-manager", "private-client-secret")
	if err != nil {
		t.Fatal(err)
	}
	identityID, created, err := client.CreateTemporaryUser(context.Background(), accountID, "student-c", "Student C", password)
	if err != nil || !created || identityID != userID {
		t.Fatalf("create: %s %v %v", identityID, created, err)
	}
	identityID, created, err = client.CreateTemporaryUser(context.Background(), accountID, "student-c", "Student C", password)
	if err != nil || created || identityID != userID {
		t.Fatalf("replay: %s %v %v", identityID, created, err)
	}
	if err := client.SetEnabled(context.Background(), userID, true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if err := client.ResetTemporaryPassword(context.Background(), userID, "replacement-temporary-password"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if tokens != 1 || reset != 1 || !user.Enabled {
		t.Fatalf("token/cache/reset/state: %d %d %+v", tokens, reset, user)
	}
	if _, _, err := client.CreateTemporaryUser(context.Background(), accountID, "someone-else", "Wrong", password); err != ErrConflict {
		t.Fatalf("different username adopted: %v", err)
	}
}

func TestClientRejectsInvalidConfigurationWithoutRevealingSecret(t *testing.T) {
	_, err := New("http://localhost/other-path", "polygate", "account-manager", "do-not-print-me")
	if err == nil || strings.Contains(err.Error(), "do-not-print-me") {
		t.Fatalf("invalid config error: %v", err)
	}
}
