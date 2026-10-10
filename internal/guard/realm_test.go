package guard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"squared/internal/keycloak"
)

func TestCheckRealmEmail(t *testing.T) {
	// The development realm after hack/kind-up.sh.
	safe := keycloak.RealmEmail{}
	if warnings, err := checkRealmEmail(safe, nil); err != nil || len(warnings) != 0 {
		t.Fatalf("err %v, warnings %v", err, warnings)
	}

	tests := []struct {
		name   string
		change func(*keycloak.RealmEmail)
		errMsg string
	}{
		{"duplicates", func(r *keycloak.RealmEmail) { r.DuplicateEmailsAllowed = true }, "duplicateEmailsAllowed"},
		{"edit by user", func(r *keycloak.RealmEmail) { r.EmailEditableByUser = true }, "users can change their address"},
		{"registration", func(r *keycloak.RealmEmail) { r.RegistrationAllowed = true }, "registrationAllowed is on without verifyEmail"},
		{"email as user name", func(r *keycloak.RealmEmail) { r.RegistrationEmailAsUsername, r.EditUsernameAllowed = true, true },
			"registrationEmailAsUsername and editUsernameAllowed"},
		{"trusted identity provider", func(r *keycloak.RealmEmail) {
			r.IdentityProviders = []keycloak.IdentityProvider{{Alias: "github", TrustEmail: true}}
		}, "identity provider github has trustEmail on"},
		{"trusted user federation", func(r *keycloak.RealmEmail) {
			r.UserFederation = []keycloak.UserFederation{{Name: "corp", ProviderID: "ldap", TrustEmail: true}}
		}, "user federation corp has trustEmail on"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := safe
			tt.change(&r)
			if _, err := checkRealmEmail(r, nil); err == nil || !strings.Contains(err.Error(), tt.errMsg) {
				t.Fatalf("err = %v, want %q", err, tt.errMsg)
			}
		})
	}

	// Registration with verification of the address is safe.
	r := safe
	r.RegistrationAllowed, r.VerifyEmail = true, true
	if _, err := checkRealmEmail(r, nil); err != nil {
		t.Fatal(err)
	}

	// Other identity providers and user federation give warnings, until an
	// operator checks them. A checked provider with trustEmail is accepted.
	r = safe
	r.IdentityProviders = []keycloak.IdentityProvider{{Alias: "corp-sso"}, {Alias: "github", TrustEmail: true}}
	r.UserFederation = []keycloak.UserFederation{{Name: "ldap", ProviderID: "ldap"}}
	warnings, err := checkRealmEmail(r, []string{"github"})
	if err != nil || len(warnings) != 2 || !strings.Contains(warnings[0], "identity provider corp-sso") ||
		!strings.Contains(warnings[1], "user federation ldap (ldap)") {
		t.Fatalf("err %v, warnings %q", err, warnings)
	}
}

// TestRealmEmailSettings reads the settings from a server that answers as
// Keycloak 26.8.
func TestRealmEmailSettings(t *testing.T) {
	answers := map[string]string{
		"/admin/realms/squared": `{"realm": "squared", "duplicateEmailsAllowed": false, "registrationAllowed": true,
			"verifyEmail": true, "editUsernameAllowed": false, "smtpServer": {"password": "**********"}}`,
		"/admin/realms/squared/users/profile": `{"attributes": [
			{"name": "username", "permissions": {"view": ["admin", "user"], "edit": ["admin", "user"]}},
			{"name": "email", "permissions": {"view": ["admin", "user"], "edit": ["admin", "user"]}}]}`,
		"/admin/realms/squared/identity-provider/instances": `[{"alias": "github", "enabled": true, "trustEmail": true,
			"config": {"clientSecret": "**********"}}]`,
		"/admin/realms/squared/components": `[{"name": "corp", "providerId": "ldap",
			"config": {"trustEmail": ["false"], "bindCredential": ["**********"]}}]`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token") {
			writeJSON(w, map[string]any{"access_token": "t", "expires_in": 300})
			return
		}
		if r.URL.Path == "/admin/realms/squared/components" && r.URL.Query().Get("type") != "org.keycloak.storage.UserStorageProvider" {
			http.Error(w, "wrong type", http.StatusBadRequest)
			return
		}
		body, ok := answers[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	kc := keycloak.New(srv.URL, "squared", "squared-guard", "secret", nil)
	got, err := kc.RealmEmailSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !got.RegistrationAllowed || !got.VerifyEmail || got.DuplicateEmailsAllowed || !got.EmailEditableByUser ||
		len(got.IdentityProviders) != 1 || !got.IdentityProviders[0].TrustEmail ||
		len(got.UserFederation) != 1 || got.UserFederation[0].TrustEmail || got.UserFederation[0].ProviderID != "ldap" {
		t.Fatalf("settings = %+v", got)
	}

	src := &keycloakRealmSource{sc: SourceConfig{Service: "Keycloak"}, idp: kc}
	if _, _, err := src.References(context.Background()); err == nil || !strings.Contains(err.Error(), "users can change their address") {
		t.Fatalf("err = %v", err)
	}
}
