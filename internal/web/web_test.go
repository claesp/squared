package web_test

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"squared/internal/catalog"
	"squared/internal/core"
	"squared/internal/domain"
	"squared/internal/httpx"
	"squared/internal/render"
	"squared/internal/store/storetest"
	"squared/internal/web"
)

func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	st := storetest.New(t)
	cat, err := catalog.Load(os.DirFS("../../catalog"), render.Check)
	storetest.Must(t, err)
	log := storetest.Logger()
	svc := core.New(st, cat, render.Check, log, core.Config{SessionTTL: time.Hour, DefaultQuota: domain.Quota{CPU: "1", Memory: "1Gi", Pods: 5}})
	_, err = svc.BootstrapAdmin(context.Background(), "admin@example.com", "admin password 1")
	storetest.Must(t, err)
	w, err := web.New(svc, web.Config{CookieSecure: false, RegistryHost: "registry.test", LocalLogin: true}, log)
	storetest.Must(t, err)
	mux := http.NewServeMux()
	w.Routes(mux)
	h := httpx.SecurityHeaders(false, nil, http.NewCrossOriginProtection().Handler(mux))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func client(t *testing.T) *http.Client {
	jar, err := cookiejar.New(nil)
	storetest.Must(t, err)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	storetest.Must(t, err)
	return string(b)
}

var csrfPattern = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func signIn(t *testing.T, srv *httptest.Server, c *http.Client) string {
	t.Helper()
	resp, err := c.PostForm(srv.URL+"/login", url.Values{"email": {"admin@example.com"}, "password": {"admin password 1"}, "next": {"/catalog"}})
	storetest.Must(t, err)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/catalog" {
		t.Fatalf("sign-in: status %d, location %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, err = c.Get(srv.URL + "/account")
	storetest.Must(t, err)
	m := csrfPattern.FindStringSubmatch(body(t, resp))
	if m == nil {
		t.Fatal("no CSRF token on the account page")
	}
	return m[1]
}

func TestRedirectToLogin(t *testing.T) {
	srv := newServer(t)
	resp, err := client(t).Get(srv.URL + "/tenants/acme")
	storetest.Must(t, err)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login?next=%2Ftenants%2Facme" {
		t.Fatalf("status %d, location %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestSecurityHeaders(t *testing.T) {
	srv := newServer(t)
	resp, err := client(t).Get(srv.URL + "/login")
	storetest.Must(t, err)
	resp.Body.Close()
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "script-src") {
		t.Fatalf("unexpected CSP %q", csp)
	}
	if resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatal("X-Frame-Options is missing")
	}
}

func TestLoginFailure(t *testing.T) {
	srv := newServer(t)
	resp, err := client(t).PostForm(srv.URL+"/login", url.Values{"email": {"admin@example.com"}, "password": {"wrong"}})
	storetest.Must(t, err)
	b := body(t, resp)
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(b, "not correct") {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestOpenRedirectBlocked(t *testing.T) {
	srv := newServer(t)
	resp, err := client(t).PostForm(srv.URL+"/login", url.Values{"email": {"admin@example.com"}, "password": {"admin password 1"}, "next": {"//evil.example"}})
	storetest.Must(t, err)
	resp.Body.Close()
	if resp.Header.Get("Location") != "/" {
		t.Fatalf("redirect to %q", resp.Header.Get("Location"))
	}
}

func TestCSRF(t *testing.T) {
	srv := newServer(t)
	c := client(t)
	csrf := signIn(t, srv, c)

	// Without the token the server rejects the form.
	resp, err := c.PostForm(srv.URL+"/admin/users", url.Values{"email": {"x@example.com"}, "name": {"X"}, "password": {"long enough password"}})
	storetest.Must(t, err)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("form without CSRF token: status %d", resp.StatusCode)
	}

	// A request from another site is rejected also with the token.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/admin/users", strings.NewReader(url.Values{"csrf": {csrf}, "email": {"x@example.com"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err = c.Do(req)
	storetest.Must(t, err)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site form: status %d", resp.StatusCode)
	}

	// With the token the user is added.
	resp, err = c.PostForm(srv.URL+"/admin/users", url.Values{"csrf": {csrf}, "email": {"x@example.com"}, "name": {"X"}, "password": {"long enough password"}})
	storetest.Must(t, err)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("form with CSRF token: status %d", resp.StatusCode)
	}
}

func TestTenantAndDeploymentPages(t *testing.T) {
	srv := newServer(t)
	c := client(t)
	csrf := signIn(t, srv, c)

	resp, err := c.PostForm(srv.URL+"/admin/tenants", url.Values{
		"csrf": {csrf}, "slug": {"acme"}, "name": {"Acme"}, "owner": {"admin@example.com"},
		"environments": {"dev"}, "quota_cpu": {"1"}, "quota_memory": {"1Gi"}, "quota_pods": {"5"},
	})
	storetest.Must(t, err)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/tenants/acme" {
		t.Fatalf("make tenant: status %d location %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	// A validation error shows the form again with the message.
	resp, err = c.PostForm(srv.URL+"/tenants/acme/new/web-app", url.Values{"csrf": {csrf}, "name": {"shop"}, "environment": {"dev"}, "image": {"no-tag"}})
	storetest.Must(t, err)
	b := body(t, resp)
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(b, "must have a tag") || !strings.Contains(b, `value="shop"`) {
		t.Fatalf("bad deployment: status %d", resp.StatusCode)
	}

	resp, err = c.PostForm(srv.URL+"/tenants/acme/new/web-app", url.Values{
		"csrf": {csrf}, "name": {"shop"}, "environment": {"dev"}, "image": {"shop:1.0.0"},
		"replicas": {"2"}, "env": {"GREETING=Hello <b>world</b>"},
	})
	storetest.Must(t, err)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("make deployment: status %d", resp.StatusCode)
	}
	resp, err = c.Get(srv.URL + resp.Header.Get("Location"))
	storetest.Must(t, err)
	b = body(t, resp)
	if !strings.Contains(b, "acme-dev-shop") || !strings.Contains(b, "shop:1.0.0") {
		t.Fatal("deployment page does not show the deployment")
	}
	// Values from users are escaped.
	if strings.Contains(b, "<b>world</b>") || !strings.Contains(b, "&lt;b&gt;world&lt;/b&gt;") {
		t.Fatal("value is not escaped")
	}
	if !strings.Contains(b, `http-equiv="refresh"`) {
		t.Fatal("a pending deployment page does not refresh")
	}
}

func TestChangeAndRollbackForms(t *testing.T) {
	srv := newServer(t)
	c := client(t)
	csrf := signIn(t, srv, c)
	post := func(path string, v url.Values) *http.Response {
		t.Helper()
		v.Set("csrf", csrf)
		resp, err := c.PostForm(srv.URL+path, v)
		storetest.Must(t, err)
		resp.Body.Close()
		return resp
	}
	post("/admin/tenants", url.Values{"slug": {"acme"}, "name": {"Acme"}, "owner": {"admin@example.com"},
		"environments": {"dev"}, "quota_cpu": {"1"}, "quota_memory": {"1Gi"}, "quota_pods": {"5"}})
	post("/tenants/acme/new/worker", url.Values{"name": {"bg"}, "environment": {"dev"}, "image": {"bg:1"}, "env": {"A=1"}})

	// The change form shows the current values.
	resp, err := c.Get(srv.URL + "/tenants/acme/deployments/dev/bg/edit")
	storetest.Must(t, err)
	b := body(t, resp)
	if !strings.Contains(b, `value="bg:1"`) || !strings.Contains(b, "A=1") {
		t.Fatal("the change form does not show the current values")
	}
	resp = post("/tenants/acme/deployments/dev/bg/edit", url.Values{"version": {"1.0.0"}, "image": {"bg:2"}, "replicas": {"3"}, "message": {"second"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("change: status %d", resp.StatusCode)
	}
	resp, err = c.Get(srv.URL + "/tenants/acme/deployments/dev/bg")
	storetest.Must(t, err)
	b = body(t, resp)
	if !strings.Contains(b, "bg:2") || !strings.Contains(b, "second") || !strings.Contains(b, "2 (desired)") {
		t.Fatal("the deployment page does not show revision 2")
	}

	// Revision 1 has no resolved images yet, thus a rollback is refused.
	resp = post("/tenants/acme/deployments/dev/bg/rollback", url.Values{"revision": {"1"}})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("rollback to an unresolved revision: status %d", resp.StatusCode)
	}

	// Removal needs the name as confirmation.
	resp = post("/tenants/acme/deployments/dev/bg/delete", url.Values{"confirm": {"wrong"}})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("removal without confirmation: status %d", resp.StatusCode)
	}
	resp = post("/tenants/acme/deployments/dev/bg/delete", url.Values{"confirm": {"bg"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("removal: status %d", resp.StatusCode)
	}
}
