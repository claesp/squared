package guard

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"squared/internal/store/storetest"
)

// squaredRule returns the rule that the worker makes.
func squaredRule(repo string) forgejoBranchRule {
	return forgejoBranchRule{Owner: "squared", Repo: repo, Branch: "main", Protected: true, CanPush: true, EnableWhitelist: true,
		PushUsers: []string{"squared-bot"}, EnableMergeWhitelist: true, MergeUsers: []string{"squared-bot"}, ApplyToAdmins: true}
}

func TestCheckForgejoBranches(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	writers := []string{"squared-bot"}
	ok := []forgejoBranchRule{squaredRule("platform"), squaredRule("tenant-acme"),
		{Owner: "someone", Repo: "notes", Branch: "main"},                      // Not the organization.
		{Owner: "squared", Repo: "tenant-old", Branch: "main", Archived: true}} // Forgejo refuses pushes.
	if warnings, err := checkForgejoBranches(ok, "squared", writers, nil, now); err != nil || len(warnings) != 0 {
		t.Fatalf("err %v, warnings %v", err, warnings)
	}

	tests := []struct {
		name   string
		change func(r *forgejoBranchRule)
		errMsg string
	}{
		{"no rule", func(r *forgejoBranchRule) {
			*r = forgejoBranchRule{Owner: "squared", Repo: "tenant-acme", Branch: "main"}
		},
			"branch main of squared/tenant-acme has no protection rule"},
		{"nobody can push", func(r *forgejoBranchRule) { r.CanPush = false }, "nobody can push, also not Squared"},
		{"no push list", func(r *forgejoBranchRule) { r.EnableWhitelist = false }, "everybody with write access can push"},
		{"other pusher", func(r *forgejoBranchRule) { r.PushUsers = []string{"dev", "squared-bot"} }, "the users dev can push"},
		{"team pushes", func(r *forgejoBranchRule) { r.PushTeams = 1 }, "1 teams can push"},
		{"deploy keys", func(r *forgejoBranchRule) { r.PushDeployKeys = true }, "deploy keys with write access can push"},
		{"Squared cannot push", func(r *forgejoBranchRule) { r.PushUsers = nil }, "Squared (squared-bot) cannot push"},
		{"no merge list", func(r *forgejoBranchRule) { r.EnableMergeWhitelist = false }, "everybody with write access can merge"},
		{"other merger", func(r *forgejoBranchRule) { r.MergeUsers = []string{"ops", "squared-bot"} }, "the users ops can merge"},
		{"administrators", func(r *forgejoBranchRule) { r.ApplyToAdmins = false }, "the rule does not apply to Forgejo administrators"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rules := append([]forgejoBranchRule{}, ok...)
			tt.change(&rules[1])
			_, err := checkForgejoBranches(rules, "squared", writers, nil, now)
			if err == nil || !strings.Contains(err.Error(), tt.errMsg) || !strings.Contains(err.Error(), "squared/tenant-acme") {
				t.Fatalf("err = %v, want %q", err, tt.errMsg)
			}
			// An operator can accept a repository.
			if _, err := checkForgejoBranches(rules, "squared", writers, []string{"tenant-acme"}, now); err != nil {
				t.Fatal(err)
			}
		})
	}

	// A new repository gets a short time.
	fresh := forgejoBranchRule{Owner: "squared", Repo: "tenant-new", Branch: "main", RepoCreated: now.Add(-time.Minute)}
	warnings, err := checkForgejoBranches(append(ok, fresh), "squared", writers, nil, now)
	if err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "squared/tenant-new is new") {
		t.Fatalf("err %v, warnings %v", err, warnings)
	}
	fresh.RepoCreated = now.Add(-10 * time.Minute)
	if _, err := checkForgejoBranches(append(ok, fresh), "squared", writers, nil, now); err == nil {
		t.Fatal("an old repository without a rule gave no error")
	}
}

// TestForgejoBranchesSource applies deploy/kind/forgejo-guard-view.sql to
// tables like the Forgejo tables, and reads the rules as the guard.
func TestForgejoBranchesSource(t *testing.T) {
	dbURL := storetest.NewDatabase(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dbURL)
	storetest.Must(t, err)
	defer admin.Close(ctx)
	makeForgejoTables(t, admin)
	_, err = admin.Exec(ctx, `
		INSERT INTO "user" (id, name, type, login_source, login_name, is_admin, is_active, prohibit_login, passwd) VALUES
			(1, 'squared', 1, 0, '', false, true, false, ''), (2, 'squared-bot', 0, 0, '', true, true, false, ''),
			(3, 'dev', 0, 1, 'kc-dev', false, true, false, '');
		INSERT INTO repository (id, owner_id, owner_name, name, is_archived, created_unix) VALUES
			(1, 1, 'squared', 'platform', false, 1), (2, 1, 'squared', 'tenant-acme', false, 1),
			(3, 1, 'squared', 'tenant-old', true, 1), (4, 1, 'squared', 'tenant-weak', false, 1);
		INSERT INTO protected_branch (repo_id, branch_name, can_push, enable_whitelist, whitelist_user_i_ds, whitelist_team_i_ds,
			enable_merge_whitelist, whitelist_deploy_keys, merge_whitelist_user_i_ds, merge_whitelist_team_i_ds, apply_to_admins) VALUES
			(1, 'main', true, true, '[2]', 'null', true, false, '[2]', NULL, true),
			(2, 'main*', true, true, '[2]', '[]', true, false, '[2]', '[]', true), -- A pattern, not the name of the branch.
			(4, 'main', true, true, '[2,3]', '[7]', true, false, '[2]', '[]', false);`)
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
	sources, err := BuildSources(&FileConfig{Groups: []string{"/a"}, Sources: []SourceConfig{{Type: "forgejo-branches",
		Service: "Forgejo", URL: u.String(), PasswordFile: passwordFile, Writers: []string{"squared-bot"}}}}, nil, nil)
	storetest.Must(t, err)
	_, _, err = sources[0].References(ctx)
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{
		"branch main of squared/tenant-acme has no protection rule",
		"branch main of squared/tenant-weak: the users dev can push, 1 teams can push, the rule does not apply to Forgejo administrators",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error has no %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "squared/platform") || strings.Contains(err.Error(), "tenant-old") {
		t.Errorf("the error names a protected or archived repository: %v", err)
	}

	guard, err := pgx.Connect(ctx, strings.Replace(u.String(), "squared_guard@", "squared_guard:guard-test-password@", 1))
	storetest.Must(t, err)
	defer guard.Close(ctx)
	if _, err := guard.Exec(ctx, "SELECT * FROM protected_branch"); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("the guard can read protected_branch: %v", err)
	}
}
