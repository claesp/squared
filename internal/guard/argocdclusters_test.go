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

func TestCheckArgoClusters(t *testing.T) {
	in := argoCluster{Server: argoInCluster, Name: "in-cluster"}
	servers := []string{argoInCluster}
	if warnings, err := checkArgoClusters([]argoCluster{in}, servers); err != nil || len(warnings) != 0 {
		t.Fatalf("err %v, warnings %v", err, warnings)
	}

	insecure := in
	insecure.Insecure = true
	if _, err := checkArgoClusters([]argoCluster{insecure}, servers); err == nil ||
		!strings.Contains(err.Error(), "the cluster https://kubernetes.default.svc (in-cluster) does not check the TLS certificate") {
		t.Fatalf("err = %v", err)
	}

	// Another cluster gets warnings, not errors.
	other := argoCluster{Server: "http://edge.example:6443", Name: "edge", Insecure: true}
	warnings, err := checkArgoClusters([]argoCluster{in, other}, servers)
	if err != nil || len(warnings) != 2 || !strings.Contains(warnings[0], "edge") || !strings.Contains(warnings[1], "connects without TLS") {
		t.Fatalf("err %v, warnings %q", err, warnings)
	}

	// A Secret for the address of the own cluster.
	secret := argoCluster{Server: argoInCluster, Name: "admin-token"}
	warnings, err = checkArgoClusters([]argoCluster{secret}, servers)
	if err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], `cluster Secret "admin-token"`) {
		t.Fatalf("err %v, warnings %q", err, warnings)
	}

	// Without the cluster of Squared, the guard cannot check it.
	if _, err := checkArgoClusters(nil, servers); err == nil || !strings.Contains(err.Error(), `"clusters, get"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestArgoClustersSource(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("guard-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer guard-token" || r.URL.Path != "/api/v1/clusters" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items": [{"server": "https://kubernetes.default.svc", "name": "in-cluster", "config": {"tlsClientConfig": {"insecure": false}}},
			{"server": "https://edge.example:6443", "name": "edge", "project": "ops", "config": {"tlsClientConfig": {"insecure": true}}}]}`))
	}))
	defer srv.Close()
	sources, err := BuildSources(&FileConfig{Groups: []string{"/a"}, Sources: []SourceConfig{{Type: "argocd-clusters",
		Service: "Argo CD", URL: srv.URL, PasswordFile: tokenFile, ClusterServers: []string{argoInCluster}}}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	refs, warnings, err := sources[0].References(context.Background())
	if len(refs) != 0 || err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "edge.example") {
		t.Fatalf("refs %v, warnings %v, err %v", refs, warnings, err)
	}
}
