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

var devAppRules = argoAppRules{
	tenantProjectRules: devProjectRules,
	Namespace:          "argocd",
	RootApp:            "squared-platform",
	Root:               RootSource{RepoURL: "http://forgejo.localhost:30300/squared/platform.git", TargetRevision: "main", Path: "tenants"},
}

// rootApp returns the root application of the development environment
// (deploy/kind/argocd-platform.yaml).
func rootApp() map[string]any {
	return map[string]any{
		"metadata": map[string]any{"name": "squared-platform", "namespace": "argocd"},
		"spec": map[string]any{"project": "squared-platform",
			"source": map[string]any{"repoURL": devAppRules.Root.RepoURL, "targetRevision": "main", "path": "tenants",
				"directory": map[string]any{"recurse": true}},
			"syncPolicy": map[string]any{"automated": map[string]any{"prune": true, "selfHeal": true}}},
	}
}

// otherApp returns an application that Squared did not make.
func otherApp(name, project string, source map[string]any) map[string]any {
	return map[string]any{"metadata": map[string]any{"name": name, "namespace": "argocd"}, "spec": map[string]any{"project": project, "source": source}}
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
	if _, err := checkArgoApplications(raws(t, rootApp(), renderedApp(t)), devAppRules); err != nil {
		t.Fatalf("the applications that Squared makes have problems: %v", err)
	}
	// Another application outside the tenant projects and the GitOps
	// repositories does not matter.
	other := otherApp("monitoring", "default", map[string]any{"repoURL": "https://charts.example/", "chart": "monitoring"})
	if _, err := checkArgoApplications(raws(t, rootApp(), other), devAppRules); err != nil {
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
		}, "its source has the field plugin"},
		{"jsonnet", func(app, _ map[string]any) {
			spec(app)["source"].(map[string]any)["directory"] = map[string]any{"jsonnet": map[string]any{"libs": []any{"/"}}}
		}, "its source has the field directory"},
		{"path of another environment", func(app, _ map[string]any) {
			spec(app)["source"].(map[string]any)["path"] = "prod/shop"
		}, `its source path is "prod/shop", not the directory "dev/shop"`},
		{"path with dots", func(app, _ map[string]any) {
			spec(app)["source"].(map[string]any)["path"] = "dev/shop/../../prod/shop"
		}, `its source path is "dev/shop/../../prod/shop"`},
		{"more sources", func(app, _ map[string]any) {
			spec(app)["sources"] = []any{map[string]any{"repoURL": "https://charts.example/", "chart": "x", "ref": "values"}}
		}, "it has more than one source"},
		{"source hydrator", func(app, _ map[string]any) {
			spec(app)["sourceHydrator"] = map[string]any{"drySource": map[string]any{"repoURL": "https://git.example/x.git"}}
		}, "it has a source hydrator"},
		{"environment label", func(app, _ map[string]any) {
			app["metadata"].(map[string]any)["labels"].(map[string]any)["squared.dev/environment"] = "prod"
		}, "do not agree with its name"},
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
		{"root from another repository", func(_, root map[string]any) {
			spec(root)["source"].(map[string]any)["repoURL"] = "https://git.example/platform.git"
		}, `the root application squared-platform: its source "https://git.example/platform.git" is not the platform repository`},
		{"root from another branch", func(_, root map[string]any) {
			spec(root)["source"].(map[string]any)["targetRevision"] = "dev"
		}, `it follows "dev", not "main"`},
		{"root with a filter", func(_, root map[string]any) {
			spec(root)["source"].(map[string]any)["directory"] = map[string]any{"recurse": true, "exclude": "*/appproject.json"}
		}, "its source has the field directory.exclude"},
		{"root with Kustomize", func(_, root map[string]any) {
			spec(root)["source"].(map[string]any)["kustomize"] = map[string]any{"namePrefix": "x"}
		}, "the root application squared-platform: its source has the field kustomize"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, root := renderedApp(t), rootApp()
			tt.change(app, root)
			_, err := checkArgoApplications(raws(t, root, app), devAppRules)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}

	// An application that Squared did not make, in a tenant project.
	other := map[string]any{"metadata": map[string]any{"name": "sneaky", "namespace": "argocd"}, "spec": map[string]any{"project": "tenant-acme"}}
	if _, err := checkArgoApplications(raws(t, rootApp(), other), devAppRules); err == nil ||
		!strings.Contains(err.Error(), "the application sneaky uses the tenant project tenant-acme, but Squared did not make it") {
		t.Fatalf("err = %v", err)
	}
	// Applications that Squared did not make, with a GitOps repository:
	// in another notation, as one of more sources, or for the source
	// hydrator.
	for _, o := range []map[string]any{
		otherApp("copy", "default", map[string]any{"repoURL": "HTTP://user@Forgejo.localhost:30300/Squared/Tenant-Acme/", "path": "dev/shop"}),
		{"metadata": map[string]any{"name": "copy", "namespace": "argocd"}, "spec": map[string]any{"project": "default", "sources": []any{
			map[string]any{"repoURL": "https://charts.example/", "chart": "x"},
			map[string]any{"repoURL": "http://forgejo.localhost:30300/squared/platform", "ref": "values"}}}},
		{"metadata": map[string]any{"name": "copy", "namespace": "argocd"}, "spec": map[string]any{"project": "default", "sourceHydrator": map[string]any{
			"drySource": map[string]any{"repoURL": "http://forgejo.localhost:30300/squared/tenant-acme.git"}}}},
	} {
		if _, err := checkArgoApplications(raws(t, rootApp(), o), devAppRules); err == nil ||
			!strings.Contains(err.Error(), "the application copy uses the GitOps repository") {
			t.Errorf("err = %v", err)
		}
	}
	// Another organization with the same start of the name.
	o := otherApp("other", "default", map[string]any{"repoURL": "http://forgejo.localhost:30300/squared-tools/x.git"})
	if _, err := checkArgoApplications(raws(t, rootApp(), o), devAppRules); err != nil {
		t.Errorf("err = %v", err)
	}

	// Without the root application.
	if _, err := checkArgoApplications(nil, devAppRules); err == nil ||
		!strings.Contains(err.Error(), "does not see the root application") {
		t.Fatalf("err = %v", err)
	}
}

func TestCheckArgoApplicationsOtherNamespaces(t *testing.T) {
	inNamespace := func(o map[string]any, ns string) map[string]any {
		o["metadata"].(map[string]any)["namespace"] = ns
		return o
	}
	tests := []struct {
		name string
		app  map[string]any
		want string
	}{
		{"application of Squared", inNamespace(renderedApp(t), "team-a"), "the application team-a/acme-dev-shop of Squared is not in the namespace argocd"},
		{"root application", inNamespace(rootApp(), "team-a"), "the guard does not see the root application squared-platform"},
		{"GitOps repository", inNamespace(otherApp("copy", "team", map[string]any{"repoURL": devAppRules.Root.RepoURL}), "team-a"),
			"the application team-a/copy uses the GitOps repository"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := []map[string]any{tt.app}
			if tt.name != "root application" {
				objs = append(objs, rootApp())
			}
			_, err := checkArgoApplications(raws(t, objs...), devAppRules)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
	// Another application there is the business of the operator.
	o := inNamespace(otherApp("monitoring", "team", map[string]any{"repoURL": "https://git.example/team.git"}), "team-a")
	if _, err := checkArgoApplications(raws(t, rootApp(), o), devAppRules); err != nil {
		t.Fatal(err)
	}
}
