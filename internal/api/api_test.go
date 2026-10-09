package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"squared/internal/api"
	"squared/internal/catalog"
	"squared/internal/core"
	"squared/internal/render"
	"squared/internal/sso"
	"squared/internal/sso/ssotest"
	"squared/internal/store/storetest"
)

func get(t *testing.T, url, token string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	storetest.Must(t, err)
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestOIDCAccessTokens(t *testing.T) {
	idp := ssotest.New(t)
	st := storetest.New(t)
	cat, err := catalog.Load(os.DirFS("../../catalog"), render.Check)
	storetest.Must(t, err)
	svc := core.New(st, cat, render.Check, storetest.Logger(), core.Config{SessionTTL: time.Hour, SSO: true})
	client, err := sso.New(sso.Config{Issuer: idp.URL, ClientID: idp.ClientID, ClientSecret: idp.ClientSecret,
		RedirectURL: "http://unused/auth/callback", AdminGroup: "squared-admins", APIAudience: "squared-api"})
	storetest.Must(t, err)
	mux := http.NewServeMux()
	api.New(svc, storetest.Logger(), client).Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	idp.SetUser(ssotest.User{Subject: "ci-1", Email: "ci@example.com", EmailVerified: true, Name: "CI", Groups: []string{"squared-admins"}})
	status, me := get(t, srv.URL+"/api/v1/me", idp.AccessToken("squared-api"))
	if status != http.StatusOK || me["email"] != "ci@example.com" || me["admin"] != true {
		t.Fatalf("valid token: status %d, body %v", status, me)
	}

	// A token for a different audience is rejected.
	if status, _ := get(t, srv.URL+"/api/v1/me", idp.AccessToken("other-service")); status != http.StatusUnauthorized {
		t.Fatalf("wrong audience: status %d", status)
	}
	// A changed token is rejected.
	tok := idp.AccessToken("squared-api")
	if status, _ := get(t, srv.URL+"/api/v1/me", tok[:len(tok)-4]+"AAAA"); status != http.StatusUnauthorized {
		t.Fatalf("bad signature: status %d", status)
	}
	// A personal access token that does not exist is rejected.
	if status, _ := get(t, srv.URL+"/api/v1/me", "sqp_unknown"); status != http.StatusUnauthorized {
		t.Fatalf("unknown personal token: status %d", status)
	}
}
