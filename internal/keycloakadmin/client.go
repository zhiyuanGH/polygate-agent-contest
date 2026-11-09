package keycloakadmin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

var (
	ErrConflict    = errors.New("Keycloak user conflicts with another identity")
	ErrNotFound    = errors.New("Keycloak user not found")
	ErrUnavailable = errors.New("Keycloak account administration unavailable")
)

var safeRealm = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

type Client struct {
	baseURL      string
	realm        string
	clientID     string
	clientSecret string
	http         *http.Client
	mu           sync.Mutex
	token        string
	tokenUntil   time.Time
}

type User struct {
	ID              string              `json:"id"`
	Username        string              `json:"username"`
	Enabled         bool                `json:"enabled"`
	RequiredActions []string            `json:"requiredActions,omitempty"`
	Attributes      map[string][]string `json:"attributes,omitempty"`
}

func New(baseURL, realm, clientID, clientSecret string) (*Client, error) {
	base, err := url.Parse(baseURL)
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") ||
		base.RawQuery != "" || base.Fragment != "" || strings.Trim(base.Path, "/") != "" ||
		!safeRealm.MatchString(realm) || clientID == "" || clientSecret == "" {
		return nil, errors.New("invalid Keycloak administration configuration")
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), realm: realm,
		clientID: clientID, clientSecret: clientSecret, http: &http.Client{Timeout: 12 * time.Second}}, nil
}

func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.tokenUntil) {
		return c.token, nil
	}
	body := url.Values{"grant_type": {"client_credentials"}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/realms/"+c.realm+"/protocol/openid-connect/token", strings.NewReader(body))
	if err != nil {
		return "", ErrUnavailable
	}
	req.SetBasicAuth(c.clientID, c.clientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := c.http.Do(req)
	if err != nil {
		return "", ErrUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", ErrUnavailable
	}
	var token struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&token); err != nil ||
		token.AccessToken == "" || token.ExpiresIn < 1 {
		return "", ErrUnavailable
	}
	validFor := time.Duration(token.ExpiresIn) * time.Second
	if validFor > 45*time.Second {
		validFor -= 30 * time.Second
	} else {
		validFor /= 2
	}
	c.token, c.tokenUntil = token.AccessToken, time.Now().Add(validFor)
	return c.token, nil
}

func (c *Client) request(ctx context.Context, method, path string, body any) (*http.Response, error) {
	token, err := c.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	var input io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, ErrUnavailable
		}
		input = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+"/admin/realms/"+c.realm+path, input)
	if err != nil {
		return nil, ErrUnavailable
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(req)
	if err != nil {
		return nil, ErrUnavailable
	}
	return response, nil
}

func (c *Client) GetUser(ctx context.Context, id string) (User, error) {
	var user User
	response, err := c.request(ctx, http.MethodGet, "/users/"+url.PathEscape(id), nil)
	if err != nil {
		return user, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return user, ErrNotFound
	}
	if response.StatusCode != http.StatusOK {
		return user, ErrUnavailable
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&user); err != nil || user.ID != id {
		return User{}, ErrUnavailable
	}
	return user, nil
}

// CreateTemporaryUser binds Keycloak's generated user ID to a prepared tenant
// through a private attribute. On replay, a username collision is accepted
// only when that attribute still names the same tenant.
func (c *Client) CreateTemporaryUser(ctx context.Context, accountID, username, displayName, password string) (string, bool, error) {
	if accountID == "" || username == "" || displayName == "" || len(password) < 20 {
		return "", false, errors.New("invalid temporary user")
	}
	input := map[string]any{"username": username, "firstName": displayName,
		"enabled": false, "requiredActions": []string{"UPDATE_PASSWORD"},
		"attributes":  map[string][]string{"polygateAccountId": {accountID}},
		"credentials": []map[string]any{{"type": "password", "value": password, "temporary": true}}}
	response, err := c.request(ctx, http.MethodPost, "/users", input)
	if err != nil {
		return "", false, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusConflict {
		return "", false, ErrUnavailable
	}
	path := "/users?" + url.Values{"username": {username}, "exact": {"true"},
		"briefRepresentation": {"false"}}.Encode()
	usersResponse, err := c.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return "", false, err
	}
	defer usersResponse.Body.Close()
	if usersResponse.StatusCode != http.StatusOK {
		return "", false, ErrUnavailable
	}
	var users []User
	if err := json.NewDecoder(io.LimitReader(usersResponse.Body, 1<<20)).Decode(&users); err != nil {
		return "", false, ErrUnavailable
	}
	var matching []User
	for _, user := range users {
		if strings.EqualFold(user.Username, username) {
			matching = append(matching, user)
		}
	}
	if len(matching) != 1 || matching[0].ID == "" || len(matching[0].Attributes["polygateAccountId"]) != 1 ||
		matching[0].Attributes["polygateAccountId"][0] != accountID {
		return "", false, ErrConflict
	}
	return matching[0].ID, response.StatusCode == http.StatusCreated, nil
}

func (c *Client) SetEnabled(ctx context.Context, id string, enabled bool) error {
	response, err := c.request(ctx, http.MethodPut, "/users/"+url.PathEscape(id),
		map[string]any{"enabled": enabled})
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return ErrUnavailable
	}
	user, err := c.GetUser(ctx, id)
	if err != nil {
		return err
	}
	if user.Enabled != enabled {
		return fmt.Errorf("Keycloak user state did not converge: %w", ErrUnavailable)
	}
	return nil
}

func (c *Client) ResetTemporaryPassword(ctx context.Context, id, password string) error {
	if len(password) < 20 {
		return errors.New("temporary password is too short")
	}
	response, err := c.request(ctx, http.MethodPut, "/users/"+url.PathEscape(id)+"/reset-password",
		map[string]any{"type": "password", "value": password, "temporary": true})
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return ErrUnavailable
	}
	return nil
}

func (c *Client) LogoutUser(ctx context.Context, id string) error {
	response, err := c.request(ctx, http.MethodPost, "/users/"+url.PathEscape(id)+"/logout", nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return ErrUnavailable
	}
	return nil
}
