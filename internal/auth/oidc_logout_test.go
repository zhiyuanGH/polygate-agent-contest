package auth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"golang.org/x/oauth2"
)

func TestOIDCLogoutUsesIdentityProviderAndReturnsHome(t *testing.T) {
	svc := &Service{
		oauth:      oauth2.Config{ClientID: "polygate-console"},
		baseOrigin: "https://console.example.test",
		logoutURL:  "https://console.example.test/realms/polygate/protocol/openid-connect/logout",
		secure:     true,
	}
	req := httptest.NewRequest(http.MethodGet, "/auth/oidc-logout", nil)
	req.AddCookie(&http.Cookie{Name: svc.idTokenCookieName(), Value: "header.payload.signature"})
	w := httptest.NewRecorder()
	svc.OIDCLogout(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("logout status = %d", w.Code)
	}
	target, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if target.Path != "/realms/polygate/protocol/openid-connect/logout" ||
		target.Query().Get("client_id") != "polygate-console" ||
		target.Query().Get("post_logout_redirect_uri") != "https://console.example.test/" ||
		target.Query().Get("id_token_hint") != "header.payload.signature" {
		t.Fatalf("unexpected identity logout URL: %s", target.String())
	}
	if cookies := w.Result().Cookies(); len(cookies) != 1 || cookies[0].Name != svc.idTokenCookieName() || cookies[0].MaxAge >= 0 {
		t.Fatalf("identity token cookie was not cleared: %+v", cookies)
	}
}
