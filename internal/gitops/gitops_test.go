package gitops

import (
	"encoding/json"
	"strings"
	"testing"

	"squared/internal/domain"
)

var cfg = Config{
	Org: "squared", PlatformRepo: "platform", Branch: "main",
	RepoURLBase: "http://forgejo:3000", ArgoNamespace: "argocd",
	ClusterServer: "https://kubernetes.default.svc", WorkerServiceAccount: "squared-worker",
	WorkerNamespace: "squared", IngressNamespace: "ingress",
}

func TestTenantFiles(t *testing.T) {
	tenant := &domain.Tenant{Slug: "acme", Quota: domain.Quota{CPU: "2", Memory: "4Gi", Pods: 10}}
	envs := []domain.Environment{{Name: "dev", Namespace: "acme-dev"}, {Name: "prod", Namespace: "acme-prod"}}
	files, err := cfg.TenantFiles(tenant, envs)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1+2*6 {
		t.Fatalf("got %d files", len(files))
	}
	scope := TenantScope("acme")
	for path := range files {
		if !scope(path) {
			t.Errorf("file %s is not in the tenant scope", path)
		}
	}
	if scope(AppFile("acme", "dev", "shop")) {
		t.Error("the tenant scope includes the applications")
	}
	if scope("tenants/acme2/appproject.json") || scope("tenants/README.md") {
		t.Error("the tenant scope includes files of other tenants")
	}

	var project struct {
		Spec struct {
			SourceRepos              []string         `json:"sourceRepos"`
			Destinations             []map[string]any `json:"destinations"`
			ClusterResourceWhitelist []any            `json:"clusterResourceWhitelist"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(files["tenants/acme/appproject.json"], &project); err != nil {
		t.Fatal(err)
	}
	if len(project.Spec.SourceRepos) != 1 || project.Spec.SourceRepos[0] != "http://forgejo:3000/squared/tenant-acme.git" {
		t.Errorf("source repositories %v", project.Spec.SourceRepos)
	}
	if len(project.Spec.Destinations) != 2 || project.Spec.ClusterResourceWhitelist == nil || len(project.Spec.ClusterResourceWhitelist) != 0 {
		t.Errorf("destinations or cluster whitelist not correct: %+v", project.Spec)
	}
	app := string(files["tenants/acme/appproject.json"])
	for _, want := range []string{`"tenant-acme-owner"`, `"tenant-acme-viewer"`, `"p, proj:tenant-acme:member, applications, get, tenant-acme/*, allow"`} {
		if !strings.Contains(app, want) {
			t.Errorf("project does not contain %s", want)
		}
	}
	ns := string(files["tenants/acme/dev/namespace.json"])
	if !strings.Contains(ns, `"pod-security.kubernetes.io/enforce": "restricted"`) || !strings.Contains(ns, "Prune=false") {
		t.Errorf("namespace is not protected:\n%s", ns)
	}
}

func TestApplicationFile(t *testing.T) {
	tenant := &domain.Tenant{Slug: "acme"}
	d := &domain.Deployment{ID: "id-1", Name: "shop", Environment: "dev", Namespace: "acme-dev", AppName: "acme-dev-shop"}
	if _, _, err := cfg.ApplicationFile(AppInput{Tenant: tenant, Deployment: d, Commit: "main", Revision: 1}); err == nil {
		t.Fatal("a branch name was accepted as commit")
	}
	commit := strings.Repeat("a", 40)
	path, b, err := cfg.ApplicationFile(AppInput{Tenant: tenant, Deployment: d, Commit: commit, Revision: 3})
	if err != nil {
		t.Fatal(err)
	}
	if path != "tenants/acme/apps/dev-shop.json" {
		t.Fatalf("path %s", path)
	}
	s := string(b)
	for _, want := range []string{`"targetRevision": "` + commit + `"`, `"project": "tenant-acme"`, `"squared.dev/revision": "3"`, `"path": "dev/shop"`, "resources-finalizer.argocd.argoproj.io"} {
		if !strings.Contains(s, want) {
			t.Errorf("application does not contain %s", want)
		}
	}
}
