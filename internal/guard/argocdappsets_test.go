package guard

import (
	"encoding/json"
	"strings"
	"testing"
)

// appSet returns an ApplicationSet from JSON, with a list generator and a
// template of another team.
func appSet(t *testing.T, change func(spec map[string]any)) json.RawMessage {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal([]byte(`{"metadata": {"name": "team", "namespace": "argocd"}, "spec": {
		"goTemplate": true,
		"syncPolicy": {"applicationsSync": "create-only", "preserveResourcesOnDeletion": true},
		"generators": [{"list": {"elements": [{"env": "dev"}, {"env": "prod", "template": "x"}]}}],
		"template": {
			"metadata": {"name": "team-{{.env}}", "labels": {"team": "a"}},
			"spec": {"project": "team",
				"source": {"repoURL": "https://git.example/team.git", "targetRevision": "main", "path": "{{.env}}"},
				"destination": {"server": "https://kubernetes.default.svc", "namespace": "team-{{.env}}"}}}}}`), &obj); err != nil {
		t.Fatal(err)
	}
	if change != nil {
		change(obj["spec"].(map[string]any))
	}
	return projectJSON(t, obj)
}

// squaredNames are the applications of Squared in the tests.
var squaredNames = map[string]bool{"squared-platform": true, "acme-dev-shop": true}

func TestCheckArgoAppSets(t *testing.T) {
	if p, _, _, err := checkArgoAppSets([]json.RawMessage{appSet(t, nil)}, devAppRules, squaredNames, argoSetPolicy{}); err != nil || len(p) != 0 {
		t.Fatalf("problems = %q, err %v", p, err)
	}

	template := func(s map[string]any) map[string]any { return s["template"].(map[string]any) }
	tspec := func(s map[string]any) map[string]any { return template(s)["spec"].(map[string]any) }
	tests := []struct {
		name   string
		change func(spec map[string]any)
		want   string
	}{
		{"tenant project", func(s map[string]any) { tspec(s)["project"] = "tenant-acme" }, "its applications use the tenant project tenant-acme"},
		{"project from the generators", func(s map[string]any) { tspec(s)["project"] = "{{.project}}" },
			`the project "{{.project}}" of its applications comes from the generators`},
		{"GitOps repository", func(s map[string]any) {
			tspec(s)["source"].(map[string]any)["repoURL"] = "http://forgejo.localhost:30300/squared/tenant-acme"
		}, "its applications use the GitOps repository http://forgejo.localhost:30300/squared/tenant-acme"},
		{"repository from the generators", func(s map[string]any) {
			tspec(s)["source"].(map[string]any)["repoURL"] = "{{.url}}"
		}, `the repository "{{.url}}" of its applications comes from the generators`},
		{"GitOps repository as one of more sources", func(s map[string]any) {
			tspec(s)["sources"] = []any{map[string]any{"repoURL": devAppRules.Root.RepoURL, "ref": "values"}}
		}, "its applications use the GitOps repository " + devAppRules.Root.RepoURL},
		{"label of Squared", func(s map[string]any) {
			template(s)["metadata"].(map[string]any)["labels"] = map[string]any{"squared.dev/managed": "true"}
		}, "its applications get the label squared.dev/managed"},
		{"no preserveResourcesOnDeletion", func(s map[string]any) { delete(s, "syncPolicy") }, "preserveResourcesOnDeletion is not true"},
		{"preserveResourcesOnDeletion off", func(s map[string]any) {
			s["syncPolicy"] = map[string]any{"applicationsSync": "create-only", "preserveResourcesOnDeletion": false}
		}, "preserveResourcesOnDeletion is not true"},
		{"name of an application of Squared", func(s map[string]any) {
			template(s)["metadata"].(map[string]any)["name"] = "acme-dev-shop"
		}, "it makes the application acme-dev-shop, thus it takes over the application of Squared"},
		{"name of the root application in a generator template", func(s map[string]any) {
			s["generators"] = []any{map[string]any{"list": map[string]any{"elements": []any{},
				"template": map[string]any{"metadata": map[string]any{"name": "squared-platform"}, "spec": map[string]any{}}}}}
		}, "in the template of a list generator, it makes the application squared-platform"},
		{"template patch", func(s map[string]any) { s["templatePatch"] = "spec:\n  project: tenant-acme\n" },
			"in its templatePatch, its applications use the tenant project tenant-acme"},
		{"finalizer", func(s map[string]any) {
			template(s)["metadata"].(map[string]any)["finalizers"] = []any{"resources-finalizer.argocd.argoproj.io/background"}
		}, "its applications get the finalizer resources-finalizer.argocd.argoproj.io/background"},
		{"Git generator", func(s map[string]any) {
			s["generators"] = []any{map[string]any{"git": map[string]any{"repoURL": devAppRules.Root.RepoURL, "revision": "main",
				"directories": []any{map[string]any{"path": "tenants/*"}}}}}
		}, "a generator reads the GitOps repository " + devAppRules.Root.RepoURL},
		{"Git generator in a matrix", func(s map[string]any) {
			s["generators"] = []any{map[string]any{"matrix": map[string]any{"generators": []any{
				map[string]any{"list": map[string]any{"elements": []any{map[string]any{"url": "x"}}}},
				map[string]any{"git": map[string]any{"repoURL": "{{.url}}", "revision": "main"}}}}}}
		}, `a generator reads the repository "{{.url}}", which comes from other generators`},
		{"Forgejo SCM provider", func(s map[string]any) {
			s["generators"] = []any{map[string]any{"scmProvider": map[string]any{"gitea": map[string]any{
				"api": "http://forgejo.localhost:30300/", "owner": "squared", "allBranches": true}}}}
		}, "a generator reads the GitOps repositories of squared at http://forgejo.localhost:30300/"},
		{"credentials without TLS checks", func(s map[string]any) {
			s["generators"] = []any{map[string]any{"pullRequest": map[string]any{"gitea": map[string]any{
				"api": "https://git.example", "owner": "team", "repo": "app", "insecure": true,
				"tokenRef": map[string]any{"secretName": "team-token", "key": "token"}}}}}
		}, "a generator sends credentials to https://git.example without the check of the TLS certificate"},
		{"credentials without TLS", func(s map[string]any) {
			s["generators"] = []any{map[string]any{"scmProvider": map[string]any{"gitlab": map[string]any{
				"api": "http://gitlab.example", "group": "team", "tokenRef": map[string]any{"secretName": "t", "key": "k"}}}}}
		}, "a generator sends credentials to http://gitlab.example without TLS"},
		{"template of a generator", func(s map[string]any) {
			s["generators"] = []any{map[string]any{"list": map[string]any{"elements": []any{},
				"template": map[string]any{"metadata": map[string]any{}, "spec": map[string]any{"project": "tenant-acme"}}}}}
		}, "in the template of a list generator, its applications use the tenant project tenant-acme"},
		{"template of a generator in a matrix", func(s map[string]any) {
			s["generators"] = []any{map[string]any{"matrix": map[string]any{"generators": []any{
				map[string]any{"clusters": map[string]any{}},
				map[string]any{"git": map[string]any{"repoURL": "https://git.example/team.git", "revision": "main",
					"template": map[string]any{"metadata": map[string]any{"finalizers": []any{"resources-finalizer.argocd.argoproj.io"}}, "spec": map[string]any{}}}}}}}}
		}, "in the template of a git generator, its applications get the finalizer resources-finalizer.argocd.argoproj.io"},
		{"template of a generator in a nested merge", func(s map[string]any) {
			s["generators"] = []any{map[string]any{"matrix": map[string]any{"generators": []any{
				map[string]any{"list": map[string]any{"elements": []any{}}},
				map[string]any{"merge": map[string]any{"mergeKeys": []any{"x"}, "generators": []any{
					map[string]any{"list": map[string]any{"elements": []any{},
						"template": map[string]any{"spec": map[string]any{"source": map[string]any{"repoURL": "{{.url}}"}}}}}}}}}}}}
		}, `in the template of a list generator, the repository "{{.url}}" of its applications comes from the generators`},
		{"template of the matrix generator", func(s map[string]any) {
			s["generators"] = []any{map[string]any{"matrix": map[string]any{"generators": []any{},
				"template": map[string]any{"metadata": map[string]any{"name": "squared-platform"}}}}}
		}, "in the template of a matrix generator, it makes the application squared-platform"},
		{"template that is not an object", func(s map[string]any) {
			s["generators"] = []any{map[string]any{"git": map[string]any{"repoURL": "https://git.example/team.git", "template": "x"}}}
		}, "in the template of a git generator, the guard cannot read it"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, _, _, err := checkArgoAppSets([]json.RawMessage{appSet(t, tt.change)}, devAppRules, squaredNames, argoSetPolicy{})
			if err != nil || !strings.Contains(strings.Join(p, " | "), "the ApplicationSet team: "+tt.want) {
				t.Fatalf("problems = %q, err %v, want %q", p, err, tt.want)
			}
			for _, m := range p {
				if strings.Contains(m, ";") {
					t.Errorf("the message has a semicolon: %q", m)
				}
			}
		})
	}

	// Another SCM provider of the same server.
	other := appSet(t, func(s map[string]any) {
		s["generators"] = []any{map[string]any{"scmProvider": map[string]any{"gitea": map[string]any{
			"api": "http://forgejo.localhost:30300", "owner": "team"}}}}
	})
	if p, _, _, err := checkArgoAppSets([]json.RawMessage{other}, devAppRules, squaredNames, argoSetPolicy{}); err != nil || len(p) != 0 {
		t.Fatalf("problems = %q, err %v", p, err)
	}
}

// TestCheckArgoAppSetsOtherNamespace checks that an ApplicationSet in
// another namespace does not take over the applications of Squared: it
// makes applications in its own namespace.
func TestCheckArgoAppSetsOtherNamespace(t *testing.T) {
	var obj map[string]any
	if err := json.Unmarshal(appSet(t, func(s map[string]any) {
		s["template"].(map[string]any)["metadata"].(map[string]any)["name"] = "acme-dev-shop"
	}), &obj); err != nil {
		t.Fatal(err)
	}
	obj["metadata"].(map[string]any)["namespace"] = "team-a"
	if p, _, _, err := checkArgoAppSets([]json.RawMessage{projectJSON(t, obj)}, devAppRules, squaredNames, argoSetPolicy{}); err != nil || len(p) != 0 {
		t.Fatalf("problems = %q, err %v", p, err)
	}
}

// TestCheckArgoAppSetsParameters checks that the parameters of a list
// generator are not templates and not addresses that it reads.
func TestCheckArgoAppSetsParameters(t *testing.T) {
	set := appSet(t, func(s map[string]any) {
		s["generators"] = []any{map[string]any{"list": map[string]any{
			"elements": []any{map[string]any{"template": map[string]any{"metadata": map[string]any{"name": "acme-dev-shop"}},
				"repoURL": "http://forgejo.localhost:30300/squared/platform.git"}},
			"elementsYaml": "- repoURL: http://forgejo.localhost:30300/squared/platform.git"}}}
	})
	if p, w, _, err := checkArgoAppSets([]json.RawMessage{set}, devAppRules, squaredNames, argoSetPolicy{}); err != nil || len(p) != 0 || len(w) != 0 {
		t.Fatalf("problems = %q, warnings = %q, err %v", p, w, err)
	}
}

// TestCheckArgoAppSetsUnknownGenerator checks that a generator of a newer
// Argo CD gives a warning.
func TestCheckArgoAppSetsUnknownGenerator(t *testing.T) {
	set := appSet(t, func(s map[string]any) {
		s["generators"] = []any{map[string]any{"oci": map[string]any{"repoURL": "oci://registry.example/x"}, "selector": map[string]any{}}}
	})
	p, w, _, err := checkArgoAppSets([]json.RawMessage{set}, devAppRules, squaredNames, argoSetPolicy{})
	if err != nil || len(p) != 0 || len(w) != 1 || !strings.Contains(w[0], "the ApplicationSet team: a generator has the kind oci, which the guard does not know") {
		t.Fatalf("problems = %q, warnings = %q, err %v", p, w, err)
	}
}
