package guard

import (
	"encoding/json"
	"strings"
	"testing"

	"squared/internal/domain"
	"squared/internal/gitops"
)

// renderedApp returns the application that Squared makes for the
// deployment shop of tenant acme, as a generic object.
func renderedApp(t *testing.T) map[string]any {
	t.Helper()
	c := gitops.Config{Org: "squared", RepoURLBase: "http://forgejo.localhost:30300", ClusterServer: "https://kubernetes.default.svc",
		ArgoNamespace: "argocd", GroupPrefix: "tenant-"}
	d := &domain.Deployment{ID: "d1", Name: "shop", Environment: "dev", Namespace: domain.Namespace("acme", "dev"), AppName: "acme-dev-shop"}
	_, b, err := c.ApplicationFile(gitops.AppInput{Tenant: &domain.Tenant{Slug: "acme"}, Deployment: d,
		Commit: strings.Repeat("ab", 20), Revision: 3})
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(b, &obj); err != nil {
		t.Fatal(err)
	}
	return obj
}

// rootApp returns the root application of the development environment.
func rootApp() map[string]any {
	return map[string]any{
		"metadata": map[string]any{"name": "squared-platform"},
		"spec": map[string]any{"project": "squared-platform",
			"syncPolicy": map[string]any{"automated": map[string]any{"prune": true, "selfHeal": true}}},
	}
}

func raws(t *testing.T, objs ...map[string]any) []json.RawMessage {
	t.Helper()
	var out []json.RawMessage
	for _, o := range objs {
		out = append(out, projectJSON(t, o))
	}
	return out
}

func TestCheckArgoApplicationsRendered(t *testing.T) {
	if _, err := checkArgoApplications(raws(t, rootApp(), renderedApp(t)), "squared-platform", devProjectRules); err != nil {
		t.Fatalf("the applications that Squared makes have problems: %v", err)
	}
	// Another application outside the tenant projects does not matter.
	other := map[string]any{"metadata": map[string]any{"name": "monitoring"}, "spec": map[string]any{"project": "default"}}
	if _, err := checkArgoApplications(raws(t, rootApp(), other), "squared-platform", devProjectRules); err != nil {
		t.Fatal(err)
	}
}

func TestCheckArgoApplicationsChanges(t *testing.T) {
	spec := func(o map[string]any) map[string]any { return o["spec"].(map[string]any) }
	sync := func(o map[string]any) map[string]any { return spec(o)["syncPolicy"].(map[string]any) }
	tests := []struct {
		name   string
		change func(app, root map[string]any)
		want   string
	}{
		{"no self-heal", func(app, _ map[string]any) {
			sync(app)["automated"] = map[string]any{"prune": true, "selfHeal": false}
		}, "the application acme-dev-shop of tenant acme: selfHeal is off"},
		{"automation turned off", func(app, _ map[string]any) {
			sync(app)["automated"] = map[string]any{"enabled": false, "prune": true, "selfHeal": true}
		}, "it does not sync automatically"},
		{"no prune", func(app, _ map[string]any) {
			sync(app)["automated"] = map[string]any{"prune": false, "selfHeal": true}
		}, "prune is off"},
		{"manual sync", func(app, _ map[string]any) { delete(sync(app), "automated") }, "it does not sync automatically"},
		{"default project", func(app, _ map[string]any) { spec(app)["project"] = "default" }, `it uses the project "default"`},
		{"branch", func(app, _ map[string]any) { spec(app)["source"].(map[string]any)["targetRevision"] = "main" }, `it follows "main"`},
		{"other repository", func(app, _ map[string]any) {
			spec(app)["source"].(map[string]any)["repoURL"] = "https://git.example/x.git"
		}, `its source "https://git.example/x.git"`},
		{"plugin", func(app, _ map[string]any) {
			spec(app)["source"].(map[string]any)["plugin"] = map[string]any{"name": "x"}
		}, "Helm, Kustomize or a plugin"},
		{"other namespace", func(app, _ map[string]any) {
			spec(app)["destination"] = map[string]any{"server": "https://kubernetes.default.svc", "namespace": "kube-system"}
		}, `the namespace "kube-system"`},
		{"sync option", func(app, _ map[string]any) {
			sync(app)["syncOptions"] = []any{"CreateNamespace=false", "PruneLast=true", "Replace=true"}
		}, `the sync option "Replace=true"`},
		{"ignored differences", func(app, _ map[string]any) {
			spec(app)["ignoreDifferences"] = []any{map[string]any{"kind": "Deployment", "jsonPointers": []any{"/spec"}}}
		}, "it ignores differences"},
		{"root without self-heal", func(_, root map[string]any) {
			sync(root)["automated"] = map[string]any{"prune": true}
		}, "the root application squared-platform: selfHeal is off"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, root := renderedApp(t), rootApp()
			tt.change(app, root)
			_, err := checkArgoApplications(raws(t, root, app), "squared-platform", devProjectRules)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}

	// An application that Squared did not make, in a tenant project.
	other := map[string]any{"metadata": map[string]any{"name": "sneaky"}, "spec": map[string]any{"project": "tenant-acme"}}
	if _, err := checkArgoApplications(raws(t, rootApp(), other), "squared-platform", devProjectRules); err == nil ||
		!strings.Contains(err.Error(), "the application sneaky uses the tenant project tenant-acme, but Squared did not make it") {
		t.Fatalf("err = %v", err)
	}
	// Without the root application.
	if _, err := checkArgoApplications(nil, "squared-platform", devProjectRules); err == nil ||
		!strings.Contains(err.Error(), "does not see the root application") {
		t.Fatalf("err = %v", err)
	}
}
