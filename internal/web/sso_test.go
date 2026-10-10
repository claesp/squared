package web_test

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"squared/internal/catalog"
	"squared/internal/core"
	"squared/internal/httpx"
	"squared/internal/render"
	"squared/internal/sso"
	"squared/internal/sso/ssotest"
	"squared/internal/store/storetest"
	"squared/internal/web"
)

// newSSOServer starts Squared with SSO and without password sign-in.
func newSSOServer(t *testing.T, idp *ssotest.Provider) *httptest.Server {
	t.Helper()
	st := storetest.New(t)
	cat, err := catalog.Load(os.DirFS("../../catalog"), render.Check)
	storetest.Must(t, err)
	log := storetest.Logger()
	svc := core.New(st, cat, render.Check, log, core.Config{SessionTTL: time.Hour, SSO: true})

	mux := http.NewServeMux()
	srv := httptest.NewServer(nil)
	t.Cleanup(srv.Close)
	client, err := sso.New(sso.Config{
		Issuer: idp.URL, ClientID: idp.ClientID, ClientSecret: idp.ClientSecret,
		RedirectURL: srv.URL + "/auth/callback", PostLogoutURL: srv.URL + "/login", AdminGroup: "/squared-admins",
	})
	storetest.Must(t, err)
	w, err := web.New(svc, web.Config{CookieSecure: false, RegistryHost: "registry.test", SSO: client, SSOName: "Keycloak"}, log)
	storetest.Must(t, err)
	w.Routes(mux)
	srv.Config.Handler = httpx.SecurityHeaders(false, []string{client.IssuerOrigin()}, mux)
	return srv
}

// browser follows redirects, like a browser.
func browser(t *testing.T) *http.Client {
	jar, err := cookiejar.New(nil)
	storetest.Must(t, err)
	return &http.Client{Jar: jar}
}

func TestSSOSignIn(t *testing.T) {
	idp := ssotest.New(t)
	idp.SetUser(ssotest.User{Subject: "u1", Email: "ada@example.com", EmailVerified: true, Name: "Ada", Groups: []string{"/squared-admins"}})
	srv := newSSOServer(t, idp)
	b := browser(t)

	// The sign-in page shows only the SSO button.
	resp, err := b.Get(srv.URL + "/login")
	storetest.Must(t, err)
	page := body(t, resp)
	if !strings.Contains(page, "Sign in with Keycloak") || strings.Contains(page, `type="password"`) {
		t.Fatal("the sign-in page does not show only the SSO button")
	}
	if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "form-action 'self' "+idp.URL) {
		t.Fatalf("CSP does not permit the provider: %s", resp.Header.Get("Content-Security-Policy"))
	}

	// The complete flow ends at the page in "next".
	resp, err = b.Get(srv.URL + "/auth/login?next=/admin/users")
	storetest.Must(t, err)
	page = body(t, resp)
	if resp.StatusCode != http.StatusOK || resp.Request.URL.Path != "/admin/users" || !strings.Contains(page, "ada@example.com") {
		t.Fatalf("after sign-in: status %d at %s", resp.StatusCode, resp.Request.URL)
	}
	if !strings.Contains(page, "identity provider") {
		t.Fatal("the user list does not show the sign-in method")
	}

	// Password sign-in is not enabled.
	resp, err = b.PostForm(srv.URL+"/login", url.Values{"email": {"ada@example.com"}, "password": {"x"}})
	storetest.Must(t, err)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("password sign-in: status %d", resp.StatusCode)
	}

	// Sign-out sends the browser to the provider with the ID token.
	resp, err = b.Get(srv.URL + "/account")
	storetest.Must(t, err)
	csrf := csrfPattern.FindStringSubmatch(body(t, resp))[1]
	noFollow := &http.Client{Jar: b.Jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err = noFollow.PostForm(srv.URL+"/logout", url.Values{"csrf": {csrf}})
	storetest.Must(t, err)
	resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if !strings.HasPrefix(loc.String(), idp.URL+"/logout?") || loc.Query().Get("id_token_hint") == "" || loc.Query().Get("post_logout_redirect_uri") != srv.URL+"/login" {
		t.Fatalf("sign-out goes to %s", loc)
	}
}

func TestSSOAdminFlagFromGroups(t *testing.T) {
	idp := ssotest.New(t)
	// A subgroup with the name of the administrator group does not give
	// administrator rights: only the exact full path does.
	idp.SetUser(ssotest.User{Subject: "u2", Email: "dev@example.com", EmailVerified: true, Name: "Dev",
		Groups: []string{"/tenant-acme-owner/squared-admins", "squared-admins"}})
	srv := newSSOServer(t, idp)
	b := browser(t)
	resp, err := b.Get(srv.URL + "/auth/login?next=/admin/users")
	storetest.Must(t, err)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a user without the administrator group: status %d", resp.StatusCode)
	}
}

// startLogin starts a sign-in and returns the callback URL from the
// provider, without calling it.
func startLogin(t *testing.T, srv *httptest.Server, c *http.Client) string {
	t.Helper()
	resp, err := c.Get(srv.URL + "/auth/login")
	storetest.Must(t, err)
	resp.Body.Close()
	resp, err = c.Get(resp.Header.Get("Location")) // The provider.
	storetest.Must(t, err)
	resp.Body.Close()
	return resp.Header.Get("Location")
}

func TestSSOCallbackProtection(t *testing.T) {
	idp := ssotest.New(t)
	srv := newSSOServer(t, idp)
	c := client(t)

	// A wrong state is rejected.
	callback := startLogin(t, srv, c)
	bad := strings.Replace(callback, "state=", "state=x", 1)
	resp, err := c.Get(bad)
	storetest.Must(t, err)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("wrong state: status %d", resp.StatusCode)
	}
	// The attempt is used, thus the correct callback also fails now.
	resp, err = c.Get(callback)
	storetest.Must(t, err)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("callback after a failed attempt: status %d", resp.StatusCode)
	}

	// A callback in a different browser (without the cookie) is rejected.
	callback = startLogin(t, srv, c)
	resp, err = client(t).Get(callback)
	storetest.Must(t, err)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("callback without cookie: status %d", resp.StatusCode)
	}

	// A correct callback works one time only.
	callback = startLogin(t, srv, c)
	resp, err = c.Get(callback)
	storetest.Must(t, err)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("correct callback: status %d", resp.StatusCode)
	}
	resp, err = c.Get(callback)
	storetest.Must(t, err)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("replayed callback: status %d", resp.StatusCode)
	}

	// A wrong nonce in the ID token is rejected.
	idp.WrongNonce = true
	callback = startLogin(t, srv, c)
	resp, err = c.Get(callback)
	storetest.Must(t, err)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong nonce: status %d", resp.StatusCode)
	}
}

func TestSSOProviderDown(t *testing.T) {
	idp := ssotest.New(t)
	srv := newSSOServer(t, idp)
	idp.Close() // Discovery at the first use fails.
	resp, err := http.Get(srv.URL + "/auth/login")
	storetest.Must(t, err)
	b := body(t, resp)
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(b, "not available") {
		t.Fatalf("status %d", resp.StatusCode)
	}
}
