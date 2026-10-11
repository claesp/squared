package guard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const gitopsPrefix = "http://forgejo.localhost:30300/squared/"

func TestEndsInHost(t *testing.T) {
	for url, want := range map[string]bool{
		"http://forgejo.localhost:30300":         true,
		"https://":                               true,
		"https://git.example/":                   false,
		"http://forgejo.localhost:30300/squared": false,
		"git@git.example:":                       true,
		"git@git.example:squared/":               false,
	} {
		if got := endsInHost(url); got != want {
			t.Errorf("endsInHost(%q) = %v, want %v", url, got, want)
		}
	}
}

func TestCheckArgoRepositories(t *testing.T) {
	// The development environment: one template for the organization.
	dev := []argoRepo{{URL: "http://forgejo.localhost:30300/squared", Template: true}}
	if warnings, err := checkArgoRepositories(dev, []string{gitopsPrefix}, false); err != nil || len(warnings) != 0 {
		t.Fatalf("err %v, warnings %v", err, warnings)
	}
	// A repository under the prefix is also enough.
	if _, err := checkArgoRepositories([]argoRepo{{URL: gitopsPrefix + "platform"}}, []string{gitopsPrefix}, false); err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name   string
		repo   argoRepo
		errMsg string
	}{
		{"template in the host", argoRepo{URL: "http://forgejo.localhost:30300", Template: true},
			"the credential template http://forgejo.localhost:30300 ends in the host name"},
		{"insecure GitOps repository", argoRepo{URL: gitopsPrefix + "tenant-acme", Insecure: true},
			"the repository " + gitopsPrefix + "tenant-acme does not check the TLS certificate"},
		{"write repository", argoRepo{URL: gitopsPrefix + "platform", Write: true},
			"the write repository " + gitopsPrefix + "platform lets Argo CD push"},
		{"broad write template", argoRepo{URL: "http://forgejo.localhost:30300/", Template: true, Write: true},
			"the write credential template http://forgejo.localhost:30300/ lets Argo CD push"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := checkArgoRepositories(append(dev, tt.repo), []string{gitopsPrefix}, true)
			if err == nil || !strings.Contains(err.Error(), tt.errMsg) {
				t.Fatalf("err = %v, want %q", err, tt.errMsg)
			}
		})
	}

	// Another repository: only a warning for insecure, nothing else.
	other := argoRepo{URL: "https://charts.example/repo", Insecure: true}
	warnings, err := checkArgoRepositories(append(dev, other, argoRepo{URL: "https://git.example/x", Write: true}), []string{gitopsPrefix}, false)
	if err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "charts.example") {
		t.Fatalf("err %v, warnings %v", err, warnings)
	}

	// Without a visible template, the guard cannot check anything.
	if _, err := checkArgoRepositories(nil, []string{gitopsPrefix}, false); err == nil || !strings.Contains(err.Error(), "repositories, get") {
		t.Fatalf("err = %v", err)
	}
}

func TestArgoReposSource(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("guard-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer guard-token" {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/repositories":
			_, _ = w.Write([]byte(`{"items": [{"repo": "http://forgejo.localhost:30300/squared/platform", "insecure": true}]}`))
		case "/api/v1/repocreds":
			_, _ = w.Write([]byte(`{"items": [{"url": "http://forgejo.localhost:30300/squared", "username": "argocd-reader"}]}`))
		case "/api/v1/write-repositories":
			http.Error(w, `{"error": "hydrator is disabled", "code": 12}`, http.StatusNotImplemented)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	sources, err := BuildSources(&FileConfig{Groups: []string{"/a"}, Sources: []SourceConfig{{Type: "argocd-repositories",
		Service: "Argo CD", URL: srv.URL + "/", PasswordFile: tokenFile, RepoPrefixes: []string{gitopsPrefix}}}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	refs, warnings, err := sources[0].References(context.Background())
	if len(refs) != 0 || len(warnings) != 0 || err == nil || !strings.Contains(err.Error(), "platform does not check the TLS certificate") {
		t.Fatalf("refs %v, warnings %v, err %v", refs, warnings, err)
	}
}
