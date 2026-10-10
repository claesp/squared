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

func TestCheckForgejoDeployKeys(t *testing.T) {
	day := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	key := func(owner, repo, name string, mode int) forgejoDeployKey {
		return forgejoDeployKey{Owner: owner, Repo: repo, Name: name, Mode: mode, Created: day, LastUsed: day}
	}
	if warnings, err := checkForgejoDeployKeys(nil, "squared", nil); err != nil || len(warnings) != 0 {
		t.Fatalf("err %v, warnings %v", err, warnings)
	}

	// A key that can push to a GitOps repository is an error.
	keys := []forgejoDeployKey{key("squared", "tenant-acme", "ci", forgejoAccessWrite)}
	_, err := checkForgejoDeployKeys(keys, "squared", nil)
	if err == nil || !strings.Contains(err.Error(), `deploy key "ci" of squared/tenant-acme (made 2026-10-01, last use 2026-10-01) can push`) {
		t.Fatalf("err = %v", err)
	}
	// An operator can accept it.
	if _, err := checkForgejoDeployKeys(keys, "squared", []string{"tenant-acme/ci"}); err != nil {
		t.Fatal(err)
	}

	// A key that can read is a warning. Keys of other owners do not matter.
	keys = []forgejoDeployKey{key("Squared", "platform", "backup", 1), key("someone", "notes", "laptop", forgejoAccessWrite)}
	warnings, err := checkForgejoDeployKeys(keys, "squared", nil)
	if err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], `deploy key "backup" of Squared/platform`) {
		t.Fatalf("err %v, warnings %v", err, warnings)
	}
}

// TestForgejoDeployKeysSource applies deploy/kind/forgejo-guard-view.sql to
// tables like the Forgejo tables, and reads the deploy keys as the guard.
func TestForgejoDeployKeysSource(t *testing.T) {
	dbURL := storetest.NewDatabase(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dbURL)
	storetest.Must(t, err)
	defer admin.Close(ctx)
	makeForgejoTables(t, admin)
	_, err = admin.Exec(ctx, `
		INSERT INTO repository (owner_id, owner_name, name) VALUES (1, 'squared', 'platform'), (1, 'squared', 'tenant-acme'), (2, 'someone', 'notes');
		INSERT INTO public_key (owner_id, name, fingerprint, content, type, created_unix, updated_unix) VALUES
			(0, 'push', 'SHA256:top-secret-fingerprint', 'ssh-ed25519 top-secret-key', 2, 1790000000, 1790500000),
			(0, 'read', 'SHA256:top-secret-fingerprint', 'ssh-ed25519 top-secret-key', 2, 1790000000, 1790000000),
			(0, 'other', 'SHA256:top-secret-fingerprint', 'ssh-ed25519 top-secret-key', 2, 1790000000, 1790000000);
		INSERT INTO deploy_key (key_id, repo_id, name, fingerprint, mode, created_unix, updated_unix) VALUES
			(1, 1, 'push', 'SHA256:top-secret-fingerprint', 2, 1790000000, 1790000000),
			(2, 2, 'read', 'SHA256:top-secret-fingerprint', 1, 1790000000, 1790000000),
			(3, 3, 'other', 'SHA256:top-secret-fingerprint', 2, 1790000000, 1790000000);`)
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
	sources, err := BuildSources(&FileConfig{Groups: []string{"/a"}, Sources: []SourceConfig{{Type: "forgejo-deploy-keys",
		Service: "Forgejo", URL: u.String(), PasswordFile: passwordFile}}}, nil, nil)
	storetest.Must(t, err)
	_, warnings, err := sources[0].References(ctx)
	// The last use comes from the key, which Forgejo updates at each use.
	if err == nil || !strings.Contains(err.Error(), `deploy key "push" of squared/platform (made 2026-09-21, last use 2026-09-27) can push`) ||
		strings.Contains(err.Error(), "notes") {
		t.Fatalf("err = %v", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], `deploy key "read" of squared/tenant-acme`) {
		t.Fatalf("warnings = %v", warnings)
	}

	// The role can read the view, but not the tables, and the view has no
	// key or fingerprint.
	guard, err := pgx.Connect(ctx, strings.Replace(u.String(), "squared_guard@", "squared_guard:guard-test-password@", 1))
	storetest.Must(t, err)
	defer guard.Close(ctx)
	for _, table := range []string{"deploy_key", "repository", "public_key"} {
		if _, err := guard.Exec(ctx, "SELECT * FROM "+table); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("the guard can read %s: %v", table, err)
		}
	}
	var all string
	storetest.Must(t, guard.QueryRow(ctx, "SELECT string_agg(row_to_json(v)::text, ' ') FROM squared_guard_deploy_keys v").Scan(&all))
	if strings.Contains(all, "top-secret") {
		t.Fatalf("the view shows a key or a fingerprint: %s", all)
	}
}
