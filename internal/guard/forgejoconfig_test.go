package guard

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"squared/internal/forgejo"
)

// argoHosts are the webhook hosts of the development environment.
var argoHosts = []string{"argocd-server.argocd.svc.cluster.local"}

// safeSignIn returns the settings of the development environment, as the
// reporter serves them.
func safeSignIn() map[string]map[string]string {
	return map[string]map[string]string{
		"admin":         {"EXTERNAL_USER_DISABLE_FEATURES": "manage_password"},
		"oauth2_client": {"ACCOUNT_LINKING": "disabled", "USERNAME": "nickname", "ENABLE_AUTO_REGISTRATION": "true", "UPDATE_AVATAR": "false"},
		"openid":        {"ENABLE_OPENID_SIGNIN": "false", "ENABLE_OPENID_SIGNUP": "false"},
		"service": {"DISABLE_REGISTRATION": "false", "ALLOW_ONLY_EXTERNAL_REGISTRATION": "true", "SHOW_REGISTRATION_BUTTON": "false",
			"ENABLE_INTERNAL_SIGNIN": "false", "REQUIRE_SIGNIN_VIEW": forgejo.HiddenValue},
		"security": {"INSTALL_LOCK": "true"},
		"webhook":  {"ALLOWED_HOST_LIST": "argocd-server.argocd.svc.cluster.local", "PROXY_URL": forgejo.HiddenValue},
	}
}

func cloneSignIn(s map[string]map[string]string) map[string]map[string]string {
	out := map[string]map[string]string{}
	for k, v := range s {
		out[k] = maps.Clone(v)
	}
	return out
}

func TestCheckForgejoSignInSafe(t *testing.T) {
	rep := forgejo.ConfigReport{Start: safeSignIn(), Now: safeSignIn()}
	if warnings, err := checkForgejoSettings(rep, nil, argoHosts); err != nil || len(warnings) != 0 {
		t.Fatalf("err %v, warnings %v", err, warnings)
	}
}

func TestCheckForgejoSignInProblems(t *testing.T) {
	both := func(change func(s map[string]map[string]string)) func(r *forgejo.ConfigReport) {
		return func(r *forgejo.ConfigReport) { change(r.Start); change(r.Now) }
	}
	tests := []struct {
		name   string
		change func(r *forgejo.ConfigReport)
		errMsg string
	}{
		{"auto linking", both(func(s map[string]map[string]string) { s["oauth2_client"]["ACCOUNT_LINKING"] = "auto" }),
			"ACCOUNT_LINKING is auto: "},
		{"auto linking after the next start", func(r *forgejo.ConfigReport) { r.Now["oauth2_client"]["ACCOUNT_LINKING"] = "auto" },
			"(after the next start)"},
		{"local registration", both(func(s map[string]map[string]string) { s["service"]["ALLOW_ONLY_EXTERNAL_REGISTRATION"] = "false" }),
			"local registration is open"},
		{"local registration with the default", both(func(s map[string]map[string]string) { delete(s["service"], "ALLOW_ONLY_EXTERNAL_REGISTRATION") }),
			"local registration is open"},
		{"OpenID sign-in", both(func(s map[string]map[string]string) { s["openid"]["ENABLE_OPENID_SIGNIN"] = "on" }),
			"ENABLE_OPENID_SIGNIN is on"},
		{"OpenID with the default before the installation", both(func(s map[string]map[string]string) {
			s["openid"] = map[string]string{}
			delete(s["security"], "INSTALL_LOCK")
		}), "ENABLE_OPENID_SIGNIN is on"},
		{"OpenID sign-up", both(func(s map[string]map[string]string) {
			s["openid"]["ENABLE_OPENID_SIGNIN"], s["openid"]["ENABLE_OPENID_SIGNUP"] = "true", "1"
		}), "ENABLE_OPENID_SIGNUP is on"},
		{"reverse proxy", both(func(s map[string]map[string]string) {
			s["service"]["ENABLE_REVERSE_PROXY_AUTHENTICATION_API"] = "true"
			s["service"]["ENABLE_REVERSE_PROXY_AUTO_REGISTRATION"] = "true"
		}), "such a header also makes new accounts"},
		{"internal sign-in", both(func(s map[string]map[string]string) { s["service"]["ENABLE_INTERNAL_SIGNIN"] = "true" }),
			"ENABLE_INTERNAL_SIGNIN is on"},
		{"internal sign-in with the default", both(func(s map[string]map[string]string) { delete(s["service"], "ENABLE_INTERNAL_SIGNIN") }),
			"ENABLE_INTERNAL_SIGNIN is on"},
		{"passwords of Keycloak users", both(func(s map[string]map[string]string) {
			s["admin"]["EXTERNAL_USER_DISABLE_FEATURES"] = "deletion, manage_ssh_keys"
		}), "Keycloak users can set a Forgejo password"},
		{"renamed feature key", both(func(s map[string]map[string]string) {
			s["admin"]["EXTERNAL_USERS_DISABLED_FEATURES"] = s["admin"]["EXTERNAL_USER_DISABLE_FEATURES"]
			delete(s["admin"], "EXTERNAL_USER_DISABLE_FEATURES")
		}), "unknown key admin.EXTERNAL_USERS_DISABLED_FEATURES"},
		{"all hosts in the cluster", both(func(s map[string]map[string]string) { s["webhook"]["ALLOWED_HOST_LIST"] = "*.svc.cluster.local" }),
			"ALLOWED_HOST_LIST of [webhook] has *.svc.cluster.local"},
		{"private networks", both(func(s map[string]map[string]string) {
			s["webhook"]["ALLOWED_HOST_LIST"] = "argocd-server.argocd.svc.cluster.local, Private, 10.0.0.0/8"
		}), "ALLOWED_HOST_LIST of [webhook] has private, 10.0.0.0/8"},
		{"unknown key of webhook", both(func(s map[string]map[string]string) { s["webhook"]["ALLOWED_HOSTS"] = forgejo.HiddenValue }),
			"unknown key webhook.ALLOWED_HOSTS"},
		{"import of local paths", both(func(s map[string]map[string]string) { s["security"]["IMPORT_LOCAL_PATHS"] = "true" }),
			"IMPORT_LOCAL_PATHS of [security] is on"},
		{"local networks", both(func(s map[string]map[string]string) {
			s["migrations"] = map[string]string{"ALLOW_LOCALNETWORKS": "true"}
		}),
			"ALLOW_LOCALNETWORKS of [migrations] is on"},
		{"domain pattern of the cluster", both(func(s map[string]map[string]string) {
			s["migrations"] = map[string]string{"ALLOWED_DOMAINS": "github.com, *.SVC.cluster.local"}
		}), "ALLOWED_DOMAINS of [migrations] has *.svc.cluster.local, which matches the name kubernetes.default.svc.cluster.local"},
		{"short names", both(func(s map[string]map[string]string) { s["migrations"] = map[string]string{"ALLOWED_DOMAINS": "*.*"} }),
			"ALLOWED_DOMAINS of [migrations] has *.*, which matches the name kubernetes.default"},
		{"unknown key of migrations", both(func(s map[string]map[string]string) {
			s["migrations"] = map[string]string{"ALLOW_LOCAL_NETWORKS": "true"}
		}), "unknown key migrations.ALLOW_LOCAL_NETWORKS"},
		{"changed file", func(r *forgejo.ConfigReport) { r.Now["service"]["SHOW_REGISTRATION_BUTTON"] = "true" },
			"the section [service] of app.ini changed after the start"},
		{"unknown key of oauth2_client", both(func(s map[string]map[string]string) { s["oauth2_client"]["account_linking"] = "auto" }),
			"unknown key oauth2_client.account_linking"},
		{"unknown sign-in key of service", both(func(s map[string]map[string]string) { s["service"]["ALLOW_PASSKEY_SIGNUP"] = forgejo.HiddenValue }),
			"unknown key service.ALLOW_PASSKEY_SIGNUP"},
		{"unreadable file", func(r *forgejo.ConfigReport) { r.Now, r.Error = nil, "permission denied" }, "cannot read app.ini"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := forgejo.ConfigReport{Start: safeSignIn(), Now: safeSignIn()}
			tt.change(&r)
			if _, err := checkForgejoSettings(r, nil, argoHosts); err == nil || !strings.Contains(err.Error(), tt.errMsg) {
				t.Fatalf("err = %v, want %q", err, tt.errMsg)
			}
		})
	}
}

func TestCheckForgejoSignInWarnings(t *testing.T) {
	// Forgejo uses "login" without the key or with a value that is not
	// valid, and shows a page to link a local account if it does not make
	// accounts for Keycloak users.
	for _, v := range []string{"", "login", "Auto"} {
		s := safeSignIn()
		s["oauth2_client"]["ACCOUNT_LINKING"] = v
		s["oauth2_client"]["ENABLE_AUTO_REGISTRATION"] = "false"
		warnings, err := checkForgejoSettings(forgejo.ConfigReport{Start: s, Now: cloneSignIn(s)}, nil, argoHosts)
		if err != nil || len(warnings) != 2 || !strings.Contains(warnings[0], "ACCOUNT_LINKING is login") ||
			!strings.Contains(warnings[1], "does not make accounts for Keycloak users") {
			t.Fatalf("value %q: err %v, warnings %q", v, err, warnings)
		}
	}

	// A new key of [service] without a sign-in name is a warning, until an
	// operator checks it.
	s := safeSignIn()
	s["service"]["DEFAULT_THEME_COLOR"] = forgejo.HiddenValue
	r := forgejo.ConfigReport{Start: s, Now: cloneSignIn(s)}
	warnings, err := checkForgejoSettings(r, nil, argoHosts)
	if err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "service.DEFAULT_THEME_COLOR") {
		t.Fatalf("err %v, warnings %q", err, warnings)
	}
	if warnings, err := checkForgejoSettings(r, []string{"service.DEFAULT_THEME_COLOR"}, argoHosts); err != nil || len(warnings) != 0 {
		t.Fatalf("ignoreFields: err %v, warnings %q", err, warnings)
	}
}

func TestForgejoConfigSource(t *testing.T) {
	report := forgejo.ConfigReport{Start: safeSignIn(), Now: safeSignIn()}
	report.Now["oauth2_client"]["ACCOUNT_LINKING"] = "auto"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(report)
	}))
	defer srv.Close()
	sources, err := BuildSources(&FileConfig{Groups: []string{"/a"}, Sources: []SourceConfig{
		{Type: "forgejo-config", Service: "Forgejo", URL: srv.URL + "/settings"}}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	refs, _, err := sources[0].References(context.Background())
	if len(refs) != 0 || err == nil || !strings.Contains(err.Error(), "ACCOUNT_LINKING is auto") {
		t.Fatalf("refs %v, err %v", refs, err)
	}
}

// TestCheckForgejoSignInFeatureLists checks that the guard reads the lists
// of disabled features as Forgejo does.
func TestCheckForgejoSignInFeatureLists(t *testing.T) {
	for _, admin := range []map[string]string{
		{"EXTERNAL_USER_DISABLE_FEATURES": "deletion , manage_password"},
		{"USER_DISABLED_FEATURES": "manage_password"}, // For all users.
	} {
		s := safeSignIn()
		s["admin"] = admin
		if _, err := checkForgejoSettings(forgejo.ConfigReport{Start: s, Now: cloneSignIn(s)}, nil, argoHosts); err != nil {
			t.Fatalf("%v: %v", admin, err)
		}
	}
}

// TestCheckForgejoWebhookHosts checks the values of ALLOWED_HOST_LIST that
// are safe.
func TestCheckForgejoWebhookHosts(t *testing.T) {
	for _, v := range []string{"", "external", "External, ARGOCD-SERVER.argocd.svc.cluster.local"} {
		s := safeSignIn()
		s["webhook"]["ALLOWED_HOST_LIST"] = v
		if _, err := checkForgejoSettings(forgejo.ConfigReport{Start: s, Now: cloneSignIn(s)}, nil, argoHosts); err != nil {
			t.Fatalf("%q: %v", v, err)
		}
	}
	// Without webhookHosts, an internal host is an error.
	s := safeSignIn()
	if _, err := checkForgejoSettings(forgejo.ConfigReport{Start: s, Now: cloneSignIn(s)}, nil, nil); err == nil {
		t.Fatal("an internal host without webhookHosts gave no error")
	}
}

// TestForgejoProblemsHaveNoSemicolon checks that no message has ";": the
// guard joins the messages with it, and the tests and the operators split
// them there.
func TestForgejoProblemsHaveNoSemicolon(t *testing.T) {
	bad := forgejoSignIn{accountLinking: "auto", openIDSignIn: true, openIDSignUp: true, reverseProxyAuth: true,
		reverseProxyNew: true, internalSignIn: true, externalPasswords: true, webhookHosts: []string{"*"},
		importLocalPaths: true, allowLocalNetworks: true, migrationDomains: []string{"*"}}
	problems := bad.problems(nil)
	if len(problems) < 11 {
		t.Fatalf("problems = %q", problems)
	}
	for _, p := range append(problems, bad.warnings()...) {
		if strings.Contains(p, ";") {
			t.Errorf("the message has a semicolon: %q", p)
		}
	}
}

// TestCheckForgejoMigrations checks the values of [migrations] that are
// safe.
func TestCheckForgejoMigrations(t *testing.T) {
	for _, mig := range []map[string]string{
		{},
		{"ALLOW_LOCALNETWORKS": "false", "ALLOWED_DOMAINS": "github.com,*.github.com,codeberg.org", "BLOCKED_DOMAINS": "*"},
	} {
		s := safeSignIn()
		s["migrations"] = mig
		if _, err := checkForgejoSettings(forgejo.ConfigReport{Start: s, Now: cloneSignIn(s)}, nil, argoHosts); err != nil {
			t.Fatalf("%v: %v", mig, err)
		}
	}
}
