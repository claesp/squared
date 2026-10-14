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
	if !slices.Equal(plugins["one"], []string{"team", "other"}) || !slices.Equal(plugins["two"], []string{"other"}) || len(plugins) != 2 {
		t.Fatalf("plugins = %v", plugins)
	}

	templatedName := appSet(t, func(s map[string]any) { s["generators"] = []any{plugin("{{.plugin}}")} })
	p, _, _, err = checkArgoAppSets([]json.RawMessage{templatedName}, devAppRules, squaredNames, argoSetPolicy{})
	if err != nil || !strings.Contains(strings.Join(p, " | "), `the ConfigMap "{{.plugin}}" of a plugin generator comes from other generators`) {
		t.Fatalf("problems = %q, err %v", p, err)
	}
}
