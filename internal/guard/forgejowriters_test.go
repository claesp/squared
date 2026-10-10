package guard

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"squared/internal/store/storetest"
)

func TestCheckForgejoWriters(t *testing.T) {
	writers := []string{"squared-bot"}
	ok := []forgejoRepoAccess{
		{Org: "squared", Kind: "team", Via: "Owners", User: "squared-bot", Mode: 4, Repos: "*"},
		{Org: "squared", Kind: "team", Via: "readers", User: "argocd-reader", Mode: 1, Repos: "*"},
		{Org: "squared", Kind: "team", Via: "tenant-acme", User: "dev", Mode: 1, Repos: "tenant-acme"},
		{Org: "someone", Kind: "collaborator", Via: "notes", User: "dev", Mode: 2, Repos: "notes"}, // Not the organization.
	}
	if warnings, err := checkForgejoWriters(ok, "squared", writers, nil); err != nil || len(warnings) != 0 {
		t.Fatalf("err %v, warnings %v", err, warnings)
	}

	tests := []struct {
		name   string
		access forgejoRepoAccess
		errMsg string
	}{
		{"second owner", forgejoRepoAccess{Org: "squared", Kind: "team", Via: "Owners", User: "platform-admin", Mode: 4, Repos: "*"},
			"user platform-admin is in the team Owners of squared (all repositories) and can push"},
		{"write team", forgejoRepoAccess{Org: "squared", Kind: "team", Via: "ops", User: "dev", Mode: 2, Repos: "platform,tenant-acme"},
			"user dev is in the team ops of squared (platform,tenant-acme) and can push"},
		{"write collaborator", forgejoRepoAccess{Org: "squared", Kind: "collaborator", Via: "tenant-acme", User: "dev", Mode: 2, Repos: "tenant-acme"},
			"user dev is a collaborator of squared/tenant-acme and can push"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := checkForgejoWriters(append(ok, tt.access), "squared", writers, nil)
			if err == nil || !strings.Contains(err.Error(), tt.errMsg) {
				t.Fatalf("err = %v, want %q", err, tt.errMsg)
			}
			// An operator can accept it.
			if _, err := checkForgejoWriters(append(ok, tt.access), "squared", writers, []string{tt.access.key()}); err != nil {
				t.Fatal(err)
			}
		})
	}

	// A collaborator who can read is a warning.
	read := forgejoRepoAccess{Org: "squared", Kind: "collaborator", Via: "platform", User: "dev", Mode: 1, Repos: "platform"}
	warnings, err := checkForgejoWriters(append(ok, read), "squared", writers, nil)
	if err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "collaborator:platform/dev") {
		t.Fatalf("err %v, warnings %v", err, warnings)
	}
}

// TestForgejoWritersSource applies deploy/kind/forgejo-guard-view.sql to
// tables like the Forgejo tables, and reads the access as the guard.
func TestForgejoWritersSource(t *testing.T) {
	dbURL := storetest.NewDatabase(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dbURL)
	storetest.Must(t, err)
	defer admin.Close(ctx)
	makeForgejoTables(t, admin)
	_, err = admin.Exec(ctx, `
		INSERT INTO "user" (id, name, type, login_source, login_name, is_admin, is_active, prohibit_login, passwd) VALUES
			(1, 'squared', 1, 0, '', false, true, false, ''),
			(2, 'squared-bot', 0, 0, '', true, true, false, 'top-secret-hash'),
			(3, 'argocd-reader', 0, 0, '', false, true, false, ''),
			(4, 'dev', 0, 1, 'kc-dev', false, true, false, ''),
			(5, 'ops', 0, 1, 'kc-ops', false, true, false, ''),
			(6, 'boss', 0, 1, 'kc-boss', false, true, false, '');
		INSERT INTO repository (id, owner_id, owner_name, name) VALUES (1, 1, 'squared', 'platform'), (2, 1, 'squared', 'tenant-acme');
		INSERT INTO team (id, org_id, name, authorize, includes_all_repositories) VALUES
			(1, 1, 'Owners', 4, true), (2, 1, 'readers', 1, true), (3, 1, 'tenant-acme', 1, false),
			(4, 1, 'ops', 1, false), (5, 1, 'admins', 3, false);
		INSERT INTO team_unit (org_id, team_id, type, access_mode) VALUES
			(1, 2, 1, 1), (1, 3, 1, 1), (1, 3, 3, 2), -- tenant-acme writes issues (type 3), not code.
			(1, 4, 1, 2);
		INSERT INTO team_repo (org_id, team_id, repo_id) VALUES (1, 3, 2), (1, 4, 1), (1, 4, 2), (1, 5, 2);
		INSERT INTO team_user (org_id, team_id, uid) VALUES (1, 1, 2), (1, 2, 3), (1, 3, 4), (1, 4, 5), (1, 5, 6);
		INSERT INTO collaboration (repo_id, user_id, mode) VALUES (1, 4, 1);`)
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
	sources, err := BuildSources(&FileConfig{Groups: []string{"/a"}, Sources: []SourceConfig{{Type: "forgejo-writers",
		Service: "Forgejo", URL: u.String(), PasswordFile: passwordFile, Writers: []string{"squared-bot"}}}}, nil, nil)
	storetest.Must(t, err)
	_, warnings, err := sources[0].References(ctx)
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{
		"user boss is in the team admins of squared (tenant-acme) and can push", // Admin access gives all units.
		"user ops is in the team ops of squared (platform,tenant-acme) and can push",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error has no %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "squared-bot") || strings.Contains(err.Error(), "argocd-reader") || strings.Contains(err.Error(), "user dev") {
		t.Errorf("the error names an expected access: %v", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "user dev is a collaborator of squared/platform and can read it") {
		t.Errorf("warnings = %v", warnings)
	}

	// The role can read the view, but not the tables.
	guard, err := pgx.Connect(ctx, strings.Replace(u.String(), "squared_guard@", "squared_guard:guard-test-password@", 1))
	storetest.Must(t, err)
	defer guard.Close(ctx)
	for _, table := range []string{"team", "team_user", "team_unit", "team_repo", "collaboration"} {
		if _, err := guard.Exec(ctx, "SELECT * FROM "+table); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("the guard can read %s: %v", table, err)
		}
	}
}
