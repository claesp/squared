package guard

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestCheckArgoPlugin(t *testing.T) {
	for _, data := range []map[string]string{
		// The example of the documentation of Argo CD.
		{"token": "$plugin.myplugin.token", "baseUrl": "http://myplugin.plugin-ns.svc.cluster.local."},
		{"token": "$my-plugin:plugin.myplugin.token", "baseUrl": "http://myplugin.argocd.svc:8080"},
		{"token": "$plugin.myplugin.token", "baseUrl": "https://plugin.example"},
		{"token": "$plugin.myplugin.token", "baseUrl": "http://myplugin:8080"},
		{"baseUrl": "http://plugin.example"},
	} {
		if p := checkArgoPlugin(data); len(p) != 0 {
			t.Errorf("%v: problems = %q", data, p)
		}
	}

	tests := []struct {
		name string
		data map[string]string
		want string
	}{
		{"client secret of Keycloak", map[string]string{"token": "$oidc.keycloak.clientSecret", "baseUrl": "https://plugin.example"},
			"its token is the key oidc.keycloak.clientSecret of the Secret argocd-secret, which does not start with plugin."},
		{"repository credentials", map[string]string{"token": "$argocd-repo-creds-forgejo:password", "baseUrl": "http://p.argocd.svc"},
			"its token is the key password of the Secret argocd-repo-creds-forgejo"},
		{"token as text", map[string]string{"token": "abc", "baseUrl": "https://plugin.example"}, "it has the token as text"},
		{"no TLS", map[string]string{"token": "$plugin.x.token", "baseUrl": "http://plugin.example"}, "it sends its token to http://plugin.example without TLS"},
		{"no address", map[string]string{"token": "$plugin.x.token"}, "it has no baseUrl"},
		{"other scheme", map[string]string{"baseUrl": "ftp://plugin.example"}, `its baseUrl "ftp://plugin.example" is not an http or https address`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := checkArgoPlugin(tt.data)
			if !strings.Contains(strings.Join(p, " | "), tt.want) {
				t.Fatalf("problems = %q, want %q", p, tt.want)
			}
			for _, m := range p {
				if strings.Contains(m, ";") {
					t.Errorf("the message has a semicolon: %q", m)
				}
			}
		})
	}
}

// TestCheckArgoAppSetsPlugins checks that the guard finds the ConfigMaps
// of the plugin generators, also in a matrix.
func TestCheckArgoAppSetsPlugins(t *testing.T) {
	plugin := func(name string) map[string]any {
		return map[string]any{"plugin": map[string]any{"configMapRef": map[string]any{"name": name},
			"input": map[string]any{"parameters": map[string]any{"repoURL": "http://forgejo.localhost:30300/squared/platform.git"}}}}
	}
	a := appSet(t, func(s map[string]any) { s["generators"] = []any{plugin("one")} })
	b := appSet(t, func(s map[string]any) {
		s["generators"] = []any{map[string]any{"matrix": map[string]any{"generators": []any{plugin("one"), plugin("two")}}}}
	})
	var obj map[string]any
	if err := json.Unmarshal(b, &obj); err != nil {
		t.Fatal(err)
	}
	obj["metadata"].(map[string]any)["name"] = "other"
	p, _, plugins, err := checkArgoAppSets([]json.RawMessage{a, projectJSON(t, obj)}, devAppRules, squaredNames, argoSetPolicy{})
	if err != nil || len(p) != 0 {
		t.Fatalf("problems = %q, err %v", p, err)
	}
	if !slices.Equal(plugins[configMapUse{"plugin", "one"}], []string{"team", "other"}) ||
		!slices.Equal(plugins[configMapUse{"plugin", "two"}], []string{"other"}) || len(plugins) != 2 {
		t.Fatalf("plugins = %v", plugins)
	}

	templatedName := appSet(t, func(s map[string]any) { s["generators"] = []any{plugin("{{.plugin}}")} })
	p, _, _, err = checkArgoAppSets([]json.RawMessage{templatedName}, devAppRules, squaredNames, argoSetPolicy{})
	if err != nil || !strings.Contains(strings.Join(p, " | "), `the ConfigMap "{{.plugin}}" of a plugin generator comes from other generators`) {
		t.Fatalf("problems = %q, err %v", p, err)
	}
}

func TestCheckArgoDecision(t *testing.T) {
	// The example of the documentation of Argo CD (Open Cluster Management).
	ocm := map[string]string{"apiVersion": "cluster.open-cluster-management.io/v1beta1", "kind": "placementdecisions",
		"statusListKey": "decisions", "matchKey": "clusterName"}
	if p, w := checkArgoDecision(ocm); len(p) != 0 || len(w) != 0 {
		t.Fatalf("problems = %q, warnings = %q", p, w)
	}
	core := map[string]string{"apiVersion": "apps/v1", "kind": "deployments", "statusListKey": "conditions", "matchKey": "type"}
	if p, w := checkArgoDecision(core); len(p) != 0 || len(w) != 0 {
		t.Fatalf("problems = %q, warnings = %q", p, w)
	}

	apps := map[string]string{"apiVersion": "argoproj.io/v1alpha1", "kind": "applications", "statusListKey": "resources", "matchKey": "name"}
	if p, _ := checkArgoDecision(apps); len(p) != 1 || !strings.Contains(p[0], "it takes the decisions from the status of the Argo CD applications, which has data from the manifests of the tenants") {
		t.Fatalf("problems = %q", p)
	}
	if p, w := checkArgoDecision(map[string]string{"apiVersion": "apps/v1", "kind": "deployments"}); len(p) != 0 || len(w) != 1 ||
		!strings.Contains(w[0], "it has no statusListKey, matchKey, thus the generator makes no applications") {
		t.Fatalf("problems = %q, warnings = %q", p, w)
	}
}

// TestCheckArgoAppSetsDecisions checks that the guard finds the ConfigMaps
// of the cluster decision resource generators, which are a name, not an
// object.
func TestCheckArgoAppSetsDecisions(t *testing.T) {
	set := appSet(t, func(s map[string]any) {
		s["generators"] = []any{
			map[string]any{"clusterDecisionResource": map[string]any{"configMapRef": "ocm", "labelSelector": map[string]any{}}},
			map[string]any{"matrix": map[string]any{"generators": []any{
				map[string]any{"list": map[string]any{"elements": []any{map[string]any{"cm": "x"}}}},
				map[string]any{"clusterDecisionResource": map[string]any{"configMapRef": "{{.cm}}", "name": "x"}}}}},
		}
	})
	p, _, refs, err := checkArgoAppSets([]json.RawMessage{set}, devAppRules, squaredNames, argoSetPolicy{})
	if err != nil || !strings.Contains(strings.Join(p, " | "), `the ConfigMap "{{.cm}}" of a clusterDecisionResource generator comes from other generators`) {
		t.Fatalf("problems = %q, err %v", p, err)
	}
	if !slices.Equal(refs[configMapUse{"clusterDecisionResource", "ocm"}], []string{"team"}) || len(refs) != 1 {
		t.Fatalf("refs = %v", refs)
	}
}
