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

func TestCheckArgoAppSets(t *testing.T) {
	if p, err := checkArgoAppSets([]json.RawMessage{appSet(t, nil)}, devAppRules); err != nil || len(p) != 0 {
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
		{"template patch", func(s map[string]any) { s["templatePatch"] = "spec:\n  project: tenant-acme\n" }, "it has a templatePatch"},
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
		{"template of a generator", func(s map[string]any) {
			s["generators"] = []any{map[string]any{"list": map[string]any{"elements": []any{},
				"template": map[string]any{"metadata": map[string]any{}, "spec": map[string]any{"project": "tenant-acme"}}}}}
		}, "in the template of a generator, its applications use the tenant project tenant-acme"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := checkArgoAppSets([]json.RawMessage{appSet(t, tt.change)}, devAppRules)
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
	if p, err := checkArgoAppSets([]json.RawMessage{other}, devAppRules); err != nil || len(p) != 0 {
		t.Fatalf("problems = %q, err %v", p, err)
	}
}
