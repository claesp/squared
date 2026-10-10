package guard

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"squared/internal/keycloak"
	"squared/internal/store/storetest"
)

func TestCheckForgejoCredentials(t *testing.T) {
	ctx := context.Background()
	used := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	users := map[string]keycloakUserState{
		"kc-dev":    {Exists: true, Enabled: true},
		"kc-admin":  {Exists: true, Enabled: true, Groups: []string{"/squared-admins"}},
		"kc-gone":   {},
		"kc-off":    {Exists: true},
		"kc-former": {Exists: true, Enabled: true, Groups: []string{"/tenant-acme-owner"}},
	}
	lookup := func(_ context.Context, id string) (keycloakUserState, error) { return users[id], nil }
	owner := func(name, id string, admin bool) forgejoCredentialOwner {
		return forgejoCredentialOwner{UserName: name, LoginSource: 1, LoginName: id, IsAdmin: admin, IsActive: true, Tokens: 2, TokensLastUsed: used}
	}

	ok := []forgejoCredentialOwner{owner("dev", "kc-dev", false), owner("platform-admin", "kc-admin", true)}
	if warnings, err := checkForgejoCredentials(ctx, ok, 1, "/squared-admins", lookup); err != nil || len(warnings) != 0 {
		t.Fatalf("err %v, warnings %v", err, warnings)
	}

	tests := []struct {
		name   string
		owner  forgejoCredentialOwner
		errMsg string
	}{
		{"deleted user", owner("gone", "kc-gone", false), "Forgejo user gone has personal access tokens (2, last use 2026-10-01), but Keycloak has no user kc-gone"},
		{"disabled user", owner("off", "kc-off", false), "Forgejo user off has personal access tokens (2, last use 2026-10-01), but Keycloak disabled the user"},
		{"administrator without the group", owner("former", "kc-former", true), "Forgejo user former is a Forgejo administrator, but not a member of /squared-admins"},
		{"SSH keys of a disabled user", forgejoCredentialOwner{UserName: "off", LoginSource: 1, LoginName: "kc-off", IsActive: true,
			SSHKeys: 1, SSHKeysLastUsed: used}, "Forgejo user off has SSH keys (1, last use 2026-10-01), but Keycloak disabled the user"},
		{"tokens and SSH keys", forgejoCredentialOwner{UserName: "gone", LoginSource: 1, LoginName: "kc-gone", IsActive: true,
			Tokens: 1, TokensLastUsed: used, SSHKeys: 3, SSHKeysLastUsed: used},
			"has personal access tokens (1, last use 2026-10-01) and SSH keys (3, last use 2026-10-01), but Keycloak has no user kc-gone"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := checkForgejoCredentials(ctx, append(ok, tt.owner), 1, "/squared-admins", lookup)
			if err == nil || !strings.Contains(err.Error(), tt.errMsg) {
				t.Fatalf("err = %v, want %q", err, tt.errMsg)
			}
		})
	}

	// Forgejo refuses all requests of a blocked or inactive account.
	blocked, inactive := owner("gone", "kc-gone", true), owner("off", "kc-off", false)
	blocked.ProhibitLogin, inactive.IsActive = true, false
	if _, err := checkForgejoCredentials(ctx, []forgejoCredentialOwner{blocked, inactive}, 1, "/squared-admins", lookup); err != nil {
		t.Fatal(err)
	}

	// Without an administrator group, Forgejo does not take the flag from
	// Keycloak, thus the flag is no problem.
	if _, err := checkForgejoCredentials(ctx, []forgejoCredentialOwner{owner("former", "kc-former", true)}, 1, "", lookup); err != nil {
		t.Fatal(err)
	}

	// The users of another source get a warning, not a lookup in Keycloak.
	other := owner("github-user", "12345", false)
	other.LoginSource = 2
	warnings, err := checkForgejoCredentials(ctx, []forgejoCredentialOwner{other}, 1, "/squared-admins", lookup)
	if err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "source 2") {
		t.Fatalf("err %v, warnings %v", err, warnings)
	}

	// A Keycloak error stops the source.
	fail := func(context.Context, string) (keycloakUserState, error) {
		return keycloakUserState{}, errors.New("keycloak down")
	}
	if _, err := checkForgejoCredentials(ctx, ok, 1, "/squared-admins", fail); err == nil {
		t.Fatal("a Keycloak error gave no error")
	}
}

// TestForgejoTokensSource applies deploy/kind/forgejo-guard-view.sql to
// tables like the Forgejo tables, and reads the view as the guard.
func TestForgejoCredentialsSource(t *testing.T) {
	dbURL := storetest.NewDatabase(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dbURL)
	storetest.Must(t, err)
	defer admin.Close(ctx)

	makeForgejoTables(t, admin)
	keycloakCfg := forgejo15()
	_, err = admin.Exec(ctx, `INSERT INTO login_source (type, name, is_active, cfg) VALUES (6, 'keycloak', true, $1)`, cfgJSON(keycloakCfg))
	storetest.Must(t, err)
	_, err = admin.Exec(ctx, `
		INSERT INTO "user" (name, type, login_source, login_name, is_admin, is_active, prohibit_login, passwd) VALUES
			('dev', 0, 1, 'kc-dev', false, true, false, ''),
			('former', 0, 1, 'kc-former', true, true, false, ''),
			('forgejo-admin', 0, 0, '', true, true, false, 'top-secret-hash'),
			('without-tokens', 0, 1, 'kc-none', false, true, false, ''),
			('keys-only', 0, 1, 'kc-keys', false, true, false, ''),
			('deploy-only', 0, 1, 'kc-deploy', false, true, false, '');
		INSERT INTO access_token (uid, name, token_hash, token_salt, token_last_eight, scope, created_unix, updated_unix) VALUES
			(1, 'ci', 'top-secret-hash', 'top-secret-salt', 'abcdefgh', 'read:repository', 1, 1790000000),
			(2, 'old', 'top-secret-hash', 'top-secret-salt', 'abcdefgh', 'write:admin', 1, 1790000000),
			(2, 'older', 'top-secret-hash', 'top-secret-salt', 'abcdefgh', 'all', 1, 1780000000),
			(3, 'bot', 'top-secret-hash', 'top-secret-salt', 'abcdefgh', 'all', 1, 1790000000);
		INSERT INTO public_key (owner_id, name, fingerprint, content, type, created_unix, updated_unix) VALUES
			(5, 'laptop', 'SHA256:top-secret-fingerprint', 'ssh-ed25519 top-secret-key', 1, 1, 1790000000),
			(5, 'principal', 'SHA256:top-secret-fingerprint', 'top-secret-principal', 3, 1, 1780000000),
			(6, 'deploy', 'SHA256:top-secret-fingerprint', 'ssh-ed25519 top-secret-key', 2, 1, 1790000000);`)
	storetest.Must(t, err)
	viewSQL, err := os.ReadFile("../../deploy/kind/forgejo-guard-view.sql")
	storetest.Must(t, err)
	_, err = admin.Exec(ctx, string(viewSQL))
	storetest.Must(t, err)
	_, err = admin.Exec(ctx, "ALTER ROLE squared_guard PASSWORD 'guard-test-password'")
	storetest.Must(t, err)

	u, err := url.Parse(dbURL)
	storetest.Must(t, err)
	u.User = url.User("squared_guard")
	passwordFile := filepath.Join(t.TempDir(), "password")
	storetest.Must(t, os.WriteFile(passwordFile, []byte("guard-test-password\n"), 0o600))

	idp := stubClients{
		users: map[string]keycloak.User{
			"kc-dev":    {ID: "kc-dev", Enabled: true},
			"kc-former": {ID: "kc-former", Enabled: true},
			"kc-keys":   {ID: "kc-keys"},   // Disabled.
			"kc-deploy": {ID: "kc-deploy"}, // Disabled, but deploy keys belong to repositories.
		},
		groups: map[string][]string{"kc-former": {"/tenant-acme-owner"}},
	}
	sources, err := BuildSources(&FileConfig{Groups: []string{"/a"}, Sources: []SourceConfig{{Type: "forgejo-credentials", Service: "Forgejo",
		URL: u.String(), PasswordFile: passwordFile, SourceID: 1}}}, nil, idp)
	storetest.Must(t, err)
	_, _, err = sources[0].References(ctx)
	if err == nil || !strings.Contains(err.Error(), "Forgejo user former is a Forgejo administrator, but not a member of /squared-admins in Keycloak; its personal access tokens (2, last use 2026-09-21)") ||
		!strings.Contains(err.Error(), "Forgejo user keys-only has SSH keys (2, last use 2026-09-21), but Keycloak disabled the user") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "dev ") || strings.Contains(err.Error(), "forgejo-admin") || strings.Contains(err.Error(), "deploy-only") {
		t.Fatalf("the error names a user without a problem: %v", err)
	}

	// The role can read the view, but not the tables, and the view has no
	// token, hash, salt or password.
	guard, err := pgx.Connect(ctx, strings.Replace(u.String(), "squared_guard@", "squared_guard:guard-test-password@", 1))
	storetest.Must(t, err)
	defer guard.Close(ctx)
	for _, table := range []string{"access_token", `"user"`, "public_key"} {
		if _, err := guard.Exec(ctx, "SELECT * FROM "+table); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("the guard can read %s: %v", table, err)
		}
	}
	var all string
	storetest.Must(t, guard.QueryRow(ctx, "SELECT string_agg(row_to_json(v)::text, ' ') FROM squared_guard_credential_owners v").Scan(&all))
	if strings.Contains(all, "top-secret") || strings.Contains(all, "abcdefgh") || strings.Contains(all, "forgejo-admin") ||
		strings.Contains(all, "without-tokens") || strings.Contains(all, "deploy-only") {
		t.Fatalf("the view shows more than the credential owners of OAuth sources: %s", all)
	}
}
