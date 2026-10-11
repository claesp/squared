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

func TestCheckForgejoMirrors(t *testing.T) {
	ok := []forgejoMirror{
		{Owner: "someone", Repo: "notes", Kind: "pull"},                                       // Not the organization.
		{Owner: "squared", Repo: "tenant-old", Archived: true, Kind: "push", RemoteHost: "x"}, // Archived.
	}
	if warnings, err := checkForgejoMirrors(ok, "squared", nil); err != nil || len(warnings) != 0 {
		t.Fatalf("err %v, warnings %v", err, warnings)
	}
	for _, tt := range []struct {
		mirror forgejoMirror
		errMsg string
	}{
		{forgejoMirror{Owner: "squared", Repo: "tenant-acme", Kind: "pull"}, "squared/tenant-acme is a pull mirror"},
		{forgejoMirror{Owner: "Squared", Repo: "platform", Kind: "push", RemoteHost: "backup.example"},
			"Squared/platform has a push mirror to backup.example"},
	} {
		_, err := checkForgejoMirrors(append(ok, tt.mirror), "squared", nil)
		if err == nil || !strings.Contains(err.Error(), tt.errMsg) {
			t.Fatalf("err = %v, want %q", err, tt.errMsg)
		}
		if _, err := checkForgejoMirrors(append(ok, tt.mirror), "squared", []string{tt.mirror.Repo + "/" + tt.mirror.Kind}); err != nil {
			t.Fatalf("ignoreFields: %v", err)
		}
	}
}

// TestForgejoMirrorsSource applies deploy/kind/forgejo-guard-view.sql to
// tables like the Forgejo tables, and reads the mirrors as the guard.
func TestForgejoMirrorsSource(t *testing.T) {
	dbURL := storetest.NewDatabase(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dbURL)
	storetest.Must(t, err)
	defer admin.Close(ctx)
	makeForgejoTables(t, admin)
	_, err = admin.Exec(ctx, `
		INSERT INTO repository (id, owner_id, owner_name, name, is_mirror) VALUES
			(1, 1, 'squared', 'platform', false), (2, 1, 'squared', 'tenant-acme', true), (3, 1, 'squared', 'tenant-beta', false);
		INSERT INTO mirror (repo_id, encrypted_remote_address) VALUES (2, 'top-secret-encrypted');
		INSERT INTO push_mirror (repo_id, remote_name, remote_address, private_key, sync_on_commit, "interval") VALUES
			(1, 'remote_mirror_x', 'https://user:top-secret@Backup.Example:8443/squared/platform.git', 'top-secret-key', false, 0),
			(3, 'remote_mirror_y', 'git@ssh.example:squared/beta.git', 'top-secret-key', false, 0);`)
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
	sources, err := BuildSources(&FileConfig{Groups: []string{"/a"}, Sources: []SourceConfig{{Type: "forgejo-mirrors",
		Service: "Forgejo", URL: u.String(), PasswordFile: passwordFile}}}, nil, nil)
	storetest.Must(t, err)
	_, _, err = sources[0].References(ctx)
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{
		"squared/platform has a push mirror to backup.example",
		"squared/tenant-acme is a pull mirror",
		"squared/tenant-beta has a push mirror to ssh.example",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error has no %q: %v", want, err)
		}
	}

	guard, err := pgx.Connect(ctx, strings.Replace(u.String(), "squared_guard@", "squared_guard:guard-test-password@", 1))
	storetest.Must(t, err)
	defer guard.Close(ctx)
	for _, table := range []string{"mirror", "push_mirror"} {
		if _, err := guard.Exec(ctx, "SELECT * FROM "+table); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("the guard can read %s: %v", table, err)
		}
	}
	var all string
	storetest.Must(t, guard.QueryRow(ctx, "SELECT string_agg(row_to_json(v)::text, ' ') FROM squared_guard_mirrors v").Scan(&all))
	if strings.Contains(all, "top-secret") || strings.Contains(all, "user") || strings.Contains(all, ".git") {
		t.Fatalf("the view shows an address, a password or a key: %s", all)
	}
}
