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

const argoWebhook = "http://argocd-server.argocd.svc.cluster.local/api/webhook"

func TestCheckForgejoWebhooks(t *testing.T) {
	argo := forgejoWebhook{Scope: "owner", Owner: "squared", Type: "gitea", Active: true,
		URLSHA256: urlSHA256(argoWebhook), URLHost: "argocd-server.argocd.svc.cluster.local"}
	other := forgejoWebhook{Scope: "repository", Owner: "someone", Repo: "notes", Type: "slack", Active: true,
		URLSHA256: urlSHA256("https://hooks.example.com/secret"), URLHost: "hooks.example.com"}
	allowed := []string{argoWebhook}
	if warnings, err := checkForgejoWebhooks([]forgejoWebhook{argo, other}, "squared", allowed); err != nil || len(warnings) != 0 {
		t.Fatalf("err %v, warnings %v", err, warnings)
	}

	tests := []struct {
		name   string
		hook   forgejoWebhook
		errMsg string
	}{
		{"repository", forgejoWebhook{Scope: "repository", Owner: "squared", Repo: "platform", Type: "gitea", Active: true,
			URLSHA256: urlSHA256("http://evil.example/x"), URLHost: "evil.example"},
			"a webhook of squared/platform (gitea, active) sends events to evil.example"},
		{"inactive organization webhook", forgejoWebhook{Scope: "owner", Owner: "Squared", Type: "slack",
			URLSHA256: urlSHA256("https://hooks.example.com/t"), URLHost: "hooks.example.com"},
			"a webhook of Squared (slack, inactive) sends events to hooks.example.com"},
		{"system webhook", forgejoWebhook{Scope: "system", Type: "forgejo", Active: true,
			URLSHA256: urlSHA256("http://squared-server.squared.svc.cluster.local/x"), URLHost: "squared-server.squared.svc.cluster.local"},
			"a system webhook (all repositories) (forgejo, active) sends events to squared-server.squared.svc.cluster.local"},
		{"default webhook", forgejoWebhook{Scope: "default", Type: "gitea", Active: true, URLSHA256: urlSHA256("http://x.example/"), URLHost: "x.example"},
			"a default webhook (all repositories)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := checkForgejoWebhooks([]forgejoWebhook{argo, tt.hook}, "squared", allowed)
			if err == nil || !strings.Contains(err.Error(), tt.errMsg) {
				t.Fatalf("err = %v, want %q", err, tt.errMsg)
			}
		})
	}

	// An address with a secret can be allowed with its hash.
	secret := forgejoWebhook{Scope: "owner", Owner: "squared", Type: "slack", Active: true,
		URLSHA256: urlSHA256("https://hooks.example.com/T0/B0/token"), URLHost: "hooks.example.com"}
	hashed := append(allowed, "sha256:"+strings.ToUpper(urlSHA256("https://hooks.example.com/T0/B0/token")))
	if _, err := checkForgejoWebhooks([]forgejoWebhook{argo, secret}, "squared", hashed); err != nil {
		t.Fatal(err)
	}

	// Without the active webhook of Argo CD, Argo CD only polls.
	argo.Active = false
	warnings, err := checkForgejoWebhooks([]forgejoWebhook{argo}, "squared", allowed)
	if err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "argocd-server.argocd.svc.cluster.local") {
		t.Fatalf("err %v, warnings %v", err, warnings)
	}
}

func TestHostOf(t *testing.T) {
	for in, want := range map[string]string{
		"http://argocd-server.argocd.svc.cluster.local/api/webhook": "argocd-server.argocd.svc.cluster.local",
		"https://user:pass@Hooks.Example.com:8443/x?y#z":            "hooks.example.com",
		"sha256:00ff": "(given as a hash)",
	} {
		if got := hostOf(in); got != want {
			t.Errorf("hostOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestForgejoWebhooksSource applies deploy/kind/forgejo-guard-view.sql to
// tables like the Forgejo tables, and reads the webhooks as the guard.
func TestForgejoWebhooksSource(t *testing.T) {
	dbURL := storetest.NewDatabase(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dbURL)
	storetest.Must(t, err)
	defer admin.Close(ctx)
	makeForgejoTables(t, admin)
	_, err = admin.Exec(ctx, `
		INSERT INTO "user" (id, name, type, login_source, login_name, is_admin, is_active, prohibit_login, passwd) VALUES
			(1, 'squared', 1, 0, '', false, true, false, ''), (2, 'someone', 0, 0, '', false, true, false, '');
		INSERT INTO repository (id, owner_id, owner_name, name) VALUES (1, 1, 'squared', 'platform'), (2, 2, 'someone', 'notes');`)
	storetest.Must(t, err)
	_, err = admin.Exec(ctx, `
		INSERT INTO webhook (repo_id, owner_id, is_system_webhook, url, secret, header_authorization_encrypted, is_active, type) VALUES
			(0, 1, false, $1, 'top-secret', 'top-secret', true, 'gitea'),
			(1, 1, false, 'https://hooks.example.com/services/T0/B0/top-secret-token', 'top-secret', '', false, 'slack'),
			(0, 0, true, 'https://user:top-secret@audit.example:8443/in', '', '', true, 'forgejo'),
			(2, 2, false, 'https://notes.example/hook', '', '', true, 'gitea');`, argoWebhook)
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
	sources, err := BuildSources(&FileConfig{Groups: []string{"/a"}, Sources: []SourceConfig{{Type: "forgejo-webhooks",
		Service: "Forgejo", URL: u.String(), PasswordFile: passwordFile, Webhooks: []string{argoWebhook}}}}, nil, nil)
	storetest.Must(t, err)
	_, warnings, err := sources[0].References(ctx)
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{
		"a webhook of squared/platform (slack, inactive) sends events to hooks.example.com",
		"a system webhook (all repositories) (forgejo, active) sends events to audit.example",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error has no %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "notes") || strings.Contains(err.Error(), "argocd") || len(warnings) != 0 {
		t.Errorf("err %v, warnings %v", err, warnings)
	}

	// The role can read the view, but not the table, and the view has no
	// address, secret or user.
	guard, err := pgx.Connect(ctx, strings.Replace(u.String(), "squared_guard@", "squared_guard:guard-test-password@", 1))
	storetest.Must(t, err)
	defer guard.Close(ctx)
	if _, err := guard.Exec(ctx, "SELECT * FROM webhook"); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("the guard can read webhook: %v", err)
	}
	var all string
	storetest.Must(t, guard.QueryRow(ctx, "SELECT string_agg(row_to_json(v)::text, ' ') FROM squared_guard_webhooks v").Scan(&all))
	if strings.Contains(all, "top-secret") || strings.Contains(all, "user") || strings.Contains(all, "/api/webhook") {
		t.Fatalf("the view shows an address or a secret: %s", all)
	}
}
