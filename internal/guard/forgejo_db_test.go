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

	// The columns of the Forgejo table that the view uses.
	_, err = admin.Exec(ctx, `CREATE TABLE login_source (
		id bigserial PRIMARY KEY, type integer NOT NULL, name text NOT NULL,
		is_active boolean NOT NULL, cfg text)`)
	storetest.Must(t, err)
	_, err = admin.Exec(ctx, `INSERT INTO login_source (type, name, is_active, cfg) VALUES
		(6, 'keycloak', true, '{"Provider":"openidConnect","ClientID":"forgejo","ClientSecret":"top-secret",
		  "AdminGroup":"/squared-admins","RestrictedGroup":"/guests","GroupTeamMap":"{\"/ops\": {\"squared\": [\"operators\"]}}"}'),
		(2, 'ldap', true, 'not JSON: the view must ignore other types'),
		(6, 'old', false, '{"AdminGroup":"/old admins"}')`)
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
	refs, err := src.References(ctx)
	storetest.Must(t, err)
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
	if strings.Contains(all, "top-secret") || strings.Contains(all, "ClientID") {
		t.Fatalf("the view shows more than the groups: %s", all)
	}
}
