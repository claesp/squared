package guard

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"squared/internal/store/storetest"
)

// TestForgejoDatabaseSource applies deploy/kind/forgejo-guard-view.sql to a
// table like the Forgejo table login_source, and reads it as the guard.
func TestForgejoDatabaseSource(t *testing.T) {
	dbURL := storetest.NewDatabase(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dbURL)
	storetest.Must(t, err)
	defer admin.Close(ctx)

	makeForgejoTables(t, admin)
	keycloak := forgejo15()
	keycloak["RestrictedGroup"] = "/guests"
	keycloak["GroupTeamMap"] = `{"/ops": {"squared": ["operators"]}}`
	old := forgejo15()
	old["AdminGroup"] = "/old admins"
	_, err = admin.Exec(ctx, `INSERT INTO login_source (type, name, is_active, cfg) VALUES
		(6, 'keycloak', true, $1), (2, 'ldap', true, 'not JSON: the view must ignore it'), (6, 'old', false, $2)`,
		cfgJSON(keycloak), cfgJSON(old))
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

	src := &forgejoDatabaseSource{sc: SourceConfig{Service: "Forgejo", URL: u.String(), PasswordFile: passwordFile, Name: "squared_guard_auth_sources"}}
	refs, warnings, err := src.References(ctx)
	storetest.Must(t, err)
	if len(warnings) != 0 {
		t.Fatalf("warnings %v", warnings)
	}
	var groups []string
	for _, r := range refs {
		groups = append(groups, r.Group)
	}
	sort.Strings(groups)
	if want := []string{"/guests", "/old admins", "/ops", "/squared-admins"}; !slices.Equal(groups, want) {
		t.Fatalf("groups = %q, want %q", groups, want)
	}

	// The role can read the view, but not the table, and the view has no
	// client secret.
	guard, err := pgx.Connect(ctx, strings.Replace(u.String(), "squared_guard@", "squared_guard:guard-test-password@", 1))
	storetest.Must(t, err)
	defer guard.Close(ctx)
	if _, err := guard.Exec(ctx, "SELECT cfg FROM login_source"); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("the guard can read the table: %v", err)
	}
	var all string
	storetest.Must(t, guard.QueryRow(ctx, "SELECT string_agg(row_to_json(v)::text, ' ') FROM squared_guard_auth_sources v").Scan(&all))
	if strings.Contains(all, "top-secret") || strings.Contains(all, "openid-configuration") {
		t.Fatalf("the view shows more than the groups and the field names: %s", all)
	}

	// A Forgejo upgrade renames a field: the source fails and names it.
	_, err = admin.Exec(ctx, `UPDATE login_source SET cfg = (cfg::jsonb - 'AdminGroup' || '{"AdministratorGroup": "/squared-admins"}')::text WHERE name = 'keycloak'`)
	storetest.Must(t, err)
	refs, _, err = src.References(ctx)
	if err == nil || !strings.Contains(err.Error(), "AdminGroup is missing") {
		t.Fatalf("a renamed field gave no error: %v", err)
	}
	// The groups of the other sources stay.
	if got := groupsOf(refs); !slices.Equal(got, []string{"/old admins"}) {
		t.Fatalf("groups with the error = %q", got)
	}
}

// makeForgejoTables makes the Forgejo tables with the columns that the
// views of deploy/kind/forgejo-guard-view.sql use.
func makeForgejoTables(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	_, err := conn.Exec(context.Background(), `
		CREATE TABLE login_source (id bigserial PRIMARY KEY, type integer NOT NULL, name text NOT NULL, is_active boolean NOT NULL, cfg text);
		CREATE TABLE "user" (id bigserial PRIMARY KEY, name text NOT NULL, type integer NOT NULL, login_source bigint NOT NULL,
			login_name text NOT NULL, is_admin boolean NOT NULL, is_active boolean NOT NULL, prohibit_login boolean NOT NULL, passwd text);
		CREATE TABLE access_token (id bigserial PRIMARY KEY, uid bigint NOT NULL, name text NOT NULL, token_hash text,
			token_salt text, token_last_eight text, scope text, created_unix bigint, updated_unix bigint);`)
	storetest.Must(t, err)
}
