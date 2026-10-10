package guard

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"squared/internal/store/storetest"
)

// harbor215 returns the settings as the view shows them for a Harbor 2.15
// with Keycloak sign-in.
func harbor215() map[string]string {
	return map[string]string{
		"auth_mode": "oidc_auth", "oidc_admin_group": "/squared-admins", "oidc_groups_claim": "groups",
		"oidc_auto_onboard": "", "oidc_client_id": "", "oidc_client_secret": "", "oidc_endpoint": "",
		"oidc_name": "", "oidc_scope": "", "oidc_user_claim": "", "oidc_verify_cert": "",
		"skip_audit_log_database": "",
	}
}

func TestCheckHarborConfigGroups(t *testing.T) {
	settings := harbor215()
	settings["http_authproxy_admin_groups"] = "/proxy-admins, /more"
	refs, warnings, err := checkHarborConfig(settings, nil)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("err %v, warnings %v", err, warnings)
	}
	if got := groupsOf(refs); !slices.Equal(got, []string{"/more", "/proxy-admins", "/squared-admins"}) {
		t.Fatalf("groups = %q", got)
	}

	// Without the key, Harbor has no administrator group.
	delete(settings, "oidc_admin_group")
	delete(settings, "http_authproxy_admin_groups")
	if refs, _, err := checkHarborConfig(settings, nil); err != nil || len(refs) != 0 {
		t.Fatalf("refs %v, err %v", refs, err)
	}
}

func TestCheckHarborConfigOtherMode(t *testing.T) {
	settings := harbor215()
	settings["auth_mode"] = "db_auth"
	refs, warnings, err := checkHarborConfig(settings, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The group stays protected: an administrator can change the mode back.
	if got := groupsOf(refs); !slices.Equal(got, []string{"/squared-admins"}) {
		t.Fatalf("groups = %q", got)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "db_auth") {
		t.Fatalf("warnings %v", warnings)
	}
}

func TestCheckHarborConfigNewKeys(t *testing.T) {
	// An upgrade renames the administrator group setting.
	settings := harbor215()
	settings["oidc_admin_groups"] = "/squared-admins"
	delete(settings, "oidc_admin_group")
	_, _, err := checkHarborConfig(settings, nil)
	if err == nil || !strings.Contains(err.Error(), "oidc_admin_groups can give rights") {
		t.Fatalf("a renamed setting gave no error: %v", err)
	}
	// An operator checked the key and accepted it.
	if _, _, err := checkHarborConfig(settings, []string{"oidc_admin_groups"}); err != nil {
		t.Fatal(err)
	}

	// A new key without a rights-like name gives only a warning.
	settings = harbor215()
	settings["ui_theme"] = ""
	refs, warnings, err := checkHarborConfig(settings, nil)
	if err != nil || len(refs) != 1 || len(warnings) != 1 || !strings.Contains(warnings[0], "ui_theme") {
		t.Fatalf("refs %v, warnings %v, err %v", refs, warnings, err)
	}
}

// TestHarborDatabaseSource applies deploy/kind/harbor-guard-view.sql to a
// table like the Harbor table properties, and reads it as the guard.
func TestHarborDatabaseSource(t *testing.T) {
	dbURL := storetest.NewDatabase(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dbURL)
	storetest.Must(t, err)
	defer admin.Close(ctx)

	_, err = admin.Exec(ctx, `CREATE TABLE properties (id serial PRIMARY KEY, k varchar(64) NOT NULL UNIQUE, v text NOT NULL);
		INSERT INTO properties (k, v) VALUES ('auth_mode', 'oidc_auth'), ('oidc_admin_group', '/squared-admins'),
			('oidc_client_secret', 'top-secret'), ('oidc_endpoint', 'http://kc/realms/squared'), ('oidc_groups_claim', 'groups')`)
	storetest.Must(t, err)
	viewSQL, err := os.ReadFile("../../deploy/kind/harbor-guard-view.sql")
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

	src := &harborDatabaseSource{sc: SourceConfig{Service: "Harbor", URL: u.String(), PasswordFile: passwordFile, Name: "squared_guard_harbor_config"}}
	refs, warnings, err := src.References(ctx)
	storetest.Must(t, err)
	if len(warnings) != 0 {
		t.Fatalf("warnings %v", warnings)
	}
	if got := groupsOf(refs); !slices.Equal(got, []string{"/squared-admins"}) || refs[0].Service != "Harbor" {
		t.Fatalf("refs = %+v", refs)
	}

	// The role can read the view, but not the table, and the view has no
	// secret or other value.
	guard, err := pgx.Connect(ctx, strings.Replace(u.String(), "squared_guard@", "squared_guard:guard-test-password@", 1))
	storetest.Must(t, err)
	defer guard.Close(ctx)
	if _, err := guard.Exec(ctx, "SELECT v FROM properties"); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("the guard can read the table: %v", err)
	}
	var all string
	storetest.Must(t, guard.QueryRow(ctx, "SELECT string_agg(row_to_json(v)::text, ' ') FROM squared_guard_harbor_config v").Scan(&all))
	if strings.Contains(all, "top-secret") || strings.Contains(all, "realms") {
		t.Fatalf("the view shows more than the groups and the keys: %s", all)
	}

	// A Harbor upgrade renames the setting: the source fails and names it.
	_, err = admin.Exec(ctx, `UPDATE properties SET k = 'oidc_admin_groups' WHERE k = 'oidc_admin_group'`)
	storetest.Must(t, err)
	if _, _, err := src.References(ctx); err == nil || !strings.Contains(err.Error(), "oidc_admin_groups") {
		t.Fatalf("a renamed setting gave no error: %v", err)
	}
}
